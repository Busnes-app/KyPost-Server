package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func setupTestRoots(t *testing.T) {
	t.Helper()
	for _, key := range []string{"CONFIG_DIR", "STATE_DIR", "SECRET_DIR"} {
		p := t.TempDir()
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, p)
	}
	for _, key := range []string{"PAIRING_SECRET", "PAIRING_SECRET_FILE", "KYPOST_BULK_BACKUP_REPOSITORY", "KYPOST_BACKUP_DIR", "KYPOST_BACKUP_SCRATCH_DIR"} {
		t.Setenv(key, "")
	}
	st, err := state.New(config.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
}
func setupTestBundle(t *testing.T, b setupBundle) (setupReport, error) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runApplySetup([]string{"--file", "-"}, bytes.NewReader(raw), &out)
	var r setupReport
	if err == nil {
		if e := json.Unmarshal(out.Bytes(), &r); e != nil {
			t.Fatal(e)
		}
	}
	return r, err
}
func TestApplySetupPreservesKeysChallengesAndSettings(t *testing.T) {
	setupTestRoots(t)
	b := setupBundle{Version: 1, SSO: &sso.SSOSettings{Enabled: true, IssuerURL: "https://identity.example.com", ClientID: "kypost", ClientSecret: "private-test-client-secret", AutoProvision: true}, PairingSecret: setupTestKey(t), Domains: []string{"example.com", "example.org"}}
	r, e := setupTestBundle(t, b)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Applied) != 3 || len(r.DNS) != 2 || !r.ConfigurationApplied || len(r.Pending) < 5 {
		t.Fatalf("unexpected report: %+v", r)
	}
	raw, _ := json.Marshal(r)
	if bytes.Contains(raw, []byte(b.PairingSecret)) || bytes.Contains(raw, []byte(b.SSO.ClientSecret)) {
		t.Fatal("report disclosed credentials")
	}
	paths := []string{filepath.Join(config.ConfigDir(), "sso.json"), filepath.Join(config.ConfigDir(), sso.NativeDomainsFile), config.SecretFile("PAIRING_SECRET_FILE", "pairing.key")}
	before := map[string][]byte{}
	for _, p := range paths {
		v, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		before[p] = v
	}
	r, e = setupTestBundle(t, b)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Applied) != 0 || len(r.Unchanged) != 4 {
		t.Fatalf("rerun changed setup: %+v", r)
	}
	for _, p := range paths {
		v, _ := os.ReadFile(p)
		if !bytes.Equal(v, before[p]) {
			t.Fatalf("rerun changed %s", filepath.Base(p))
		}
	}
	// Retain an established challenge too, not just the unverified case.
	domains := sso.NewNativeDomainStore(config.ConfigDir())
	domains.SetLookupForTest(func(context.Context, string) ([]string, error) { return []string{r.DNS[0].Value}, nil })
	if _, e = domains.VerifyDomain(context.Background(), "example.com"); e != nil {
		t.Fatal(e)
	}
	r, e = setupTestBundle(t, b)
	if e != nil || !r.DNS[0].Established {
		t.Fatalf("lost proof: %+v %v", r, e)
	}
}
func TestApplySetupRefusesConflictsBeforeConfigurationWrites(t *testing.T) {
	for _, kind := range []string{"sso", "key", "domain", "hold"} {
		t.Run(kind, func(t *testing.T) {
			setupTestRoots(t)
			b := setupBundle{Version: 1, SSO: &sso.SSOSettings{Enabled: true, IssuerURL: "https://identity.example.com", ClientID: "kypost"}, PairingSecret: setupTestKey(t), Domains: []string{"example.com"}}
			switch kind {
			case "sso":
				os.WriteFile(filepath.Join(config.ConfigDir(), "sso.json"), []byte(`{"enabled":false}`), 0o600)
			case "key":
				os.WriteFile(config.SecretFile("PAIRING_SECRET_FILE", "pairing.key"), []byte(strings.Repeat("cd", 32)), 0o600)
			case "domain":
				b.Domains = append(b.Domains, "bad;domain")
			case "hold":
				os.WriteFile(filepath.Join(config.StateDir(), sso.NativeRestoreHoldFile), []byte("{}"), 0o600)
			}
			if _, e := setupTestBundle(t, b); e == nil {
				t.Fatal("unsafe setup passed")
			}
			if _, e := os.Stat(filepath.Join(config.ConfigDir(), sso.NativeDomainsFile)); !os.IsNotExist(e) {
				t.Fatal("refusal created domains")
			}
			if kind != "sso" {
				if _, e := os.Stat(filepath.Join(config.ConfigDir(), "sso.json")); !os.IsNotExist(e) {
					t.Fatal("refusal created SSO")
				}
			}
		})
	}
}
func TestApplySetupRejectsUnsafeBundleInputs(t *testing.T) {
	setupTestRoots(t)
	for _, raw := range []string{`{"version":1,"unknown":"secret"}`, `{"version":1,"pairingSecret":"short"}`, `{"version":1,"sso":{"enabled":true,"issuerUrl":"http://identity.example.com","clientId":"a"}}`, `{"version":1,"sso":{"enabled":true,"issuerUrl":"http://localhost:8080","clientId":"a"}}`, `{"version":1,"domains":["example.com"]} {}`, strings.Repeat("x", setupLimit+1)} {
		if e := runApplySetup([]string{"--file", "-"}, strings.NewReader(raw), &bytes.Buffer{}); e == nil {
			t.Fatal("invalid input passed")
		}
	}
	p := filepath.Join(t.TempDir(), "bundle.json")
	if e := os.WriteFile(p, []byte(`{"version":1,"pairingSecret":"`+strings.Repeat("ab", 32)+`"}`), 0o644); e != nil {
		t.Fatal(e)
	}
	if e := runApplySetup([]string{"--file", p}, nil, &bytes.Buffer{}); e == nil {
		t.Fatal("public bundle passed")
	}
	os.Chmod(p, 0o600)
	link := p + ".link"
	os.Symlink(p, link)
	if e := runApplySetup([]string{"--file", link}, nil, &bytes.Buffer{}); e == nil {
		t.Fatal("symlink passed")
	}
}

func TestSetupStatusListsBlockersWithoutLeakingOrChangingSettings(t *testing.T) {
	setupTestRoots(t)
	b := setupBundle{Version: 1, SSO: &sso.SSOSettings{Enabled: true, IssuerURL: "https://identity.example.com", ClientID: "kypost", ClientSecret: "private-test-client-secret"}, PairingSecret: setupTestKey(t), Domains: []string{"example.com"}}
	if _, e := setupTestBundle(t, b); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(config.ConfigDir(), "sso.json")
	before, _ := os.ReadFile(path)
	var out bytes.Buffer
	if e := runSetupStatus(nil, &out); e != nil {
		t.Fatal(e)
	}
	var report setupStatus
	if e := json.Unmarshal(out.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	if report.ReadyForExternalChecks || len(report.Checks) != 13 || len(report.ExternalChecks) != 4 {
		t.Fatalf("incorrect status: %+v", report)
	}
	for _, c := range report.Checks {
		if !c.Configured && c.Message == "" {
			t.Fatalf("missing remediation for %s", c.ID)
		}
	}
	if bytes.Contains(out.Bytes(), []byte(b.SSO.ClientSecret)) || bytes.Contains(out.Bytes(), []byte(b.PairingSecret)) {
		t.Fatal("status leaked secret")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("status changed settings")
	}
}

func setupTestKey(t *testing.T) string {
	t.Helper()
	path := config.SecretFile("PAIRING_SECRET_FILE", "pairing.key")
	if _, e := cryptutil.LoadOrCreateKey(path); e != nil {
		t.Fatal(e)
	}
	return api.ReadPairingSecret(path)
}

func TestApplySetupUsesRuntimePairingAuthority(t *testing.T) {
	for _, kind := range []string{"generated", "environment", "weak-environment", "missing"} {
		t.Run(kind, func(t *testing.T) {
			setupTestRoots(t)
			key := setupTestKey(t)
			path := config.SecretFile("PAIRING_SECRET_FILE", "pairing.key")
			before, _ := os.ReadFile(path)
			switch kind {
			case "environment":
				key = strings.Repeat("runtime-override-", 4)
				t.Setenv("PAIRING_SECRET", "  "+key+"  ")
			case "weak-environment":
				t.Setenv("PAIRING_SECRET", "short")
			case "missing":
				os.Remove(path)
			}
			r, e := setupTestBundle(t, setupBundle{Version: 1, PairingSecret: key})
			if kind == "missing" || kind == "weak-environment" {
				if e == nil {
					t.Fatal("unavailable runtime authority passed")
				}
				return
			}
			if e != nil || len(r.Applied) != 0 || api.ReadPairingSecret(path) != key {
				t.Fatalf("runtime key not retained: %v", e)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("installer replaced bootstrap key")
			}
			var out bytes.Buffer
			if e = runSetupStatus(nil, &out); e != nil {
				t.Fatal(e)
			}
			var status setupStatus
			json.Unmarshal(out.Bytes(), &status)
			for _, c := range status.Checks {
				if c.ID == "webhook-key" && !c.Configured {
					t.Fatal("effective runtime key not recognized")
				}
			}
		})
	}
}

func TestSetupStatusLatestBackupFailureOverridesHistory(t *testing.T) {
	setupTestRoots(t)
	st, e := state.New(config.StateDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if e = st.RecordBackupAudit("admin.backup_run", "test", "capsule", "success", nil); e != nil {
		t.Fatal(e)
	}
	check := func(want bool) {
		t.Helper()
		var out bytes.Buffer
		if e := runSetupStatus(nil, &out); e != nil {
			t.Fatal(e)
		}
		var status setupStatus
		json.Unmarshal(out.Bytes(), &status)
		for _, c := range status.Checks {
			if c.ID == "backup-result" {
				if c.Configured != want {
					t.Fatalf("latest backup result: %+v", c)
				}
				return
			}
		}
		t.Fatal("missing backup result")
	}
	check(true)
	if e = st.RecordBackupAudit("admin.backup_intent", "test", "/api/admin/backup/run", "started", nil); e != nil {
		t.Fatal(e)
	}
	check(false)
	if e = st.RecordBackupAudit("admin.backup_run", "test", "", "failure", map[string]any{"error": "private provider detail"}); e != nil {
		t.Fatal(e)
	}
	check(false)
	// Unrelated audits cannot hide a failure and turn historical evidence green.
	for range 25 {
		if e = st.RecordBackupAudit("installer.setup", "test", "domain", "committed", nil); e != nil {
			t.Fatal(e)
		}
	}
	check(false)
}
