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
	"reflect"
	"sync"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

func TestNativeContentViewReadsBeforeReuse(t *testing.T) {
	var view nativeContentView[string]
	path := filepath.Join(t.TempDir(), "contents")
	decodes := 0
	decode := func(raw []byte) (string, error) { decodes++; return string(raw), nil }
	eligible := func(string) bool { return true }
	read := func(want string) {
		t.Helper()
		if err := view.withCurrent(path, decode, eligible, func(v string) {
			if v != want {
				t.Fatalf("stale contents: %q, want %q", v, want)
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	read("one")
	read("one")
	if decodes != 1 {
		t.Fatal("unchanged bytes decoded again", decodes)
	}
	if err := fsutil.AtomicWriteFile(path, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	read("two")
	if decodes != 2 {
		t.Fatal("replacement did not decode", decodes)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := view.withCurrent(path, decode, eligible, func(string) { called = true }); err == nil || called {
		t.Fatal("missing file used retained contents", err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := view.withCurrent(path, decode, eligible, func(string) { called = true }); err == nil || called {
		t.Fatal("read error used retained contents", err)
	}
}

func TestNativeContentViewRetentionBounds(t *testing.T) {
	var view nativeContentView[int]
	path := filepath.Join(t.TempDir(), "contents")
	decodes := 0
	decode := func(raw []byte) (int, error) { decodes++; return len(raw), nil }
	for _, test := range []struct {
		size     int
		eligible bool
	}{{nativeViewBytes + 1, true}, {8, false}} {
		if err := os.WriteFile(path, bytes.Repeat([]byte(" "), test.size), 0600); err != nil {
			t.Fatal(err)
		}
		before := decodes
		for range 2 {
			if err := view.withCurrent(path, decode, func(int) bool { return test.eligible }, func(n int) {
				if n != test.size {
					t.Fatal("oversized input lost normal decoding")
				}
			}); err != nil {
				t.Fatal(err)
			}
		}
		if decodes != before+2 || view.raw != nil {
			t.Fatal("ineligible input retained")
		}
	}
	directory := nativeDirectoryContents{directory: make(map[string]DirectoryState)}
	assignments := make(map[string]NativeAssignment)
	for i := range nativeViewAccounts + 1 {
		key := fmt.Sprint(i)
		directory.directory[key] = DirectoryState{}
		assignments[key] = NativeAssignment{}
	}
	if retainNativeDirectory(directory) || retainNativeAssignments(assignments) {
		t.Fatal("account bound ignored")
	}
	resource := &DirectoryUser{Schemas: make([]string, 9)}
	if retainNativeDirectory(nativeDirectoryContents{directory: map[string]DirectoryState{"one": {Resource: resource}}}) {
		t.Fatal("resource shape bound ignored")
	}
}

func TestNativeAdmissionViewCopiesDirectoryAndAssignments(t *testing.T) {
	f := newAddressFixture(t, "one")
	key := directoryKey(nativeIssuer, "one")
	initial, err := f.life.load()
	if err != nil {
		t.Fatal(err)
	}
	initial.Directory[key].Resource.Roles = json.RawMessage(`["worker"]`)
	if err := fsutil.PersistJSONFile(f.life.path, initial); err != nil {
		t.Fatal(err)
	}
	d, known, initialized, err := f.life.nativeDirectoryForAdmission(key)
	if err != nil || !known || !initialized {
		t.Fatal("missing directory", err)
	}
	want, err := f.life.load()
	if err != nil || !reflect.DeepEqual(d, want.Directory[key]) {
		t.Fatal("directory differs", err)
	}
	*d.Resource.Active = false
	d.Resource.ID = "changed"
	d.Resource.Schemas[0] = "changed"
	d.Resource.Emails[0].Value = "changed@example.test"
	d.Resource.Roles[0] = 'x'
	again, _, _, err := f.life.nativeDirectoryForAdmission(key)
	if err != nil || !reflect.DeepEqual(again, want.Directory[key]) {
		t.Fatal("caller mutated shared directory", err)
	}
	a, ok, err := f.life.nativeAssignmentForAdmission(key, true)
	if err != nil || !ok {
		t.Fatal(err)
	}
	ledger, _, err := f.life.readNativeLedger(want, false)
	if err != nil || a != ledger.Accounts[key] {
		t.Fatal("assignment differs", err)
	}
	a.Source = "changed"
	againAssignment, _, err := f.life.nativeAssignmentForAdmission(key, true)
	if err != nil || againAssignment != ledger.Accounts[key] {
		t.Fatal("assignment mutated shared data", err)
	}
}

func TestNativeAdmissionViewRechecksChangedFiles(t *testing.T) {
	for _, damage := range []string{"same-size-same-time", "malformed", "unrelated-field", "history", "uninitialized"} {
		t.Run(damage, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			id := f.ids["one"]
			admit := func() error {
				_, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts)
				return err
			}
			if err := admit(); err != nil {
				t.Fatal(err)
			}
			current, err := f.life.load()
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "same-size-same-time":
				info, err := os.Stat(f.life.path)
				if err != nil {
					t.Fatal(err)
				}
				current.Directory[directoryKey(nativeIssuer, "one")].Resource.ID = "two"
				if err := fsutil.PersistJSONFile(f.life.path, current); err != nil {
					t.Fatal(err)
				}
				updated, err := os.Stat(f.life.path)
				if err != nil || updated.Size() != info.Size() {
					t.Fatal("test must keep size", err)
				}
				if err := os.Chtimes(f.life.path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(f.life.path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unrelated-field":
				raw, err := os.ReadFile(f.life.path)
				if err != nil {
					t.Fatal(err)
				}
				var object map[string]json.RawMessage
				if err := json.Unmarshal(raw, &object); err != nil {
					t.Fatal(err)
				}
				object["logouts"] = json.RawMessage(`[]`)
				if err := fsutil.PersistJSONFile(f.life.path, object); err != nil {
					t.Fatal(err)
				}
			case "history":
				ledger, err := f.life.loadNative()
				if err != nil {
					t.Fatal(err)
				}
				for key, address := range ledger.stored.Addresses {
					address.History = nil
					ledger.stored.Addresses[key] = address
				}
				if err := fsutil.PersistJSONFile(f.life.nativePath(), ledger.stored); err != nil {
					t.Fatal(err)
				}
			case "uninitialized":
				current.NativeProvisioningInitialized = false
				if err := fsutil.PersistJSONFile(f.life.path, current); err != nil {
					t.Fatal(err)
				}
			}
			if err := admit(); err == nil {
				t.Fatal("prior decoded view admitted changed authority")
			}
		})
	}
}

func TestNativeAdmissionViewIndependentProcess(t *testing.T) {
	for _, change := range []string{"offboard", "source", "role"} {
		t.Run(change, func(t *testing.T) {
			f := newAddressFixture(t, "one")
			id := f.ids["one"]
			if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestNativeAdmissionViewWriterProcess$")
			command.Env = append(os.Environ(), "KYPOST_VIEW_CHILD="+change, "KYPOST_VIEW_CONFIG="+f.config)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("independent writer failed: %v\n%s", err, output)
			}
			if _, err := f.life.AdmitNativeMailbox(context.Background(), f.root, nativeIssuer, id, id, f.accounts); err == nil {
				t.Fatal("retained view survived independent process authority change")
			}
		})
	}
}

func TestNativeAdmissionViewWriterProcess(t *testing.T) {
	change := os.Getenv("KYPOST_VIEW_CHILD")
	if change == "" {
		return
	}
	s := NewLifecycleStore(os.Getenv("KYPOST_VIEW_CONFIG"))
	if change == "offboard" {
		nativeDesired(t, s, "one", "one@example.test", 2, false)
		return
	}
	release, err := s.LockDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	switch change {
	case "source":
		editAssignment(t, s, func(a *NativeAssignment) { a.Source = "foreign-source" })
	case "role":
		f, err := s.load()
		if err != nil {
			t.Fatal(err)
		}
		f.Directory[directoryKey(nativeIssuer, "one")].Resource.Roles = json.RawMessage(`["kypost.admin"]`)
		if err := fsutil.PersistJSONFile(s.path, f); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown test writer")
	}
}

func TestNativeAdmissionViewConcurrentCopies(t *testing.T) {
	f := newAddressFixture(t, "one")
	key := directoryKey(nativeIssuer, "one")
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 10 {
				d, known, initialized, err := f.life.nativeDirectoryForAdmission(key)
				if err != nil || !known || !initialized || d.Resource.ID != "one" {
					t.Error("concurrent stale or mutated directory", err)
					return
				}
				d.Resource.Emails[0].Value = "changed@example.test"
				*d.Resource.Active = false
				a, known, err := f.life.nativeAssignmentForAdmission(key, true)
				if err != nil || !known || a.Owner.Subject != "one" {
					t.Error("concurrent ledger changed", err)
					return
				}
			}
		})
	}
	wg.Wait()
}

func BenchmarkNativeAdmissionView(b *testing.B) {
	for _, n := range []int{200, 3000, nativeViewAccounts + 1} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := NewLifecycleStore(b.TempDir())
			fixture := ledgerDecodeFixture(n)
			lifecycle := lifecycleFile{NativeProvisioningInitialized: true, Directory: map[string]DirectoryState{}}
			var key string
			for k, a := range fixture.Accounts {
				key = k
				active := true
				resource := &DirectoryUser{Schemas: []string{scimUserSchema}, ID: a.Owner.Subject, ExternalID: a.Owner.Subject, UserName: a.Owner.Subject, Active: &active}
				resource.Meta.Version = fmt.Sprintf(`W/"%d"`, a.Revision)
				resource.Emails = append(resource.Emails, struct {
					Value   string `json:"value"`
					Primary bool   `json:"primary"`
				}{a.Address, true})
				lifecycle.Directory[k] = DirectoryState{Active: true, Revision: a.Revision, Resource: resource}
			}
			if err := fsutil.PersistJSONFile(s.path, lifecycle); err != nil {
				b.Fatal(err)
			}
			if err := fsutil.PersistJSONFile(s.nativePath(), fixture.ledger(nil)); err != nil {
				b.Fatal(err)
			}
			for _, kind := range []string{"directory", "ledger"} {
				for _, mode := range []string{"original", "reuse"} {
					b.Run(kind+"/"+mode, func(b *testing.B) {
						read := func() {
							if kind == "directory" {
								if mode == "original" {
									f, err := s.load()
									if err != nil || !f.Directory[key].Active {
										b.Fatal(err)
									}
								} else {
									d, ok, initialized, err := s.nativeDirectoryForAdmission(key)
									if err != nil || !ok || !initialized || !d.Active {
										b.Fatal(err)
									}
								}
							} else if mode == "original" {
								f, _, err := s.readNativeLedger(lifecycle, false)
								if err != nil || f.Accounts[key].Owner.Subject == "" {
									b.Fatal(err)
								}
							} else {
								a, ok, err := s.nativeAssignmentForAdmission(key, true)
								if err != nil || !ok || a.Owner.Subject == "" {
									b.Fatal(err)
								}
							}
						}
						read()
						b.ReportAllocs()
						b.ResetTimer()
						for range b.N {
							read()
						}
						b.StopTimer()
					})
				}
			}
		})
	}
}

// nativeAssignmentForAdmission reads the ledger itself; production callers
// pass the bytes their format check read.
func (s *LifecycleStore) nativeAssignmentForAdmission(key string, initialized bool) (NativeAssignment, bool, error) {
	raw, err := os.ReadFile(s.nativePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return NativeAssignment{}, false, err
	}
	return nativeAssignmentFromLedger(raw, err == nil, key, initialized)
}

// nativeDirectoryForAdmission reads the lifecycle itself; production callers
// decode the bytes their transaction read.
func (s *LifecycleStore) nativeDirectoryForAdmission(key string) (DirectoryState, bool, bool, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return DirectoryState{}, false, false, nil
	}
	if err != nil {
		return DirectoryState{}, false, false, err
	}
	return nativeDirectoryFromLifecycle(raw, key)
}

func (c *nativeContentView[T]) withCurrent(path string, decode func([]byte) (T, error), eligible func(T) bool, copyValue func(T)) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.with(raw, decode, eligible, copyValue)
}
