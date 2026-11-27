package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	goimap "github.com/BrianLeishman/go-imap"
)

var (
	// ErrCASMismatch indicates the stored raw message bytes changed before
	// compare-and-swap replacement could commit.
	ErrCASMismatch = errors.New("stored message hash mismatch")
	// ErrMessageNotFound indicates the target message UID is absent in the folder.
	ErrMessageNotFound = errors.New("message not found")
)

// ArchiveReencryptClient is implemented by mail backends that support Spec A
// Phase P6 archive re-encryption: scanning folders, reading exact stored raw
// bytes, and replacing a message atomically with compare-and-swap.
type ArchiveReencryptClient interface {
	ListAllFolders(ctx context.Context) ([]string, error)
	ListFolderMessageUIDs(ctx context.Context, folder string, afterUID int, limit int) ([]int, error)
	GetRawMessage(ctx context.Context, folder string, uid int) ([]byte, error)
	ReplaceRawMessageCAS(ctx context.Context, folder string, uid int, expectedSHA256 string, newRaw []byte) (newUID int, err error)
}

var _ ArchiveReencryptClient = (*APIClient)(nil)

// ListAllFolders returns all selectable mailbox folder names in deterministic order,
// filtering out IMAP \Noselect and \NonExistent hierarchy containers.
func (c *APIClient) ListAllFolders(ctx context.Context) ([]string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []string
	_, err = d.Exec(`LIST "" "*"`, false, 0, func(line []byte) error {
		attrs, _, name, ok := parseListLine(string(line))
		if !ok {
			return nil
		}
		for _, attr := range attrs {
			lower := strings.ToLower(attr)
			if lower == `\noselect` || lower == `\nonexistent` {
				return nil
			}
		}
		if name == "" || seen[name] || ValidateMailboxName(name) != nil {
			return nil
		}
		seen[name] = true
		out = append(out, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ErrCondstoreUnsupported is returned when the IMAP server cannot perform
// race-safe conditional deletion (CONDSTORE / usable MODSEQ).
var ErrCondstoreUnsupported = errors.New("IMAP server lacks CONDSTORE/IMAP4rev2 conditional deletion; refusing archive replacement")

// Matches FLAGS (...), MODSEQ (n), and UID n in untagged FETCH responses.
var (
	flagsResponse    = regexp.MustCompile(`(?i)\bFLAGS\s+\(([^)]*)\)`)
	modSeqResponse   = regexp.MustCompile(`(?i)\bMODSEQ\s+\((\d+)\)`)
	uidTokenResponse = regexp.MustCompile(`(?i)\bUID\s+(\d+)\b`)
)

// fetchResponseHasDeletedUID returns true only when storeResp contains an
// untagged FETCH response for the exact target uid whose FLAGS list includes
// \Deleted. Unsolicited FETCH updates for other UIDs cannot satisfy this check.
func fetchResponseHasDeletedUID(storeResp string, uid int) bool {
	wantUID := strconv.Itoa(uid)
	for _, rawLine := range strings.Split(storeResp, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "*") || !strings.Contains(strings.ToUpper(line), "FETCH") {
			continue
		}
		uidMatch := uidTokenResponse.FindStringSubmatch(line)
		if len(uidMatch) != 2 || uidMatch[1] != wantUID {
			continue
		}
		flagMatch := flagsResponse.FindStringSubmatch(line)
		if len(flagMatch) != 2 {
			continue
		}
		for _, tok := range strings.Fields(flagMatch[1]) {
			if strings.EqualFold(tok, `\Deleted`) {
				return true
			}
		}
	}
	return false
}

func supportsCondstore(d *goimap.Dialer) bool {
	resp, err := d.Exec("CAPABILITY", true, 0, nil)
	if err != nil {
		return false
	}
	for _, tok := range strings.Fields(strings.ToUpper(resp)) {
		if tok == "CONDSTORE" || tok == "IMAP4REV2" {
			return true
		}
	}
	return false
}

// fetchFlagsAndModSeq captures FLAGS and MODSEQ atomically in a single
// UID FETCH (UID FLAGS MODSEQ) snapshot so no flag mutation can slip between
// reading flags and obtaining the MODSEQ precondition, ignoring any
// unsolicited FETCH updates for unrelated UIDs.
func fetchFlagsAndModSeq(d *goimap.Dialer, uid int) ([]string, uint64, error) {
	resp, err := d.Exec(fmt.Sprintf("UID FETCH %d (UID FLAGS MODSEQ)", uid), true, 0, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch original flags and MODSEQ: %w", err)
	}
	return parseFetchFlagsAndModSeqForUID(resp, uid)
}

func parseFetchFlagsAndModSeqForUID(resp string, uid int) ([]string, uint64, error) {
	wantUID := strconv.Itoa(uid)
	sawTargetUID := false
	for _, rawLine := range strings.Split(resp, "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "*") || !strings.Contains(strings.ToUpper(line), "FETCH") {
			continue
		}
		uidMatch := uidTokenResponse.FindStringSubmatch(line)
		if len(uidMatch) != 2 || uidMatch[1] != wantUID {
			continue
		}
		sawTargetUID = true
		modMatch := modSeqResponse.FindStringSubmatch(line)
		if len(modMatch) != 2 {
			continue
		}
		modSeq, err := strconv.ParseUint(modMatch[1], 10, 64)
		if err != nil || modSeq == 0 {
			return nil, 0, ErrCondstoreUnsupported
		}
		flagMatch := flagsResponse.FindStringSubmatch(line)
		if len(flagMatch) != 2 {
			continue
		}
		rawFlags := strings.Fields(flagMatch[1])
		for _, f := range rawFlags {
			if strings.EqualFold(f, `\Deleted`) {
				return nil, 0, fmt.Errorf("%w: original message is already marked deleted", ErrCASMismatch)
			}
		}
		flags, err := incomingFlags(rawFlags)
		if err != nil {
			return nil, 0, err
		}
		return flags, modSeq, nil
	}
	if sawTargetUID {
		return nil, 0, ErrCondstoreUnsupported
	}
	return nil, 0, errors.New("cannot parse original message flags and MODSEQ for target UID")
}

// finishExpungeLocked executes UID EXPUNGE for uid and, on failure (such as a
// dropped connection after the server already processed the expunge or a
// transient error), reconnects once to verify whether uid is already gone and
// retries UID EXPUNGE if it is still present.
func (c *APIClient) finishExpungeLocked(d *goimap.Dialer, folder string, uid int) error {
	expungeCmd := fmt.Sprintf("UID EXPUNGE %d", uid)
	if _, err := d.Exec(expungeCmd, false, 0, nil); err == nil {
		return nil
	}
	if c.dialer != nil {
		_ = c.dialer.Close()
		c.dialer = nil
	}
	fresh, err := c.ensureConnectedLocked()
	if err != nil {
		return err
	}
	if err := fresh.SelectFolder(folder); err != nil {
		return err
	}
	found, searchErr := incomingSearch(fresh, "UID "+strconv.Itoa(uid))
	if searchErr == nil && len(found) == 0 {
		return nil
	}
	if _, err := fresh.Exec(expungeCmd, false, 0, nil); err != nil {
		return err
	}
	return nil
}

func (c *APIClient) cleanupCopyUIDLocked(d *goimap.Dialer, folder string, copyUID int) {
	if copyUID <= 0 {
		return
	}
	storeCmd := fmt.Sprintf(`UID STORE %d +FLAGS.SILENT (\Deleted)`, copyUID)
	expungeCmd := fmt.Sprintf("UID EXPUNGE %d", copyUID)
	if _, err := d.Exec(storeCmd, false, 0, nil); err == nil {
		_, _ = d.Exec(expungeCmd, false, 0, nil)
		return
	}
	// go-imap closes the underlying connection after command errors (e.g. NO
	// from a failed verification FETCH). Reconnect once so cleanup still
	// expunges the appended replacement instead of leaking a duplicate.
	if c.dialer != nil {
		_ = c.dialer.Close()
		c.dialer = nil
	}
	fresh, err := c.ensureConnectedLocked()
	if err != nil {
		return
	}
	if err := fresh.SelectFolder(folder); err != nil {
		return
	}
	if _, err := fresh.Exec(storeCmd, false, 0, nil); err == nil {
		_, _ = fresh.Exec(expungeCmd, false, 0, nil)
	}
}

const uidScanCacheTTL = 60 * time.Second

type folderUIDScanCache struct {
	folder    string
	afterUID  int
	uids      []int
	fetchedAt time.Time
}

// ListFolderMessageUIDs returns up to limit message UIDs strictly greater than
// afterUID in ascending order for folder. Within a folder scan it reuses a
// short-lived sorted UID snapshot so paginating a large mailbox does not
// re-download the remaining UID suffix on every batch.
func (c *APIClient) ListFolderMessageUIDs(ctx context.Context, folder string, afterUID int, limit int) ([]int, error) {
	if err := ValidateMailboxName(folder); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var sortedUIDs []int
	if afterUID > 0 &&
		c.uidScanCache != nil &&
		c.uidScanCache.folder == folder &&
		afterUID >= c.uidScanCache.afterUID &&
		time.Since(c.uidScanCache.fetchedAt) < uidScanCacheTTL {
		sortedUIDs = c.uidScanCache.uids
	} else {
		d, err := c.ensureConnectedLocked()
		if err != nil {
			return nil, err
		}
		if err := d.SelectFolder(folder); err != nil {
			return nil, err
		}
		startUID := afterUID + 1
		if startUID < 1 {
			startUID = 1
		}
		uids, err := incomingSearch(d, fmt.Sprintf("NOT DELETED UID %d:*", startUID))
		if err != nil {
			return nil, err
		}
		filtered := make([]int, 0, len(uids))
		for _, uid := range uids {
			if uid > afterUID {
				filtered = append(filtered, uid)
			}
		}
		sort.Ints(filtered)
		c.uidScanCache = &folderUIDScanCache{
			folder:    folder,
			afterUID:  afterUID,
			uids:      filtered,
			fetchedAt: time.Now(),
		}
		sortedUIDs = filtered
	}

	idx := sort.SearchInts(sortedUIDs, afterUID+1)
	if idx >= len(sortedUIDs) {
		c.uidScanCache = nil
		return nil, nil
	}
	end := idx + limit
	if end > len(sortedUIDs) {
		end = len(sortedUIDs)
	}
	out := make([]int, end-idx)
	copy(out, sortedUIDs[idx:end])
	if end >= len(sortedUIDs) {
		c.uidScanCache = nil
	}
	return out, nil
}

// GetRawMessage fetches the exact stored RFC 5322 bytes for (folder, uid).
func (c *APIClient) GetRawMessage(ctx context.Context, folder string, uid int) ([]byte, error) {
	if err := ValidateMailboxName(folder); err != nil {
		return nil, err
	}
	if uid <= 0 {
		return nil, ErrMessageNotFound
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	if err := d.SelectFolder(folder); err != nil {
		return nil, err
	}
	found, err := incomingSearch(d, "UID "+strconv.Itoa(uid))
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrMessageNotFound
	}
	return incomingRaw(d, uid)
}

// ReplaceRawMessageCAS verifies that (folder, uid) still has SHA-256 equal to
// expectedSHA256, appends newRaw preserving flags and INTERNALDATE, verifies
// the appended copy byte-for-byte, and removes only the original UID with
// UIDPLUS UID EXPUNGE.
func (c *APIClient) ReplaceRawMessageCAS(ctx context.Context, folder string, uid int, expectedSHA256 string, newRaw []byte) (int, error) {
	if err := ValidateMailboxName(folder); err != nil {
		return 0, err
	}
	if uid <= 0 {
		return 0, ErrMessageNotFound
	}
	if len(newRaw) == 0 {
		return 0, errors.New("empty replacement message")
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return 0, err
	}
	if err := d.SelectFolder(folder); err != nil {
		return 0, err
	}
	supported, err := supportsUIDExpunge(d)
	if err != nil {
		return 0, fmt.Errorf("check UIDPLUS capability: %w", err)
	}
	if !supported {
		return 0, errors.New("IMAP server lacks UIDPLUS/IMAP4rev2 UID EXPUNGE; refusing archive replacement")
	}
	if !supportsCondstore(d) {
		return 0, ErrCondstoreUnsupported
	}

	found, err := incomingSearch(d, "UID "+strconv.Itoa(uid))
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, ErrMessageNotFound
	}

	oldRaw, err := incomingRaw(d, uid)
	if err != nil {
		return 0, err
	}
	if !strings.EqualFold(incomingHash(oldRaw), strings.TrimSpace(expectedSHA256)) {
		return 0, ErrCASMismatch
	}

	// Capture FLAGS and MODSEQ atomically in one UID FETCH snapshot so no
	// concurrent flag change can slip between reading flags and MODSEQ, and
	// refuse to append again if uid is already marked \Deleted.
	flags, origModSeq, err := fetchFlagsAndModSeq(d, uid)
	if err != nil {
		return 0, err
	}
	ov, err := incomingMetadata(d, uid)
	if err != nil || ov == nil {
		return 0, errors.New("cannot read original message date")
	}
	date := ov.Received
	if date.IsZero() {
		date = time.Now().UTC()
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	appendedUID, err := d.AppendUID(folder, flags, date, newRaw)
	if err != nil {
		return 0, fmt.Errorf("append replacement message: %w", err)
	}

	// Require an unambiguous RFC 4315 APPENDUID from UIDPLUS/IMAP4rev2 rather
	// than guessing a UID by byte equality, which could match another session's
	// concurrent append and delete an unrelated message on rollback.
	if appendedUID <= 0 || appendedUID == uid {
		return 0, errors.New("IMAP server omitted APPENDUID for replacement; refusing ambiguous replacement")
	}
	candRaw, err := incomingRaw(d, appendedUID)
	if err != nil || !bytes.Equal(candRaw, newRaw) {
		c.cleanupCopyUIDLocked(d, folder, appendedUID)
		if err != nil {
			return 0, fmt.Errorf("verify appended replacement message: %w", err)
		}
		return 0, errors.New("appended replacement message failed byte verification; original preserved")
	}
	copyUID := appendedUID

	if len(flags) > 0 {
		if _, err := d.Exec(fmt.Sprintf("UID STORE %d FLAGS.SILENT (%s)", copyUID, strings.Join(flags, " ")), false, 0, nil); err != nil {
			c.cleanupCopyUIDLocked(d, folder, copyUID)
			return 0, fmt.Errorf("preserve flags on replacement copy: %w", err)
		}
	}

	// Require RFC 7162 conditional deletion against the atomically captured
	// MODSEQ so concurrent flag updates on the original are never silently lost.
	// Use non-silent +FLAGS with buildResponse=true because go-imap omits the
	// tagged OK [MODIFIED] line; the server emits an untagged FETCH carrying
	// \Deleted only when UNCHANGEDSINCE succeeded.
	storeResp, err := d.Exec(fmt.Sprintf(`UID STORE %d (UNCHANGEDSINCE %d) +FLAGS (\Deleted)`, uid, origModSeq), true, 0, nil)
	if err != nil {
		c.cleanupCopyUIDLocked(d, folder, copyUID)
		return 0, fmt.Errorf("mark original message deleted: %w", err)
	}
	if !fetchResponseHasDeletedUID(storeResp, uid) {
		c.cleanupCopyUIDLocked(d, folder, copyUID)
		return 0, ErrCASMismatch
	}
	if err := c.finishExpungeLocked(d, folder, uid); err != nil {
		return 0, fmt.Errorf("expunge original message after replacement %d committed: %w", copyUID, err)
	}
	return copyUID, nil
}
