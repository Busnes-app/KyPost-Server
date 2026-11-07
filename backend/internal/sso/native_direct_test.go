//go:build linux

package sso

import (
	"context"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"path/filepath"
	"testing"
)

func TestNativeOutboundDirectIndependentRefusalsAndRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{true: "revoked", false: "independent"}[revoke], func(t *testing.T) {
			ctx := context.Background()
			sender, u, job := outboundFixture(t)
			_, err := mailmsg.SaveDomainRelay(ctx, filepath.Join(sender.ConfigDir, "native-relay.json"), sender.keyPath(), mailmsg.DomainRelay{Mode: "direct", Domains: []string{"example.test"}, Issuer: nativeIssuer, Host: "mail.example.test"})
			if err != nil {
				t.Fatal(err)
			}
			// Single-label recipient domains are refused before DNS or TCP. This
			// exercises real direct transport selection without any external traffic.
			job.Deliveries[0].Recipients = []string{"first@invalid", "second@invalid"}
			id, err := fsutil.NewUUIDv4()
			if err != nil {
				t.Fatal(err)
			}
			if err = sender.Queue(ctx, u.ID, id, job); err != nil {
				t.Fatal(err)
			}
			box, key, err := sender.openStorage(u.ID)
			if err != nil {
				t.Fatal(err)
			}
			stored, _, _, err := box.ReadOutbound(ctx, key, id)
			_ = box.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !stored.Independent || len(stored.Deliveries) != 2 {
				t.Fatal("direct recipients not durably split")
			}
			if revoke {
				if _, err = sender.Accounts.Deactivate(u.ID); err != nil {
					t.Fatal(err)
				}
			}
			_ = sender.Recover(ctx, u.ID, id)
			statuses, _, err := sender.Status(ctx, u.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if revoke {
				want = "quarantined"
			}
			for _, status := range statuses {
				if status.State != want {
					t.Fatalf("recipient blocked or authority bypassed: %+v", statuses)
				}
			}
		})
	}
}
