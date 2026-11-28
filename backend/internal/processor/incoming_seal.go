package processor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// IncomingPublicKey is the key new incoming mail for u is encrypted to: the v6 key only
// once every enrolled device can read v6 (PQC v6 design), the v4 key until then.
func IncomingPublicKey(u users.User, devices func() (*state.Store, error)) (string, error) {
	if u.PGPPublicKeyV6 != "" && u.PGPKeyring != nil && u.PGPKeyring.Version == 2 && pgpmail.EncryptsNow(u.PGPPublicKeyV6) {
		st, err := devices()
		if err != nil {
			return "", err
		}
		ready, err := st.PGPV6Ready(u.PGPKeyring.MaterialGeneration, u.PGPFingerprint)
		if err != nil {
			return "", err
		}
		if ready {
			return u.PGPPublicKeyV6, nil
		}
	}
	return u.PGPPublicKey, nil
}

// errNoIncomingKey: u has no key mail can be encrypted to (none, expired, revoked, weak
// RSA). Its mail is kept as received until the backlog sweep seals it.
var errNoIncomingKey = errors.New("no usable key to encrypt incoming mail to")

// sealTo returns raw encrypted to u's incoming key, or nil when raw is already
// PGP-encrypted and stays as it is.
func sealTo(u users.User, raw []byte, devices func() (*state.Store, error)) ([]byte, error) {
	if usableIncomingKey(u) != nil {
		return nil, errNoIncomingKey
	}
	if parsed, err := imapadapter.ParseRawContent(raw); err == nil && parsed.Content.PGPEncrypted {
		return nil, nil
	}
	key, err := IncomingPublicKey(u, devices)
	if err != nil {
		return nil, err
	}
	sealed, err := pgpmail.EncryptStoredMIME(raw, key)
	if errors.Is(err, pgpmail.ErrWeakRSAKey) {
		return nil, errNoIncomingKey
	}
	if err != nil {
		return nil, errors.New("incoming encryption failed; mail not stored, check the PGP key")
	}
	return sealed, nil
}

// IncomingSeal is the mailbox seal every mailbox-writing process installs (E2E_PGP.md,
// "Incoming mail encryption"): mail for an account with a usable key is encrypted to it
// before it is stored. global holds the admin policy; off stores mail as received.
func IncomingSeal(accounts *users.Store, stateDir string, global *state.Store) mailbox.Seal {
	return func(ctx context.Context, owner mailbox.Owner, raw []byte) ([]byte, error) {
		if global != nil && global.IncomingEncryptionOff() {
			return raw, nil
		}
		// Inside the users fence the owner comes from its snapshot: calling the Store
		// there deadlocks. An unreadable owner refuses: the mail stays in the buffer.
		u, ok := users.MailboxOwner(ctx, owner.Mailbox)
		if !ok || u.SSOSub != owner.Subject {
			var err error
			if u, err = accounts.GetBySSOSubIssuer(owner.Issuer, owner.Subject); err != nil {
				return nil, fmt.Errorf("incoming encryption cannot read the mailbox owner: %w", err)
			}
		}
		var devices *state.Store
		sealed, err := sealTo(u, raw, func() (*state.Store, error) {
			st, err := state.OpenReadOnly(config.UserDir(stateDir, u.ID))
			devices = st
			return st, err
		})
		if devices != nil {
			_ = devices.Close()
		}
		if sealed == nil && (err == nil || errors.Is(err, errNoIncomingKey)) {
			return raw, nil
		}
		return sealed, err
	}
}

// backlogBatch bounds one tick's sweep. ponytail: a fixed count, not bytes; a backlog of
// large mail takes longer per tick, never more memory than one message at a time.
var backlogBatch = 100

// backlogSealer is the native mailbox surface the sweep needs.
type backlogSealer interface {
	MessagesAfter(context.Context, int64, int) ([]mailbox.Ref, error)
	GetRawMessage(context.Context, string, int) ([]byte, error)
	ReplaceRawMessageCAS(context.Context, string, int, string, []byte) (int, error)
	MailSourceIdentity() string
	Scrub(context.Context)
}

// backlogCache is the mail cache surface the sweep scrubs.
type backlogCache interface {
	OmitBodies() error
	Remove(mailbox string, uid int) error
}

type backlogCursor struct {
	Source string `json:"source"`
	After  int64  `json:"after"`
}

// sealBacklog encrypts mail a native mailbox holds in plaintext (received while its
// owner had no usable key) once the owner has one: up to backlogBatch messages a tick,
// in ID order, resuming from a cursor bound to the mailbox's source. Idempotent: mail
// already encrypted is skipped, and each replacement is compare-and-swap on the bytes.
func (p *Poller) sealBacklog(ctx context.Context, uc userCtx, cache backlogCache) error {
	mail, ok := uc.mail.(backlogSealer)
	if !ok || uc.mailbox == "" || (p.globalStore != nil && p.globalStore.IncomingEncryptionOff()) {
		return nil
	}
	u, err := p.users.Get(uc.id)
	if err != nil {
		return err
	}
	if usableIncomingKey(u) != nil {
		return nil
	}
	doc := vol.OpenDocument(filepath.Join(p.userStateDir(uc.id), "sealed-backlog-"+uc.mailbox+".json"))
	var cur backlogCursor
	if body, err := doc.Read(); err == nil {
		if json.Unmarshal(body, &cur) != nil {
			cur = backlogCursor{}
		}
	}
	// A recreated or restored mailbox is a new ID space: start over (sealed mail is skipped).
	if cur.Source != mail.MailSourceIdentity() {
		cur = backlogCursor{Source: mail.MailSourceIdentity()}
	}
	refs, err := mail.MessagesAfter(ctx, cur.After, backlogBatch)
	if err != nil || len(refs) == 0 {
		return err
	}
	devices := func() (*state.Store, error) { return uc.primary, nil }
	cleared := false
	for _, ref := range refs {
		raw, err := mail.GetRawMessage(ctx, ref.Folder, int(ref.ID))
		if errors.Is(err, imapadapter.ErrMessageNotFound) {
			// Moved (same ID, new folder) or deleted since listed: keep the cursor so the
			// next tick re-lists it; a deleted one is simply not listed again.
			break
		}
		if err != nil {
			return err
		}
		sealed, err := sealTo(u, raw, devices)
		if errors.Is(err, errNoIncomingKey) {
			break // resume when there is a usable key
		}
		if err != nil {
			return err
		}
		if sealed != nil {
			if !cleared {
				if err := cache.OmitBodies(); err != nil {
					return err
				}
				cleared = true
			}
			sum := sha256.Sum256(raw)
			_, err = mail.ReplaceRawMessageCAS(ctx, ref.Folder, int(ref.ID), hex.EncodeToString(sum[:]), sealed)
			switch {
			case errors.Is(err, mailmsg.ErrMessageTooLarge):
				// Never silent: the one case that stays plaintext, logged per message.
				p.log.Error("stored message too large to encrypt within the mailbox limit; it stays as received", "user_id", uc.id, "message_id", fmt.Sprint(ref.ID))
			case errors.Is(err, imapadapter.ErrCASMismatch), errors.Is(err, imapadapter.ErrMessageNotFound):
				return saveBacklog(doc, cur) // changed under us: retry it next tick
			case err != nil:
				return err
			default:
				// Drop the cached entry, in whichever folder's window it sits: it holds
				// the plaintext subject. The next sync re-adds it from the sealed row.
				if err := cache.Remove("", int(ref.ID)); err != nil {
					return err
				}
			}
		}
		cur.After = ref.ID
	}
	if cleared {
		mail.Scrub(ctx)
	}
	return saveBacklog(doc, cur)
}

func saveBacklog(doc *vol.Document, cur backlogCursor) error {
	data, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return doc.Write(data)
}
