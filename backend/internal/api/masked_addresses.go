package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// maskedDomainsSetting is the administrator's list of domains users may make
// masked addresses on, comma-separated in root state.db meta. Empty is off.
const maskedDomainsSetting = "masked_address_domains"

const (
	// maskedLimit caps a user's masked addresses that are not deleted.
	maskedLimit = 100
	// maskedPerDay caps creations in any 24 hours, deleted ones included,
	// so the feature cannot drive mass sign-ups.
	maskedPerDay    = 20
	maskedNoteRunes = 100
)

var maskedLifetimes = map[string]time.Duration{"": 0, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

var (
	errMaskedUnknown = errors.New("masked address not found")
	errMaskedDeleted = errors.New("a deleted masked address stays reserved and cannot be used again")
	errMaskedExpired = errors.New("this disposable address has expired")
	errMaskedLimit   = errors.New("masked address limit reached (" + strconv.Itoa(maskedLimit) + "); delete one you no longer use")
	errMaskedRate    = errors.New("too many masked addresses made today (" + strconv.Itoa(maskedPerDay) + " per 24 hours); try again later")
	errMaskedNote    = errors.New("note must be at most 100 characters")
)

// Masked local parts read "<adjective>.<animal>.<4 digits>": about 40 million
// per domain, so a guess rarely reaches a live one.
var (
	maskedAdjectives = strings.Fields("amber brave brisk calm clever cosmic crisp curly dapper dusty eager fancy fluffy fond gentle glad golden grand happy hazy jolly keen kind lively lucky mellow merry misty modest neat nimble noble olive patient plain polite proud quick quiet rapid rosy rustic shy silent silver sleek sly snowy soft sunny swift tidy tiny tranquil vivid warm wavy wild windy wise witty young zesty")
	maskedAnimals    = strings.Fields("badger beaver bison bobcat camel cobra condor coyote crane dingo dove eagle egret falcon ferret finch fox gecko gibbon goose heron hippo ibis jackal koala lemur lark llama lynx magpie marten mink moose newt ocelot okapi orca osprey otter owl panda parrot pelican puffin quail rabbit raven robin salmon seal shrew skunk sloth swan tapir tiger toucan trout turtle walrus weasel whale wolf wombat yak zebra")
)

func randomPick(list []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return "", err
	}
	return list[n.Int64()], nil
}

func randomMaskedAddress(domain string) (string, error) {
	adjective, err := randomPick(maskedAdjectives)
	if err != nil {
		return "", err
	}
	animal, err := randomPick(maskedAnimals)
	if err != nil {
		return "", err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(9000))
	if err != nil {
		return "", err
	}
	return adjective + "." + animal + "." + strconv.FormatInt(1000+n.Int64(), 10) + "@" + domain, nil
}

// maskedDomains reads the administrator's list; only an unset one reads as
// empty. Any other failure is an error, never "no domains".
func (s *Server) maskedDomains() ([]string, error) {
	if s.globalStore == nil {
		return nil, errors.New("the server state database did not open")
	}
	v, err := s.globalStore.Setting(maskedDomainsSetting)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return []string{}, nil
	case err != nil:
		return nil, err
	case v == "":
		return []string{}, nil
	}
	return strings.Split(v, ","), nil
}

// handleMaskedDomains is GET|PUT /api/admin/masked-domains {domains}. Each
// must be a configured mail domain. Removing one stops new addresses there;
// existing ones keep working until their owners delete them.
func (s *Server) handleMaskedDomains(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.nativeMail || s.nativeDomains == nil {
		http.Error(w, "masked addresses need native mail", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Domains []string `json:"domains"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req) != nil || req.Domains == nil || len(req.Domains) > 100 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		set, err := s.nativeDomains.ReadSet()
		if err != nil {
			http.Error(w, "mail domains unreadable", http.StatusServiceUnavailable)
			return
		}
		domains := []string{}
		for _, d := range req.Domains {
			d = strings.ToLower(strings.TrimSpace(d))
			if _, ok := set.Domains[d]; !ok {
				http.Error(w, "not a configured mail domain: add and verify it under Mail domain first", http.StatusBadRequest)
				return
			}
			if !slices.Contains(domains, d) {
				domains = append(domains, d)
			}
		}
		slices.Sort(domains)
		ac, _ := authFromContext(r)
		if s.globalStore == nil || s.globalStore.SetSetting(maskedDomainsSetting, strings.Join(domains, ",")) != nil {
			http.Error(w, "could not save the masked address domains", http.StatusServiceUnavailable)
			return
		}
		s.logger.Info("masked address domains change", "actor", ac.UserID, "action", "set_masked_domains", "target", maskedDomainsSetting, "result", "saved", "detail", strconv.Itoa(len(domains)))
	}
	domains, err := s.maskedDomains()
	if err != nil {
		http.Error(w, "could not read the masked address domains", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": domains})
}

// maskedGate admits a hosted (native) account and returns its primary
// mailbox ID; it reports whether the caller may continue.
func (s *Server) maskedGate(w http.ResponseWriter, r *http.Request) (userID, mailboxID string, ok bool) {
	w.Header().Set("Cache-Control", "no-store")
	ac, _ := authFromContext(r)
	a, native, err := s.nativeMailboxAssignment(r.Context(), ac.UserID, "")
	switch {
	case err != nil && native:
		http.Error(w, "native mailbox unavailable; preserve mail and repair account authority", http.StatusServiceUnavailable)
	case err != nil || !native:
		http.Error(w, "plus addressing and masked addresses need a hosted mailbox", http.StatusNotFound)
	default:
		return ac.UserID, a.Owner.Mailbox, true
	}
	return "", "", false
}

// withMaskedSettings runs fn under the user's settings lock (the lock
// config.UpdateUserSettings takes; order settings → domains → directory, so
// fn may change the ledger). save persists st. Mail flows only while both the
// record is live and the ledger address active, so fn closes the record before
// the ledger and opens it after: a crash between the two leaves mail refused.
func (s *Server) withMaskedSettings(userID string, fn func(st *config.UserSettings, save func() error) error) error {
	path := s.userSettingsPath(userID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WithFileLock(path, func() error {
		st, err := config.LoadUserSettings(path)
		if err != nil {
			return err
		}
		return fn(&st, func() error { return config.SaveUserSettings(path, st) })
	})
}

// maskedClose reserves address in the ledger unless it already is, the
// existing released-alias mechanism: routes off, never reissued.
func (s *Server) maskedClose(ctx context.Context, address string) error {
	x, err := s.ssoLifecycle.NativeAddress(address)
	if err != nil || x.Address == "" || x.State == "reserved" {
		return err
	}
	if _, err = s.ssoLifecycle.ReleaseNativeAlias(ctx, s.stateDir, address); errors.Is(err, sso.ErrNativeRoutesPending) {
		return nil
	}
	return err
}

// maskedOpen hands a reserved address back to its own mailbox.
func (s *Server) maskedOpen(ctx context.Context, mailboxID, address string) error {
	x, err := s.ssoLifecycle.NativeAddress(address)
	if err != nil {
		return err
	}
	if x.Address == "" || x.Mailbox != mailboxID {
		return errMaskedUnknown
	}
	if x.State != "reserved" {
		return nil
	}
	if _, err = s.ssoLifecycle.ReassignNativeAddress(ctx, s.stateDir, address, mailboxID); errors.Is(err, sso.ErrNativeRoutesPending) {
		return nil
	}
	return err
}

// maskedHeld reports whether address is a user's masked address: an
// administrator may not release or reassign it (its owner pauses or deletes).
func (s *Server) maskedHeld(address string) (bool, error) {
	address = strings.ToLower(address)
	x, err := s.ssoLifecycle.NativeAddress(address)
	if err != nil || x.Address == "" {
		return false, err
	}
	a, ok, err := s.ssoLifecycle.NativeMailboxAssignment(x.Mailbox)
	if err != nil || !ok {
		return false, err
	}
	st, err := config.LoadUserSettings(s.userSettingsPath(a.UserID()))
	if err != nil {
		return false, err
	}
	_, held := st.Masked(address)
	return held, nil
}

type addressesView struct {
	PlusAddressing bool                   `json:"plusAddressing"`
	MaskedDomains  []string               `json:"maskedDomains"`
	Masked         []config.MaskedAddress `json:"masked"`
	MaskedLimit    int                    `json:"maskedLimit"`
	MaskedPerDay   int                    `json:"maskedPerDay"`
}

// writeAddresses answers the caller's address settings. A record the ledger
// does not route is shown paused (Resume repairs it), an expired one deleted.
func (s *Server) writeAddresses(w http.ResponseWriter, userID string) {
	st, err := config.LoadUserSettings(s.userSettingsPath(userID))
	domains, domainErr := s.maskedDomains()
	if err != nil || domainErr != nil {
		http.Error(w, "address settings unreadable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	view := addressesView{PlusAddressing: !st.RefusePlusAddresses, MaskedDomains: domains, Masked: []config.MaskedAddress{}, MaskedLimit: maskedLimit, MaskedPerDay: maskedPerDay}
	for _, m := range st.MaskedAddresses {
		switch x, err := s.ssoLifecycle.NativeAddress(m.Address); {
		case m.State == "active" && m.ExpiresAt != 0 && now.Unix() >= m.ExpiresAt:
			m.State = "deleted"
		case m.State == "active" && (err != nil || x.State != "active"):
			m.State = "paused"
		}
		view.Masked = append(view.Masked, m)
	}
	writeJSON(w, http.StatusOK, view)
}

// handleAddresses is GET /api/addresses and PUT /api/addresses/plus {enabled}.
func (s *Server) handleAddresses(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.maskedGate(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req) != nil || req.Enabled == nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := config.UpdateUserSettings(s.userSettingsPath(userID), func(st *config.UserSettings) error {
			st.RefusePlusAddresses = !*req.Enabled
			return nil
		}); err != nil {
			http.Error(w, "settings could not be saved", http.StatusServiceUnavailable)
			return
		}
		s.logger.Info("plus addressing switched", "actor", userID, "action", "set_plus_addressing", "result", strconv.FormatBool(*req.Enabled))
	}
	s.writeAddresses(w, userID)
}

func maskedNote(note string) (string, bool) {
	note = strings.TrimSpace(note)
	return note, utf8.ValidString(note) && utf8.RuneCountInString(note) <= maskedNoteRunes && !strings.ContainsFunc(note, unicode.IsControl)
}

// answerMasked maps errors and audits by user, never by address.
func (s *Server) answerMasked(w http.ResponseWriter, userID, action string, err error) bool {
	result := "committed"
	defer func() {
		s.logger.Info("masked address change", "actor", userID, "action", action, "target", userID, "result", result)
	}()
	if err == nil {
		return false
	}
	result = "refused"
	switch {
	case errors.Is(err, errMaskedNote):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errMaskedUnknown):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errMaskedLimit), errors.Is(err, errMaskedRate):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, errMaskedDeleted), errors.Is(err, errMaskedExpired), errors.Is(err, sso.ErrNativeAddressConflict), errors.Is(err, sso.ErrNativeRestoreHold), errors.Is(err, sso.ErrNativeAddressDomain):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, sso.ErrNativeAddressAdministrator):
		http.Error(w, "administrator accounts own no masked addresses; use your everyday account", http.StatusConflict)
	default:
		http.Error(w, "masked address change refused; storage must be readable", http.StatusServiceUnavailable)
	}
	return true
}

// handleMaskedCreate is POST /api/addresses/masked {domain, note, expires}.
// expires "" makes a masked address, "24h"/"7d"/"30d" a disposable one.
func (s *Server) handleMaskedCreate(w http.ResponseWriter, r *http.Request) {
	userID, mailboxID, ok := s.maskedGate(w, r)
	if !ok {
		return
	}
	var req struct {
		Domain  string `json:"domain"`
		Note    string `json:"note"`
		Expires string `json:"expires"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 2<<10)).Decode(&req) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	note, noteOK := maskedNote(req.Note)
	lifetime, lifetimeOK := maskedLifetimes[req.Expires]
	if !noteOK || !lifetimeOK {
		http.Error(w, "note must be at most 100 characters; expiry must be 24h, 7d, 30d or none", http.StatusBadRequest)
		return
	}
	domains, err := s.maskedDomains()
	if err != nil {
		http.Error(w, "masked address domains unreadable; try again later", http.StatusServiceUnavailable)
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if !slices.Contains(domains, domain) {
		http.Error(w, "choose one of the masked address domains your administrator set", http.StatusBadRequest)
		return
	}
	var created config.MaskedAddress
	err = s.withMaskedSettings(userID, func(st *config.UserSettings, save func() error) error {
		now := time.Now()
		live, recent := 0, 0
		for _, m := range st.MaskedAddresses {
			if m.State != "deleted" {
				live++
			}
			if now.Unix()-m.CreatedAt < 24*60*60 {
				recent++
			}
		}
		if live >= maskedLimit {
			return errMaskedLimit
		}
		if recent >= maskedPerDay {
			return errMaskedRate
		}
		for range 5 {
			address, err := randomMaskedAddress(domain)
			if err != nil {
				return err
			}
			if x, err := s.ssoLifecycle.NativeAddress(address); err != nil {
				return err
			} else if x.Address != "" {
				continue
			}
			created = config.MaskedAddress{Address: address, Note: note, State: "active", CreatedAt: now.Unix()}
			if lifetime > 0 {
				created.ExpiresAt = now.Add(lifetime).Unix()
			}
			// The record first: a crash before the ledger write leaves a
			// record with no address, which the sweep marks deleted.
			st.MaskedAddresses = append(st.MaskedAddresses, created)
			if err := save(); err != nil {
				return err
			}
			_, err = s.ssoLifecycle.AddNativeAlias(context.WithoutCancel(r.Context()), s.stateDir, mailboxID, address)
			if err == nil || errors.Is(err, sso.ErrNativeRoutesPending) {
				return nil
			}
			st.MaskedAddresses = st.MaskedAddresses[:len(st.MaskedAddresses)-1]
			if saveErr := save(); saveErr != nil || !errors.Is(err, sso.ErrNativeAddressConflict) {
				return err
			}
		}
		return sso.ErrNativeAddressConflict
	})
	if s.answerMasked(w, userID, "create_masked", err) {
		return
	}
	writeJSON(w, http.StatusOK, created)
}

// handleMaskedUpdate is PUT /api/addresses/masked/{address} {note?, state?}
// (state "active" or "paused") and DELETE, which keeps it reserved forever.
func (s *Server) handleMaskedUpdate(w http.ResponseWriter, r *http.Request) {
	userID, mailboxID, ok := s.maskedGate(w, r)
	if !ok {
		return
	}
	var req struct {
		Note  *string `json:"note"`
		State string  `json:"state"`
	}
	if r.Method == http.MethodDelete {
		req.State = "deleted"
	} else if json.NewDecoder(io.LimitReader(r.Body, 2<<10)).Decode(&req) != nil || req.State != "" && req.State != "active" && req.State != "paused" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	address := strings.ToLower(r.PathValue("address"))
	var updated config.MaskedAddress
	err := s.withMaskedSettings(userID, func(st *config.UserSettings, save func() error) error {
		i := slices.IndexFunc(st.MaskedAddresses, func(m config.MaskedAddress) bool { return m.Address == address })
		if i < 0 {
			return errMaskedUnknown
		}
		m := &st.MaskedAddresses[i]
		if m.State == "deleted" {
			return errMaskedDeleted
		}
		if req.Note != nil {
			note, ok := maskedNote(*req.Note)
			if !ok {
				return errMaskedNote
			}
			m.Note = note
		}
		ctx := context.WithoutCancel(r.Context())
		switch req.State {
		case "active":
			if m.ExpiresAt != 0 && time.Now().Unix() >= m.ExpiresAt {
				return errMaskedExpired
			}
			if err := s.maskedOpen(ctx, mailboxID, address); err != nil {
				return err
			}
			m.State = "active"
		case "paused", "deleted":
			m.State = req.State
			if err := save(); err != nil {
				return err
			}
			updated = *m
			return s.maskedClose(ctx, address)
		}
		updated = *m
		return save()
	})
	if s.answerMasked(w, userID, "update_masked_"+cmp.Or(req.State, "note"), err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// sweepMaskedAddresses is the minute pass: an expired disposable address or
// a record whose ledger address never appeared becomes deleted, and any
// record not active has its ledger address reserved. It never opens mail.
// ponytail: reads every account's settings each minute; fine for a household
// or office. Past a few thousand accounts, index accounts holding records.
func (s *Server) sweepMaskedAddresses(ctx context.Context) {
	all, err := s.users.List()
	if err != nil {
		return
	}
	now := time.Now()
	for _, u := range all {
		if ctx.Err() != nil {
			return
		}
		st, err := config.LoadUserSettings(s.userSettingsPath(u.ID))
		if err != nil || len(st.MaskedAddresses) == 0 {
			continue
		}
		closed := 0
		err = s.withMaskedSettings(u.ID, func(st *config.UserSettings, save func() error) error {
			changed := false
			for i := range st.MaskedAddresses {
				m := &st.MaskedAddresses[i]
				if m.State == "deleted" {
					continue
				}
				x, err := s.ssoLifecycle.NativeAddress(m.Address)
				if err != nil {
					return err
				}
				if x.Address == "" || m.ExpiresAt != 0 && now.Unix() >= m.ExpiresAt {
					m.State, changed = "deleted", true
				}
			}
			if changed {
				if err := save(); err != nil {
					return err
				}
			}
			for _, m := range st.MaskedAddresses {
				if m.State == "active" {
					continue
				}
				if x, err := s.ssoLifecycle.NativeAddress(m.Address); err != nil {
					return err
				} else if x.Address != "" && x.State != "reserved" {
					if err := s.maskedClose(ctx, m.Address); err != nil {
						return err
					}
					closed++
				}
			}
			return nil
		})
		if err != nil || closed > 0 {
			result := "closed"
			if err != nil {
				result = "pending"
			}
			s.logger.Info("masked address sweep", "actor", "system", "action", "sweep_masked", "target", u.ID, "result", result, "detail", strconv.Itoa(closed))
		}
	}
}
