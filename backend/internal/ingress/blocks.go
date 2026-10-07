package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// BlocksFile sits beside ingress.db; backups collect it with the state root.
const BlocksFile = "sender-blocks.json"

const (
	// MaxBlocks is the Worker's blockedSenders limit.
	MaxBlocks = 5000
	// maxBlockWire is the blocks' exact share of the Worker's 1 MiB table
	// (half), leaving the rest for routes. The publisher still truncates by
	// priority if routes grow into it (cfreceiving.FitBlocks).
	maxBlockWire = 512 << 10
)

var (
	ErrBlockInvalid = errors.New("invalid sender block: kind is address or domain; an address is a sender KyPost accepts (dot-atom local part, ASCII domain; no quoted local parts or domain literals), a domain is ASCII (A-labels for internationalized names); until must be in the future")
	ErrBlockOwn     = errors.New("refused: that is one of this deployment's own mail domains; blocking it would refuse your own users' mail")
	ErrBlockFull    = errors.New("sender block list is full; remove blocks before adding more")
	ErrBlockStore   = errors.New("receiving is not initialized; run receiving init before managing sender blocks")
	ErrSenderBlock  = errors.New("sender blocked")
)

// Block reasons are codes, never free text: a block records no correspondence.
var blockReasons = []string{"spam", "phishing", "abuse", "other"}

// SenderBlock blocks one envelope sender address or domain until Until (Unix
// milliseconds, the Worker's unit); nil never expires.
type SenderBlock struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	Until     *int64 `json:"until"`
	Source    string `json:"source"`
	Level     int    `json:"level"`
	CreatedAt int64  `json:"createdAt"`
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
}

func (b SenderBlock) active(now int64) bool { return b.Until == nil || *b.Until > now }

// Blocks is the sender block list in an existing receiving directory. Writers
// serialize on its lock file; readers see whole files (rename publish).
type Blocks struct{ dir string }

func NewBlocks(receivingDir string) Blocks { return Blocks{dir: receivingDir} }

func (s Blocks) path() string { return filepath.Join(s.dir, BlocksFile) }

// BlockID is a short stable identifier for a normalized kind/value: audit
// lines carry it instead of the address.
func BlockID(kind, value string) string {
	sum := sha256.Sum256([]byte(kind + ":" + value))
	return hex.EncodeToString(sum[:8])
}

// NormalizeBlock returns the stored form, LowerASCII of the value. Every
// sender either profile accepts (cfreceiving.ValidSender) can be blocked by
// address, and its domain by domain; non-ASCII local parts are kept and
// compared exactly, as the Worker does.
func NormalizeBlock(kind, value string) (string, error) {
	v := cfreceiving.LowerASCII(value)
	switch {
	case kind == "address" && cfreceiving.ValidBlock(cfreceiving.Block{Address: v}),
		kind == "domain" && cfreceiving.ValidBlock(cfreceiving.Block{Domain: v}):
		return v, nil
	}
	return "", ErrBlockInvalid
}

// Wire is the block in the signed table's form.
func (b SenderBlock) Wire() cfreceiving.Block {
	if b.Kind == "domain" {
		return cfreceiving.Block{Domain: b.Value, Until: b.Until}
	}
	return cfreceiving.Block{Address: b.Value, Until: b.Until}
}

// valid is the stored-entry rule; load refuses a file breaking it, so a
// hand-edited list can never publish a table the Worker refuses.
func (b SenderBlock) valid() bool {
	v, err := NormalizeBlock(b.Kind, b.Value)
	return err == nil && v == b.Value && b.ID == BlockID(b.Kind, b.Value) && (b.Until == nil || *b.Until >= 1 && *b.Until <= 1<<53-1) &&
		(b.Source == "manual" && b.Level == 0 || b.Source == "automatic" && b.Level >= 1) && b.CreatedAt >= 1 &&
		identifier(b.Actor) && len(b.Actor) <= 128 && slices.Contains(blockReasons, b.Reason)
}

func (s Blocks) load() ([]SenderBlock, error) {
	raw, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseBlocks(raw)
}

// ParseBlocks is the file rule load enforces; backups validate collected
// bytes with it.
func ParseBlocks(raw []byte) ([]SenderBlock, error) {
	var doc struct {
		Version int           `json:"version"`
		Blocks  []SenderBlock `json:"blocks"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&doc) != nil || d.Decode(new(any)) != io.EOF || doc.Version != 1 || len(doc.Blocks) > MaxBlocks {
		return nil, errors.New("sender block list is malformed; restore it from backup or remove it")
	}
	seen, wire := map[string]bool{}, 0
	for _, b := range doc.Blocks {
		wire += cfreceiving.BlockWireBytes(b.Wire())
		if !b.valid() || seen[b.ID] || wire > maxBlockWire {
			return nil, errors.New("sender block list is malformed; restore it from backup or remove it")
		}
		seen[b.ID] = true
	}
	return doc.Blocks, nil
}

// List returns blocks still in force, by kind then value.
func (s Blocks) List(now time.Time) ([]SenderBlock, error) {
	all, err := s.load()
	out := slices.DeleteFunc(all, func(b SenderBlock) bool { return !b.active(now.UnixMilli()) })
	slices.SortFunc(out, func(a, b SenderBlock) int { return strings.Compare(a.Kind+":"+a.Value, b.Kind+":"+b.Value) })
	return out, err
}

// Blocked reports whether an envelope sender is blocked now: its whole
// address or exactly its domain (never a parent domain), compared after
// LowerASCII as the Worker does. The null sender has no address or domain
// and is never blocked, so bounces for mail this deployment sent arrive.
// ponytail: re-reads and parses the list on every RCPT; fine up to the
// 512 KiB cap at household volume. Past that, cache it by file inode and
// mtime (the publish-by-rename already changes both).
func (s Blocks) Blocked(sender string, now time.Time) (bool, error) {
	at := strings.LastIndexByte(sender, '@')
	if sender == "" || at < 0 {
		return false, nil
	}
	address := cfreceiving.LowerASCII(sender)
	blocks, err := s.List(now)
	return slices.ContainsFunc(blocks, func(b SenderBlock) bool {
		return b.Kind == "address" && b.Value == address || b.Kind == "domain" && b.Value == address[at+1:]
	}), err
}

// Put adds or replaces the block for b.Kind/b.Value. own is the deployment's
// mail domains: neither they nor addresses on them can be blocked. An
// automatic block (Evidence.Reject) never replaces or duplicates a manual
// block that covers it. Expired entries are dropped.
func (s Blocks) Put(ctx context.Context, b SenderBlock, own []string, now time.Time) (SenderBlock, error) {
	value, err := NormalizeBlock(b.Kind, b.Value)
	if err != nil {
		return SenderBlock{}, err
	}
	if b.Reason == "" {
		b.Reason = "other"
	}
	b.Value, b.ID, b.CreatedAt = value, BlockID(b.Kind, value), now.UnixMilli()
	if !b.valid() || !b.active(b.CreatedAt) {
		return SenderBlock{}, ErrBlockInvalid
	}
	if slices.Contains(own, value[strings.LastIndexByte(value, '@')+1:]) {
		return SenderBlock{}, ErrBlockOwn
	}
	return b, s.change(ctx, now, func(blocks []SenderBlock) ([]SenderBlock, error) {
		at := strings.LastIndexByte(b.Value, '@')
		if b.Source == "automatic" && slices.ContainsFunc(blocks, func(x SenderBlock) bool {
			return x.Source == "manual" && (x.ID == b.ID || at >= 0 && x.Kind == "domain" && x.Value == b.Value[at+1:])
		}) {
			return nil, errManualCovers
		}
		blocks = slices.DeleteFunc(blocks, func(x SenderBlock) bool { return x.ID == b.ID })
		blocks = append(blocks, b)
		wire := 0
		for _, x := range blocks {
			wire += cfreceiving.BlockWireBytes(x.Wire())
		}
		if len(blocks) > MaxBlocks || wire > maxBlockWire {
			return nil, ErrBlockFull
		}
		return blocks, nil
	})
}

// Remove deletes the block with this ID (BlockID of its normalized
// kind/value); false when none is in force. Removing one first suppresses
// automatic re-blocking of it for UnblockWindow, so the administrator's
// decision stands even if the removal is interrupted.
func (s Blocks) Remove(ctx context.Context, id string, now time.Time) (bool, error) {
	if raw, err := hex.DecodeString(id); err != nil || len(raw) != 8 || hex.EncodeToString(raw) != id {
		return false, ErrBlockInvalid
	}
	list, err := s.List(now)
	if i := slices.IndexFunc(list, func(x SenderBlock) bool { return x.ID == id }); err == nil && i >= 0 {
		err = NewEvidence(s.dir).unblocked(ctx, list[i].Kind, list[i].Value, now)
	}
	if err != nil {
		return false, err
	}
	found := false
	err = s.change(ctx, now, func(blocks []SenderBlock) ([]SenderBlock, error) {
		before := len(blocks)
		blocks = slices.DeleteFunc(blocks, func(x SenderBlock) bool { return x.ID == id })
		found = len(blocks) < before
		return blocks, nil
	})
	return found, err
}

// change is the locked read-modify-write. It never creates the receiving
// directory: initialization alone does, and refuses one that exists.
func (s Blocks) change(ctx context.Context, now time.Time, edit func([]SenderBlock) ([]SenderBlock, error)) error {
	if info, err := os.Lstat(s.dir); err != nil || !info.IsDir() {
		return ErrBlockStore
	}
	release, err := fsutil.LockFileContext(ctx, s.path())
	if err != nil {
		return err
	}
	defer release()
	blocks, err := s.List(now)
	if err != nil {
		return err
	}
	if blocks, err = edit(blocks); err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Version int           `json:"version"`
		Blocks  []SenderBlock `json:"blocks"`
	}{1, append([]SenderBlock{}, blocks...)})
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(s.path(), raw, 0o600)
}
