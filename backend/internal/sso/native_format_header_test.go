//go:build linux

package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

func TestNativeFormatHeaderReuseKeepsFilesystemFences(t *testing.T) {
	for _, damage := range []string{"none", "legacy-data", "malformed-legacy", "unreadable-legacy", "missing-domains", "missing-ledger", "unreadable-ledger", "malformed-ledger", "v1-ledger", "unvalidated-v2"} {
		t.Run(damage, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			key := directoryKey(nativeIssuer, "one")
			if _, known, err := f.life.nativeAssignmentForAdmission(key, true); err != nil || !known {
				t.Fatal("warm complete ledger validation", err)
			}
			raw, err := os.ReadFile(f.life.nativePath())
			if err != nil || !nativeAssignmentsAlreadyDecoded(raw) {
				t.Fatal("test must warm the exact ledger bytes", err)
			}
			legacy := filepath.Join(f.config, legacyNativeDomainFile)
			if err := fsutil.AtomicWriteFile(legacy, []byte(`{"migratedTo":"native-domains.json"}`), 0600); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "legacy-data":
				err = fsutil.AtomicWriteFile(legacy, []byte(`{"domain":"example.test"}`), 0600)
			case "malformed-legacy":
				err = fsutil.AtomicWriteFile(legacy, []byte(`{`), 0600)
			case "unreadable-legacy":
				err = os.Remove(legacy)
				if err == nil {
					err = os.Mkdir(legacy, 0700)
				}
			case "missing-domains":
				err = os.Remove(filepath.Join(f.config, NativeDomainsFile))
			case "missing-ledger", "unreadable-ledger":
				err = os.Remove(f.life.nativePath())
				if err == nil && damage == "unreadable-ledger" {
					err = os.Mkdir(f.life.nativePath(), 0700)
				}
			case "malformed-ledger":
				err = fsutil.AtomicWriteFile(f.life.nativePath(), []byte(`{"version":2,`), 0600)
			case "v1-ledger":
				err = fsutil.AtomicWriteFile(f.life.nativePath(), []byte(`{"version":1,"accounts":{}}`), 0600)
			case "unvalidated-v2":
				// The original format guard checks version, not full ownership.
				// Unvalidated v2 must use that same fallback, never populate a view.
				err = fsutil.AtomicWriteFile(f.life.nativePath(), []byte(`{"version":2}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = checkNativeFormat(f.config)
			if damage == "none" || damage == "unvalidated-v2" {
				if err != nil {
					t.Fatal("changed standalone format behavior", err)
				}
			} else if !errors.Is(err, ErrNativeMigration) {
				t.Fatal("warm bytes bypassed migration refusal", err)
			}
			// Admission decodes the ledger bytes its format check read, so it
			// must refuse exactly where the standalone check does.
			id := f.ids["one"]
			_, err = f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts)
			switch damage {
			case "none":
				if err != nil {
					t.Fatal("shared ledger read refused valid admission", err)
				}
			case "unvalidated-v2":
				if err == nil {
					t.Fatal("admitted an unvalidated ledger")
				}
			default:
				if !errors.Is(err, ErrNativeMigration) {
					t.Fatal("admission bypassed migration refusal", err)
				}
			}
			if damage == "unvalidated-v2" {
				if nativeAssignmentsAlreadyDecoded([]byte(`{"version":2}`)) {
					t.Fatal("format-only check published an unvalidated view")
				}
				if _, _, err := f.life.nativeAssignmentForAdmission(key, true); err == nil {
					t.Fatal("format guard granted ownership")
				}
			}
		})
	}
}

func TestNativeFormatHeaderIndependentReplacement(t *testing.T) {
	f := newAddressFixture(t, "one")
	id := f.ids["one"]
	if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(f.life.nativePath())
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestNativeFormatHeaderWriterProcess$")
	command.Env = append(os.Environ(), "KYPOST_FORMAT_CHILD_CONFIG="+f.config)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("independent format writer failed: %v\n%s", err, output)
	}
	after, err := os.Stat(f.life.nativePath())
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("test must keep size and mtime", err)
	}
	if err := checkNativeFormat(f.config); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("warm view bypassed changed version", err)
	}
	if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts); !errors.Is(err, ErrNativeMigration) {
		t.Fatal("mail admitted changed format", err)
	}
}

func TestNativeFormatHeaderWriterProcess(t *testing.T) {
	config := os.Getenv("KYPOST_FORMAT_CHILD_CONFIG")
	if config == "" {
		return
	}
	s := NewLifecycleStore(config)
	release, err := s.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	raw, err := os.ReadFile(s.nativePath())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.nativePath())
	if err != nil {
		t.Fatal(err)
	}
	index := bytes.Index(raw, []byte(`"version"`))
	if index < 0 {
		t.Fatal("missing fixture version")
	}
	index += bytes.IndexByte(raw[index:], ':') + 1
	for raw[index] == ' ' || raw[index] == '\n' || raw[index] == '\t' {
		index++
	}
	if raw[index] != '2' {
		t.Fatal("fixture is not v2")
	}
	raw[index] = '3'
	if err := fsutil.AtomicWriteFile(s.nativePath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.nativePath(), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkNativeFormatHeader(b *testing.B) {
	for _, n := range []int{200, 3000, nativeViewAccounts + 1} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := NewLifecycleStore(b.TempDir())
			if err := fsutil.PersistJSONFile(s.nativePath(), ledgerDecodeFixture(n).ledger(nil)); err != nil {
				b.Fatal(err)
			}
			if _, _, err := s.nativeAssignmentForAdmission("absent", true); err != nil {
				b.Fatal(err)
			}
			for _, mode := range []string{"original", "reuse"} {
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if mode == "reuse" {
							if err := checkNativeFormat(filepath.Dir(s.path)); err != nil {
								b.Fatal(err)
							}
						} else {
							// These fixtures have no legacy file: preserve its fresh
							// read and the original version-only JSON decode.
							if _, _, err := legacyNativeDomain(filepath.Dir(s.path)); err != nil {
								b.Fatal(err)
							}
							raw, err := os.ReadFile(s.nativePath())
							var head struct {
								Version int `json:"version"`
							}
							if err != nil || json.Unmarshal(raw, &head) != nil || head.Version != 2 {
								b.Fatal("original format check failed", err)
							}
						}
					}
				})
			}
		})
	}
}

// Admission's ledger step: format check plus assignment, reading once or twice.
func BenchmarkNativeAdmissionLedgerRead(b *testing.B) {
	for _, n := range []int{200, 3000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := NewLifecycleStore(b.TempDir())
			if err := fsutil.PersistJSONFile(s.nativePath(), ledgerDecodeFixture(n).ledger(nil)); err != nil {
				b.Fatal(err)
			}
			dir := filepath.Dir(s.path)
			for _, mode := range []string{"twice", "once"} {
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						var err error
						if mode == "twice" {
							if err = checkNativeFormat(dir); err == nil {
								_, _, err = s.nativeAssignmentForAdmission("absent", true)
							}
						} else {
							ledger, exists, ferr := currentNativeFormat(dir)
							if err = ferr; err == nil {
								_, _, err = nativeAssignmentFromLedger(ledger, exists, "absent", true)
							}
						}
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
