//go:build linux

package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type hostedDomainsTransport func(*http.Request) (*http.Response, error)

func (f hostedDomainsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func hostedDomainsFixture(t *testing.T, config string) {
	t.Helper()
	if err := NewStore(config).Save(SSOSettings{Enabled: true, IssuerURL: nativeIssuer, ClientID: "kypost", ClientSecret: "suite-secret"}); err != nil {
		t.Fatal(err)
	}
	prior := SetTransport(hostedDomainsTransport(func(r *http.Request) (*http.Response, error) {
		id, secret, ok := r.BasicAuth()
		if r.URL.String() != nativeIssuer+"/api/suite/email-domains" || !ok || id != "kypost" || secret != "suite-secret" {
			t.Fatalf("unexpected suite request: %s", r.URL.Redacted())
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"domains":["example.test"]}`)), Header: make(http.Header)}, nil
	}))
	t.Cleanup(func() { SetTransport(prior) })
}

func hostedDesired(t *testing.T, life *LifecycleStore, sub, signIn, address string, revision int) {
	t.Helper()
	body := map[string]any{"schemas": []string{scimUserSchema}, "id": sub, "externalId": sub, "userName": sub, "active": true, "roles": []string{}, "emails": []map[string]any{{"value": signIn, "type": "work", "primary": true}}, "meta": map[string]string{"version": fmt.Sprintf(`W/"%d"`, revision)}}
	if address != "" {
		body["emails"] = append(body["emails"].([]map[string]any), map[string]any{"value": address, "type": "mailbox", "primary": false})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var resource DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: sub + fmt.Sprintf(`W/"%d"`, revision), Type: "user.updated", At: time.Now()}
	if _, err := life.ApplyDirectoryUser(nativeIssuer, ev, resource, EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestHostedDirectoryMailboxAllocation(t *testing.T) {
	for _, tc := range []struct{ name, address, wantStatus, wantFailure string }{
		{"suite-domain", "member@example.test", "applied", ""},
		{"sign-in-only", "", "pending", "mailbox_address_missing"},
		{"non-suite-domain", "member@other.test", "failed", "primary_domain_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, root := t.TempDir(), t.TempDir()
			life := NewLifecycleStore(config)
			domains := provenNativeDomain(t, config)
			// A DNS-proven domain outside the suite list is still refused.
			if _, err := domains.ConfigureDomain(context.Background(), "other.test", nativeIssuer); err != nil {
				t.Fatal(err)
			}
			answerEveryDomain(domains, map[string]bool{})
			hostedDomainsFixture(t, config)
			accounts, err := users.LoadOrMigrate(context.Background(), config, filepath.Join(config, "admin.env"))
			if err != nil {
				t.Fatal(err)
			}
			hostedDesired(t, life, "member", "signin@example.test", tc.address, 1)
			u, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "member", domains, accounts, nativeLimits, heldOrg)
			a, found, readErr := life.NativeAssignment(nativeIssuer, "member")
			if readErr != nil || !found || a.Status != tc.wantStatus || a.Failure != tc.wantFailure {
				t.Fatalf("assignment %+v, %v", a, readErr)
			}
			if tc.wantStatus != "applied" {
				if !errors.Is(err, ErrNativeProvisioning) || u.ID != "" || a.Address != "" || a.Source != "" {
					t.Fatalf("unexpected mailbox: %+v %+v %v", u, a, err)
				}
				if _, err := accounts.GetBySSOSub("member"); !errors.Is(err, users.ErrNotFound) {
					t.Fatal("published without a mailbox", err)
				}
				// A newer directory profile can finish the pending allocation.
				hostedDesired(t, life, "member", "signin@example.test", "member@example.test", 2)
				if _, err := life.AllocateNativeAccount(context.Background(), root, nativeIssuer, "member", domains, accounts, nativeLimits, heldOrg); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil || a.Address != tc.address || u.OrgID != heldOrg || u.NativeMailboxSource == "" {
				t.Fatal(u, a, err)
			}
			d, _, err := life.Directory(nativeIssuer, "member")
			if err != nil || d.Resource.Emails[1].Type != "mailbox" || d.Resource.Emails[0].Type != "work" {
				t.Fatal("email types lost", err)
			}
			if _, err := life.AdmitNativeMailbox(context.Background(), root, nativeIssuer, u.ID, u.ID, accounts); err != nil {
				t.Fatal("hosted admission", err)
			}
			if _, err := mailbox.ValidatePreparedAccount(root, a.Owner, a.Address, nativeLimits); err != nil {
				t.Fatal(err)
			}
		})
	}
}
