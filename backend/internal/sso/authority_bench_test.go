//go:build linux

package sso

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
)

// benchAuthority is the real account "one" plus n-1 accounts written through
// the store (directory resource, account, primary mailbox and address each).
func benchAuthority(b *testing.B, n int) addressFixture {
	b.Helper()
	f := newAddressFixture(b, "one")
	life, err := f.life.load()
	if err != nil {
		b.Fatal(err)
	}
	ledger, err := f.life.loadNative()
	if err != nil {
		b.Fatal(err)
	}
	source := ledger.Accounts[directoryKey(nativeIssuer, "one")].Source
	for i := 1; i < n; i++ {
		sub := fmt.Sprintf("bulk-%05d", i)
		raw := fmt.Sprintf(`{"schemas":[%q],"id":%q,"externalId":%q,"userName":%q,"active":true,"roles":[],"emails":[{"value":"%s@example.test","primary":true},{"value":"%s.other@example.test","primary":false}],"meta":{"version":"W/\"1\""}}`, scimUserSchema, sub, sub, sub, sub, sub)
		var u DirectoryUser
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			b.Fatal(err)
		}
		key := directoryKey(nativeIssuer, sub)
		life.Directory[key] = DirectoryState{Resource: &u, Revision: 1, Digest: strings.Repeat("d", 64), Active: true, EventID: sub + "-1"}
		ledger.Accounts[key] = NativeAssignment{Owner: mailbox.Owner{Issuer: nativeIssuer, Subject: sub, Mailbox: fmt.Sprintf("bulk-id-%05d", i)}, Address: sub + "@example.test", StateRoot: f.root, Limits: nativeLimits, Revision: 1, Digest: strings.Repeat("d", 64), DesiredActive: true, Status: "applied", Source: source}
	}
	if err := f.life.persistLifecycle(life); err != nil {
		b.Fatal(err)
	}
	if err := f.life.saveNative(ledger); err != nil {
		b.Fatal(err)
	}
	return f
}

var benchAccounts = []int{1000, 3000, 10000}

// BenchmarkAdmitOpenMailbox is one admission of one account among n. The O(1)
// claim of docs/NATIVE_AUTHORITY_PER_ACCOUNT.md: ns/op and B/op at 10k within
// 1.2x of 3k. Without a held client the storage check uses the process's
// admission pool, as in the api and daemon; pool=off reopens both mailbox
// databases, as every other process does.
func BenchmarkAdmitOpenMailbox(b *testing.B) {
	for _, pool := range []int{mailbox.MaxHeldAdmissions, 0} {
		for _, n := range benchAccounts {
			b.Run(fmt.Sprintf("pool=%v/accounts=%d", pool != 0, n), func(b *testing.B) {
				mailbox.HoldAdmissionHandles(pool)
				defer mailbox.HoldAdmissionHandles(0)
				benchmarkAdmitOpenMailbox(b, n)
			})
		}
	}
}

func benchmarkAdmitOpenMailbox(b *testing.B, n int) {
	f := benchAuthority(b, n)
	ctx := context.Background()
	id := f.ids["one"]
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := f.life.AdmitOpenMailbox(ctx, f.root, nativeIssuer, id, id, f.accounts, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAdmissionAuthorityReads is the authority part of one admission
// alone: one View with the directory entry, the initialization flag and the
// account, as admitNativeMailUser reads them.
func BenchmarkAdmissionAuthorityReads(b *testing.B) {
	for _, n := range benchAccounts {
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			f := benchAuthority(b, n)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				err := f.life.authority().View(ctx, func(r authorityReader) error {
					if _, ok, err := r.Directory(nativeIssuer, "one"); err != nil || !ok {
						return cmp.Or(err, ErrNativeProvisioning)
					}
					if ok, err := r.Initialized(); err != nil || !ok {
						return cmp.Or(err, ErrNativeProvisioning)
					}
					_, ok, err := r.Account(nativeIssuer, "one")
					if err == nil && !ok {
						err = ErrNativeProvisioning
					}
					return err
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNativeMailboxes is the whole-ledger mailbox listing with addresses.
func BenchmarkNativeMailboxes(b *testing.B) {
	for _, n := range benchAccounts {
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			f := benchAuthority(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if m, err := f.life.NativeMailboxes(); err != nil || len(m) != n {
					b.Fatal(len(m), err)
				}
			}
		})
	}
}

// BenchmarkNativeOutboxDiscovery is the outbox runtime's per-pass ledger read
// (origin/main read NativeMailboxes here). Target: 10k within 1.2x of 3k per
// mailbox.
func BenchmarkNativeOutboxDiscovery(b *testing.B) {
	for _, n := range benchAccounts {
		b.Run(fmt.Sprintf("accounts=%d", n), func(b *testing.B) {
			f := benchAuthority(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if m, err := f.life.NativeMailboxesWithoutAddresses(); err != nil || len(m) != n {
					b.Fatal(len(m), err)
				}
			}
		})
	}
}
