package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpdiscovery"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/wkdpublish"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// wkdClaimResponse is the POST /api/pgp/wkd/domains response: the created
// claim (fields promoted to the top level via embedding) plus the literal
// DNS TXT record the caller must add to prove ownership.
type wkdClaimResponse struct {
	wkdpublish.Claim
	RecordName  string `json:"recordName"`
	RecordValue string `json:"recordValue"`
}

// domainOfEmail returns the lowercased domain part of an email address, or
// "" if email has no usable "@". Mirrors the inline split fetchWKDKey uses
// (internal/api/pgp_wkd.go) since there's no shared domainOf helper in this
// package.
func domainOfEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

// handleWKDDomains serves GET (list all instance-wide claims) and POST
// (claim a new domain) on /api/pgp/wkd/domains. Both are s.withAdmin-gated:
// domain ownership is an instance-level property, not a per-user one, so an
// admin may claim any domain they control — there is no "domain you send
// from" restriction here.
func (s *Server) handleWKDDomains(w http.ResponseWriter, r *http.Request) {
	store, err := s.wkdPublishStore()
	if err != nil {
		http.Error(w, "failed to open wkd store", http.StatusInternalServerError)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// A failed re-read is reported, not papered over with a stale cache:
		// an admin looking at this list is deciding whether their domains are
		// published, and a list that silently predates a corrupt claims file
		// answers that question wrongly.
		claims, err := store.List()
		if err != nil {
			http.Error(w, "failed to read wkd claims", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"domains": claims,
		})

	case http.MethodPost:
		var req struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		domain := strings.ToLower(strings.TrimSpace(req.Domain))
		if domain == "" {
			http.Error(w, "domain is required", http.StatusBadRequest)
			return
		}
		claim, err := store.Create(domain)
		if err != nil {
			http.Error(w, "failed to create claim", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, wkdClaimResponse{
			Claim:       claim,
			RecordName:  wkdpublish.TXTRecordName(domain),
			RecordValue: "kypost-wkd-verify=" + claim.Token,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleWKDDomainVerify re-checks the DNS TXT record for {domain} against
// the stored claim's token and persists the result. wkdpublish.CheckTXT
// already classifies a definitive "not found" (NXDOMAIN/NODATA) as
// (false, nil), so only a genuinely transient resolver failure (timeout,
// SERVFAIL, network down, ...) comes back as a non-nil error here. On a
// transient error, the claim's current verified value is left untouched —
// persisting SetVerified(domain, false, ...) on a mere DNS blip would
// unpublish an entire domain's users, which is exactly the invariant the
// background re-check (recheckWKDDomains) is careful to preserve. The
// endpoint still answers 200 in that case (transient DNS trouble is an
// expected, non-exceptional outcome here, not a server error), reporting the
// claim's unchanged verified value plus a checkError hint the frontend may
// ignore.
func (s *Server) handleWKDDomainVerify(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(r.PathValue("domain")))
	if domain == "" {
		http.Error(w, "domain is required", http.StatusBadRequest)
		return
	}
	store, err := s.wkdPublishStore()
	if err != nil {
		http.Error(w, "failed to open wkd store", http.StatusInternalServerError)
		return
	}

	claims, err := store.List()
	if err != nil {
		http.Error(w, "failed to read wkd claims", http.StatusInternalServerError)
		return
	}
	var claim wkdpublish.Claim
	found := false
	for _, c := range claims {
		if c.Domain == domain {
			claim = c
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "no claim for domain", http.StatusNotFound)
		return
	}

	verified, checkErr := wkdpublish.CheckTXT(domain, claim.Token)
	if checkErr != nil {
		// Transient DNS failure: don't persist a false verified value over
		// the claim's real current state. Report what's on record instead.
		writeJSON(w, http.StatusOK, map[string]any{
			"verified":   claim.Verified,
			"checkError": checkErr.Error(),
		})
		return
	}
	if err := store.SetVerified(domain, verified, time.Now()); err != nil {
		http.Error(w, "failed to update claim", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": verified})
}

// handleWKDDomainDelete removes the instance-wide claim for {domain}.
func (s *Server) handleWKDDomainDelete(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(r.PathValue("domain")))
	if domain == "" {
		http.Error(w, "domain is required", http.StatusBadRequest)
		return
	}
	store, err := s.wkdPublishStore()
	if err != nil {
		http.Error(w, "failed to open wkd store", http.StatusInternalServerError)
		return
	}
	if _, err := store.Delete(domain); err != nil {
		http.Error(w, "failed to delete claim", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWKD is the single public (unauthenticated) catch-all for both Web Key
// Directory serving methods, registered at GET /.well-known/openpgpkey/. A
// single handler dispatching on the trimmed path is required rather than
// separate mux patterns, because a literal "hu"/"policy" segment pattern and
// a "{domain}" wildcard pattern at the same position conflict under Go 1.22's
// ServeMux (both would match "/.well-known/openpgpkey/hu/xyz").
//
// Shapes handled:
//   - .../openpgpkey/policy                 → 200, empty body (direct method)
//   - .../openpgpkey/{domain}/policy         → 200, empty body (advanced method)
//   - .../openpgpkey/hu/{hu}                 → binary key or 404 (direct; domain from r.Host)
//   - .../openpgpkey/{domain}/hu/{hu}        → binary key or 404 (advanced; domain from path)
func (s *Server) handleWKD(w http.ResponseWriter, r *http.Request) {
	const prefix = "/.well-known/openpgpkey/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)

	var domain, hu string
	switch {
	case rest == "policy":
		writeWKDPolicy(w)
		return
	case strings.HasSuffix(rest, "/policy"):
		// Checked before the "hu/" prefix case below so a (nonsensical but
		// possible) domain literally named "hu" — ".../openpgpkey/hu/policy"
		// — still routes to the policy response instead of being misread as
		// the direct-method "hu/{hu}" shape with hu="policy".
		writeWKDPolicy(w)
		return
	case strings.HasPrefix(rest, "hu/"):
		domain = hostDomain(r.Host)
		hu = strings.ToLower(strings.TrimPrefix(rest, "hu/"))
	default:
		// <domain>/hu/<hu>
		parts := strings.SplitN(rest, "/hu/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		domain, hu = strings.ToLower(parts[0]), strings.ToLower(parts[1])
	}
	if domain == "" || hu == "" {
		http.NotFound(w, r)
		return
	}

	binary, ok := s.lookupPublishedKey(domain, hu)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=3600")
	_, _ = w.Write(binary)
}

// writeWKDPolicy answers the WKD "policy" file: a 200 with an empty body,
// meaning "no submission-address / no special policy restrictions".
func writeWKDPolicy(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
}

// hostDomain strips an optional port from an HTTP Host header and, for the
// direct method (where WKD clients request "openpgpkey.<domain>"), the
// leading "openpgpkey." label if present.
func hostDomain(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	return strings.TrimPrefix(host, "openpgpkey.")
}

// lookupPublishedKey finds the one Active user with a publishable address at
// domain (which must hold a verified instance-level WKD claim) whose hashed
// local-part matches hu, and returns the BINARY (unarmored) form of their
// public key, as WKD requires, if they have one and PublishWKD is on. Two
// users proving the same address serve nothing; one user's corrupt key or
// unreadable settings affects only their own addresses.
func (s *Server) lookupPublishedKey(domain, hu string) ([]byte, bool) {
	store, err := s.wkdPublishStore()
	if err != nil {
		return nil, false
	}
	// Fail closed on a refresh error, not back onto whatever this process last
	// managed to read. This is the authorization step for a PUBLIC, unauthenticated
	// endpoint: "may this instance serve keys for this domain at all". An
	// unreadable claims file means the answer is currently unknown, and unknown
	// has to serve nothing.
	verified, err := store.VerifiedDomains()
	if err != nil || !verified[domain] {
		return nil, false
	}
	all, err := s.users.List()
	if err != nil {
		return nil, false
	}
	// Every active user proving the address is a claimant, published or not:
	// two claimants must serve nothing rather than whichever lists first.
	var owner *users.User
	for i, u := range all {
		if !u.Active {
			continue
		}
		if !slices.ContainsFunc(s.publishableAddressesAt(u, domain), func(addr string) bool {
			at := strings.LastIndex(addr, "@")
			return at > 0 && wkdHashLocalPart(addr[:at]) == hu
		}) {
			continue
		}
		if owner != nil {
			if s.logger != nil {
				for _, id := range []string{owner.ID, u.ID} {
					s.logger.Warn("wkd: address domain-proven by more than one user; serving no key", "user_id", id)
				}
			}
			return nil, false
		}
		owner = &all[i]
	}
	if owner == nil || owner.PGPPublicKey == "" {
		return nil, false
	}
	settings, err := pgpdiscovery.Load(s.userStateDir(owner.ID))
	if err != nil || !settings.PublishWKD {
		return nil, false
	}
	key, err := crypto.NewKeyFromArmored(owner.PGPPublicKey)
	if err != nil {
		return nil, false
	}
	binary, err := key.GetPublicKey()
	if err != nil {
		return nil, false
	}
	return binary, true
}

// publishableAddressesAt returns the lowercased addresses belonging to user
// u whose domain equals domain: the IMAP account address (the same address
// handleMailSend treats as the account's own From address) plus any of the
// user's verified send-as aliases at that domain. This is the anti-
// impersonation gate at serve time: even a verified instance-level domain
// claim only lets a user's key be served under an address that is actually
// theirs. Read errors (no config yet, corrupt store, etc.) simply yield no
// addresses rather than an error, since this is a best-effort lookup over
// all users.
func (s *Server) publishableAddressesAt(u users.User, domain string) []string {
	var out []string

	// Only addresses proven by the send-as challenge are publishable —
	// including the account's own.
	//
	// This used to also return the IMAP config's username unconditionally, and
	// that string is entirely self-declared: POST /api/imap/config accepts any
	// username with no connection attempt and no ownership challenge. On an
	// instance whose admin has DNS-verified the organization's domain, any
	// ordinary user could therefore set their IMAP username to a colleague's
	// address and have their own public key served over WKD as that
	// colleague's — silent key substitution for every correspondent who uses
	// WKD discovery, which is most of the point of publishing at all.
	//
	// Proving an IMAP login does not fix it: the user chooses the IMAP host
	// too, so a login only proves they control *some* mailbox, not that
	// address at that domain. The only proof this codebase has that actually
	// binds an address to its domain is the send-as challenge, so that is the
	// single gate now. Users whose own address is not yet verified stop being
	// published until they run it — fail-closed, and recoverable by a flow
	// that already exists.
	if store, err := s.userSendAsStore(u.ID); err == nil {
		verified, verr := store.ListVerified()
		if verr != nil {
			// No addresses: an unreadable alias file must not publish a key
			// under an address whose verification this process cannot confirm.
			return nil
		}
		for _, alias := range verified {
			// A code-confirmed alias may send; only the DKIM proof publishes.
			if !alias.DomainProven() {
				continue
			}
			addr := strings.ToLower(strings.TrimSpace(alias.Email))
			if domainOfEmail(addr) == domain {
				out = append(out, addr)
			}
		}
	}

	return out
}
