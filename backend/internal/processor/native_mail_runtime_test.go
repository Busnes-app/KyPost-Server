//go:build linux

package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	imapadapter "github.com/Busnes-app/kypost-server/backend/internal/adapters/imap"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestNativeRuntimePollsWithoutIMAP(t *testing.T) {
	p, _ := newTickTestPoller(t, nil)
	p.EnableNativeMail()
	p.newMailClient = func(string, string) imapadapter.Client {
		t.Error("native polling constructed an IMAP client")
		return nil
	}
	const issuer = "https://identity.example.test"
	if err := sso.NewStore(p.configDir).Save(sso.SSOSettings{Enabled: true, IssuerURL: issuer, ClientID: "kypost"}); err != nil {
		t.Fatal(err)
	}
	life := sso.NewLifecycleStore(p.configDir)
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"runtime","externalId":"runtime","userName":"runtime","active":true,"emails":[{"value":"runtime@example.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "runtime-create", Type: "user.created", At: time.Now()}
	if _, err := life.ApplyDirectoryUser(issuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	domains := sso.NewNativeDomainStore(p.configDir)
	if _, err := domains.Configure(context.Background(), "example.test", issuer); err != nil {
		t.Fatal(err)
	}
	domains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := domains.Read()
		return []string{d.RecordValue()}, err
	})
	u, err := life.AllocateNativeAccount(context.Background(), p.stateDir, issuer, "runtime", domains, p.users, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 4 << 20, Records: 100})
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultUserSettings()
	settings.Labels.AutoApplyEnabled = false
	if err := config.SaveUserSettings(p.userSettingsPath(u.ID), settings); err != nil {
		t.Fatal(err)
	}
	a, _, err := p.nativeMailAssignment(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(p.userStateDir(u.ID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.Append(context.Background(), "INBOX", bytes.NewBufferString("From: sender@outside.test\r\nTo: runtime@example.test\r\nSubject: direct runtime\r\n\r\nnative body"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.userIMAPConfigPath(u.ID)); !os.IsNotExist(err) {
		t.Fatal("native fixture has IMAP credentials")
	}
	p.tickSem = make(chan struct{}, 1)
	p.tickSem <- struct{}{}
	p.tick()
	state := tickStore(t, p, u)
	if decisions := state.Decisions(10); len(decisions) != 1 || decisions[0].MessageID != strconv.FormatInt(id, 10) || decisions[0].Status != "applied" {
		t.Fatalf("native poll did not process message: %+v", decisions)
	}
	// Leftover credentials and a publishable key must not start a legacy
	// self-probe or create an alias before the domain relay exists.
	p.imapKeyPath = filepath.Join(t.TempDir(), "imap.key")
	writeTestIMAPConfig(t, p.configDir, p.imapKeyPath, u.ID, a.Address, "unused")
	seedPollerPGPIdentity(t, p, u.ID, "Native runtime", a.Address)
	sent := stubSelfProbeSender(t, nil)
	p.ensureOwnAddressProven(u.ID)
	if len(*sent) != 0 {
		t.Fatal("native own-address probe used legacy SMTP")
	}
	aliases, err := p.userSendAsStore(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := aliases.List(); err != nil || len(list) != 0 {
		t.Fatalf("native own-address probe created a pending alias: %+v %v", list, err)
	}
	held, err := p.mailClientForUser(u.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.users.Deactivate(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := held.ListOverviews(context.Background(), "INBOX", 10); err == nil {
		t.Fatal("held daemon client retained access after deactivation")
	}
}
