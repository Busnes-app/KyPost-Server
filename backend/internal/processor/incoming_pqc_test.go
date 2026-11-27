package processor

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail/pgpmailtest"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/armor"
)

func TestIncomingPQCWaitsForEveryDevice(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(map[bool]string{false: "Linux still classical", true: "all confirmed"}[capable], func(t *testing.T) {
			p, uc, mailbox := incomingPollerForTest(t)
			uc.autoLabelEnabled = false
			u, _ := p.users.Get(uc.id)
			u, err := p.users.SetDerivedAuth(context.Background(), u.ID, strings.Repeat("a", 64), base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), 600000, false)
			if err != nil {
				t.Fatal(err)
			}
			old, _ := pgpmail.ReadPublicKey(u.PGPPublicKey)
			pq, err := pgpmailtest.GenerateV6PublicKey("Alice", "alice@example.com", true)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := pgpmail.InspectPQCKey(pq)
			u, err = p.users.CommitPGPKeyring(context.Background(), u.ID, users.PGPKeyringUpdate{ExpectedRevision: u.PGPRevision, MaterialGeneration: 1,
				PrimaryFingerprints: []string{u.PGPFingerprint, info.Fingerprint}, KeyFingerprints: append(old.KeyFingerprints, info.KeyFingerprints...),
				PublicKey: u.PGPPublicKey, PublicKeyV6: pq, PasswordEnvelope: `{"v":2}`, RecoveryEnvelope: `{"v":2}`, Source: "generated"})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"phone", "linux"} {
				if err := uc.store.UpsertNativeDevice(state.NativeDevice{DeviceID: id, Platform: id, PushToken: id}); err != nil {
					t.Fatal(err)
				}
				caps := []string{"v4", "v6", "rfc9980"}
				if id == "linux" && !capable {
					caps = []string{"v4"}
				}
				if _, err := uc.store.SetNativeDeviceEnrollmentKey(id, "KEY", "now", []int{6}, caps); err != nil {
					t.Fatal(err)
				}
				delivery := state.DeviceEnrollment{Version: 6, Generation: 1, Fingerprint: u.PGPFingerprint}
				if err := uc.store.RecordNativeDeviceDelivery(id, delivery); err != nil {
					t.Fatal(err)
				}
				if ok, err := uc.store.ConfirmNativeDeviceEnrollment(id, delivery, caps); err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			if err := p.encryptIncomingMessage(context.Background(), uc, imapadapter.Message{ID: "7", Sender: "bob@example.com", SentTo: "alice@example.com", Subject: "secret subject", Body: "secret body"}); err != nil {
				t.Fatal(err)
			}
			start := bytes.Index(mailbox.cipher, []byte("-----BEGIN PGP MESSAGE-----"))
			if start < 0 {
				t.Fatal("no ciphertext")
			}
			end := bytes.Index(mailbox.cipher[start:], []byte("-----END PGP MESSAGE-----")) + len("-----END PGP MESSAGE-----")
			raw, err := armor.Unarmor(string(mailbox.cipher[start : start+end]))
			if err != nil {
				t.Fatal(err)
			}
			reader := bytes.NewReader(raw)
			pk, err := packet.Read(reader)
			if err != nil {
				t.Fatal(err)
			}
			want := 3
			if capable {
				want = 6
			}
			key, ok := pk.(*packet.EncryptedKey)
			if !ok || key.Version != want {
				t.Fatalf("PKESK = %#v; want v%d", pk, want)
			}
			data, err := packet.Read(reader)
			if err != nil {
				t.Fatal(err)
			}
			encrypted, ok := data.(*packet.SymmetricallyEncrypted)
			expected := 1
			if capable {
				expected = 2
			}
			if !ok || encrypted.Version != expected {
				t.Fatalf("data = %#v", data)
			}
		})
	}
}
