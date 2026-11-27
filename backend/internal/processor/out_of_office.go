package processor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/rules"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Test seams: SMTP for external accounts, the native outbox, and the suite
// directory lookup.
var (
	oooSendSMTP    = mailmsg.SMTPDeliver
	oooQueueNative = func(ctx context.Context, p *Poller, mailboxID, id string, job mailbox.OutboundJob) error {
		return sso.NativeOutbound{ConfigDir: p.configDir, StateRoot: p.stateDir, SecretDir: config.SecretDir(), Accounts: p.users,
			Domains: sso.NewNativeDomainStore(p.configDir), Settings: sso.NewStore(p.configDir), Orgs: p.orgGate, Logger: p.log}.Queue(ctx, mailboxID, id, job)
	}
	oooSuiteMember = func(ctx context.Context, p *Poller, u users.User, addr string) bool {
		dir, ok := sso.NewStore(p.configDir).Load().SuiteDirectory()
		trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
		if !ok || u.SSOSub == "" || u.SSOLinkRevoked() || trim(u.SSOIssuer) != trim(dir.Issuer) {
			return false
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		people, err := dir.Resolve(ctx, u.SSOSub, []string{addr})
		if err != nil {
			p.log.Info("out-of-office directory lookup failed; treating sender as remote", "user_id", u.ID)
			return false
		}
		for _, person := range people {
			if strings.EqualFold(person.Email, addr) {
				return true
			}
		}
		return false
	}
)

const maxOOOSubjectRunes = 200

// oooOriginal is what an eligible message contributes to the reply.
type oooOriginal struct {
	sender, messageID, references, inReplyTo string
}

// nativeOwn adds the mailbox's active addresses to own, except masked ones:
// the reply comes from the account address, so answering mail sent to a
// masked address would reveal it.
func nativeOwn(own map[string]bool, all map[string]sso.NativeAddress, mailbox string, settings config.UserSettings) {
	for addr, x := range all {
		if _, masked := settings.Masked(addr); x.Mailbox == mailbox && x.State == "active" && !masked {
			own[strings.ToLower(addr)] = true
		}
	}
}

// oooEligible applies RFC 3834 and the recipient rules to a message's header
// (docs/OUT_OF_OFFICE.md). It names the refusal, or "" with the original.
func oooEligible(h mail.Header, own map[string]bool) (oooOriginal, string) {
	if v := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))); v != "" && v != "no" {
		return oooOriginal{}, "auto_submitted"
	}
	switch strings.ToLower(strings.TrimSpace(h.Get("Precedence"))) {
	case "bulk", "list", "junk", "auto_reply":
		return oooOriginal{}, "precedence"
	}
	for name := range h {
		if strings.HasPrefix(textproto.CanonicalMIMEHeaderKey(name), "List-") {
			return oooOriginal{}, "list"
		}
	}
	for _, name := range []string{"X-Auto-Response-Suppress", "X-Autoreply", "X-Autorespond"} {
		if len(h[name]) > 0 {
			return oooOriginal{}, "auto_submitted"
		}
	}
	if strings.TrimSpace(h.Get("Return-Path")) == "<>" {
		return oooOriginal{}, "bounce"
	}
	if spamFlagged(h) {
		return oooOriginal{}, "junk"
	}
	from, err := h.AddressList("From")
	if err != nil || len(from) != 1 || len(h["From"]) != 1 {
		return oooOriginal{}, "bad_from"
	}
	sender := strings.ToLower(from[0].Address)
	local, _, _ := strings.Cut(sender, "@")
	switch {
	case local == "mailer-daemon", local == "postmaster", local == "donotreply",
		strings.Contains(local, "noreply"), strings.Contains(local, "no-reply"):
		return oooOriginal{}, "machine_sender"
	case own[sender]:
		return oooOriginal{}, "own_sender"
	}
	addressed := false
	for _, field := range []string{"To", "Cc"} {
		list, _ := h.AddressList(field)
		for _, a := range list {
			lower := strings.ToLower(a.Address)
			addressed = addressed || own[lower] || own[sso.PlusBase(lower)]
		}
	}
	if !addressed {
		return oooOriginal{}, "not_addressed"
	}
	ids := mailmsg.MessageIDs(h.Get("Message-Id"))
	if len(ids) != 1 {
		return oooOriginal{}, "no_message_id"
	}
	return oooOriginal{sender: sender, messageID: ids[0], references: h.Get("References"), inReplyTo: h.Get("In-Reply-To")}, ""
}

// spamFlagged reads an upstream filter's verdict. Sender-forgeable, which is
// safe here: forging it only suppresses a reply.
func spamFlagged(h mail.Header) bool {
	for _, v := range h["X-Spam-Flag"] {
		if strings.EqualFold(strings.TrimSpace(v), "yes") {
			return true
		}
	}
	for _, name := range []string{"X-Spam", "X-Spam-Status"} {
		for _, v := range h[name] {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "yes") {
				return true
			}
		}
	}
	return false
}

// ruledJunk reports whether one of the user's rules would mark the message
// as spam or delete it.
func ruledJunk(ctx context.Context, uc userCtx, msg imapadapter.Message, uid int) bool {
	out := rules.Evaluate(ctx, rules.EvalInput{UID: uid, MessageID: msg.ID, From: msg.Sender, To: msg.SentTo, CC: msg.CC, BCC: msg.BCC,
		Subject: msg.Subject, Body: msg.Body, Keywords: msg.Keywords, Folder: "INBOX", Headers: uc.headers[uid]}, uc.rules)
	for _, a := range out.Applied {
		if a.Type == "spam" || a.Type == "delete" {
			return true
		}
	}
	return false
}

// ownAddresses are the polled primary mailbox's own addresses: the account
// address plus verified send-as aliases (external) or nativeOwn (native).
func (p *Poller) ownAddresses(uc userCtx, account string) map[string]bool {
	own := map[string]bool{strings.ToLower(account): true}
	if uc.mailbox != "" && p.users != nil {
		settings, err := config.LoadUserSettings(config.UserSettingsPath(p.configDir, uc.id))
		if all, ledgerErr := sso.NewLifecycleStore(p.configDir).NativeAddresses(); err == nil && ledgerErr == nil {
			nativeOwn(own, all, uc.mailbox, settings)
		}
		return own
	}
	if st, err := p.userSendAsStore(uc.id); err == nil {
		if aliases, err := st.ListVerified(); err == nil {
			for _, a := range aliases {
				own[strings.ToLower(a.Email)] = true
			}
		}
	}
	return own
}

// outOfOfficeReply answers a new primary-mailbox message while the user's
// reply is on. Best effort: it never fails the message, and logs IDs and a
// reason only. The claim in the state store makes it once per message.
func (p *Poller) outOfOfficeReply(ctx context.Context, uc userCtx, msg imapadapter.Message, account string) {
	now := time.Now()
	if !uc.ooo.ActiveAt(now) || uc.extraMailbox() || msg.TooLarge || msg.ParseError || account == "" || p.users == nil {
		return
	}
	skip := func(reason string) {
		p.log.Info("out-of-office reply not sent", "user_id", uc.id, "message_id", msg.ID, "reason", reason)
	}
	// Native mail delivered to a masked address never answers: the reply
	// comes from the account address. The receipt, not the headers (which
	// may also name a regular alias), says where it was delivered.
	if len(msg.DeliveredTo) > 0 {
		settings, err := config.LoadUserSettings(config.UserSettingsPath(p.configDir, uc.id))
		if err != nil {
			skip("settings_unreadable")
			return
		}
		for _, address := range msg.DeliveredTo {
			if _, masked := settings.Masked(address); masked {
				skip("masked_recipient")
				return
			}
		}
	}
	uid, err := strconv.Atoi(strings.TrimSpace(msg.ID))
	if err != nil {
		return
	}
	raw, err := uc.mail.FetchRawMessage(ctx, "", uid)
	if err != nil || len(raw) == 0 {
		skip("unreadable")
		return
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		skip("unreadable")
		return
	}
	orig, reason := oooEligible(parsed.Header, p.ownAddresses(uc, account))
	// A UID reset re-offers old mail as new; never answer it.
	if sent, err := parsed.Header.Date(); reason == "" && err == nil && now.Sub(sent) > time.Duration(state.OOOSenderWindow)*time.Second {
		reason = "old"
	}
	if reason == "" && ruledJunk(ctx, uc, msg, uid) {
		reason = "junk"
	}
	if reason == "" && dkimAlignedFrom(raw) != orig.sender {
		reason = "unauthenticated"
	}
	if reason != "" {
		skip(reason)
		return
	}
	u, err := p.users.Get(uc.id)
	if err != nil || !u.Active {
		skip("account")
		return
	}
	text := uc.ooo.LocalMessage
	if !p.oooLocal(ctx, uc, u, orig.sender) {
		switch {
		case !p.currentConfig().OutOfOfficeRemote || uc.ooo.RemoteMode() == config.RemoteOff:
			skip("remote_off")
			return
		case uc.ooo.RemoteMode() == config.RemoteContacts && !uc.knownSenders[orig.sender]:
			skip("not_contact")
			return
		}
		text = uc.ooo.RemoteMessage
	}
	if strings.TrimSpace(text) == "" {
		skip("no_text")
		return
	}
	sum := sha256.Sum256([]byte(uc.id + "\x00" + uc.mailbox + "\x00" + orig.messageID))
	key := hex.EncodeToString(sum[:])
	if ok, err := uc.primaryStore().ClaimOutOfOfficeReply(ctx, key, orig.sender, uc.ooo.Period, now.Unix()); err != nil || !ok {
		skip("already_answered")
		return
	}
	if !p.admitOOO(ctx, u, key, now) {
		skip("quota")
		return
	}
	inReplyTo, refs := mailmsg.ReplyThreading(orig.messageID, orig.references, orig.inReplyTo)
	reply := mailmsg.Message{From: account, To: []string{orig.sender}, Subject: "Automatic reply: " + oooSubject(msg.Subject),
		Body: text, Mode: "plain", InReplyTo: inReplyTo, References: refs, AutoReplied: true}.Stamp().Build()
	if err := p.sendOOO(ctx, uc, u, account, orig.sender, reply); err != nil {
		p.log.Error("out-of-office reply failed", "user_id", uc.id, "message_id", msg.ID, "error", err.Error())
		return
	}
	p.log.Info("out-of-office reply sent", "user_id", uc.id, "message_id", msg.ID)
}

// oooSubject is the original subject, header-sanitised and cut.
func oooSubject(s string) string {
	s = mailmsg.SanitizeHeaderValue(strings.ToValidUTF8(s, ""))
	if utf8.RuneCountInString(s) > maxOOOSubjectRunes {
		s = string([]rune(s)[:maxOOOSubjectRunes])
	}
	return s
}

// oooLocal reports whether sender is inside the organisation: at an
// established native mail domain (native mailbox) or a suite member in
// KyIdentity. Retired domains prove nothing: they may now belong to anyone.
func (p *Poller) oooLocal(ctx context.Context, uc userCtx, u users.User, sender string) bool {
	if uc.mailbox != "" {
		if set, err := sso.NewNativeDomainStore(p.configDir).ReadSet(); err == nil && set.Domains[sso.AddressDomain(sender)].Established {
			return true
		}
	}
	return oooSuiteMember(ctx, p, u, sender)
}

// admitOOO charges the reply to the user's send quota, keyed by the claim so
// it is never charged twice.
func (p *Poller) admitOOO(ctx context.Context, u users.User, key string, now time.Time) bool {
	if p.globalStore == nil {
		return false
	}
	q := p.currentConfig().SendQuota
	if u.SendQuota != nil && u.SendQuota.Valid() {
		q = *u.SendQuota
	}
	if !q.Valid() {
		q = config.Default().SendQuota
	}
	_, err := p.globalStore.AdmitSend(ctx, "ooo-"+key, u.ID, 1, q.MessagesPerHour, q.RecipientsPerDay, now.Unix())
	return err == nil
}

// sendOOO sends the reply from the mailbox: native mail through the outbox,
// which files Sent; an external account through its SMTP server, then Sent.
func (p *Poller) sendOOO(ctx context.Context, uc userCtx, u users.User, from, to string, raw []byte) error {
	if uc.mailbox != "" {
		wire, err := mailmsg.NormalizeSMTPMessage(raw)
		if err != nil {
			return err
		}
		id, err := fsutil.NewUUIDv4()
		if err != nil {
			return err
		}
		job := mailbox.OutboundJob{From: from, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{to}, Raw: wire}}, Sent: raw,
			NativeSendEpoch: u.NativeSendEpoch, PGPRevision: u.PGPRevision}
		if u.PGPKeyring != nil {
			job.MaterialGeneration = u.PGPKeyring.MaterialGeneration
		}
		return oooQueueNative(ctx, p, uc.mailbox, id, job)
	}
	payload, exists, err := mailmsg.ReadIMAPConfigPayload(p.userIMAPConfigPath(uc.id), p.imapKeyPath)
	if err != nil || !exists {
		return errors.New("no outgoing mail server")
	}
	host, port, addr, err := mailmsg.ResolveSMTPTarget(payload)
	if err != nil {
		return err
	}
	if err := oooSendSMTP(host, port, addr, payload.Username, payload.Password, from, []string{to}, raw); err != nil && !errors.Is(err, mailmsg.ErrSMTPAcceptedThenFailed) {
		return err
	}
	if err := uc.mail.SaveSent(ctx, imapadapter.DraftMessage{To: []string{to}, Raw: raw}); err != nil {
		p.log.Error("out-of-office reply sent but not saved to Sent", "user_id", uc.id)
	}
	return nil
}
