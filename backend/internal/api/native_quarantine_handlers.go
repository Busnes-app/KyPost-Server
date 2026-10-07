package api

import (
	"database/sql"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// openHolding opens the receiving spool; nil without error when receiving was
// never initialized.
func (s *Server) openHolding() (*ingress.Store, error) {
	dir := filepath.Join(s.stateDir, "receiving")
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return ingress.OpenExisting(dir, ingress.ReceivingLimits)
}

// handleCloudflareReceivingStatus reports the continuous Cloudflare profile:
// state, revisions, times and counts, never addresses or envelopes.
func (s *Server) handleCloudflareReceivingStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.nativeMail {
		http.Error(w, "native mail is disabled", http.StatusNotFound)
		return
	}
	status, err := cfreceiving.CurrentStatus(r.Context(), cfreceiving.Keys{Dir: config.SecretDir()}, filepath.Join(s.stateDir, "receiving"))
	if err != nil {
		http.Error(w, "cloudflare receiving state unreadable; preserve it and repair", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleQuarantineList pages quarantined envelopes (?after=<sequence>), never
// bodies, subjects or headers.
func (s *Server) handleQuarantineList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	after, err := int64(0), error(nil)
	if v := r.URL.Query().Get("after"); v != "" {
		after, err = strconv.ParseInt(v, 10, 64)
	}
	if err != nil || after < 0 {
		http.Error(w, "invalid after", http.StatusBadRequest)
		return
	}
	if !s.nativeMail {
		http.Error(w, "native mail is disabled", http.StatusNotFound)
		return
	}
	out := []sso.QuarantinedDelivery{}
	holding, err := s.openHolding()
	if err == nil && holding != nil {
		defer holding.Close()
		out, err = s.ssoLifecycle.QuarantinedDeliveries(r.Context(), holding, after)
	}
	if nativeMigrationRefused(w, err) {
		return
	}
	if err != nil {
		http.Error(w, "receiving store unreadable; preserve it and repair", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

func (s *Server) handleQuarantineRelease(w http.ResponseWriter, r *http.Request) {
	s.changeQuarantine(w, r, "release_quarantine", "released")
}

func (s *Server) handleQuarantineDiscard(w http.ResponseWriter, r *http.Request) {
	s.changeQuarantine(w, r, "discard_quarantine", "discarded")
}

// changeQuarantine is step-up confirmed and audited with the delivery ID only.
// {"toCurrentOwner": true, "currentMailbox": "<id shown>"} releases an
// unresolved delivery (original owner unknown) to its address's current owner,
// only while that owner's mailbox is still the one the administrator reviewed;
// the step-up binds both, and the audit names that action.
func (s *Server) changeQuarantine(w http.ResponseWriter, r *http.Request, action, done string) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		ToCurrentOwner bool   `json:"toCurrentOwner"`
		CurrentMailbox string `json:"currentMailbox"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	if body.ToCurrentOwner && (done != "released" || body.CurrentMailbox == "") || !body.ToCurrentOwner && body.CurrentMailbox != "" {
		http.Error(w, "invalid mail administration request", http.StatusBadRequest)
		return
	}
	if body.ToCurrentOwner {
		action = "release_quarantine_to_current_owner"
	}
	ac, _ := authFromContext(r)
	gateway, id := r.PathValue("gateway"), r.PathValue("id")
	result := "refused"
	// Malformed path values are neither logged nor looked up.
	valid := ingress.ValidIdentifier(gateway) && ingress.ValidIdentifier(id)
	if !valid {
		gateway, id = "", ""
	}
	defer func() {
		s.logger.Info("receiving quarantine change", "actor", ac.UserID, "task_id", "native-receiving", "action", action, "target", gateway+"/"+id, "result", result, "correlation_id", id)
	}()
	if !s.nativeMail || !valid {
		http.Error(w, "quarantined delivery not found", http.StatusNotFound)
		return
	}
	holding, err := s.openHolding()
	if err == nil && holding == nil {
		err = sql.ErrNoRows
	}
	if err == nil {
		defer holding.Close()
		if done == "released" {
			// A disabled SSO configuration has no issuer, so admission refuses.
			issuer := ""
			if settings := s.ssoStore.Load(); settings.Enabled {
				issuer = settings.IssuerURL
			}
			err = s.ssoLifecycle.ReleaseQuarantined(r.Context(), s.stateDir, issuer, s.users, holding, gateway, id, body.CurrentMailbox)
		} else if err = sso.RequireNativeRestoreReleased(s.stateDir); err == nil {
			// partially_released: an interrupted release may have reached
			// some frozen mailboxes before this discard.
			done, err = holding.Discard(r.Context(), gateway, id)
		}
	}
	switch {
	case err == nil:
		result = done
		writeJSON(w, http.StatusOK, map[string]any{"gateway": gateway, "id": id, "result": done})
	case nativeMigrationRefused(w, err):
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "quarantined delivery not found", http.StatusNotFound)
	case errors.Is(err, ingress.ErrNotQuarantined), errors.Is(err, ingress.ErrLease), errors.Is(err, sso.ErrQuarantineRelease), errors.Is(err, sso.ErrNativeRestoreHold):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "quarantine change refused; the holding copy is retained. Check mailbox capacity and storage, then retry", http.StatusServiceUnavailable)
	}
}

func (s *Server) senderBlocks() ingress.Blocks {
	return ingress.NewBlocks(filepath.Join(s.stateDir, "receiving"))
}

// handleSenderBlocksList lists manual and automatic sender blocks in force.
func (s *Server) handleSenderBlocksList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.nativeMail {
		http.Error(w, "native mail is disabled", http.StatusNotFound)
		return
	}
	list, err := s.senderBlocks().List(time.Now())
	if err != nil {
		http.Error(w, "sender block list unreadable; preserve it and repair", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocks": append([]ingress.SenderBlock{}, list...)})
}

// handleSenderBlockAdd blocks {"kind":"address"|"domain","value",
// "until"?: Unix ms, "reason"?: code} for both receiving profiles. Step-up
// confirmed; the audit carries the block ID, never the address.
func (s *Server) handleSenderBlockAdd(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Kind   string `json:"kind"`
		Value  string `json:"value"`
		Until  *int64 `json:"until"`
		Reason string `json:"reason"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	ac, _ := authFromContext(r)
	result, id := "refused", ""
	// Same correlation as the CLI: the ID whenever the value normalizes.
	if value, err := ingress.NormalizeBlock(body.Kind, body.Value); err == nil {
		id = ingress.BlockID(body.Kind, value)
	}
	defer func() { s.auditSenderBlock(ac.UserID, "block_sender", body.Kind, result, id) }()
	if !s.nativeMail {
		http.Error(w, "native mail is disabled", http.StatusNotFound)
		return
	}
	set, err := s.nativeDomains.ReadSet()
	if nativeMigrationRefused(w, err) {
		return
	}
	var block ingress.SenderBlock
	if err == nil {
		block, err = s.senderBlocks().Put(r.Context(), ingress.SenderBlock{Kind: body.Kind, Value: body.Value, Until: body.Until, Reason: body.Reason, Source: "manual", Actor: ac.UserID}, slices.Collect(maps.Keys(set.Domains)), time.Now())
	}
	if err == nil {
		result = "blocked"
		writeJSON(w, http.StatusOK, map[string]any{"block": block})
		return
	}
	senderBlockError(w, err)
}

// handleSenderBlockRemove unblocks /api/admin/receiving/blocks/{id}. The
// path carries the block ID, never the address, so proxy logs record none.
func (s *Server) handleSenderBlockRemove(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.nativeDomainGate(w, r, &struct{}{}) {
		return
	}
	ac, _ := authFromContext(r)
	id, result := r.PathValue("id"), "refused"
	found, err := false, error(nil)
	if s.nativeMail {
		found, err = s.senderBlocks().Remove(r.Context(), id, time.Now())
	}
	if errors.Is(err, ingress.ErrBlockInvalid) {
		id = "" // malformed path values are not logged
	}
	defer func() { s.auditSenderBlock(ac.UserID, "unblock_sender", "", result, id) }()
	switch {
	case !s.nativeMail:
		http.Error(w, "native mail is disabled", http.StatusNotFound)
	case err != nil:
		senderBlockError(w, err)
	case !found:
		http.Error(w, "no such block in force", http.StatusNotFound)
	default:
		result = "unblocked"
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "result": result})
	}
}

func (s *Server) auditSenderBlock(actor, action, kind, result, id string) {
	if kind != "address" && kind != "domain" {
		kind = ""
	}
	s.logger.Info("receiving sender block change", "actor", actor, "task_id", "native-receiving", "action", action, "target", kind, "result", result, "correlation_id", id)
}

func senderBlockError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ingress.ErrBlockInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ingress.ErrBlockOwn), errors.Is(err, ingress.ErrBlockFull), errors.Is(err, ingress.ErrBlockStore):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "sender block change refused; the list is unchanged. Check receiving storage, then retry", http.StatusServiceUnavailable)
	}
}

// refuseBlockedDomain answers 409 when a sender block matches domain or an
// address on it: adding it as a mail domain would refuse its own mail.
func (s *Server) refuseBlockedDomain(w http.ResponseWriter, domain string) bool {
	list, err := s.senderBlocks().List(time.Now())
	if err != nil {
		http.Error(w, "sender block list unreadable; preserve it and repair", http.StatusServiceUnavailable)
		return true
	}
	domain = cfreceiving.LowerASCII(domain)
	if slices.ContainsFunc(list, func(b ingress.SenderBlock) bool {
		return b.Value == domain || b.Kind == "address" && strings.HasSuffix(b.Value, "@"+domain)
	}) {
		http.Error(w, "a sender block matches this domain or an address on it; unblock it first (receiving blocks remove, or DELETE /api/admin/receiving/blocks/{id}), then add the domain", http.StatusConflict)
		return true
	}
	return false
}
