//go:build linux

package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/ky-primitives/capsule"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// An extra mailbox round-trips through a sealed backup: its mail is kept, its
// references rotate and the restore stays held; a device row in its mail-only
// state refuses the whole restore.
func TestNativeBackupRestoresExtraMailbox(t *testing.T) {
	ctx := context.Background()
	s, u := nativeService(t)
	life := sso.NewLifecycleStore(s.dirs.Config)
	m, err := life.CreateNativeMailbox(ctx, s.dirs.State, u.ID, "sales@example.test")
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := life.NativeMailboxAssignment(m.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(a.Dir(s.dirs.State), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@outside.test\r\nTo: sales@example.test\r\nSubject: kept\r\n\r\nextra mailbox mail\r\n")
	id, err := store.Import(ctx, mailbox.Receipt{Gateway: "qualified-test", Delivery: "one", Sender: "sender@outside.test", Recipients: []mailbox.Recipient{{Address: "sales@example.test", Generation: 2}}}, bytes.NewReader(raw))
	generation := store.MessageReferenceGeneration()
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	key := pinTestKey(t, s)
	result, err := s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capsuleBytes, err := os.ReadFile(result.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, damage := range []string{"none", "device-row"} {
		t.Run(damage, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "restored")
			if _, _, err := capsule.Open(capsuleBytes, key, dir); err != nil {
				t.Fatal(err)
			}
			extra := filepath.Join(dir, "state/mailboxes", m.ID)
			if damage == "device-row" {
				st, err := state.OpenNative(extra, a.Source)
				if err != nil {
					t.Fatal(err)
				}
				err = st.UpsertNativeDevice(state.NativeDevice{DeviceID: "phone", Platform: "android", PushToken: "push", SecretHash: "credential"})
				_ = st.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			native, err := QuarantineNativeRestore(dir)
			if !native || (err == nil) != (damage == "none") {
				t.Fatalf("native=%v err=%v", native, err)
			}
			if !errors.Is(sso.RequireNativeRestoreReleased(filepath.Join(dir, "state")), sso.ErrNativeRestoreHold) {
				t.Fatal("restore did not persist a hold")
			}
			if damage != "none" {
				return
			}
			restored, err := mailbox.OpenExisting(filepath.Join(extra, "mailbox"), a.Owner, a.Limits, a.Source)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if got, err := restored.Raw(ctx, "INBOX", id); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("extra mailbox mail lost", err)
			}
			if restored.MessageReferenceGeneration() == generation {
				t.Fatal("extra mailbox references not rotated")
			}
		})
	}
}
