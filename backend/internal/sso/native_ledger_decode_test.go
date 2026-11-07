package sso

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

func ledgerDecodeFixture(n int) nativeAssignments {
	f := nativeAssignments{Accounts: map[string]NativeAssignment{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("decode-user-%04d", i)
		a := NativeAssignment{
			Owner:   mailbox.Owner{Issuer: "https://decode.example.test", Subject: id, Mailbox: id},
			Address: id + "@example.test", StateRoot: "/private-state",
			Limits:   mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 8 << 20, Records: 100},
			Revision: 1, Digest: "fixture-digest", DesiredActive: true, Status: "applied",
			Source: fmt.Sprintf("native:%064x", i+1), LegacyMixedUse: i%2 == 0,
		}
		f.Accounts[directoryKey(a.Owner.Issuer, a.Owner.Subject)] = a
	}
	return f
}

func TestNativeLedgerVersionDispatchPreservesAssignments(t *testing.T) {
	want := ledgerDecodeFixture(2)
	legacy, err := json.Marshal(struct {
		Version  int                         `json:"version"`
		Accounts map[string]NativeAssignment `json:"accounts"`
	}{1, want.Accounts})
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.Marshal(want.ledger(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseNativeLedger(legacy, false); err == nil {
		t.Fatal("runtime accepted legacy ledger")
	}
	for _, tc := range []struct {
		name       string
		raw        []byte
		historical bool
		version    int
	}{
		{"legacy-historical", legacy, true, 1},
		{"current-runtime", current, false, 2},
		{"current-historical", current, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, version, err := parseNativeLedger(tc.raw, tc.historical)
			if err != nil || version != tc.version || !reflect.DeepEqual(got.Accounts, want.Accounts) {
				t.Fatalf("assignment view changed: version=%d err=%v got=%+v", version, err, got.Accounts)
			}
		})
	}
	broken := want.ledger(nil)
	for key, a := range broken.Accounts {
		a.PrimaryMailbox = "foreign-mailbox"
		broken.Accounts[key] = a
		break
	}
	foreign, err := json.Marshal(broken)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"foreign-mailbox":         foreign,
		"truncated":               current[:len(current)-1],
		"malformed-accounts":      []byte(`{"version":2,"accounts":[],"mailboxes":{},"addresses":{}}`),
		"unsupported":             []byte(`{"version":3,"accounts":{}}`),
		"missing-legacy-accounts": []byte(`{"version":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			for _, historical := range []bool{false, true} {
				if _, _, err := parseNativeLedger(raw, historical); err == nil {
					t.Fatal("accepted malformed/foreign ledger")
				}
			}
		})
	}
}

func BenchmarkParseNativeLedgerV2(b *testing.B) {
	raw, err := json.Marshal(ledgerDecodeFixture(200).ledger(nil))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		f, version, err := parseNativeLedger(raw, false)
		if err != nil || version != 2 || len(f.Accounts) != 200 {
			b.Fatal("invalid decoded ledger", err)
		}
	}
}
