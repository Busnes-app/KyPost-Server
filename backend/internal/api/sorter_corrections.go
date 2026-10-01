package api

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailcache"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// The embedding sorter learns from the user moving a message to another label.
// Both ways that is observed end here: a label added through
// POST /api/inbox/actions, and a keyword change made in another IMAP client
// that the inbox cache sync notices. The decision of whether it is a lesson at
// all (a prediction exists, its check matches, the label differs) belongs to
// state.Store.RecordSorterCorrection; this file only works out which label the
// user meant. Learning is best effort and never fails the request.

// labelForKeyword maps an IMAP keyword back to the account label that produces
// it: the label itself, or a label whose keyword mapping includes it.
func labelForKeyword(keyword string, labels config.UserLabelSettings) string {
	for _, label := range labels.Allowlist {
		if strings.EqualFold(label, keyword) {
			return label
		}
	}
	for _, label := range labels.Allowlist {
		for _, mapped := range labels.KeywordMappings[label] {
			if strings.EqualFold(mapped, keyword) {
				return label
			}
		}
	}
	return ""
}

// learnFromLabelAction records a correction for each message the user just
// labelled with keyword through the reader. Label/unlabel act on the default
// mailbox (see handleInboxActions), which is the INBOX window of the cache.
func (s *Server) learnFromLabelAction(userID, keyword string, messageIDs []string) {
	labels := s.userLabels(userID)
	label := labelForKeyword(keyword, labels)
	if label == "" {
		return
	}
	store, cache, ok := s.sorterStores(userID)
	if !ok {
		return
	}
	// Asking for more than any window holds returns the whole window.
	entries, _, err := cache.Snapshot(inboxCacheMailboxKey(""), math.MaxInt32)
	if err != nil {
		s.logger.Error("sorter: cannot read mail cache; label change not learned", "user_id", userID, "error", err.Error())
		return
	}
	byID := make(map[string]mailcache.Entry, len(entries))
	for _, e := range entries {
		byID[strconv.Itoa(e.UID)] = e
	}
	for _, id := range messageIDs {
		if e, ok := byID[id]; ok {
			s.recordSorterCorrection(userID, store, id, e, label)
		}
	}
}

// learnFromSyncedKeywords inspects INBOX entries whose metadata changed since
// the caller last synced. A message now carrying exactly one account label
// other than the one it was filed under was relabelled, in KyPost or elsewhere.
// ponytail: only seen when somebody opens the KyPost inbox; a relabel made on a
// phone and never followed by a KyPost visit within the prediction retention
// window is not learned.
func (s *Server) learnFromSyncedKeywords(userID, cacheKey string, updated []mailcache.Entry) {
	if cacheKey != inboxCacheMailboxKey("") || len(updated) == 0 {
		return
	}
	labels := s.userLabels(userID)
	store, _, ok := s.sorterStores(userID)
	if !ok {
		return
	}
	for _, e := range updated {
		id := strconv.Itoa(e.UID)
		_, predicted, found, err := store.SorterPrediction(id)
		if err != nil {
			s.logger.Error("sorter: cannot read prediction", "user_id", userID, "message_id", id, "error", err.Error())
			return
		}
		if !found {
			continue
		}
		var present []string
		for _, k := range e.Keywords {
			if l := labelForKeyword(k, labels); l != "" && !slices.Contains(present, l) {
				present = append(present, l)
			}
		}
		others := slices.DeleteFunc(slices.Clone(present), func(l string) bool { return l == predicted })
		switch {
		case len(others) == 1:
			s.recordSorterCorrection(userID, store, id, e, others[0])
		case len(others) == 0 && slices.Contains(present, predicted):
			// Back under the sorter's own label: withdraws an earlier correction.
			s.recordSorterCorrection(userID, store, id, e, predicted)
		}
		// Several other labels at once is ambiguous: teach nothing.
	}
}

func (s *Server) sorterStores(userID string) (*state.Store, *mailcache.Store, bool) {
	store, err := s.userStore(userID)
	if err != nil {
		s.logger.Error("sorter: cannot open user state", "user_id", userID, "error", err.Error())
		return nil, nil, false
	}
	cache, err := s.userMailCacheStore(userID)
	if err != nil {
		s.logger.Error("sorter: cannot open mail cache", "user_id", userID, "error", err.Error())
		return nil, nil, false
	}
	return store, cache, true
}

func (s *Server) recordSorterCorrection(userID string, store *state.Store, id string, e mailcache.Entry, label string) {
	learned, err := store.RecordSorterCorrection(id, state.SorterCheck(e.Sender, e.Subject), label)
	if err != nil {
		s.logger.Error("sorter: cannot record correction", "user_id", userID, "message_id", id, "error", err.Error())
		return
	}
	if learned {
		s.logger.Info("sorter: learned from relabel", "user_id", userID, "message_id", id, "selected_label", label)
	}
}
