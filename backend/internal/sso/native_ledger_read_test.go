package sso

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// This is the pre-optimization reader, retained as a compatibility oracle for
// versions, partial results and error types/text, not as an alternate runtime.
func priorNativeLedgerRead(s *LifecycleStore, lifecycle lifecycleFile, historical bool) (nativeAssignments, int, error) {
	f := nativeAssignments{Accounts: map[string]NativeAssignment{}}
	loaded, version := false, 0
	var parseErr error
	err := fsutil.LoadJSONFile(s.nativePath(), func(raw json.RawMessage) {
		loaded = true
		f, version, parseErr = parseNativeLedger(raw, historical)
	}, func() error {
		if lifecycle.NativeProvisioningInitialized {
			return ErrNativeProvisioning
		}
		return nil
	})
	if err == nil && (parseErr != nil || loaded && !lifecycle.NativeProvisioningInitialized) {
		err = ErrNativeProvisioning
	}
	f.stored.seedGenerations(lifecycle.Directory)
	return f, version, err
}

func TestNativeLedgerReadCompatibility(t *testing.T) {
	fixture := ledgerDecodeFixture(2)
	current, err := json.Marshal(fixture.ledger(nil))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(struct {
		Version  int                         `json:"version"`
		Accounts map[string]NativeAssignment `json:"accounts"`
	}{1, fixture.Accounts})
	if err != nil {
		t.Fatal(err)
	}
	for _, initialized := range []bool{false, true} {
		for _, historical := range []bool{false, true} {
			for name, raw := range map[string][]byte{
				"current": current, "legacy": legacy,
				"empty-current": []byte(`{"version":2,"accounts":{},"mailboxes":{},"addresses":{}}`),
				"missing":       nil, "empty": {}, "null": []byte(`null`),
				"truncated":              current[:len(current)-1],
				"bad-version":            []byte(`{"version":"2"}`),
				"future-version":         []byte(`{"version":3}`),
				"bad-accounts":           []byte(`{"version":2,"accounts":[],"mailboxes":{},"addresses":{}}`),
				"semantic-before-syntax": []byte(`{"version":"2","accounts":[}`),
				"trailing-value":         append(append([]byte{}, current...), []byte(` {}`)...),
			} {
				t.Run(fmt.Sprintf("%s/initialized=%t/historical=%t", name, initialized, historical), func(t *testing.T) {
					store := NewLifecycleStore(t.TempDir())
					if raw != nil {
						if err := os.WriteFile(store.nativePath(), raw, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					lifecycle := lifecycleFile{NativeProvisioningInitialized: initialized, Directory: map[string]DirectoryState{}}
					for key := range fixture.Accounts {
						lifecycle.Directory[key] = DirectoryState{Revision: 9}
					}
					want, wantVersion, wantErr := priorNativeLedgerRead(store, lifecycle, historical)
					got, version, err := store.readNativeLedger(lifecycle, historical)
					if version != wantVersion || !reflect.DeepEqual(got, want) || reflect.TypeOf(err) != reflect.TypeOf(wantErr) {
						t.Fatalf("reader result changed: version=%d/%d error=%T/%T", version, wantVersion, err, wantErr)
					}
					if err != nil && (err.Error() != wantErr.Error() || errors.Is(err, ErrNativeProvisioning) != errors.Is(wantErr, ErrNativeProvisioning)) {
						t.Fatalf("reader error changed: %v / %v", err, wantErr)
					}
				})
			}
		}
	}
}

func TestNativeLedgerReadRefreshAndIOFailure(t *testing.T) {
	store := NewLifecycleStore(t.TempDir())
	lifecycle := lifecycleFile{NativeProvisioningInitialized: true}
	fixture := ledgerDecodeFixture(1)
	var key string
	for k := range fixture.Accounts {
		key = k
	}
	for _, revision := range []int64{1, 2} {
		a := fixture.Accounts[key]
		a.Revision = revision
		fixture.Accounts[key] = a
		if err := fsutil.PersistJSONFile(store.nativePath(), fixture.ledger(nil)); err != nil {
			t.Fatal(err)
		}
		got, _, err := store.readNativeLedger(lifecycle, false)
		if err != nil || got.Accounts[key].Revision != revision {
			t.Fatal("reader reused old authority", revision, err)
		}
	}
	if err := os.Remove(store.nativePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.nativePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, wantErr := priorNativeLedgerRead(store, lifecycle, false)
	_, _, err := store.readNativeLedger(lifecycle, false)
	if err == nil || reflect.TypeOf(err) != reflect.TypeOf(wantErr) || err.Error() != wantErr.Error() {
		t.Fatalf("read failure changed or admitted old data: %v / %v", err, wantErr)
	}
}

// nativeUserOwnership decodes a ledger read taken before its lifecycle read,
// which is safe only while the initialization flag precedes every ledger write.
func TestNativeInitializationPrecedesLedgerWrite(t *testing.T) {
	s := NewLifecycleStore(t.TempDir())
	if err := os.Mkdir(s.nativePath(), 0700); err != nil { // the ledger write must fail
		t.Fatal(err)
	}
	if err := s.saveNative(nativeAssignments{}); err == nil {
		t.Fatal("ledger write unexpectedly succeeded")
	}
	lifecycle, err := s.load()
	if err != nil || !lifecycle.NativeProvisioningInitialized {
		t.Fatal("ledger written before the initialization flag", err)
	}
}
