package backup

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func fixtureDirs(t *testing.T) Dirs {
	t.Helper()
	d := Dirs{Config: t.TempDir(), State: t.TempDir(), Secret: t.TempDir()}
	must := func(p string, b []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(d.Config, "config.yaml"), []byte("x: 1\n"))
	must(filepath.Join(d.Config, "users.json"), []byte(`{"users":[]}`))
	must(filepath.Join(d.Config, "users", "u1", "imap-config.json"), []byte("{}"))
	must(filepath.Join(d.Secret, "imap-config.key"), []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))))
	must(filepath.Join(d.Secret, "totp-secret.key"), []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))))
	must(filepath.Join(d.Secret, "pairing.key"), []byte("k"))
	must(filepath.Join(d.State, "users", "u1", "contacts.json"), []byte("[]"))
	must(filepath.Join(d.State, "users", "u1", "mailcache.json"), []byte(`{"big":"cache"}`))
	must(filepath.Join(d.State, "users", "u1", "state.json.migrated"), []byte("old"))
	for _, dir := range []string{d.State, filepath.Join(d.State, "users", "u1")} {
		st, err := state.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	return d
}

func openService(t *testing.T, d Dirs, bc config.BackupConfig) *Service {
	t.Helper()
	st, err := state.New(d.State)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if bc.Keep == 0 {
		bc.Keep = 7
	}
	svc, err := New(d, bc, st, "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestCollectSealsConfigKeysAndDatabasesNotMail(t *testing.T) {
	svc := openService(t, fixtureDirs(t), config.BackupConfig{})
	p, err := svc.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if p.ServiceName != "KyPost" || p.AppVersion != "0.0.0-test" {
		t.Fatalf("manifest identity %q %q", p.ServiceName, p.AppVersion)
	}
	paths := map[string]bool{}
	for _, f := range p.Files {
		paths[f.Path] = true
	}
	for _, want := range []string{
		"config/config.yaml", "config/users.json", "config/users/u1/imap-config.json",
		"private/imap-config.key", "private/pairing.key",
		"state/state.db", "state/users/u1/state.db", "state/users/u1/contacts.json",
	} {
		if !paths[want] {
			t.Errorf("missing %s (have %v)", want, paths)
		}
	}
	for _, no := range []string{"state/users/u1/mailcache.json", "state/users/u1/state.json.migrated"} {
		if paths[no] {
			t.Errorf("%s must not be sealed", no)
		}
	}
	if p.VerificationRecipe["mail"] != ErrMailExcluded {
		t.Fatalf("recipe must say mail is excluded, got %v", p.VerificationRecipe["mail"])
	}
	if entries, _ := os.ReadDir(filepath.Join(svc.dirs.State, "backup-scratch")); len(entries) != 0 {
		t.Fatalf("scratch left behind: %v", entries)
	}
}

func TestNewRefusesWithoutMasterKey(t *testing.T) {
	d := fixtureDirs(t)
	os.Remove(filepath.Join(d.Secret, "imap-config.key"))
	st, _ := state.New(d.State)
	defer st.Close()
	svc, err := New(d, config.BackupConfig{Keep: 7}, st, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Collect(); err == nil {
		t.Fatal("New must refuse when the master key that seals the token is missing")
	}
}

func TestCollectRefusesOversizedFile(t *testing.T) {
	d := fixtureDirs(t)
	if err := os.WriteFile(filepath.Join(d.Config, "TUNING.md"), make([]byte, 65<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := openService(t, d, config.BackupConfig{})
	_, err := svc.Collect()
	if err == nil || !strings.Contains(err.Error(), "TUNING.md") || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("want an error naming the file and the cap, got %v", err)
	}
}

func TestCollectOptionalTuningOverride(t *testing.T) {
	for _, tc := range []struct {
		name              string
		present, external bool
		wantErr           bool
	}{
		{name: "missing compose default"},
		{name: "missing external override", external: true, wantErr: true},
		{name: "present collected override", present: true},
		{name: "present external override", present: true, external: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fixtureDirs(t)
			path := filepath.Join(d.Config, "TUNING.md")
			if tc.external {
				path = filepath.Join(t.TempDir(), "TUNING.md")
			}
			if tc.present {
				if err := os.WriteFile(path, []byte("custom prompt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TUNING_FILE", path)
			svc := openService(t, d, config.BackupConfig{})
			payload, err := svc.Collect()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "TUNING_FILE") {
					t.Fatalf("expected unsupported override error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range payload.Files {
				if f.Path == "config/TUNING.md" {
					found = string(f.Data) == "custom prompt"
				}
			}
			if found != tc.present {
				t.Fatalf("custom prompt included = %v, want %v", found, tc.present)
			}
		})
	}
}

// Cloudflare credentials are sealed; the host record is not, so a restored
// copy starts fenced. The ledger database is snapshotted, never copied raw.
func TestCollectSealsCloudflareCredentialsNotHostRecord(t *testing.T) {
	d := fixtureDirs(t)
	for _, name := range []string{cfreceiving.CredentialsFile, cfreceiving.HostFile, cfreceiving.HostFile + ".tmp.123"} {
		if err := os.WriteFile(filepath.Join(d.Secret, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := openService(t, d, config.BackupConfig{}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, f := range p.Files {
		paths[f.Path] = true
	}
	if !paths["private/"+cfreceiving.CredentialsFile] || paths["private/"+cfreceiving.HostFile] || paths["private/"+cfreceiving.HostFile+".tmp.123"] {
		t.Fatal("credentials must be sealed and the host record excluded", paths)
	}
	if !snapshotDatabase(cfreceiving.DBFile) {
		t.Fatal("ledger database copied without a SQLite snapshot")
	}
}

// The sender block list and automatic-block evidence are sealed as
// written; their lock files are not.
func TestCollectSealsSenderBlocks(t *testing.T) {
	d := fixtureDirs(t)
	dir := filepath.Join(d.State, "receiving")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ingress.NewBlocks(dir).Put(context.Background(), ingress.SenderBlock{Kind: "domain", Value: "evil.test", Source: "manual", Actor: "a", Reason: "spam"}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ingress.NewEvidence(dir).Accepted(context.Background(), ingress.Authentication{Sender: "friend@friendly.test", From: "friend@friendly.test", SPF: true, DKIM: []string{"friendly.test"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ingress.BlocksFile, ingress.EvidenceFile} {
		live, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		p, err := openService(t, d, config.BackupConfig{}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range p.Files {
			found = found || f.Path == "state/receiving/"+name && string(f.Data) == string(live)
			if strings.HasSuffix(f.Path, ".lock") {
				t.Fatal("lock file collected", f.Path)
			}
		}
		if !found {
			t.Fatal("not sealed", name)
		}
	}
	// A block list load would refuse is refused at backup time too.
	path := filepath.Join(dir, ingress.BlocksFile)
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"blocks":[{"kind":"domain","value":"Evil.test"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openService(t, d, config.BackupConfig{}).Collect(); err == nil || !strings.Contains(err.Error(), ingress.BlocksFile) {
		t.Fatal("malformed block list sealed", err)
	}
	if err := os.WriteFile(path, live, 0o600); err != nil {
		t.Fatal(err)
	}
	// Evidence is heuristic: a malformed or oversized copy, and a damaged one
	// set aside, are left out and the backup still seals.
	if err := os.WriteFile(filepath.Join(dir, ingress.EvidenceDamagedFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{0, ingress.MaxEvidenceBytes + 1, recoveryclient.MaxCapsuleFileBytes + 1} {
		evidence := filepath.Join(dir, ingress.EvidenceFile)
		if err := os.WriteFile(evidence, []byte(`{"version":1,"subject":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if size > 0 { // sparse: no disk is spent on the oversized cases
			if err := os.Truncate(evidence, size); err != nil {
				t.Fatal(err)
			}
		}
		p, err := openService(t, d, config.BackupConfig{}).Collect()
		if err != nil {
			t.Fatal("bad evidence refused the backup", err)
		}
		for _, f := range p.Files {
			if strings.Contains(f.Path, "sender-evidence") {
				t.Fatal("bad evidence sealed", f.Path)
			}
		}
	}
}
