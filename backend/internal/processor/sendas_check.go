package processor

import (
	"bytes"
	"context"
	"errors"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
)

// verifyDKIMCoversHeader and dkimSignedHeaders indirect the imap adapter so
// tests here can reach the verified branches: the real verifier resolves the
// signing key from live DNS. Production never reassigns them; the DKIM crypto
// is covered in internal/adapters/imap.
var (
	verifyDKIMCoversHeader = imapadapter.VerifyDKIMCoversHeader
	dkimSignedHeaders      = imapadapter.DKIMSignedHeaders
)

// sendAsProofHeaders must all be covered by the alias domain's signature:
// From names the alias, Message-ID binds the message to this probe, Subject
// carries the code and Date places it after the challenge.
var sendAsProofHeaders = []string{"from", "message-id", "subject", "date"}

// userSendAsStore returns the cached send-as alias store for a user,
// mirroring userMailCacheStore/userRulesStore — the api process
// independently constructs its own sendas.Store over the same on-disk
// send_as_aliases.json (the HTTP handlers from Task 5), so refreshFromDiskLocked
// is what keeps the two processes' in-memory views coherent, exactly as with
// state.Store.
func (p *Poller) userSendAsStore(userID string) (*sendas.Store, error) {
	p.userMu.Lock()
	defer p.userMu.Unlock()
	if st, ok := p.sendAsStores[userID]; ok {
		return st, nil
	}
	st, err := sendas.New(p.userStateDir(userID))
	if err != nil {
		return nil, err
	}
	p.sendAsStores[userID] = st
	return st, nil
}

// checkPendingSendAsAliases advances every one of userID's pending send-as
// alias verifications by one poll tick.
//
// A pending record whose ExpiresAt has already passed is marked failed and
// is never checked again — no indefinite retry, matching the feature's
// fixed verification window (sendas.pendingExpiry).
//
// Every other pending record is checked by searching the user's own INBOX
// for a message whose subject contains the record's VerificationCode. A
// bare subject match is not sufficient on its own — it proves only that
// *some* message with that text exists in an inbox the account owner fully
// controls, which they could trivially fake themselves (e.g. via IMAP
// APPEND, which involves no MTA and lets the owner write any header they
// like). Verification additionally requires that message to be the probe
// itself, signed by the candidate address's own domain (provesSendAs), and
// the address not to be domain-proven by another user.
//
// Errors from the mail client (search/fetch failures) are logged and
// leave the affected record pending for the next tick — they are not
// escalated to the caller, matching this file's general policy of never
// letting one user's IMAP trouble abort a poll tick.
func (p *Poller) checkPendingSendAsAliases(ctx context.Context, userID string, mail imapadapter.Client) {
	store, err := p.userSendAsStore(userID)
	if err != nil {
		p.log.Error("failed to open send-as store", "user_id", userID, "error", err.Error())
		return
	}

	aliases, err := store.List()
	if err != nil {
		p.log.Error("failed to read send-as aliases", "user_id", userID, "error", err.Error())
		return
	}
	for _, alias := range aliases {
		// A code-confirmed alias is checked too, until its window closes: the
		// DKIM proof upgrades it to domain-proven (sendas.Alias.DomainProven).
		codeOnly := alias.Status == "verified" && alias.VerifiedBy == sendas.VerifiedByCode
		if alias.Status != "pending" && !codeOnly {
			continue
		}

		expiresAt, perr := time.Parse(time.RFC3339, alias.ExpiresAt)
		if perr != nil || !expiresAt.After(time.Now()) {
			if codeOnly {
				continue
			}
			if err := store.MarkFailed(alias.ID); err != nil {
				p.log.Error("failed to mark expired send-as alias failed",
					"user_id", userID, "alias_id", alias.ID, "error", err.Error())
			}
			continue
		}

		matches, err := mail.SearchMessages(ctx, "INBOX", "subject", alias.VerificationCode, 10)
		if err != nil {
			p.log.Error("send-as verification search failed",
				"user_id", userID, "alias_id", alias.ID, "error", err.Error())
			continue
		}
		if len(matches) == 0 {
			continue
		}

		verified := false
		for _, m := range matches {
			raw, err := mail.FetchRawMessage(ctx, "INBOX", m.UID)
			if err != nil {
				p.log.Error("send-as verification raw fetch failed",
					"user_id", userID, "alias_id", alias.ID, "uid", strconv.Itoa(m.UID), "error", err.Error())
				continue
			}
			if provesSendAs(raw, alias) {
				verified = true
				break
			}
		}
		if !verified {
			continue
		}
		if owners, err := p.domainProvenOwners(); err != nil || otherOwner(owners[alias.Email], userID) {
			// One address, one key on WKD. Leave the record pending to expire.
			p.log.Warn("send-as domain proof refused: address already proven by another user or owners unreadable",
				"user_id", userID, "alias_id", alias.ID)
			continue
		}
		if err := store.MarkVerified(alias.ID); err != nil {
			p.log.Error("failed to mark send-as alias verified",
				"user_id", userID, "alias_id", alias.ID, "error", err.Error())
		}
	}

	if err := store.SweepTerminal(24 * time.Hour); err != nil {
		p.log.Error("send-as terminal sweep failed", "user_id", userID, "error", err.Error())
	}
}

// domainOf returns the portion of email after '@', lowercased, or "" if
// email has no '@' — used to scope the Authentication-Results check to the
// candidate address's own domain, never the account's own.
func domainOf(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 && i+1 < len(email) {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

// provesSendAs reports whether raw is the alias's own probe come back signed by
// the alias domain: the account's SMTP got it signed there, or the alias
// delivered it here. The IMAP server answering is the account holder's choice,
// so only the signed instances of signed headers are read, and a reply never
// counts: an out-of-office or a colleague answering a probe the account holder
// sent through their own SMTP would otherwise prove the colleague's address.
func provesSendAs(raw []byte, alias sendas.Alias) bool {
	signed := dkimSignedHeaders(raw, domainOf(alias.Email))
	for _, h := range sendAsProofHeaders {
		if _, ok := signed[h]; !ok {
			return false
		}
	}
	ids := mailmsg.MessageIDs(signed["message-id"])
	if alias.ProbeMessageID == "" || len(ids) != 1 || ids[0] != alias.ProbeMessageID {
		return false
	}
	if !strings.EqualFold(parseFromAddress(signed["from"]), alias.Email) ||
		!strings.Contains(signed["subject"], alias.VerificationCode) {
		return false
	}
	createdAt, cerr := time.Parse(time.RFC3339, alias.CreatedAt)
	sentAt, derr := mail.ParseDate(signed["date"])
	if cerr != nil || derr != nil || sentAt.Before(createdAt) {
		return false
	}
	return !rawIsAutoReply(raw)
}

// domainProvenOwners maps each domain-proven send-as address to the active
// users holding it. An error means ownership is unknown and must not be assumed.
func (p *Poller) domainProvenOwners() (map[string][]string, error) {
	if p.users == nil {
		return nil, errors.New("no users store")
	}
	all, err := p.users.List()
	if err != nil {
		return nil, err
	}
	owners := map[string][]string{}
	for _, u := range all {
		if !u.Active {
			continue
		}
		store, err := p.userSendAsStore(u.ID)
		if err != nil {
			return nil, err
		}
		verified, err := store.ListVerified()
		if err != nil {
			return nil, err
		}
		for _, a := range verified {
			if a.DomainProven() {
				addr := strings.ToLower(strings.TrimSpace(a.Email))
				if !slices.Contains(owners[addr], u.ID) {
					owners[addr] = append(owners[addr], u.ID)
				}
			}
		}
	}
	return owners, nil
}

func otherOwner(owners []string, userID string) bool {
	return slices.ContainsFunc(owners, func(id string) bool { return id != userID })
}

// logDuplicateProvenAliases reports addresses domain-proven by more than one
// user, proven before uniqueness was enforced. They are left as they are (WKD
// serves neither); an administrator decides which record to remove.
func (p *Poller) logDuplicateProvenAliases() {
	if p.users == nil {
		return
	}
	owners, err := p.domainProvenOwners()
	if err != nil {
		p.log.Error("failed to check send-as aliases for duplicates", "error", err.Error())
		return
	}
	for _, ids := range owners {
		if len(ids) < 2 {
			continue
		}
		for _, id := range ids {
			p.log.Warn("send-as address domain-proven by more than one user; WKD serves no key for it",
				"user_id", id)
		}
	}
}

// rawIsAutoReply reports whether the message announces itself as machine
// generated.
//
// The probe is sent FROM the requesting account's own address, so an
// out-of-office or helpdesk auto-acknowledgement at the target address returns
// a domain-signed reply that quotes the code in its Subject and carries the
// target's From — satisfying every other check without anyone at that address
// ever reading it. RFC 3834 exists precisely so a responder can be recognised;
// honouring it is what stops an unattended bounce from constituting proof of
// control.
func rawIsAutoReply(raw []byte) bool {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	if v := strings.ToLower(strings.TrimSpace(msg.Header.Get("Auto-Submitted"))); v != "" && v != "no" {
		return true
	}
	for _, h := range []string{"X-Autoreply", "X-Autorespond", "X-Auto-Response-Suppress"} {
		if strings.TrimSpace(msg.Header.Get(h)) != "" {
			return true
		}
	}
	// A mailing list is an unattended REDISTRIBUTOR, which fails this function's
	// premise the same way an auto-responder does: a machine at the alias domain
	// emitting a signed message is not a person proving control of the mailbox.
	//
	// It is the sharper case, because no forgery is involved. A list applying
	// DMARC From-munging (Mailman's dmarc_moderation_action = Munge From, and
	// Google Groups' equivalent) rewrites From to the list address precisely so
	// alignment holds against its own domain — which means it DKIM-signs that
	// rewritten From and the echoed Subject with d=<list domain>. Post a message
	// carrying the challenge code, and the redistributed copy satisfies every
	// other gate here genuinely: real signature, real domain, real From. The
	// code is readable from GET /api/mail/send-as, so the attacker does not even
	// need the server's own probe to reach the list.
	for _, h := range []string{"List-Id", "List-Post", "List-Unsubscribe", "List-Help"} {
		if strings.TrimSpace(msg.Header.Get(h)) != "" {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(msg.Header.Get("Precedence"))) {
	case "auto_reply", "list", "bulk":
		return true
	}
	return false
}
