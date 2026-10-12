//go:build linux

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// A due native job enters the outbox only then, bound to the relay
// generation current at that moment, and a canceled one lands in native Drafts.
func TestScheduledNativeSendQueuesWithCurrentRelay(t *testing.T) {
	s := newNativeRuntimeServer(t)
	directoryStatus(t, postDirectory(t, s, testSyncKey, "user.created", "scheduled-create", 1, runtimeDirectoryUser(true)))
	u, err := s.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	relayPath, keyPath := filepath.Join(s.configDir, "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key")
	saveRelay := func() mailmsg.DomainRelay {
		if _, err := mailmsg.SaveDomainRelay(context.Background(), relayPath, keyPath, mailmsg.DomainRelay{Domains: []string{"example.test"}, Issuer: "https://idp.example", Host: "127.0.0.1", Port: 1, Username: "operator-login", Password: "operator-secret"}); err != nil {
			t.Fatal(err)
		}
		relay, _, err := mailmsg.ReadDomainRelay(relayPath, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		return relay
	}
	scheduledRelay := saveRelay()
	at := sendTime()
	schedule := func() string {
		body, _ := json.Marshal(map[string]any{"to": "visible@outside.test", "subject": "native later", "body": "native body", "sendAt": at.Format(time.RFC3339)})
		r := httptest.NewRequest("POST", "/api/mail/send", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		authRequestAs(s, r, u.ID)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return scheduledID(t, w)
	}
	id, canceled := schedule(), schedule()

	a, _, err := s.nativeMailAssignment(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := mailbox.OpenExisting(filepath.Join(s.userStateDir(u.ID), "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	key, err := cryptutil.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := box.ReadOutbound(context.Background(), key, id); err == nil {
		t.Fatal("scheduled job entered the outbox before it was due")
	}

	r := httptest.NewRequest("POST", "/api/scheduled/"+canceled+"/cancel", nil)
	authRequestAs(s, r, u.ID)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if drafts, err := box.List(context.Background(), "Drafts", 0, 10); err != nil || len(drafts) != 1 {
		t.Fatal("canceled native mail not in Drafts", drafts, err)
	}

	current := saveRelay()
	if current.Generation == scheduledRelay.Generation {
		t.Fatal("relay generation did not rotate")
	}
	s.sweepScheduledSends(context.Background(), at)
	row, _, _ := s.globalStore.GetScheduledSend(context.Background(), id)
	if row.Status != "queued" {
		t.Fatalf("native scheduled send: %+v", row)
	}
	job, statuses, _, err := box.ReadOutbound(context.Background(), key, id)
	if err != nil || len(statuses) != 1 || statuses[0].State != "queued" || job.RelayGeneration != current.Generation {
		t.Fatalf("outbox job %v %v relay %q, want %q", statuses, err, job.RelayGeneration, current.Generation)
	}
}
