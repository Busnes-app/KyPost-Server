package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Busnes-app/ky-primitives/suitedir"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Chat from mail (docs/KYMESSAGES.md, suite contract H): KyPost asks the
// paired KyMessages console for a person's Element user view.
const (
	kymessageEventChat = "kypost.chat.open"
	kymessagePathChat  = "/api/suite/chat"
	// kymessageChatApp is Element's entry in the suite app list; its origin is
	// the only host a chat URL may point at.
	kymessageChatApp       = "kymessage-chat"
	kymessageBurst         = 30
	kymessageRefillPerSec  = 1
	maxKyMessageAddresses  = 100
	maxKyMessageChatURL    = 2048
	kymessageLookupTimeout = 5 * time.Second
)

// KyMessagePeer is the pairing KyPost keeps with the KyMessages console.
var KyMessagePeer = suitepair.Peer{ID: "kymessage", Name: "KyMessages", Env: "KYPOST_KYMESSAGE"}

// SetKyMessage pairs KyMessages. Startup-only; nil or unpaired leaves it off.
func (s *Server) SetKyMessage(c *suitepair.Client) { s.kymessage = c }

func (s *Server) handleAdminKyMessage(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.kymessage.Status())
}

func writeChat(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// chatUser admits the caller to a chat route: rate limit, not an
// administrator identity, KyMessages paired, a live link to the configured
// KyIdentity, and Element in the user's suite app list. It answers the
// refusal itself; 409 kymessage_off means "hide Chat".
func (s *Server) chatUser(w http.ResponseWriter, r *http.Request) (users.User, suitedir.Client, string, bool) {
	ac, ok := authFromContext(r)
	if !ok {
		writeChat(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return users.User{}, suitedir.Client{}, "", false
	}
	if ok, wait := s.kymessageLimiter.allow(ac.UserID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeChat(w, http.StatusTooManyRequests, "rate_limited", "too many chat requests; try again shortly")
		return users.User{}, suitedir.Client{}, "", false
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeChat(w, http.StatusServiceUnavailable, "unavailable", "account unavailable")
		return users.User{}, suitedir.Client{}, "", false
	}
	_, native, err := s.nativeMailAssignment(r.Context(), u.ID)
	if s.nativeMail && u.Role == users.RoleAdmin && (!native || err != nil) {
		s.refuseNativeAdministrator(w, r, u.ID, sso.ErrNativeAdministrator)
		return users.User{}, suitedir.Client{}, "", false
	}
	dir, ok := s.ssoStore.Load().SuiteDirectory()
	element := ""
	if ok && s.kymessage.Available() && u.Active && linkedTo(u, dir.Issuer) {
		element = s.elementOrigin(r.Context(), u.ID)
	}
	if element == "" {
		writeChat(w, http.StatusConflict, "kymessage_off", "chat is not available for this account")
		return users.User{}, suitedir.Client{}, "", false
	}
	return u, dir, element, true
}

// elementOrigin is the https origin of Element's suite app entry, or "".
func (s *Server) elementOrigin(ctx context.Context, userID string) string {
	for _, a := range s.suiteAppsFor(ctx, userID) {
		if a.ID != kymessageChatApp {
			continue
		}
		if u, err := url.Parse(a.URL); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
			return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
		}
	}
	return ""
}

// bareAddress is raw as a lowercased bare address, or "".
func bareAddress(raw string) string {
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Name != "" || a.Address != strings.TrimSpace(raw) {
		return ""
	}
	return strings.ToLower(a.Address)
}

// resolvePeople maps addresses to organisation people under the caller's
// subject (suitedir.Resolve), keyed by lowercased address.
func resolvePeople(ctx context.Context, dir suitedir.Client, sub string, addrs []string) (map[string]suitedir.Person, error) {
	ctx, cancel := context.WithTimeout(ctx, kymessageLookupTimeout)
	defer cancel()
	people, err := dir.Resolve(ctx, sub, addrs)
	if err != nil {
		return nil, err
	}
	out := map[string]suitedir.Person{}
	for _, p := range people {
		out[strings.ToLower(p.Email)] = p
	}
	return out, nil
}

// handleKyMessagePeople answers which of the given addresses are people in
// the organisation, so the client shows Chat only beside them.
func (s *Server) handleKyMessagePeople(w http.ResponseWriter, r *http.Request) {
	u, dir, _, ok := s.chatUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Addresses []string `json:"addresses"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req) != nil || len(req.Addresses) == 0 || len(req.Addresses) > maxKyMessageAddresses {
		writeChat(w, http.StatusBadRequest, "bad_request", "1 to 100 addresses are required")
		return
	}
	var addrs []string
	for _, raw := range req.Addresses {
		a := bareAddress(raw)
		if a == "" {
			writeChat(w, http.StatusBadRequest, "bad_request", "addresses must be bare addresses")
			return
		}
		if !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	known, err := resolvePeople(r.Context(), dir, u.SSOSub, addrs)
	if err != nil {
		// suitedir errors carry neither the query (addresses, sub) nor the credentials.
		s.logger.Warn("kymessage people lookup failed", "actor", u.ID, "error", err.Error())
		writeChat(w, http.StatusServiceUnavailable, "unavailable", "could not look up the organisation; try again")
		return
	}
	inside := []string{}
	for _, a := range addrs {
		if p, ok := known[a]; ok && p.Sub != u.SSOSub {
			inside = append(inside, a)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"inside": inside})
}

// chatErrors are console refusals the client can act on, passed through.
var chatErrors = map[string]struct {
	status int
	msg    string
}{
	"no_target":    {http.StatusNotFound, "This person isn't on KyMessages yet."},
	"no_user":      {http.StatusNotFound, "You have no KyMessages account yet."},
	"rate_limited": {http.StatusTooManyRequests, "too many chat requests; try again shortly"},
}

// handleKyMessageChat asks the console for the Element user view of the
// person at address. The target is resolved here, never taken from the
// client, and the answer must point at Element's own origin.
func (s *Server) handleKyMessageChat(w http.ResponseWriter, r *http.Request) {
	u, dir, element, ok := s.chatUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	addr := ""
	if json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req) == nil {
		addr = bareAddress(req.Address)
	}
	if addr == "" {
		writeChat(w, http.StatusBadRequest, "bad_request", "a bare address is required")
		return
	}
	known, err := resolvePeople(r.Context(), dir, u.SSOSub, []string{addr})
	if err != nil {
		s.logger.Warn("kymessage people lookup failed", "actor", u.ID, "error", err.Error())
		writeChat(w, http.StatusServiceUnavailable, "unavailable", "could not look up the organisation; try again")
		return
	}
	target, ok := known[addr]
	if !ok || target.Sub == "" || target.Sub == u.SSOSub {
		writeChat(w, http.StatusNotFound, "no_target", chatErrors["no_target"].msg)
		return
	}
	rid, err := fsutil.NewUUIDv4()
	if err != nil {
		writeChat(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	eventID := "kypost-chat-" + rid
	status, body, err := s.kymessage.Post(r.Context(), kymessagePathChat, kymessageEventChat, eventID,
		map[string]string{"sub": u.SSOSub, "issuer": u.SSOIssuer, "target_sub": target.Sub})
	if err != nil {
		s.logger.Warn("kymessage request failed", "actor", u.ID, "event_id", eventID, "action", kymessageEventChat, "result", "network")
		writeChat(w, http.StatusBadGateway, "kymessage_unavailable", "KyMessages could not be reached; try again")
		return
	}
	code := suitepair.Outcome(body)
	s.logger.Info("kymessage answered", "actor", u.ID, "event_id", eventID, "action", kymessageEventChat, "result", strconv.Itoa(status)+" "+code)
	if status == http.StatusOK {
		var ans struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(body, &ans) == nil && validChatURL(ans.URL, element) {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, map[string]string{"url": ans.URL})
			return
		}
		s.logger.Warn("kymessage chat answer refused", "actor", u.ID, "event_id", eventID, "result", "bad_answer")
	}
	if e, ok := chatErrors[code]; ok && status == e.status {
		if status == http.StatusTooManyRequests {
			// ponytail: Post returns no headers, so the console's Retry-After is not passed on.
			w.Header().Set("Retry-After", "30")
		}
		writeChat(w, status, code, e.msg)
		return
	}
	writeChat(w, http.StatusBadGateway, "kymessage_unavailable", "KyMessages could not open a chat; try again")
}

// validChatURL accepts only Element's user view on Element's own origin:
// https, no credentials, no path beyond "/", fragment "/user/@…".
func validChatURL(raw, element string) bool {
	if raw == "" || len(raw) > maxKyMessageChatURL || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Opaque == "" && u.RawQuery == "" &&
		(&url.URL{Scheme: u.Scheme, Host: u.Host}).String() == element &&
		(u.Path == "" || u.Path == "/") && strings.HasPrefix(u.Fragment, "/user/@") && len(u.Fragment) > len("/user/@")
}
