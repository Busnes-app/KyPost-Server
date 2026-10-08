//go:build linux

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/backup"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const nativeReleasePath = "/api/admin/native-recovery/release"

// qualifyReleaseFixture runs the offline restore's last stages in place, before
// the challenge: a legacy SSO-linked administrator the evidence deletes, a
// rotated reference generation, the qualification marker and the token fence.
func qualifyReleaseFixture(t *testing.T, s *Server, native users.User) {
	t.Helper()
	ctx := context.Background()
	legacy, err := s.users.Create(ctx, "legacy-admin", "legacy-admin-password-1", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.users.ClearMustChangePassword(legacy.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.users.LinkSSO(legacy.ID, "legacy-sub", "legacy", ""); err != nil {
		t.Fatal(err)
	}
	// KyIdentity demoted this one after the backup; it stays active.
	demoted, err := s.users.Create(ctx, "demoted-admin", "demoted-admin-password-1", users.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.users.ClearMustChangePassword(demoted.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.users.LinkSSO(demoted.ID, "demoted-sub", "demoted", ""); err != nil {
		t.Fatal(err)
	}
	if err = mailbox.RotateRestoredMessageReferences(filepath.Join(s.userStateDir(native.ID), "mailbox", "mailbox.db"), native.NativeMailboxSource); err != nil {
		t.Fatal(err)
	}
	if err = s.ssoLifecycle.RecordNativeRestoreQualification(s.stateDir, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	all, err := s.users.List()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ssoLifecycle.FenceRestoredNativeTokens(s.stateDir, all); err != nil {
		t.Fatal(err)
	}
}

func nativeReleaseFixture(t *testing.T) nativeRepairFixture {
	t.Helper()
	f := nativeRepairHTTPFixture(t, "release", false)
	if w := gatedCall(t, f.s, f.cookie, "POST", nativeRepairPath, nativeRepairBody, ""); w.Code != 200 {
		t.Fatal("repair", w.Code, w.Body.String())
	}
	f.s.backup, _ = backup.New(backup.Dirs{Config: f.s.configDir, State: f.s.stateDir, Secret: f.s.configDir}, config.BackupConfig{Keep: 1}, f.s.globalStore, "test")
	t.Setenv("KYPOST_NATIVE_RESTORE_RELEASE", "true")
	t.Setenv("KYPOST_NATIVE_RECEIVER", "")
	return f
}

func releaseHeld(t *testing.T, s *Server) bool {
	t.Helper()
	return errors.Is(sso.RequireNativeRestoreReleased(s.stateDir), sso.ErrNativeRestoreHold)
}

func TestNativeRestoreReleaseHTTPFlag(t *testing.T) {
	f := nativeReleaseFixture(t)
	for value, code := range map[string]int{"": 404, "false": 404, "maybe": 409} {
		t.Setenv("KYPOST_NATIVE_RESTORE_RELEASE", value)
		if w := gatedCall(t, f.s, f.cookie, "POST", nativeReleasePath, nativeRepairBody, ""); w.Code != code || !releaseHeld(t, f.s) {
			t.Fatalf("flag %q: %d %s", value, w.Code, w.Body.String())
		}
	}
}

func TestNativeRestoreReleaseHTTPRefusesOperators(t *testing.T) {
	for name, code := range map[string]int{"bad-password": 401, "kysignon-session": 403, "generic-sso-session": 403, "expired-session": 401, "linked-admin": 403, "native-account": 401} {
		t.Run(name, func(t *testing.T) {
			f := nativeReleaseFixture(t)
			cookie, body := f.cookie, nativeRepairBody
			switch name {
			case "bad-password":
				body = `{"password":"wrong-password-for-release"}`
			case "kysignon-session":
				nativeRepairSSOSession(f)
			case "generic-sso-session":
				f.s.sessMu.Lock()
				sess := f.s.sessions[f.cookie.Value]
				sess.SSO, sess.SSOAppAdmin = sso.SessionIdentity{Issuer: "https://generic.example", ClientID: "kypost", Subject: "operator"}, true
				f.s.sessions[f.cookie.Value] = sess
				f.s.sessMu.Unlock()
			case "expired-session":
				f.s.sessMu.Lock()
				sess := f.s.sessions[f.cookie.Value]
				sess.ExpiresAt = time.Now().Add(-time.Minute)
				f.s.sessions[f.cookie.Value] = sess
				f.s.sessMu.Unlock()
			case "linked-admin":
				f.s.sessMu.RLock()
				operator := f.s.sessions[f.cookie.Value].UserID
				f.s.sessMu.RUnlock()
				if err := f.s.users.LinkSSO(operator, "operator", "operator", ""); err != nil {
					t.Fatal(err)
				}
			case "native-account":
				cookie = f.nativeCookie
			}
			if w := gatedCall(t, f.s, cookie, "POST", nativeReleasePath, body, ""); w.Code != code || code == 403 && !strings.Contains(w.Body.String(), "P6:") || !releaseHeld(t, f.s) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeRestoreReleaseHTTPPreconditions(t *testing.T) {
	for name, reason := range map[string]string{
		"P8-domain-proof": "P8: no configured mail domain has a fresh DNS proof",
		"maddy-flag":      "the bundled receiver (Maddy) has no fence",
		"maddy-config":    "the bundled receiver (Maddy) has no fence",
		"P2-drift":        "P2: authority changed since the repair",
	} {
		t.Run(name, func(t *testing.T) {
			f := nativeReleaseFixture(t)
			switch name {
			case "P8-domain-proof":
				f.s.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) { return nil, nil })
			case "maddy-flag":
				t.Setenv("KYPOST_NATIVE_RECEIVER", "true")
			case "maddy-config":
				if err := os.WriteFile(filepath.Join(f.s.configDir, "receiving.conf"), []byte("# rendered\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "P2-drift":
				if _, err := f.s.users.Reactivate(f.native.ID); err != nil {
					t.Fatal(err)
				}
			}
			lifecycle, _ := os.ReadFile(filepath.Join(f.s.configDir, "sso-lifecycle.json"))
			w := gatedCall(t, f.s, f.cookie, "POST", nativeReleasePath, nativeRepairBody, "")
			if w.Code != 409 || !strings.Contains(w.Body.String(), reason) || !releaseHeld(t, f.s) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if after, _ := os.ReadFile(filepath.Join(f.s.configDir, "sso-lifecycle.json")); !bytes.Equal(lifecycle, after) {
				t.Fatal("refusal wrote the lifecycle")
			}
			if legacy, err := f.s.users.GetByUsername("legacy-admin"); err != nil || !legacy.Active {
				t.Fatal("refusal deactivated an account", err)
			}
		})
	}
}

func TestNativeRestoreReleaseHTTPReleases(t *testing.T) {
	f := nativeReleaseFixture(t)
	t.Setenv("KYPOST_NATIVE_RECEIVER", "true")
	legacy, err := f.s.users.GetByUsername("legacy-admin")
	if err != nil {
		t.Fatal(err)
	}
	login := func(name, password string) *http.Cookie {
		w := doJSON(f.s, f.s.handleLogin, "POST", "/api/auth/login", map[string]string{"username": name, "password": password})
		c := sessionCookieFrom(w)
		if c == nil {
			t.Fatal("login", name, w.Code)
		}
		return c
	}
	legacyCookie := login("legacy-admin", "legacy-admin-password-1")
	demotedCookie := login("demoted-admin", "demoted-admin-password-1")
	demoted, err := f.s.users.GetByUsername("demoted-admin")
	if err != nil {
		t.Fatal(err)
	}
	w := gatedCall(t, f.s, f.cookie, "POST", nativeReleasePath, `{"password":"operator-test-password","confirm":"original-host-decommissioned"}`, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"released":true`) || !strings.Contains(w.Body.String(), `"alreadyReleased":false`) || releaseHeld(t, f.s) {
		t.Fatal(w.Code, w.Body.String())
	}
	if record, released, err := f.s.ssoLifecycle.NativeRestoreReleased(f.s.stateDir); err != nil || !released || record.CompletedAt == nil {
		t.Fatal("completion not recorded", err)
	}
	// (a) the deleted SSO-linked administrator is deactivated and signed out.
	if u, _ := f.s.users.Get(legacy.ID); u.Active {
		t.Fatal("deleted legacy administrator still active")
	}
	// (a) roles: the KyIdentity-demoted administrator stays active as a user.
	if u, _ := f.s.users.Get(demoted.ID); !u.Active || u.Role != users.RoleUser {
		t.Fatal("demotion not applied", u.Active, u.Role)
	}
	f.s.sessMu.RLock()
	_, live := f.s.sessions[legacyCookie.Value]
	_, demotedLive := f.s.sessions[demotedCookie.Value]
	f.s.sessMu.RUnlock()
	if live || demotedLive {
		t.Fatal("changed account session survived")
	}
	if u, _ := f.s.users.Get(f.native.ID); u.Active {
		t.Fatal("release activated a repaired account")
	}
	if w = gatedCall(t, f.s, f.cookie, "GET", "/api/admin/native-recovery/status", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"released":true`) || !strings.Contains(w.Body.String(), `"held":false`) {
		t.Fatal("status", w.Code, w.Body.String())
	}
	audit, err := f.s.globalStore.RecentBackupAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, a := range audit {
		if a.Action == "admin.native_restore_release" {
			rows[a.Outcome] = a.Details
		}
	}
	for _, outcome := range []string{"started", "completed"} {
		if !strings.Contains(rows[outcome], legacy.ID) || !strings.Contains(rows[outcome], demoted.ID) {
			t.Fatal("audit does not name the changed accounts", outcome, rows)
		}
	}
	w = gatedCall(t, f.s, f.cookie, "POST", nativeReleasePath, `{"password":"operator-test-password","confirm":"original-host-decommissioned"}`, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"alreadyReleased":true`) {
		t.Fatal("retry", w.Code, w.Body.String())
	}
}

func TestNativeRestoreReleaseHTTPCrashPoints(t *testing.T) {
	for _, point := range []string{"intent", "ledger", "lifecycle", "rename"} {
		t.Run(point, func(t *testing.T) {
			f := nativeReleaseFixture(t)
			f.s.nativeReleaseHit = func(p string) error {
				if p == point {
					return errors.New("crash at " + p)
				}
				return nil
			}
			w := gatedCall(t, f.s, f.cookie, "POST", nativeReleasePath, nativeRepairBody, "")
			if point == "rename" {
				// Released, durability unconfirmed: no "failed" audit, retry confirms.
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"confirmed":false`) || releaseHeld(t, f.s) {
					t.Fatal(w.Code, w.Body.String())
				}
			} else if w.Code != 409 {
				t.Fatal(w.Code, w.Body.String())
			}
			f.s.nativeReleaseHit = nil
			w = gatedCall(t, f.s, f.cookie, "POST", nativeReleasePath, nativeRepairBody, "")
			if point == "rename" {
				audit, _ := f.s.globalStore.RecentBackupAudit(10)
				outcomes := ""
				for _, a := range audit {
					if a.Action == "admin.native_restore_release" {
						outcomes += a.Outcome + ","
						if a.Outcome == "completed" && (!strings.Contains(a.Details, "releasedAt") || !strings.Contains(a.Details, "confirmedBy")) {
							t.Fatal("completion row lacks the original release", a.Details)
						}
					}
				}
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"alreadyReleased":true`) || strings.Contains(outcomes, "failed") || !strings.Contains(outcomes, "completed") {
					t.Fatal(w.Code, w.Body.String(), outcomes)
				}
				return
			}
			if w.Code != 409 || !strings.Contains(w.Body.String(), "interrupted") || !releaseHeld(t, f.s) {
				t.Fatal("retry without fresh evidence", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeRestoreReleaseHTTPWithoutHold(t *testing.T) {
	f := nativeReleaseFixture(t)
	if err := os.Remove(filepath.Join(f.s.stateDir, sso.NativeRestoreHoldFile)); err != nil {
		t.Fatal(err)
	}
	if w := gatedCall(t, f.s, f.cookie, "POST", nativeReleasePath, nativeRepairBody, ""); w.Code != 409 || !strings.Contains(w.Body.String(), "no native restore hold") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestMaddyProfileFailsClosed(t *testing.T) {
	t.Setenv("KYPOST_NATIVE_RECEIVER", "")
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if maddy, err := maddyProfile(notDir); err == nil || !maddy {
		t.Fatal("unreadable receiver state did not fail closed", maddy, err)
	}
	if maddy, err := maddyProfile(t.TempDir()); err != nil || maddy {
		t.Fatal("absent receiver detected", maddy, err)
	}
}
