package sso

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNativeLedgerCurrentDecodeKeepsValidation(t *testing.T) {
	want := ledgerDecodeFixture(2)
	for _, historical := range []bool{false, true} {
		for name, mutate := range map[string]func(*nativeLedger){
			"missing-accounts":  func(l *nativeLedger) { l.Accounts = nil },
			"missing-mailboxes": func(l *nativeLedger) { l.Mailboxes = nil },
			"missing-addresses": func(l *nativeLedger) { l.Addresses = nil },
			"invalid-history": func(l *nativeLedger) {
				for key, address := range l.Addresses {
					address.History = nil
					l.Addresses[key] = address
					break
				}
			},
			"foreign-account-key": func(l *nativeLedger) {
				for key, account := range l.Accounts {
					delete(l.Accounts, key)
					l.Accounts["foreign"] = account
					break
				}
			},
		} {
			t.Run(fmt.Sprintf("%s/historical=%t", name, historical), func(t *testing.T) {
				ledger := want.ledger(nil)
				mutate(&ledger)
				raw, err := json.Marshal(ledger)
				if err != nil {
					t.Fatal(err)
				}
				if _, version, err := parseNativeLedger(raw, historical); err == nil || version != 2 {
					t.Fatalf("invalid ledger admitted: version=%d err=%v", version, err)
				}
			})
		}
	}
	// An empty current ledger is valid. A successful decode must still return
	// the same complete stored representation for runtime and historical use.
	raw := []byte(`{"version":2,"accounts":{},"mailboxes":{},"addresses":{}}`)
	runtime, version, err := parseNativeLedger(raw, false)
	historical, historicalVersion, historicalErr := parseNativeLedger(raw, true)
	if err != nil || historicalErr != nil || version != 2 || historicalVersion != 2 || !reflect.DeepEqual(runtime, historical) {
		t.Fatalf("current ledger views differ: %d/%d %v/%v", version, historicalVersion, err, historicalErr)
	}
}

func BenchmarkNativeLedgerParseScale(b *testing.B) {
	for _, n := range []int{200, 3000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			raw, err := json.Marshal(ledgerDecodeFixture(n).ledger(nil))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for range b.N {
				f, version, err := parseNativeLedger(raw, false)
				if err != nil || version != 2 || len(f.Accounts) != n {
					b.Fatal("invalid decoded ledger", err)
				}
			}
		})
	}
}

func BenchmarkNativeLedgerReadScale(b *testing.B) {
	for _, n := range []int{200, 3000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			store := NewLifecycleStore(b.TempDir())
			raw, err := json.Marshal(ledgerDecodeFixture(n).ledger(nil))
			if err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(store.path), nativeProvisioningFile), raw, 0o600); err != nil {
				b.Fatal(err)
			}
			lifecycle := lifecycleFile{NativeProvisioningInitialized: true}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for range b.N {
				f, version, err := store.readNativeLedger(lifecycle, false)
				if err != nil || version != 2 || len(f.Accounts) != n {
					b.Fatal("invalid freshly read ledger", err)
				}
			}
		})
	}
}
