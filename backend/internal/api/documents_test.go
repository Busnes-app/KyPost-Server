package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// Runtime stays gated. This stopped fixture conversion exercises actual handlers
// against table documents; it is not the operator migration (MH8).
func mountServerDocuments(t *testing.T, s *Server) {
	t.Helper()
	db, err := volumes.Open(s.stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	options := vol.TableOptions{MasterKey: make([]byte, 32), Org: func(_ context.Context, id string) (string, error) { u, err := s.users.Get(id); return u.OrgID, err }, Owner: func(_ context.Context, path string, body []byte) (string, error) {
		if strings.HasPrefix(path, "pickup/") {
			var r struct {
				Sender string `json:"senderUserId"`
			}
			err := json.Unmarshal(body, &r)
			return r.Sender, err
		}
		id, _, _ := strings.Cut(strings.TrimPrefix(path, "mailboxes/"), "/")
		a, ok, err := s.ssoLifecycle.NativeMailboxAssignment(id)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", os.ErrNotExist
		}
		u, err := s.users.GetBySSOSubIssuer(a.Owner.Issuer, a.Owner.Subject)
		return u.ID, err
	}}
	for _, root := range []struct{ kind, path string }{{"config", s.configDir}, {"state", s.stateDir}} {
		table, err := vol.NewTable(db, root.kind, options)
		if err != nil {
			t.Fatal(err)
		}
		err = filepath.WalkDir(root.path, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if name == "users.json" || name == "outbox-idle" || strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
				return nil
			}
			rel, err := filepath.Rel(root.path, path)
			if err != nil {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, err = table.Write(t.Context(), filepath.ToSlash(rel), body, 0)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		release, err := vol.Mount(root.path, table)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	s.userMu.Lock()
	clear(s.userContacts)
	clear(s.userGroups)
	clear(s.userRules)
	clear(s.userSendAs)
	clear(s.userMailCache)
	s.userMu.Unlock()
}

func TestCrossOrgMatrixTableDocuments(t *testing.T) {
	dbbackend.RequirePostgres(t)
	fx, logs := newOrgFixture(t)
	mountServerDocuments(t, fx.srv)
	runCrossOrgMatrix(t, fx, logs)
}

func TestTableDocumentsMemberErasureKeepsOtherMembersKey(t *testing.T) {
	dbbackend.RequirePostgres(t)
	px := newPurgeFixture(t)
	mountServerDocuments(t, px.srv)
	for _, m := range []orgMember{px.a, px.other, px.b} {
		seedContact(px.orgFixture, m)
	}
	if _, err := px.srv.pickupStore.CreateClientSealed(px.other.ID, "other@example.test", "sealed-blob", time.Hour); err != nil {
		t.Fatal(err)
	}
	if w := postDirectory(t, px.srv, testSyncKey, "user.erased", "mh2-member-erasure", 2, orgUser("member-a2", orgA, false)); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	if err := px.srv.runPurges(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := volumes.KeyRows(t.Context(), px.srv.stateDir, orgA); err != nil || n != 1 {
		t.Fatal("member erasure removed the shared key", n, err)
	}
	if n, err := tenancy.DocumentRows(t.Context(), px.srv.stateDir, []string{px.other.ID}); err != nil || n != 0 {
		t.Fatal("erased documents survived", n, err)
	}
	for _, m := range []orgMember{px.a, px.b} {
		store, err := px.srv.userContactsStore(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if list, err := store.List(); err != nil || len(list) == 0 {
			t.Fatal("survivor cannot open contacts", err)
		}
	}
}

func TestTableDocumentConfigFollower(t *testing.T) {
	dbbackend.RequirePostgres(t)
	s := newNativeRuntimeServer(t)
	s.configPath = filepath.Join(s.configDir, "config.yaml")
	makeMultiTenant(t, s)
	mountServerDocuments(t, s)
	if err := config.Save(s.configPath, config.Default()); err != nil {
		t.Fatal(err)
	}
	s.followConfig()
	next := config.Default()
	next.Scan.IntervalSeconds = 137
	if err := config.Save(s.configPath, next); err != nil {
		t.Fatal(err)
	}
	// The same callback is invoked every five seconds by StartConfigFollower.
	s.followConfig()
	s.cfgMu.RLock()
	got := s.cfg.Scan.IntervalSeconds
	s.cfgMu.RUnlock()
	if got != 137 {
		t.Fatal("replica kept stale root config", got)
	}
}

func TestTablePickupConsumeIsOnceAcrossReplicas(t *testing.T) {
	dbbackend.RequirePostgres(t)
	fx, _ := newOrgFixture(t)
	mountServerDocuments(t, fx.srv)
	a := fx.srv.pickupStore
	b := pgpmail.NewPickupStore(filepath.Join(fx.srv.stateDir, "pickup"), "unused-for-client-sealed")
	id, err := a.CreateClientSealed(fx.a.ID, "recipient@example.test", "only-one-blob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	result := make(chan error, 2)
	for _, store := range []*pgpmail.PickupStore{a, b} {
		go func() { <-start; _, err := store.ViewClientSealed(id); result <- err }()
	}
	close(start)
	successes, expired := 0, 0
	for range 2 {
		err := <-result
		if err == nil {
			successes++
		} else if errors.Is(err, pgpmail.ErrPickupExpired) {
			expired++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || expired != 1 {
		t.Fatal("pickup consumed twice", successes, expired)
	}
	if err := b.Sweep(-time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Kind(id); !errors.Is(err, pgpmail.ErrPickupNotFound) {
		t.Fatal("expired pickup survived", err)
	}
}

type contendedDocuments struct {
	writes  int
	failure error
}

func (*contendedDocuments) Read(context.Context, string) ([]byte, int64, error) {
	return []byte(`{}`), 1, nil
}
func (v *contendedDocuments) Write(context.Context, string, []byte, int64) (int64, error) {
	v.writes++
	if v.failure != nil {
		return 0, v.failure
	}
	return 0, vol.ErrStale
}
func (*contendedDocuments) Remove(context.Context, string, int64) error    { return vol.ErrStale }
func (*contendedDocuments) List(context.Context, string) ([]string, error) { return nil, nil }

func TestDocumentContentionAnswers503WithoutCreatingContact(t *testing.T) {
	fx, _ := newOrgFixture(t)
	fake := &contendedDocuments{}
	release, err := vol.Mount(fx.srv.userStateDir(fx.a.ID), fake)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fx.srv.userMu.Lock()
	delete(fx.srv.userContacts, fx.a.ID)
	fx.srv.userMu.Unlock()
	w := fx.call(fx.a, http.MethodPost, "/api/contacts", `{"fn":"must-not-be-created"}`, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code, w.Body)
	}
	if fake.writes != 5 {
		t.Fatal("unbounded or missing CAS retry", fake.writes)
	}
}

func TestDocumentTooLargeAnswers413WithoutCreatingContact(t *testing.T) {
	fx, _ := newOrgFixture(t)
	fake := &contendedDocuments{failure: vol.ErrTooLarge}
	release, err := vol.Mount(fx.srv.userStateDir(fx.a.ID), fake)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fx.srv.userMu.Lock()
	delete(fx.srv.userContacts, fx.a.ID)
	fx.srv.userMu.Unlock()
	w := fx.call(fx.a, http.MethodPost, "/api/contacts", `{"fn":"must-not-be-created"}`, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatal(w.Code, w.Body)
	}
	if fake.writes != 1 {
		t.Fatal("retried a permanent size refusal", fake.writes)
	}
}

type unreadableGroupDocuments struct {
	contendedDocuments
	failure  error
	fallback vol.Vol
}

func (v *unreadableGroupDocuments) Read(ctx context.Context, path string) ([]byte, int64, error) {
	if strings.HasSuffix(path, "groups.json") {
		return nil, 0, v.failure
	}
	return v.fallback.Read(ctx, path)
}

func TestDocumentGroupReadErrorPreservesContactAndStatus(t *testing.T) {
	for _, failure := range []struct {
		name   string
		err    error
		status int
	}{
		{"busy", vol.ErrBusy, http.StatusServiceUnavailable},
		{"too-large", vol.ErrTooLarge, http.StatusRequestEntityTooLarge},
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, "sync"} {
			t.Run(failure.name+"/"+method, func(t *testing.T) {
				fx, _ := newOrgFixture(t)
				uid := fx.created(fx.a, "/api/contacts", `{"fn":"original"}`, "uid")
				var deviceID, secret string
				if method == "sync" {
					deviceID, secret = pairNativeDevice(t, fx.srv, fx.a.ID, "group-read-error")
				}
				fake := &unreadableGroupDocuments{failure: failure.err, fallback: vol.File{Dir: fx.srv.userStateDir(fx.a.ID)}}
				release, err := vol.Mount(fx.srv.userStateDir(fx.a.ID), fake)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				fx.srv.userMu.Lock()
				delete(fx.srv.userGroups, fx.a.ID)
				fx.srv.userMu.Unlock()
				payload := `{"fn":"changed","groupIDs":["unreadable"]}`
				var w *httptest.ResponseRecorder
				if method == "sync" {
					req := httptest.NewRequest(http.MethodPost, "/api/contacts/sync", strings.NewReader(`{"changes":[{"uid":"`+uid+`","fn":"changed","groupIDs":["unreadable"]}]}`))
					setDeviceHeaders(req, deviceID, secret)
					w = httptest.NewRecorder()
					fx.srv.routes().ServeHTTP(w, req)
				} else {
					path := "/api/contacts"
					if method == http.MethodPut {
						path += "/" + uid
					}
					w = fx.call(fx.a, method, path, payload, nil)
				}
				if w.Code != failure.status {
					t.Fatal(w.Code, w.Body)
				}
				if fake.writes != 0 {
					t.Fatal("group failure wrote a document", fake.writes)
				}
				contacts, err := fx.srv.userContactsStore(fx.a.ID)
				if err != nil {
					t.Fatal(err)
				}
				all, err := contacts.List()
				if err != nil || len(all) != 1 || all[0].FormattedName != "original" {
					t.Fatal("contact mutated", all, err)
				}
			})
		}
	}
}
