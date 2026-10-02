package processor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/adapters/classifier"
	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

var errIncomingRateLimited = errors.New("incoming classification rate limit reached")

var errIncomingPermanent = errors.New("incoming message cannot be encrypted; original remains plaintext")

type incomingEncryptionErr struct{ err error }

func (e *incomingEncryptionErr) Error() string { return e.err.Error() }
func (e *incomingEncryptionErr) Unwrap() error { return e.err }

// Only the production adapter and encryption-specific tests implement this.
// Existing Client implementations do not gain unrelated methods.
type incomingEncryptor interface {
	PrepareIncoming(context.Context, int, bool) (imapadapter.IncomingSource, error)
	ReplaceIncoming(context.Context, imapadapter.IncomingSource, string, []byte) (int, error)
	ApplyIncomingAction(context.Context, imapadapter.IncomingSource, int, string, []byte, string, string) error
}

type incomingJob struct {
	Version        int
	Fingerprint    string
	Source         imapadapter.IncomingSource
	Marker         string
	Ciphertext     []byte
	ReplacementUID int
	LabelsApplied  bool
	Decision       state.Decision
	Keywords       []string
	Actions        []rules.Action
	NextAction     int
}

func (p *Poller) incomingJobPath(userID string) string {
	return filepath.Join(p.userStateDir(userID), "incoming-encryption.json")
}

func usableIncomingKey(u users.User) error {
	if !u.Active || u.PGPProtection() != users.PGPProtectionClient || strings.TrimSpace(u.PGPPrivateKeyWrapped) == "" || u.PGPFingerprint == "" {
		return errors.New("incoming encryption requires an active account with a client-protected PGP key; set up or recover your key in Security")
	}
	status, err := pgpmail.CheckKeyStatus(u.PGPPublicKey)
	if err != nil || !status.Usable() {
		return errors.New("incoming encryption requires a usable, non-revoked, unexpired public key; restore or update your key in Security")
	}
	return nil
}

func saveIncomingJob(path string, job incomingJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

func (p *Poller) encryptIncomingMessage(ctx context.Context, uc userCtx, msg imapadapter.Message) error {
	if _, err := os.Stat(p.incomingJobPath(uc.id)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("an incoming encryption job is pending; retry it before admitting more mail")
	}
	mail, ok := uc.mail.(incomingEncryptor)
	if !ok {
		return errors.New("IMAP adapter does not support incoming encryption")
	}
	u, err := p.users.Get(uc.id)
	if err != nil {
		return errors.New("cannot read incoming encryption identity")
	}
	if err := usableIncomingKey(u); err != nil {
		return err
	}
	cache, err := p.userMailCacheStore(uc.id)
	if err != nil {
		return err
	}
	if err := cache.OmitBodies(); err != nil {
		return err
	}
	uid, err := strconv.Atoi(msg.ID)
	if err != nil || uid <= 0 {
		return fmt.Errorf("%w: invalid incoming UID", errIncomingPermanent)
	}
	input := rules.EvalInput{UID: uid, MessageID: msg.ID, From: msg.Sender, To: msg.SentTo, CC: msg.CC, BCC: msg.BCC, Subject: msg.Subject, Body: msg.Body, Keywords: msg.Keywords, Folder: "INBOX", Headers: uc.headers[uid]}
	outcome := rules.Evaluate(ctx, input, uc.rules)
	if err := validateIncomingActions(outcome.Applied); err != nil {
		return fmt.Errorf("%w: %s", errIncomingPermanent, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	requiresMove := false
	for _, action := range outcome.Applied {
		switch action.Type {
		case "move", "archive", "spam", "delete":
			requiresMove = true
		}
	}
	source, err := mail.PrepareIncoming(ctx, uid, requiresMove)
	if err != nil {
		return err
	}
	encrypted, err := pgpmail.EncryptStoredMIME(source.Raw, u.PGPPublicKey)
	if err != nil {
		return errors.New("public-key encryption failed; original mail preserved, check your PGP key")
	}
	markerBytes := make([]byte, 32)
	if _, err := rand.Read(markerBytes); err != nil {
		return err
	}
	marker := hex.EncodeToString(markerBytes)
	encrypted = append([]byte("X-KyPost-Incoming: "+marker+"\r\n"), encrypted...)
	if int64(len(encrypted)) > mailmsg.MaxInboundMessageBytes {
		return fmt.Errorf("%w: encrypted replacement exceeds the message size limit", errIncomingPermanent)
	}
	source.Raw = nil
	selected, detail := "", "incoming mail encrypted after rule evaluation"
	if !outcome.Stopped {
		if !uc.autoLabelEnabled {
			selected, detail = disabledLabelingFallback(uc.allowlist), "automatic keyword labeling disabled; incoming mail encrypted"
		} else {
			presorted, allowlist, note := presort(uc, msg, uid)
			guess := p.guess(uc, msg, allowlist)
			switch {
			case presorted != "":
				selected = presorted
				detail = "sender is a contact; incoming mail encrypted"
			case guess.confident(p.embedMin):
				selected = guess.label
				detail = "classified by embedding sorter and encrypted incoming mail" + note
			default:
				if !p.allowByRate(uc.id) {
					return errIncomingRateLimited
				}
				label, err := p.classifyMessage(ctx, uc, msg, allowlist)
				if err != nil {
					return err
				}
				selected = classifier.SelectLabelFromText(allowlist, label)
				detail = "classified and encrypted incoming mail" + note
				if selected == "" {
					detail = "no known label returned; incoming mail encrypted" + note
				}
			}

		}
	}
	keywords := keywordsForSelectedLabel(selected, uc.keywordMappings)
	for _, keyword := range keywords {
		if err := imapadapter.ValidateKeyword(keyword); err != nil {
			return fmt.Errorf("%w: invalid configured classification keyword; update labels", errIncomingPermanent)
		}
	}
	job := incomingJob{Version: 1, Fingerprint: u.PGPFingerprint, Source: source, Marker: marker, Ciphertext: encrypted,
		Decision: state.Decision{MessageID: msg.ID, Sender: msg.Sender, SentTo: msg.SentTo, Subject: pgpmail.OuterPlaceholderSubject, Label: selected, Status: "applied", Detail: detail},
		Actions:  outcome.Applied, Keywords: keywords}
	// ponytail: one durable job per account serializes replacements. A blocked job
	// pauses that account; use a bounded queue only if throughput requires it.
	if _, err := p.users.ReserveIncomingEncryption(uc.id, u.PGPFingerprint, u.PGPRevision); err != nil {
		return err
	}
	if err := saveIncomingJob(p.incomingJobPath(uc.id), job); err != nil {
		// AtomicWrite may have committed before directory fsync failed. Preserve
		// the key reservation whenever the durable job might exist.
		if _, statErr := os.Stat(p.incomingJobPath(uc.id)); errors.Is(statErr, os.ErrNotExist) {
			_ = p.users.ReleaseIncomingEncryption(uc.id)
		}
		return err
	}
	return p.resumeIncomingEncryption(ctx, uc)
}

func (p *Poller) resumeIncomingEncryption(ctx context.Context, uc userCtx) error {
	path := p.incomingJobPath(uc.id)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// A crash between reserving the key and writing the job has not touched
		// IMAP. Only the single daemon owns creating jobs for this account.
		if uc.incomingPending {
			return p.users.ReleaseIncomingEncryption(uc.id)
		}
		return nil
	}
	if err != nil {
		return err
	}
	var job incomingJob
	// The ciphertext is base64 in JSON; never accept an unbounded journal read.
	decodeErr := json.NewDecoder(io.LimitReader(file, mailmsg.MaxInboundMessageBytes*2+1<<20)).Decode(&job)
	closeErr := file.Close()
	if decodeErr != nil {
		return errors.New("incoming encryption journal is unreadable; preserve it and repair before retrying")
	}
	if closeErr != nil {
		return closeErr
	}
	if job.Version != 1 || job.Source.UID <= 0 || job.NextAction < 0 || job.NextAction > len(job.Actions) {
		return errors.New("invalid incoming encryption journal; preserve it for reconciliation")
	}
	u, err := p.users.Get(uc.id)
	if err != nil {
		return err
	}
	// Existing ciphertext remains readable with an expired/revoked historical
	// key. Requiring new-encryption eligibility here would strand cleanup and
	// prevent identity rotation behind the pending reservation.
	if !u.Active || u.PGPProtection() != users.PGPProtectionClient || u.PGPPrivateKeyWrapped == "" {
		return errors.New("recover the original client-protected PGP identity before resuming incoming encryption")
	}

	// Older jobs may contain values rejected only after replacement. Drop unsafe
	// saved actions, retain the ciphertext and finish with an explicit failure.
	repaired := false
	if err := validateIncomingActions(job.Actions); err != nil {
		job.Actions, job.NextAction = nil, 0
		repaired = true
	}
	for _, keyword := range job.Keywords {
		if err := imapadapter.ValidateKeyword(keyword); err != nil {
			job.Keywords = nil
			repaired = true
			break
		}
	}
	if repaired {
		job.Decision.Status = "failed"
		job.Decision.Detail = "encrypted copy retained; invalid saved rule or label values skipped; update filters and labels"
		if err := saveIncomingJob(path, job); err != nil {
			return err
		}
	}
	if _, err := p.users.ReserveIncomingEncryption(uc.id, job.Fingerprint, u.PGPRevision); err != nil {
		return err
	}
	if !strings.EqualFold(u.PGPFingerprint, job.Fingerprint) {
		return errors.New("PGP identity changed during encryption; restore the original key before resuming")
	}
	mail, ok := uc.mail.(incomingEncryptor)
	if !ok {
		return errors.New("IMAP adapter does not support incoming encryption")
	}
	cache, err := p.userMailCacheStore(uc.id)
	if err != nil {
		return err
	}
	if err := cache.OmitBodies(); err != nil {
		return err
	}
	if job.ReplacementUID == 0 {
		uid, err := mail.ReplaceIncoming(ctx, job.Source, job.Marker, job.Ciphertext)
		if err != nil {
			return err
		}
		if uid <= 0 || uid == job.Source.UID {
			return errors.New("invalid encrypted replacement UID; journal preserved")
		}
		job.ReplacementUID = uid
		if err := saveIncomingJob(path, job); err != nil {
			return err
		}
	}
	originalID, replacementID := strconv.Itoa(job.Source.UID), strconv.Itoa(job.ReplacementUID)
	if err := cache.Remove("INBOX", job.Source.UID); err != nil {
		return err
	}
	seen, err := uc.store.Seen(replacementID)
	if err != nil {
		return err
	}
	if seen {
		return p.finishIncomingJournal(uc.id, path)
	}
	if !job.LabelsApplied {
		for _, keyword := range job.Keywords {
			if err := mail.ApplyIncomingAction(ctx, job.Source, job.ReplacementUID, job.Marker, job.Ciphertext, "keyword", keyword); err != nil {
				return err
			}
		}
		job.LabelsApplied = true
		if err := saveIncomingJob(path, job); err != nil {
			return err
		}
	}
	for job.NextAction < len(job.Actions) {
		action := job.Actions[job.NextAction]
		if err := mail.ApplyIncomingAction(ctx, job.Source, job.ReplacementUID, job.Marker, job.Ciphertext, action.Type, action.Value); err != nil {
			return err
		}
		job.NextAction++
		if err := saveIncomingJob(path, job); err != nil {
			return err
		}
	}
	job.Decision.MessageID = replacementID
	if err := uc.store.RecordReplacementDecision(originalID, job.Decision); err != nil {
		return err
	}
	if err := p.finishIncomingJournal(uc.id, path); err != nil {
		return err
	}
	// Do not send the old UID or original subject in notification previews.
	msg := imapadapter.Message{ID: replacementID, Sender: job.Decision.Sender, SentTo: job.Decision.SentTo, Subject: pgpmail.OuterPlaceholderSubject, PGPEncrypted: true}
	p.maybeSendPushNotification(uc, msg, job.Decision.Label, job.Keywords)
	p.maybeSendNativePushNotification(uc, msg, job.Decision.Label, job.Keywords)
	p.log.Info("incoming mail classified and encrypted", "user_id", uc.id, "message_id", replacementID)
	return nil
}

func (p *Poller) finishIncomingJournal(userID, path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := fsutil.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return p.users.ReleaseIncomingEncryption(userID)
}

// A move ends the inbox UID's lifetime. Reject ambiguous combinations before
// replacement rather than claim later actions ran against a nonexistent UID.
func validateIncomingActions(actions []rules.Action) error {
	moved := false
	for i := range actions {
		action := &actions[i]
		action.Type = strings.ToLower(strings.TrimSpace(action.Type))
		action.Value = strings.TrimSpace(action.Value)
		if action.Type == "stop" {
			continue
		}
		if moved {
			return errors.New("incoming encryption requires a move/archive/spam/delete to be the last rule action; update your matching filters")
		}
		switch action.Type {
		case "keyword", "unkeyword":
			if err := imapadapter.ValidateKeyword(action.Value); err != nil {
				return errors.New("invalid incoming keyword; update matching filters")
			}
		case "move":
			if err := imapadapter.ValidateMailboxName(action.Value); err != nil {
				return errors.New("invalid incoming destination; update matching filters")
			}
			moved = true
		case "archive", "spam", "delete":
			moved = true
		case "read":
		default:
			return errors.New("unsupported incoming action; update matching filters")
		}
	}
	return nil
}
