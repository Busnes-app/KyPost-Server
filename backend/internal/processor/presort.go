package processor

import (
	"strings"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/contacts"
)

// presortHeaderNames are fetched for every unread message: the signals that
// it was sent to a list or by a machine rather than written to this user.
var presortHeaderNames = []string{"Auto-Submitted", "List-Id", "List-Unsubscribe", "Precedence"}

// isBulkOrAutomated reports whether h marks a message as list or machine
// mail. The headers are sender-controlled, which is why they only ever take
// Primary away: forging one can only demote the forger's own message.
func isBulkOrAutomated(h map[string][]string) bool {
	if len(h["List-Id"]) > 0 || len(h["List-Unsubscribe"]) > 0 {
		return true
	}
	for _, v := range h["Precedence"] {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "bulk", "list", "junk":
			return true
		}
	}
	for _, v := range h["Auto-Submitted"] {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && v != "no" {
			return true
		}
	}
	return false
}

// headersByUID folds each fetched UID's lines into a header map.
// FetchHeaderFields gives every UID it fetched an entry, so a UID missing here
// was not fetched and its headers read as unknown (nil), which header rules
// treat as unevaluable rather than absent.
func headersByUID(lines map[int][]string) map[int]map[string][]string {
	out := make(map[int]map[string][]string, len(lines))
	for uid, l := range lines {
		out[uid] = imapadapter.HeaderMap(l)
	}
	return out
}

// knownSenders returns the lowercased addresses of contacts the user added.
// Contacts created by key discovery are left out: Autocrypt harvest creates
// one from a stranger's first message, just before pre-sort would ask.
func knownSenders(store *contacts.Store) (map[string]bool, error) {
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range all {
		if c.DiscoveryCreated {
			continue
		}
		for _, e := range c.Emails {
			if addr := strings.ToLower(strings.TrimSpace(e.Value)); addr != "" {
				out[addr] = true
			}
		}
	}
	return out, nil
}

// presort decides what it can without the classifier. A non-empty label means
// "apply this and skip the LLM"; otherwise allowlist is what the LLM may pick
// from and note explains any narrowing for the audit log. Only accounts whose
// label set includes Primary are touched: a custom set has no Primary to aim at.
//
// ponytail: From is not DKIM-checked, so a spoofed contact lands in Primary.
// Labels are a sorting hint, not a trust signal (SECURITY.md); gate on
// imapadapter.VerifyDKIMForDomain if that ever changes.
func presort(uc userCtx, msg imapadapter.Message, uid int) (label string, allowlist []string, note string) {
	if msg.TooLarge {
		return "", uc.allowlist, ""
	}
	primary := ""
	var rest []string
	for _, l := range uc.allowlist {
		if primary == "" && strings.EqualFold(strings.TrimSpace(l), "Primary") {
			primary = strings.TrimSpace(l)
			continue
		}
		rest = append(rest, l)
	}
	if primary == "" {
		return "", uc.allowlist, ""
	}
	if isBulkOrAutomated(uc.headers[uid]) {
		if len(rest) == 0 {
			return "", uc.allowlist, ""
		}
		return "", rest, "; Primary excluded by list/automated headers"
	}
	if uc.knownSenders[parseFromAddress(msg.Sender)] {
		return primary, nil, ""
	}
	return "", uc.allowlist, ""
}
