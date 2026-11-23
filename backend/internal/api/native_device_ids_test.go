//go:build linux

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// Exercise the register boundary with a fresh owner-bound token every time.
func registerDeviceID(t *testing.T, s *Server, owner users.User, id, secret, token string, direct bool) (string, string, *httptest.ResponseRecorder) {
	t.Helper()
	store, err := s.userStore(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := store.GetOrCreateSubscriberID()
	if err != nil {
		t.Fatal(err)
	}
	pairing, _, err := s.mintPairingToken(subscriber, pairingPurposeNativeDevice, time.Minute, direct)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"subscriberId": subscriber, "pairingToken": pairing, "deviceId": id, "deviceToken": token, "platform": "android"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/notifications/native/register", bytes.NewReader(body))
	r.Header.Set(headerDeviceSecret, secret)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	var result struct {
		ID     string `json:"deviceId"`
		Secret string `json:"deviceSecret"`
	}
	if w.Code == http.StatusOK || w.Code == http.StatusAccepted {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.ID == "" || result.Secret == "" {
			t.Fatalf("missing credential: %s", w.Body)
		}
	}
	return result.ID, result.Secret, w
}

func TestNativeDeviceIDsAcrossReplicas(t *testing.T) {
	dbbackend.RequirePostgres(t)
	a, _, db := multiSSOServer(t)
	ownerA := provisionMember(t, a, db, "device-owner-a", orgA)
	ownerB := provisionMember(t, a, db, "device-owner-b", orgB)
	b := replicaOf(t, a)
	b.useTenancy(db, staffOrg)
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)
	now := time.Now()
	a.deviceRescan.now = func() time.Time { return now }
	if _, found := a.lookupUserByDevice("prime-discovery"); found {
		t.Fatal("unexpected ID")
	}
	idB, secretB, w := registerDeviceID(t, b, ownerB, "client-device", "", "push-b", true)
	if w.Code != http.StatusOK {
		t.Fatalf("B: %d %s", w.Code, w.Body)
	}
	if _, found := a.deviceIndex[idB]; found {
		t.Fatal("A already discovered B")
	}
	idA, _, w := registerDeviceID(t, a, ownerA, idB, "", "push-a", true)
	if w.Code != http.StatusOK {
		t.Fatalf("A: %d %s", w.Code, w.Body)
	}
	if idA == idB || idB == "client-device" {
		t.Fatalf("IDs were not freshly minted: A=%q B=%q", idA, idB)
	}
	// Allow the documented discovery interval to elapse without sleeping. The
	// registration above ran entirely while A's discovery was still cooling down.
	now = now.Add(deviceRescanInterval + time.Second)
	for _, s := range []*Server{a, b} {
		owner, _, ok, wait := s.deviceAuthFromRequest(deviceRequest(idB, secretB))
		if !ok || owner != ownerB.ID || wait != 0 {
			t.Fatalf("B credential: owner=%q ok=%v wait=%v", owner, ok, wait)
		}
	}
}

func TestNativeDeviceRegistrationIDs(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			s, _, db := multiSSOServer(t)
			a := provisionMember(t, s, db, "device-a", orgA)
			b := provisionMember(t, s, db, "device-b", orgB)
			foreign, foreignSecret := pairNativeDevice(t, s, b.ID, "foreign-device")
			own, ownSecret := pairNativeDevice(t, s, a.ID, "owned-device")
			seen := map[string]bool{foreign: true, own: true}
			for i, requested := range []string{foreign, "missing-device", own, ""} {
				id, _, w := registerDeviceID(t, s, a, requested, "wrong-secret", fmt.Sprintf("new-push-%d", i), direct)
				want := http.StatusOK
				if !direct {
					want = http.StatusAccepted
				}
				if w.Code != want || id == requested || seen[id] {
					t.Fatalf("new registration: status=%d id=%q body=%s", w.Code, id, w.Body)
				}
				seen[id] = true
			}
			id, _, w := registerDeviceID(t, s, a, own, ownSecret, "push-owned", direct)
			want := http.StatusOK
			if !direct {
				want = http.StatusAccepted
			}
			if w.Code != want || id != own {
				t.Fatalf("verified re-pair: %d id=%q body=%s", w.Code, id, w.Body)
			}
			if _, _, ok, _ := s.deviceAuthFromRequest(deviceRequest(foreign, foreignSecret)); !ok {
				t.Fatal("foreign device changed")
			}
		})
	}
}

func TestNativeDeviceNewIDDoesNotMergeByPushToken(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			s, _, db := multiSSOServer(t)
			owner := provisionMember(t, s, db, "token-owner", orgA)
			oldID, oldSecret := pairNativeDevice(t, s, owner.ID, "old-device")
			id, _, w := registerDeviceID(t, s, owner, "new-client-id", "", "tok-"+oldID, direct)
			if !direct && w.Code == http.StatusAccepted {
				r := httptest.NewRequest(http.MethodPost, "/api/notifications/native/pending/"+id+"/approve", nil)
				r.SetPathValue("deviceId", id)
				authRequestAs(s, r, owner.ID)
				w = httptest.NewRecorder()
				s.routes().ServeHTTP(w, r)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("registration/approval: %d %s", w.Code, w.Body)
			}
			store, err := s.userStore(owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, found := store.GetNativeDevice(id); !found || id == oldID {
				t.Fatal("new server ID was replaced")
			}
			if _, _, ok, _ := s.deviceAuthFromRequest(deviceRequest(oldID, oldSecret)); !ok {
				t.Fatal("existing credential changed")
			}
			if direct {
				retained, _, w := registerDeviceID(t, s, owner, oldID, oldSecret, "tok-"+oldID, true)
				if w.Code != http.StatusOK || retained != oldID {
					t.Fatalf("verified ID with shared push token: %d id=%q body=%s", w.Code, retained, w.Body)
				}
			}

		})
	}
}

func TestDeviceAuthFailuresAreUniform(t *testing.T) {
	for _, path := range []string{"/api/notifications/native/pull", "/api/inbox", "/api/contacts/sync"} {
		t.Run(path, func(t *testing.T) {
			s := newTestServer(t)
			id, _ := pairNativeDevice(t, s, s.mustBootstrapUserID(t), "uniform-device")
			now := time.Now()
			s.deviceLockout.now = func() time.Time { return now }
			for i := 0; i < deviceMaxFailures+1; i++ {
				var prior *httptest.ResponseRecorder
				for j, candidate := range []string{id, "missing-device"} {
					r := httptest.NewRequest(http.MethodGet, path, nil)
					r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", j+1)
					setDeviceHeaders(r, candidate, "wrong-secret")
					w := httptest.NewRecorder()
					s.routes().ServeHTTP(w, r)
					want := http.StatusUnauthorized
					if i == deviceMaxFailures {
						want = http.StatusTooManyRequests
					}
					if w.Code != want {
						t.Fatalf("attempt %d: %d %s", i+1, w.Code, w.Body)
					}
					if prior != nil && (w.Code != prior.Code || w.Body.String() != prior.Body.String() || !reflect.DeepEqual(w.Header(), prior.Header())) {
						t.Fatalf("attempt %d responses differ: existing=%v %s missing=%v %s", i+1, prior.Header(), prior.Body, w.Header(), w.Body)
					}
					prior = w
				}
			}
		})
	}
}
