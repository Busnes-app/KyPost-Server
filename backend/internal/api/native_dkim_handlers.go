package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/logging"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// DKIMRotationFor names the files rotation works on in configDir; the API
// and the background tick share it.
func DKIMRotationFor(configDir string, resolver mailmsg.DNSResolver) mailmsg.DKIMRotation {
	return mailmsg.DKIMRotation{
		Path: filepath.Join(configDir, "native-relay.json"), KeyPath: filepath.Join(config.SecretDir(), "native-relay.key"),
		Fence: filepath.Join(configDir, sso.NativeDomainsFile), Resolver: resolver,
	}
}

// LogDKIMEvents writes one "dkim rotation" line per transition: selectors,
// phase and trigger, never key material.
func LogDKIMEvents(logger *logging.Logger, events []mailmsg.DKIMEvent) {
	for _, e := range events {
		logger.Info("dkim rotation", "domain", e.Domain, "old_selector", e.OldSelector, "new_selector", e.NewSelector, "phase", e.Phase, "trigger", e.Trigger)
	}
}

// dkimRotationError maps a refusal to its status; other errors keep the
// profile and ask for a retry.
func dkimRotationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mailmsg.ErrDKIMUnknown):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, mailmsg.ErrDKIMNotDirect), errors.Is(err, mailmsg.ErrDKIMRetiring), errors.Is(err, mailmsg.ErrDKIMNoNext), errors.Is(err, mailmsg.ErrDKIMNextChanged):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "DKIM rotation failed; the current key is unchanged and still signs. Retry.", http.StatusServiceUnavailable)
	}
}

type dkimRequest struct {
	Domain     string `json:"domain"`
	Emergency  bool   `json:"emergency"`
	AcceptGap  bool   `json:"acceptGap"`
	Password   string `json:"password"`
	AuthSecret string `json:"authSecret"`
}

// dkimGate applies the relay PUT's protection: step-up, then the restore hold.
func (s *Server) dkimGate(w http.ResponseWriter, r *http.Request, body *dkimRequest) bool {
	if !s.nativeDomainGate(w, r, body) {
		return false
	}
	if err := sso.RequireNativeRestoreReleased(s.stateDir); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return false
	}
	return true
}

// POST /api/admin/mail-relay/dkim/rotate prepares a next key (the current key
// keeps signing until its record is live) or, with emergency and acceptGap,
// replaces the active key at once.
func (s *Server) handleDKIMRotate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body dkimRequest
	if !s.dkimGate(w, r, &body) {
		return
	}
	if body.Emergency && !body.AcceptGap {
		http.Error(w, "emergency rotation signs with a key whose record is not published yet; send acceptGap to accept that mail fails DKIM until it is", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rotation := DKIMRotationFor(s.configDir, s.dnsResolver)
	now := s.dkimNow()
	status := map[string]any{}
	var c mailmsg.DomainRelay
	var events []mailmsg.DKIMEvent
	var err error
	if body.Emergency {
		c, events, err = rotation.RotateNow(ctx, now, body.Domain)
		if err == nil {
			active := activeRecord(c, now, body.Domain)
			old := ""
			if len(events) == 1 && events[0].OldSelector != "" {
				old = " Remove the old record " + events[0].OldSelector + "._domainkey." + body.Domain + " now."
			}
			status["record"] = active
			status["warning"] = fmt.Sprintf("Mail from %s will fail DKIM until you publish %s. Receivers may reject it or mark it as spam.%s", body.Domain, active.RecordName, old)
		}
	} else {
		var next mailmsg.DKIMKey
		c, next, events, err = rotation.Rotate(ctx, now, body.Domain)
		if err == nil {
			for _, rec := range c.DKIMRecords(now) {
				if rec.Domain == body.Domain && rec.Selector == next.Selector {
					status["record"] = rec
				}
			}
		}
	}
	if err != nil {
		dkimRotationError(w, err)
		return
	}
	LogDKIMEvents(s.logger, events)
	for k, v := range s.relayStatus(c, true) {
		status[k] = v
	}
	writeJSON(w, http.StatusOK, status)
}

func activeRecord(c mailmsg.DomainRelay, now time.Time, domain string) mailmsg.DNSRecord {
	for _, rec := range c.DKIMRecords(now) {
		if rec.Domain == domain && rec.State == mailmsg.DKIMActive {
			return rec
		}
	}
	return mailmsg.DNSRecord{}
}

// POST /api/admin/mail-relay/dkim/confirm looks up the waiting next record
// and switches signing to it once it is live. It shares the deliverability
// check's DNS limit.
func (s *Server) handleDKIMConfirm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body dkimRequest
	if !s.dkimGate(w, r, &body) {
		return
	}
	if ok, wait := s.deliverabilityCooldown.allow("deliverability"); !ok {
		seconds := int(math.Ceil(wait.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		http.Error(w, fmt.Sprintf("DNS check ran recently; wait %d seconds before retrying", seconds), http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c, check, events, err := DKIMRotationFor(s.configDir, s.dnsResolver).Confirm(ctx, s.dkimNow(), body.Domain)
	if errors.Is(err, mailmsg.ErrDKIMNotLive) {
		http.Error(w, fmt.Sprintf("The new DKIM record is not live yet (%s): %s. KyPost keeps signing with the current key.", check.Status, check.Message), http.StatusConflict)
		return
	}
	if err != nil {
		dkimRotationError(w, err)
		return
	}
	LogDKIMEvents(s.logger, events)
	writeJSON(w, http.StatusOK, s.relayStatus(c, true))
}
