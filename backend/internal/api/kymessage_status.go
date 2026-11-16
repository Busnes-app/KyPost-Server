package api

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// Colleagues' status from KyMessages (KyMessages docs/PRESENCE.md, KyPost
// docs/KYMESSAGES.md "Status"): one signed, batched lookup under the
// kymessage pair; organisation people only.
const (
	kymessageEventStatus = "kypost.presence.lookup"
	kymessagePathStatus  = "/api/suite/presence/lookup"
	maxStatusAddresses   = 50
	statusCacheFor       = 60 * time.Second
	statusCacheMax       = 5000
)

var statusStates = map[string]bool{"available": true, "busy": true, "in_a_meeting": true, "away": true, "out_of_office": true, "unknown": true}

type personStatus struct {
	State string `json:"state"`
	Until string `json:"until,omitempty"`
}

// statusCache keeps KyMessages' answers by subject for statusCacheFor.
type statusCache struct {
	mu sync.Mutex
	m  map[string]struct {
		st personStatus
		at time.Time
	}
}

func (c *statusCache) get(sub string, now time.Time) (personStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[sub]
	return e.st, ok && now.Sub(e.at) < statusCacheFor
}

func (c *statusCache) put(sub string, st personStatus, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > statusCacheMax {
		c.m = map[string]struct {
			st personStatus
			at time.Time
		}{}
	}
	c.m[sub] = struct {
		st personStatus
		at time.Time
	}{st, now}
}

// handleKyMessageStatus answers {"addresses":[...]} with the status of the
// organisation people among them: {"people":[{"address","state","until"?}]}.
// Anyone outside the organisation is absent.
func (s *Server) handleKyMessageStatus(w http.ResponseWriter, r *http.Request) {
	u, dir, _, ok := s.chatUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Addresses []string `json:"addresses"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req) != nil || len(req.Addresses) == 0 {
		writeChat(w, http.StatusBadRequest, "bad_request", "1 to 50 addresses are required")
		return
	}
	if len(req.Addresses) > maxStatusAddresses {
		writeChat(w, http.StatusBadRequest, "too_many", "at most 50 addresses at a time")
		return
	}
	var addrs []string
	for _, raw := range req.Addresses {
		if a := bareAddress(raw); a != "" && !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	people := []map[string]string{}
	answer := func() {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"people": people})
	}
	if len(addrs) == 0 {
		answer()
		return
	}
	known, err := resolvePeople(r.Context(), dir, u.SSOSub, addrs)
	if err != nil {
		s.logger.Warn("kymessage people lookup failed", "actor", u.ID, "error", err.Error())
		writeChat(w, http.StatusServiceUnavailable, "unavailable", "could not look up the organisation; try again")
		return
	}
	now := time.Now()
	found := map[string]personStatus{}
	var ask []string
	for _, a := range addrs {
		if p, ok := known[a]; ok && p.Sub != "" {
			if st, hit := s.kymessageStatus.get(p.Sub, now); hit {
				found[p.Sub] = st
			} else if !slices.Contains(ask, p.Sub) {
				ask = append(ask, p.Sub)
			}
		}
	}
	if len(ask) > 0 {
		rid, err := fsutil.NewUUIDv4()
		if err != nil {
			writeChat(w, http.StatusServiceUnavailable, "unavailable", "try again")
			return
		}
		eventID := "kypost-status-" + rid
		status, body, err := s.kymessage.Post(r.Context(), kymessagePathStatus, kymessageEventStatus, eventID,
			map[string]any{"sub": u.SSOSub, "issuer": u.SSOIssuer, "subs": ask})
		var ans struct {
			People []struct {
				Sub, State, Until string
			} `json:"people"`
		}
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &ans) != nil {
			s.logger.Warn("kymessage status lookup failed", "actor", u.ID, "event_id", eventID, "status", strconv.Itoa(status))
			writeChat(w, http.StatusBadGateway, "kymessage_unavailable", "KyMessages could not be reached; try again")
			return
		}
		for _, p := range ans.People {
			if !slices.Contains(ask, p.Sub) {
				continue
			}
			st := personStatus{State: "unknown"}
			if statusStates[p.State] {
				st.State = p.State
			}
			if _, err := time.Parse(time.RFC3339, p.Until); err == nil && (st.State == "in_a_meeting" || st.State == "out_of_office") {
				st.Until = p.Until
			}
			found[p.Sub] = st
			s.kymessageStatus.put(p.Sub, st, now)
		}
	}
	for _, a := range addrs {
		if p, ok := known[a]; ok {
			if st, ok := found[p.Sub]; ok {
				row := map[string]string{"address": strings.ToLower(a), "state": st.State}
				if st.Until != "" {
					row["until"] = st.Until
				}
				people = append(people, row)
			}
		}
	}
	answer()
}
