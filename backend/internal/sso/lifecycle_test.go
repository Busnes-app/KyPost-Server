package sso

import (
	"testing"
	"time"

	"github.com/Busness-app/ky-primitives/oidcverify"
)

func TestLifecycleStoreRefusesReplayAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	claims := oidcverify.LogoutClaims{Issuer: "https://idp.example", Subject: "alice", SessionID: "sid-1", JWTID: "jti-1", IssuedAt: now, ReplayUntil: now.Add(5 * time.Minute)}

	store := NewLifecycleStore(dir)
	if _, fresh, err := store.RecordLogout("kypost", claims, now.Add(10*time.Minute)); err != nil || !fresh {
		t.Fatalf("first delivery: fresh=%v err=%v", fresh, err)
	}

	// A restart is a new store over the same directory.
	restarted := NewLifecycleStore(dir)
	if _, fresh, err := restarted.RecordLogout("kypost", claims, now.Add(10*time.Minute)); err != nil || fresh {
		t.Fatalf("replay after restart: fresh=%v err=%v, want false", fresh, err)
	}
	// Same jti at another client is another token.
	if _, fresh, err := restarted.RecordLogout("other", claims, now.Add(10*time.Minute)); err != nil || !fresh {
		t.Fatalf("same jti, other client: fresh=%v err=%v, want true", fresh, err)
	}

	id := SessionIdentity{Issuer: "https://idp.example", ClientID: "kypost", Subject: "alice", SessionID: "sid-1", IssuedAt: now.Add(-time.Minute)}
	if out, err := restarted.LoggedOut(id); err != nil || !out {
		t.Fatalf("fence after restart: %v %v", out, err)
	}
	id.SessionID = "sid-2"
	if out, _ := restarted.LoggedOut(id); out {
		t.Fatal("a sid-bound logout fenced a different session")
	}
}

func TestLifecycleStorePrunesExpiredRecords(t *testing.T) {
	store := NewLifecycleStore(t.TempDir())
	now := time.Now()
	old := oidcverify.LogoutClaims{Issuer: "https://idp.example", Subject: "alice", JWTID: "old", IssuedAt: now.Add(-time.Hour)}
	if _, _, err := store.RecordLogout("kypost", old, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	fresh := oidcverify.LogoutClaims{Issuer: "https://idp.example", Subject: "bob", JWTID: "new", IssuedAt: now}
	if _, _, err := store.RecordLogout("kypost", fresh, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f, err := store.load()
	if err != nil || len(f.Logouts) != 1 {
		t.Fatalf("records after prune = %d (%v), want 1", len(f.Logouts), err)
	}
	if out, _ := store.LoggedOut(SessionIdentity{Issuer: "https://idp.example", ClientID: "kypost", Subject: "alice", IssuedAt: now.Add(-2 * time.Hour)}); out {
		t.Fatal("an expired record still fences")
	}
}

func TestLogoutEventCovers(t *testing.T) {
	base := SessionIdentity{Issuer: "i", ClientID: "c", Subject: "alice", SessionID: "s1", IssuedAt: time.Unix(100, 0)}
	cases := []struct {
		name string
		ev   LogoutEvent
		want bool
	}{
		{"sid match", LogoutEvent{Issuer: "i", ClientID: "c", Subject: "alice", SessionID: "s1", IssuedAt: 50}, true},
		{"sid match, subject mismatch", LogoutEvent{Issuer: "i", ClientID: "c", Subject: "bob", SessionID: "s1", IssuedAt: 200}, false},
		{"sid only, no subject on token", LogoutEvent{Issuer: "i", ClientID: "c", SessionID: "s1", IssuedAt: 50}, true},
		{"other sid", LogoutEvent{Issuer: "i", ClientID: "c", Subject: "alice", SessionID: "s2", IssuedAt: 200}, false},
		{"subject-wide, earlier session", LogoutEvent{Issuer: "i", ClientID: "c", Subject: "alice", IssuedAt: 100}, true},
		{"subject-wide, later session", LogoutEvent{Issuer: "i", ClientID: "c", Subject: "alice", IssuedAt: 99}, false},
		{"other client", LogoutEvent{Issuer: "i", ClientID: "x", Subject: "alice", SessionID: "s1", IssuedAt: 200}, false},
		{"empty token", LogoutEvent{Issuer: "i", ClientID: "c"}, false},
	}
	for _, tc := range cases {
		if got := tc.ev.Covers(base); got != tc.want {
			t.Errorf("%s: Covers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
