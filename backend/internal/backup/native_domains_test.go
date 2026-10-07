//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func TestNativeRetiredRelayDomainKeepsHistoricalJobsValid(t *testing.T) {
	ctx := context.Background()
	s, u := nativeService(t)
	domains := sso.NewNativeDomainStore(s.dirs.Config)
	if _, err := domains.ConfigureDomain(ctx, "second.test", u.NativeMailboxIssuer); err != nil {
		t.Fatal(err)
	}
	a, _, err := sso.NewLifecycleStore(s.dirs.Config).NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	configPath, keyPath := filepath.Join(s.dirs.Config, "native-relay.json"), filepath.Join(s.dirs.Secret, "native-relay.key")
	relay, err := mailmsg.SaveDomainRelay(ctx, configPath, keyPath, mailmsg.DomainRelay{Domains: []string{"example.test", "second.test"}, Issuer: u.NativeMailboxIssuer, Host: "smtp.provider.test", Port: 465, Username: "operator-login", Password: "operator-secret"})
	if err != nil {
		t.Fatal(err)
	}
	master, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := mailbox.OpenExisting(filepath.Join(s.dirs.State, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "07a7cd86-6d9d-4b91-890d-c6d78d096097"
	raw := []byte("From: one@second.test\r\nTo: receiver@outside.test\r\nSubject: historical\r\n\r\nbody\r\n")
	if err = store.QueueOutbound(ctx, master, id, mailbox.OutboundJob{From: "one@second.test", RelayGeneration: relay.Generation, Deliveries: []mailbox.OutboundDelivery{{Recipients: []string{"receiver@outside.test"}, Raw: raw}}}); err != nil {
		t.Fatal(err)
	}
	_, claim, err := store.ClaimOutbound(ctx, master, id, 0, relay.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteOutbound(ctx, id, 0, claim, &textproto.Error{Code: 550, Msg: "permanent rejection"}); err != nil {
		t.Fatal(err)
	}
	if _, err = mailmsg.SetDomainRelayDomains(ctx, configPath, keyPath, []string{"example.test"}); err != nil {
		t.Fatal(err)
	}
	if err = domains.RetireDomain(ctx, "second.test", s.dirs.State, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collect(); err != nil {
		t.Fatal("historical job on a retired relay domain refused", err)
	}
	// Re-adding the domain keeps the snapshot and the historical job valid,
	// on and off the relay; then retire it again.
	if _, err = domains.ConfigureDomain(ctx, "second.test", u.NativeMailboxIssuer); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collect(); err != nil {
		t.Fatal("snapshot after re-add refused", err)
	}
	readded, err := mailmsg.SetDomainRelayDomains(ctx, configPath, keyPath, []string{"example.test", "second.test"})
	if err != nil || len(readded.RetiredDomains) != 0 || readded.Generation != relay.Generation {
		t.Fatal("relay re-add", readded, err)
	}
	if _, err = s.Collect(); err != nil {
		t.Fatal("snapshot with the domain back on the relay refused", err)
	}
	if _, err = mailmsg.SetDomainRelayDomains(ctx, configPath, keyPath, []string{"example.test"}); err != nil {
		t.Fatal(err)
	}
	if err = domains.RetireDomain(ctx, "second.test", s.dirs.State, keyPath); err != nil {
		t.Fatal(err)
	}
	// The relay may not still send for a retired domain.
	if _, err = mailmsg.SetDomainRelayDomains(ctx, configPath, keyPath, []string{"example.test", "second.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collect(); err == nil {
		t.Fatal("live relay domain that is retired accepted")
	}
	if _, err = mailmsg.SetDomainRelayDomains(ctx, configPath, keyPath, []string{"example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collect(); err != nil {
		t.Fatal(err)
	}
	// A relay domain outside the domain set's history is refused.
	path := filepath.Join(s.dirs.Config, sso.NativeDomainsFile)
	var set map[string]any
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &set) != nil {
		t.Fatal(err)
	}
	set["retired"] = []string{}
	if data, err = json.Marshal(set); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Collect(); err == nil {
		t.Fatal("relay domain outside the domain set accepted")
	}
}
