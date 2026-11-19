package replica

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// TestEveryEnvironmentVariableIsClassified fails on a variable the operator docs, the
// example .env or the server's own reads name that is neither Shared nor PerHost, and on a
// classified name nothing names any more.
func TestEveryEnvironmentVariableIsClassified(t *testing.T) {
	names := map[string]string{}
	add := func(src, text string, re *regexp.Regexp) {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			names[m[1]] = src
		}
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	add(".env.example", read("../../../.env.example"), regexp.MustCompile(`(?m)^#? ?([A-Z][A-Z0-9_]{2,})=`))
	add("docs/CONFIGURATION.md", read("../../../docs/CONFIGURATION.md"), regexp.MustCompile("`([A-Z][A-Z0-9_]{3,})`"))
	code := regexp.MustCompile(`(?:Getenv|LookupEnv|SecretFile|envBool|envInt|envDuration)\("([A-Z][A-Z0-9_]+)"`)
	consts := regexp.MustCompile(`"((?:KYPOST|POW|ALLOW|TRUSTED|RATE|CLASSIFY|SMTP|UNHEALTHY|MFA|LOG|SECRET|STATE|CONFIG|WEB)_[A-Z0-9_]+)"`)
	_ = filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.Contains(p, "third_party") {
			return err
		}
		text := read(p)
		add(p, text, code)
		if strings.Contains(p, "/config/") {
			add(p, text, consts)
		}
		return nil
	})
	perHost := map[string]bool{}
	for _, n := range PerHost {
		if perHost[n] {
			t.Errorf("%s is listed twice as per host", n)
		}
		perHost[n] = true
		if _, ok := Shared[n]; ok {
			t.Errorf("%s is both shared and per host", n)
		}
	}
	// Prose words the docs put in backticks, not settings.
	notSettings := map[string]bool{"TRUE": true}
	for n, src := range names {
		if _, shared := Shared[n]; !shared && !perHost[n] && !notSettings[n] {
			t.Errorf("%s (named in %s) is not classified as Shared or PerHost", n, src)
		}
	}
	for n := range Shared {
		if _, ok := names[n]; !ok {
			t.Errorf("shared %s is named nowhere", n)
		}
	}
}

func fingerprintKey(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "totp-secret.key")
	if _, err := cryptutil.LoadOrCreateKey(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// Each shared setting that differs is named, alone and without its value; per-host settings
// may differ; canonical forms agree.
func TestEverySharedFieldIsCompared(t *testing.T) {
	key := fingerprintKey(t)
	base := map[string]string{}
	stored, err := Fingerprint(key, env(base))
	if err != nil {
		t.Fatal(err)
	}
	for name := range Shared {
		changed := map[string]string{name: "a-different-value-7f3a"}
		cur, _ := Fingerprint(key, env(changed))
		if got := Diff(stored, cur); !slices.Equal(got, []string{name}) {
			t.Errorf("changing %s: diff %v", name, got)
		}
		if err := mismatch(Diff(stored, cur)); strings.Contains(err.Error(), "7f3a") {
			t.Errorf("the refusal for %s carries its value: %v", name, err)
		}
	}
	for _, name := range PerHost {
		cur, _ := Fingerprint(key, env(map[string]string{name: "per-host-value"}))
		if got := Diff(stored, cur); len(got) != 0 {
			t.Errorf("per-host %s differs: %v", name, got)
		}
	}
	a, _ := Fingerprint(key, env(map[string]string{"RATE_LIMIT_EXEMPT_CIDRS": "10.0.0.1/8, 192.168.1.0/24,10.0.0.0/8", "TRUSTED_PROXY_CIDRS": "::1"}))
	b, _ := Fingerprint(key, env(map[string]string{"RATE_LIMIT_EXEMPT_CIDRS": "192.168.1.0/24 10.0.0.0/8", "TRUSTED_PROXY_CIDRS": "::1/128"}))
	if got := Diff(a, b); len(got) != 0 {
		t.Errorf("canonical networks differ: %v", got)
	}
	other := fingerprintKey(t)
	cur, _ := Fingerprint(other, env(map[string]string{"SERVER_BASE_URL": "x"}))
	if got := Diff(stored, cur); !slices.Equal(got, []string{KeyField}) {
		t.Errorf("a different key: diff %v, want the key alone", got)
	}
	// A setting an upgrade added is no difference.
	delete(cur, "SERVER_BASE_URL")
	same, _ := Fingerprint(key, env(base))
	delete(same, "SERVER_BASE_URL")
	if got := Diff(same, stored); len(got) != 0 {
		t.Errorf("a field missing from the record differs: %v", got)
	}
}

func globalStore(t *testing.T, dir string) *state.Store {
	t.Helper()
	s, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestReplicaConfigRecordsAndRefuses(t *testing.T) {
	dbbackend.RequirePostgres(t)
	ctx := context.Background()
	key := fingerprintKey(t)
	dir := t.TempDir()
	st := globalStore(t, dir)
	t.Setenv("SERVER_BASE_URL", "https://mail.example.org")
	t.Setenv("WEB_PORT", "5866")
	if err := CheckConfig(ctx, st, key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEB_PORT", "6000")
	if err := CheckConfig(ctx, globalStore(t, dir), key); err != nil {
		t.Fatalf("a per-host difference refused: %v", err)
	}
	t.Setenv("SERVER_BASE_URL", "https://other.example.org")
	err := CheckConfig(ctx, globalStore(t, dir), key)
	if !errors.Is(err, ErrConfigMismatch) || !strings.Contains(err.Error(), "SERVER_BASE_URL") || strings.Contains(err.Error(), "other.example") {
		t.Fatalf("shared difference: %v", err)
	}
	if err := CheckConfig(ctx, st, fingerprintKey(t)); err == nil || !strings.Contains(err.Error(), KeyField) || strings.Contains(err.Error(), "SERVER_BASE_URL") {
		t.Fatalf("different key: %v", err)
	}
	changed, err := Accept(ctx, st, key)
	if err != nil || !slices.Equal(changed, []string{"SERVER_BASE_URL"}) {
		t.Fatalf("Accept = %v, %v", changed, err)
	}
	if err := CheckConfig(ctx, st, key); err != nil {
		t.Fatalf("after accept: %v", err)
	}
	if _, err := Accept(ctx, st, fingerprintKey(t)); err == nil {
		t.Fatal("Accept took a different key")
	}
}

// Two first starts with different settings: one records, the other refuses.
func TestFirstStartsRaceThroughTheRecord(t *testing.T) {
	dbbackend.RequirePostgres(t)
	ctx := context.Background()
	key := fingerprintKey(t)
	dir := t.TempDir()
	a, b := globalStore(t, dir), globalStore(t, dir)
	var errs [2]error
	var wg sync.WaitGroup
	for i, st := range []*state.Store{a, b} {
		wg.Go(func() {
			cur, err := Fingerprint(key, env(map[string]string{"SERVER_BASE_URL": []string{"https://a", "https://b"}[i]}))
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = st.Serialized(ctx, "replica-config", func(context.Context) error {
				raw, err := st.Setting(FingerprintSetting)
				if errors.Is(err, state.ErrNotFound) {
					return st.SetSetting(FingerprintSetting, encode(cur))
				}
				if d := Diff(decode(t, raw), cur); len(d) > 0 {
					return mismatch(d)
				}
				return err
			})
		})
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want exactly one refusal: %v / %v", errs[0], errs[1])
	}
}

func encode(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func decode(t *testing.T, raw string) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Error(err)
	}
	return out
}

func roots(t *testing.T) []string {
	return []string{t.TempDir(), t.TempDir(), t.TempDir()}
}

func TestTopologyGuard(t *testing.T) {
	dbbackend.RequirePostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	st := globalStore(t, dir)
	set := roots(t)
	noEnv := env(nil)
	release, err := Start(ctx, Process{Store: st, Role: "api", Roots: set, BootID: "boot-1", Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range set {
		if _, err := os.Stat(filepath.Join(root, VolumeIDFile)); err != nil {
			t.Fatalf("first start wrote no ID into %s: %v", root, err)
		}
	}
	second, err := Start(ctx, Process{Store: globalStore(t, dir), Role: "daemon", Roots: set, BootID: "boot-1", Getenv: noEnv})
	if err != nil {
		t.Fatalf("a second instance on one host and volume set: %v", err)
	}
	for i := range set {
		missing := slices.Clone(set)
		missing[i] = t.TempDir()
		if _, err := Start(ctx, Process{Store: st, Role: "api", Roots: missing, BootID: "boot-1", Getenv: noEnv}); !errors.Is(err, ErrVolumes) {
			t.Fatalf("root %d missing its ID: %v", i, err)
		}
		other := slices.Clone(set)
		other[i] = t.TempDir()
		if err := os.WriteFile(filepath.Join(other[i], VolumeIDFile), []byte("another-deployment\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Start(ctx, Process{Store: st, Role: "api", Roots: other, BootID: "boot-1", Getenv: noEnv}); !errors.Is(err, ErrVolumes) {
			t.Fatalf("root %d from another deployment: %v", i, err)
		}
	}
	if _, err := Start(ctx, Process{Store: st, Role: "api", Roots: set, BootID: "boot-2", Getenv: noEnv}); !errors.Is(err, ErrHost) {
		t.Fatalf("another kernel beside a live instance: %v", err)
	}
	if _, err := Start(ctx, Process{Store: st, Role: "api", Roots: set, BootID: "boot-1", Getenv: env(map[string]string{"KYPOST_NATIVE_RECEIVER": "true"})}); !errors.Is(err, ErrReceiver) {
		t.Fatalf("receiver on PostgreSQL: %v", err)
	}
	release()
	second()
	// The dead instances no longer pin the host.
	third, err := Start(ctx, Process{Store: st, Role: "api", Roots: set, BootID: "boot-2", Getenv: noEnv})
	if err != nil {
		t.Fatalf("after every instance stopped, another boot ID: %v", err)
	}
	third()
}
