package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/syncauth"

	"github.com/Busnes-app/kypost-server/backend/internal/suitepair"
)

// Every save tells KyMessages whether the user is out of office and for when,
// signed as kypost.presence.ooo: never the reply text, and nothing while
// KyMessages is unpaired.
func TestOutOfOfficeSyncsKyMessages(t *testing.T) {
	f := newKyCalFixture(t)
	var got []string
	msgs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h := syncauth.FromRequest(r)
		if _, err := syncauth.Verify(chatSecret, h, body, syncauth.Options{}); err != nil || h.EventType != kymessageEventOOO || r.URL.Path != kymessagePathOOO {
			t.Errorf("KyMessages got %s %s: %v", r.URL.Path, h.EventType, err)
		}
		got = append(got, string(body))
		_, _ = w.Write([]byte(`{"outcome":"applied"}`))
	}))
	defer msgs.Close()
	put := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/mail/out-of-office", strings.NewReader(body))
		authRequestAs(f.srv, req, f.userID)
		rec := httptest.NewRecorder()
		f.srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %d %s", rec.Code, rec.Body.String())
		}
	}
	on := `{"enabled":true,"start":"2026-12-01T05:00:00Z","end":"2026-12-05T05:00:00Z","timeZone":"America/New_York","localMessage":"Gone fishing","remote":"on","remoteMessage":"Away from my desk"}`
	put(on)
	if len(got) != 0 {
		t.Fatalf("sent while unpaired: %v", got)
	}
	base, _ := url.Parse(msgs.URL)
	f.srv.SetKyMessage(suitepair.New(KyMessagePeer, base, chatSecret, msgs.Client().Transport))

	put(on)
	var b map[string]any
	if len(got) != 1 || json.Unmarshal([]byte(got[0]), &b) != nil {
		t.Fatalf("sent %v", got)
	}
	if b["sub"] != "sub-1" || b["issuer"] != kycalIssuer || b["away"] != true || b["start"] != "2026-12-01T05:00:00Z" ||
		b["end"] != "2026-12-05T05:00:00Z" || b["revision"].(float64) <= 0 || len(b) != 6 {
		t.Fatalf("sent %v", b)
	}
	if strings.Contains(got[0], "fishing") || strings.Contains(got[0], "desk") {
		t.Fatalf("the reply text left KyPost: %s", got[0])
	}
	// No end date, or the reply off: away false and nothing else.
	for _, body := range []string{
		`{"enabled":true,"start":"2026-12-01T09:00:00Z","localMessage":"Away"}`,
		`{"enabled":false,"start":"2026-12-01T09:00:00Z","end":"2026-12-02T09:00:00Z","localMessage":"Away"}`,
	} {
		put(body)
		var off map[string]any
		_ = json.Unmarshal([]byte(got[len(got)-1]), &off)
		if off["away"] != false || off["start"] != nil || len(off) != 4 {
			t.Fatalf("%s sent %v", body, off)
		}
	}
}
