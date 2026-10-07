//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/syncauth"
	"github.com/Busnes-app/kypost-server/backend/internal/cfreceiving"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// fakeWorker implements the continuous wire contract in Go: bearer by
// SHA-256, signed tables and rotations verified with ed25519 over exactly
// context || payload, revision/age bounds, digest-checked delete.
type fakeWorker struct {
	t     *testing.T
	mu    sync.Mutex
	now   func() time.Time
	epoch int64
	token string
	pub   ed25519.PublicKey
	body  []byte
	table fakeTable
	puts  []fakeTable
	inbox map[string]fakeObject
	calls []fakeCall
	// abort names "METHOD kind" requests to perform and then drop unanswered.
	abort    map[string]bool
	onDelete func(key string)
	// emptyTruncated answers the next listing with no items and truncated.
	emptyTruncated bool
	server         *httptest.Server
}

type fakeObject struct{ raw, envelope []byte }

type fakeCall struct {
	bearer, method, kind string
	status               int
}

type fakeRoute struct {
	Address    string `json:"address"`
	Generation int64  `json:"generation"`
	MaxBytes   int64  `json:"maxBytes"`
}

type fakeTable struct {
	Revision       int64             `json:"revision"`
	IssuedAt       int64             `json:"issuedAt"`
	Routes         []fakeRoute       `json:"routes"`
	BlockedSenders []json.RawMessage `json:"blockedSenders"`
}

func newFakeWorker(t *testing.T, m cfreceiving.Material, now func() time.Time) *fakeWorker {
	w := &fakeWorker{t: t, now: now, inbox: map[string]fakeObject{}, abort: map[string]bool{}}
	w.install(m)
	w.server = httptest.NewTLSServer(w)
	t.Cleanup(w.server.Close)
	return w
}

func (w *fakeWorker) install(m cfreceiving.Material) {
	public, _ := base64.StdEncoding.DecodeString(m.PublicKey())
	w.epoch, w.token, w.pub = m.Epoch, m.TokenSHA256(), public
}

func (w *fakeWorker) client() cfreceiving.Client {
	c := w.server.Client()
	c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return cfreceiving.Client{Origin: w.server.URL, HTTP: c}
}

func strictJSON(raw []byte, target any, keys ...string) bool {
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) != nil || len(shape) != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := shape[k]; !ok {
			return false
		}
	}
	return strictCloudflareJSON(raw, target) == nil
}

// signed checks a contract document and returns its payload string.
func (w *fakeWorker) signed(body []byte, field, context string) (string, int) {
	var doc map[string]string
	if !strictJSON(body, &doc, field, "signature") {
		return "", 400
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(doc["signature"])
	if err != nil || len(signature) != 64 {
		return "", 400
	}
	if !ed25519.Verify(w.pub, []byte(context+doc[field]), signature) {
		return "", 403
	}
	return doc[field], 0
}

func (w *fakeWorker) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	bearer, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	sum := sha256.Sum256([]byte(bearer))
	who := hex.EncodeToString(sum[:])
	kind := req.URL.Path
	if strings.HasPrefix(kind, "/messages/") {
		kind = "/messages/<id>"
	}
	status, header, body := 401, http.Header{}, []byte(nil)
	if len(bearer) == 64 && who == w.token {
		status, body = w.route(req, header)
	}
	w.calls = append(w.calls, fakeCall{who, req.Method, kind, status})
	if w.abort[req.Method+" "+kind] && status < 300 {
		delete(w.abort, req.Method+" "+kind)
		panic(http.ErrAbortHandler)
	}
	for k, v := range header {
		rw.Header()[k] = v
	}
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(status)
	_, _ = rw.Write(body)
}

func (w *fakeWorker) route(req *http.Request, header http.Header) (int, []byte) {
	body, _ := io.ReadAll(io.LimitReader(req.Body, 3<<20))
	q := req.URL.Query()
	id, item := strings.CutPrefix(req.URL.Path, "/messages/")
	switch {
	case req.URL.Path == "/messages" && req.Method == "GET" && w.emptyTruncated:
		w.emptyTruncated = false
		return 200, []byte(`{"messages":[],"truncated":true}`)
	case req.URL.Path == "/messages" && req.Method == "GET":
		limit, _ := strconv.Atoi(q.Get("limit"))
		keys := []string{}
		for k := range w.inbox {
			if k > q.Get("after") {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		page := struct {
			Messages  []cfreceiving.Item `json:"messages"`
			Truncated bool               `json:"truncated"`
		}{[]cfreceiving.Item{}, len(keys) > limit}
		for _, k := range keys[:min(limit, len(keys))] {
			sum := sha256.Sum256(w.inbox[k].raw)
			page.Messages = append(page.Messages, cfreceiving.Item{Key: k, Size: int64(len(w.inbox[k].raw)), Digest: hex.EncodeToString(sum[:])})
		}
		raw, _ := json.Marshal(page)
		return 200, raw
	case item && req.Method == "GET":
		o, ok := w.inbox[id]
		if !ok {
			return 404, nil
		}
		header.Set("X-KyPost-Envelope", base64.RawURLEncoding.EncodeToString(o.envelope))
		return 200, o.raw
	case item && req.Method == "DELETE":
		o, ok := w.inbox[id]
		if !ok {
			return 204, nil
		}
		sum := sha256.Sum256(o.raw)
		if q.Get("digest") != hex.EncodeToString(sum[:]) {
			return 409, nil
		}
		if w.onDelete != nil {
			w.onDelete(id)
		}
		delete(w.inbox, id)
		return 204, nil
	case req.URL.Path == "/routes" && req.Method == "GET":
		if w.body == nil {
			return 404, nil
		}
		return 200, w.body
	case req.URL.Path == "/routes" && req.Method == "PUT":
		payload, status := w.signed(body, "table", "kypost-cf-routes/1\n")
		var table fakeTable
		if status != 0 {
			return status, nil
		}
		if !strictJSON([]byte(payload), &table, "revision", "issuedAt", "routes", "blockedSenders") || table.Routes == nil || table.BlockedSenders == nil {
			return 400, nil
		}
		for _, r := range table.Routes {
			raw, _ := json.Marshal(r)
			if !strictJSON(raw, new(fakeRoute), "address", "generation", "maxBytes") || !cfreceiving.ValidAddress(r.Address) || r.Generation < 1 || r.MaxBytes < 1 || r.MaxBytes > 25<<20 {
				return 400, nil
			}
		}
		now := w.now().UnixMilli()
		if table.Revision > now+300_000 || table.IssuedAt > now+300_000 || now-table.IssuedAt > 14*86400_000 {
			return 422, nil
		}
		if table.Revision <= w.table.Revision {
			return 409, nil
		}
		w.table, w.body = table, body
		w.puts = append(w.puts, table)
		return 204, nil
	case req.URL.Path == "/rotate" && req.Method == "POST":
		payload, status := w.signed(body, "rotation", "kypost-cf-rotate/1\n")
		if status != 0 {
			return status, nil
		}
		var next struct {
			Epoch       int64  `json:"epoch"`
			TokenSHA256 string `json:"tokenSha256"`
			PublicKey   string `json:"publicKey"`
		}
		if !strictJSON([]byte(payload), &next, "epoch", "tokenSha256", "publicKey") {
			return 400, nil
		}
		public, err := base64.StdEncoding.DecodeString(next.PublicKey)
		if err != nil || len(public) != 32 || next.TokenSHA256 == w.token {
			return 400, nil
		}
		if next.Epoch != w.epoch+1 {
			return 409, nil
		}
		w.epoch, w.token, w.pub = next.Epoch, next.TokenSHA256, public
		return 204, nil
	}
	return 404, nil
}

func uuidv7(at time.Time) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	ms := at.UnixMilli()
	for i := 5; i >= 0; i, ms = i-1, ms>>8 {
		b[i] = byte(ms)
	}
	b[6], b[8] = 0x70|b[6]&0x0f, 0x80|b[8]&0x3f
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// capture mimics the email handler for an address the current table routes.
func (w *fakeWorker) capture(recipient, sender, body string, mutate func(*cfreceiving.Envelope)) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := slices.IndexFunc(w.table.Routes, func(r fakeRoute) bool { return r.Address == recipient })
	if i < 0 {
		w.t.Fatal("Worker would reject", recipient)
	}
	raw := []byte("From: sender@outside.test\r\nSubject: test\r\n\r\n" + body + "\r\n")
	sum := sha256.Sum256(raw)
	now := w.now()
	env := cfreceiving.Envelope{ID: uuidv7(now), Sender: sender, Recipient: recipient, Generation: w.table.Routes[i].Generation, TableRevision: w.table.Revision, CapturedAt: now.UnixMilli(), Size: int64(len(raw)), Digest: hex.EncodeToString(sum[:])}
	if mutate != nil {
		mutate(&env)
	}
	envelope, _ := json.Marshal(env)
	w.inbox[env.ID] = fakeObject{raw, envelope}
	return env.ID
}

func (w *fakeWorker) object(key string) (fakeObject, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o, ok := w.inbox[key]
	return o, ok
}

func (w *fakeWorker) put(key string, o fakeObject) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inbox[key] = o
}

func (w *fakeWorker) waiting() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.inbox)
}

func (w *fakeWorker) count(match func(fakeCall) bool) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.calls {
		if match(c) {
			n++
		}
	}
	return n
}

func (w *fakeWorker) set(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
}

func (w *fakeWorker) epochToken() (int64, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.epoch, w.token
}

func (w *fakeWorker) putCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.puts)
}

func (w *fakeWorker) lastPut() fakeTable {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.table
}

type cfClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *cfClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *cfClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type cfEnv struct {
	receiving string
	r         *receivingRuntime
	db        *cfreceiving.DB
	keys      cfreceiving.Keys
	w         *fakeWorker
	l         *cfLoop
	clock     *cfClock
	scan      *atomic.Value
	users     []users.User
}

// cfScanner serves the fixed local Rspamd endpoint with a settable action;
// "fail" answers 503.
func cfScanner(t *testing.T) *atomic.Value {
	t.Helper()
	lockReceivingRspamdProof(t)
	t.Setenv("KYPOST_RECEIVING_RSPAMD", "true")
	action := &atomic.Value{}
	action.Store("no action")
	listener, err := net.Listen("tcp", "127.0.0.1:11333")
	if err != nil {
		t.Fatal(err)
	}
	scanner := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Settings") != `{"groups_disabled":["spf","dmarc","arc"]}` || req.Header.Get("IP") != "" || req.Header.Get("Rcpt") == "" || req.Header.Get("Queue-ID") == "" {
			t.Error("scanner request lacks the fixed Cloudflare settings or frozen envelope")
		}
		_, _ = io.Copy(io.Discard, req.Body)
		if action.Load() == "fail" {
			w.WriteHeader(503)
			return
		}
		_, _ = fmt.Fprintf(w, `{"action":%q,"is_skipped":false}`, action.Load())
	}))
	scanner.Listener = listener
	scanner.Start()
	t.Cleanup(scanner.Close)
	return action
}

func cfFixture(t *testing.T) *cfEnv {
	t.Helper()
	r, created := receivingFixture(t)
	r.gateway = cfGateway
	db, err := cfreceiving.Open(filepath.Join(r.stateDir, "receiving"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	keys := cfreceiving.Keys{Dir: t.TempDir()}
	m, err := keys.Init()
	if err != nil {
		t.Fatal(err)
	}
	clock := &cfClock{at: time.Now()}
	e := &cfEnv{receiving: filepath.Join(r.stateDir, "receiving"), r: r, db: db, keys: keys, w: newFakeWorker(t, m, clock.now), clock: clock, scan: cfScanner(t), users: created}
	e.l = e.restart()
	return e
}

// restart is a new process over the same durable state.
func (e *cfEnv) restart() *cfLoop {
	l := newCFLoop(e.r, e.db, e.w.server.URL)
	l.keys, l.client, l.now = e.keys, e.w.client(), e.clock.now
	return l
}

func (e *cfEnv) cycle(t *testing.T) {
	t.Helper()
	if err := e.l.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// cfFolder lists message bodies in a user's folder.
func cfFolder(t *testing.T, r *receivingRuntime, u users.User, folder string) []string {
	t.Helper()
	a, _, err := r.life.NativeAssignment(u.NativeMailboxIssuer, u.SSOSub)
	if err != nil {
		t.Fatal(err)
	}
	s, err := mailbox.OpenExisting(filepath.Join(r.stateDir, "users", u.ID, "mailbox"), a.Owner, a.Limits, a.Source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.List(context.Background(), folder, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, row := range rows {
		raw, err := s.Raw(context.Background(), folder, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(string(raw), "\r\n\r\n")
		out = append(out, strings.TrimSpace(body))
	}
	slices.Sort(out)
	return out
}

func cfStatus(t *testing.T, e *cfEnv) cfreceiving.Status {
	t.Helper()
	s, err := cfreceiving.CurrentStatus(context.Background(), e.keys, e.receiving)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCloudflareContinuousPublishPickupDelete(t *testing.T) {
	e := cfFixture(t)
	e.cycle(t)
	table := e.w.lastPut()
	if len(table.Routes) != 2 || table.Routes[0] != (fakeRoute{"one@example.test", 1, 4 << 20}) || table.Routes[1].Address != "two@example.test" || len(table.BlockedSenders) != 0 {
		t.Fatalf("published table %+v", table)
	}
	// One recipient per item, two owners.
	e.w.capture("one@example.test", "a@outside.test", "first", nil)
	e.w.capture("two@example.test", "", "second", nil)
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[0], "INBOX"); !slices.Equal(got, []string{"first"}) {
		t.Fatal("first owner", got)
	}
	if got := cfFolder(t, e.r, e.users[1], "INBOX"); !slices.Equal(got, []string{"second"}) {
		t.Fatal("second owner", got)
	}
	if e.w.waiting() != 0 {
		t.Fatal("provider copies not deleted after commit")
	}
	s := cfStatus(t, e)
	if s.State != "running" || s.Waiting != 0 || s.LastRevision != table.Revision || s.LastPickupAt == 0 || len(s.Ledger) != 0 {
		t.Fatalf("status %+v", s)
	}
	// Unchanged directory: no new table inside the hour.
	puts := e.w.putCount()
	e.cycle(t)
	if e.w.putCount() != puts {
		t.Fatal("unchanged table republished")
	}
}

var errCrash = errors.New("simulated crash")

func TestCloudflareContinuousCrashAtEveryBoundary(t *testing.T) {
	for _, point := range []string{"recorded", "fetched", "bound", "junk", "accepted", "imported", "delete", "lost-delete"} {
		t.Run(point, func(t *testing.T) {
			e := cfFixture(t)
			ctx := context.Background()
			crashed := false
			e.l.crash = func(p string) error {
				if p == point && !crashed {
					crashed = true
					return errCrash
				}
				return nil
			}
			if point == "recorded" {
				_ = e.l.cycle(ctx)
				if e.w.lastPut().Revision != 0 {
					t.Fatal("crash before PUT still installed")
				}
				recorded, _ := e.db.LastRevision(ctx)
				e.l = e.restart()
				e.cycle(t)
				if got := e.w.lastPut().Revision; got <= recorded {
					t.Fatal("revision did not pass the recorded one", got, recorded)
				}
			} else {
				e.cycle(t)
			}
			folder := "INBOX"
			if point == "junk" {
				folder = "Junk"
				e.scan.Store("reject")
			}
			if point == "lost-delete" {
				e.w.set(func() { e.w.abort["DELETE /messages/<id>"] = true })
			}
			e.w.set(func() {
				e.w.onDelete = func(key string) {
					if d, err := e.r.holding.Get(ctx, cfGateway, key); err != nil || d.State != "archived" {
						t.Error("provider delete before a durable local record", d.State, err)
					}
				}
			})
			e.w.capture("one@example.test", "a@outside.test", "once", nil)
			_ = e.l.cycle(ctx)
			if point != "recorded" && point != "lost-delete" && !crashed {
				t.Fatal("boundary never reached")
			}
			// The junk verdict must survive on its own: the scanner now allows.
			e.scan.Store("no action")
			e.l = e.restart()
			e.cycle(t)
			e.cycle(t)
			if got := cfFolder(t, e.r, e.users[0], folder); !slices.Equal(got, []string{"once"}) {
				t.Fatal("lost or duplicated", folder, got)
			}
			other := map[string]string{"INBOX": "Junk", "Junk": "INBOX"}[folder]
			if got := cfFolder(t, e.r, e.users[0], other); len(got) != 0 {
				t.Fatal("filed twice", got)
			}
			if e.w.waiting() != 0 || len(cfStatus(t, e).Ledger) != 0 {
				t.Fatal("provider copy or ledger left behind", e.w.waiting(), cfStatus(t, e).Ledger)
			}
		})
	}
}

func TestCloudflareContinuousReplayConflictAndFrozenBindings(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	e.cycle(t)
	key := e.w.capture("one@example.test", "a@outside.test", "once", nil)
	original, _ := e.w.object(key)
	e.cycle(t)
	// Exact replay of a committed key: no second copy, deleted again.
	e.w.put(key, original)
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[0], "INBOX"); !slices.Equal(got, []string{"once"}) || e.w.waiting() != 0 {
		t.Fatal("replay", got, e.w.waiting())
	}
	// Same key, other bytes: kept at the provider, never fetched again.
	changed := fakeObject{raw: []byte("Subject: other\r\n\r\nother\r\n"), envelope: original.envelope}
	e.w.put(key, changed)
	e.cycle(t)
	fetches := e.w.count(func(c fakeCall) bool { return c.kind == "/messages/<id>" })
	e.cycle(t)
	if s := cfStatus(t, e); s.Refused != 1 || s.Waiting != 0 || s.OldestUnpickedAt != 0 {
		t.Fatalf("refused object counted as waiting: %+v", s)
	}
	if _, kept := e.w.object(key); !kept || cfStatus(t, e).Ledger["refused"] != 1 || e.w.count(func(c fakeCall) bool { return c.kind == "/messages/<id>" }) != fetches {
		t.Fatal("conflicting provider object deleted or refetched")
	}
	e.w.set(func() { delete(e.w.inbox, key) }) // the operator removes it at the provider
	e.cycle(t)
	if len(cfStatus(t, e).Ledger) != 0 {
		t.Fatal("refused row outlived its provider object")
	}
	// Generation change while mail waits: quarantined to the frozen owner.
	one, two := e.users[0], e.users[1]
	if _, err := e.r.life.AddNativeAlias(ctx, e.r.stateDir, one.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	frozen := e.w.capture("sales@example.test", "", "frozen", nil)
	if _, err := e.r.life.ReleaseNativeAlias(ctx, e.r.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.life.ReassignNativeAddress(ctx, e.r.stateDir, "sales@example.test", two.ID); err != nil {
		t.Fatal(err)
	}
	crashed := false
	e.l.crash = func(p string) error {
		if p == "quarantined" && !crashed {
			crashed = true
			return errCrash
		}
		return nil
	}
	_ = e.l.cycle(ctx)
	e.l = e.restart()
	e.cycle(t)
	d, err := e.r.holding.Get(ctx, cfGateway, frozen)
	if !crashed || err != nil || d.State != "quarantined" || len(d.Raw) == 0 || d.Bindings[0].Mailbox != one.ID || d.Bindings[0].Generation != 1 {
		t.Fatalf("old generation not quarantined with bytes: %+v %v", d.Bindings, err)
	}
	if _, kept := e.w.object(frozen); kept || len(cfFolder(t, e.r, two, "INBOX")) != 0 {
		t.Fatal("quarantined copy kept at provider or delivered to the new owner")
	}
	if i := slices.IndexFunc(e.w.lastPut().Routes, func(r fakeRoute) bool { return r.Address == "sales@example.test" }); i < 0 || e.w.lastPut().Routes[i].Generation != 3 {
		t.Fatal("reassigned address not republished at its new generation")
	}
	// A table revision this instance never recorded: quarantined, no owner guessed.
	unknown := e.w.capture("one@example.test", "", "unknown", func(env *cfreceiving.Envelope) { env.TableRevision = 12345 })
	e.cycle(t)
	d, err = e.r.holding.Get(ctx, cfGateway, unknown)
	if err != nil || d.State != "quarantined" || d.Bindings[0].Mailbox != "" || d.Bindings[0].Address != "one@example.test" {
		t.Fatalf("unknown revision not quarantined unresolved: %+v %v", d.Bindings, err)
	}
	if err := e.r.life.ReleaseQuarantined(ctx, e.r.stateDir, "https://identity.example.test", e.r.accounts, e.r.holding, cfGateway, unknown, false); err == nil {
		t.Fatal("unresolved owner released")
	}
	if got := cfFolder(t, e.r, one, "INBOX"); !slices.Equal(got, []string{"once"}) {
		t.Fatal("quarantined mail delivered", got)
	}
}

func TestCloudflareContinuousSpamScannerAndCapacity(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	e.cycle(t)
	e.scan.Store("reject")
	e.w.capture("one@example.test", "a@outside.test", "spam", nil)
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[0], "Junk"); !slices.Equal(got, []string{"spam"}) || len(cfFolder(t, e.r, e.users[0], "INBOX")) != 0 || e.w.waiting() != 0 {
		t.Fatal("rejected mail not filed to Junk", got)
	}
	for _, action := range []string{"fail", "soft reject"} {
		e.scan.Store(action)
		key := e.w.capture("one@example.test", "a@outside.test", "later-"+action, nil)
		e.cycle(t)
		if d, err := e.r.holding.Get(ctx, cfGateway, key); err != nil || d.State != "staged" || len(d.Raw) != 0 {
			t.Fatal(action, "accepted bytes without a verdict", err)
		}
		if _, kept := e.w.object(key); !kept {
			t.Fatal(action, "provider copy deleted before commit")
		}
	}
	e.scan.Store("no action")
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[0], "INBOX"); len(got) != 0 {
		t.Fatal("backed-off mail refetched before its delay", got)
	}
	e.clock.advance(2 * time.Minute)
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[0], "INBOX"); !slices.Equal(got, []string{"later-fail", "later-soft reject"}) || e.w.waiting() != 0 {
		t.Fatal("retried mail", got)
	}

	// Capacity: a one-record holding store full of quarantine stops fetching.
	dir := filepath.Join(t.TempDir(), "small")
	small, err := ingress.Open(dir, ingress.Limits{MessageBytes: receivingLimits.MessageBytes, PayloadBytes: receivingLimits.PayloadBytes, Records: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	full := *e.r
	full.holding = small
	e.l.r = &full
	e.w.capture("one@example.test", "", "fills", func(env *cfreceiving.Envelope) { env.TableRevision = 7 })
	e.cycle(t)
	e.w.capture("one@example.test", "", "waits-1", nil)
	e.w.capture("one@example.test", "", "waits-2", nil)
	fetches := e.w.count(func(c fakeCall) bool { return c.kind == "/messages/<id>" })
	e.cycle(t)
	s := cfStatus(t, e)
	if got := e.w.count(func(c fakeCall) bool { return c.kind == "/messages/<id>" }) - fetches; got != 1 || e.w.waiting() != 2 || s.State != "error" || s.Waiting != 2 || !strings.Contains(s.Detail, "receiving store is full") {
		t.Fatalf("capacity refusal kept fetching: %d fetches, status %+v", got, s)
	}
}

// copyFiles copies the named files (and SQLite sidecars) between directories.
func copyFiles(t *testing.T, from, to string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			raw, err := os.ReadFile(filepath.Join(from, name+suffix))
			if errors.Is(err, os.ErrNotExist) && suffix != "" {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(to, name+suffix), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Restore starts fenced; only a confirmed takeover moves receiving.
func TestCloudflareContinuousRestoreFencedAndTakeover(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	e := cfFixture(t)
	ctx := context.Background()
	original, _, _, err := e.keys.Load()
	if err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	e.w.capture("one@example.test", "", "m1", nil)
	e.cycle(t)

	// The backup: sealed credentials (never the host record).
	restored := &cfEnv{r: new(receivingRuntime), keys: cfreceiving.Keys{Dir: t.TempDir()}, w: e.w, clock: e.clock, users: e.users}
	restored.receiving = filepath.Join(restored.keys.Dir, "receiving")
	t.Cleanup(func() { _ = e.db.Close() })
	copyFiles(t, e.keys.Dir, restored.keys.Dir, cfreceiving.CredentialsFile)
	*restored.r = *e.r
	snapshot := func(name string) {
		receiving := filepath.Join(e.r.stateDir, "receiving")
		if err := e.r.holding.Close(); err != nil {
			t.Fatal(err)
		}
		if err := e.db.Close(); err != nil {
			t.Fatal(err)
		}
		copyFiles(t, receiving, filepath.Join(restored.keys.Dir, "receiving"), name)
		if e.r.holding, err = ingress.OpenExisting(receiving, receivingLimits); err != nil {
			t.Fatal(err)
		}
		if e.db, err = cfreceiving.Open(receiving); err != nil {
			t.Fatal(err)
		}
		e.l.db = e.db
	}

	// Fenced: the restored instance makes no request at all.
	snapshot("ingress.db")
	snapshot(cfreceiving.DBFile)
	if restored.db, err = cfreceiving.Open(filepath.Join(restored.keys.Dir, "receiving")); err != nil {
		t.Fatal(err)
	}
	defer restored.db.Close()
	restored.l = restored.restart()
	all := func(fakeCall) bool { return true }
	before := e.w.count(all)
	if err := restored.l.cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if e.w.count(all) != before || cfStatus(t, restored).State != "fenced" {
		t.Fatal("restored instance contacted the Worker or did not report fenced")
	}
	// The original keeps list, fetch, delete and PUT.
	if _, err := e.r.life.AddNativeAlias(ctx, e.r.stateDir, e.users[0].ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	e.w.capture("one@example.test", "", "m3", nil)
	e.cycle(t)
	for _, kind := range []string{"GET /messages", "GET /messages/<id>", "DELETE /messages/<id>", "PUT /routes"} {
		if e.w.count(func(c fakeCall) bool {
			return c.method+" "+c.kind == kind && c.bearer == original.TokenSHA256() && c.status < 300
		}) == 0 {
			t.Fatal("original lost", kind)
		}
	}

	// m2: the restored holding store predates it, but its ledger says the
	// original imported it (the original crashed before deleting).
	m2 := e.w.capture("one@example.test", "", "m2", nil)
	snapshot("ingress.db")
	e.l.crash = func(p string) error {
		if p == "delete" {
			return errCrash
		}
		return nil
	}
	_ = e.l.cycle(ctx)
	snapshot(cfreceiving.DBFile)
	// The original publishes again after that backup (still crashing before
	// its delete), so the restored table record is older than the Worker's.
	if _, err := e.r.life.AddNativeAlias(ctx, e.r.stateDir, e.users[1].ID, "support@example.test"); err != nil {
		t.Fatal(err)
	}
	_ = e.l.cycle(ctx)
	e.l.crash = nil
	restoredHolding, err := ingress.OpenExisting(filepath.Join(restored.keys.Dir, "receiving"), receivingLimits)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredHolding.Close()
	restored.r.holding = restoredHolding
	if err := restored.db.Close(); err != nil {
		t.Fatal(err)
	}
	if restored.db, err = cfreceiving.Open(filepath.Join(restored.keys.Dir, "receiving")); err != nil {
		t.Fatal(err)
	}
	restored.l = restored.restart()
	if entry, found, _ := restored.db.Entry(ctx, m2); !found || entry.State != "imported" {
		t.Fatal("fixture: restored ledger should claim m2", entry)
	}

	// Takeover: rotate with the restored key; the original is fenced.
	if err := cfRotate(ctx, restored.keys, e.w.client(), false, nil); err == nil {
		t.Fatal("fenced instance rotated without takeover")
	}
	if err := cfRotate(ctx, restored.keys, e.w.client(), true, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.l.cycle(ctx); !errors.Is(err, cfreceiving.ErrUnauthorized) || cfStatus(t, e).State != "fenced" {
		t.Fatal("original did not stop and report fenced", err, cfStatus(t, e))
	}
	client := e.w.client()
	if _, _, err := client.List(ctx, original.Token, ""); !errors.Is(err, cfreceiving.ErrUnauthorized) {
		t.Fatal("original still lists")
	}
	if _, _, err := client.Fetch(ctx, original.Token, m2, 1<<20); !errors.Is(err, cfreceiving.ErrUnauthorized) {
		t.Fatal("original still fetches")
	}
	if err := client.Delete(ctx, original.Token, m2, strings.Repeat("0", 64)); !errors.Is(err, cfreceiving.ErrUnauthorized) {
		t.Fatal("original still deletes")
	}
	if err := client.PutRoutes(ctx, original.Token, []byte("{}")); !errors.Is(err, cfreceiving.ErrUnauthorized) {
		t.Fatal("original still publishes")
	}
	// A later original cycle stays quiet: fenced for good.
	before = e.w.count(all)
	if err := e.l.cycle(ctx); err != nil || e.w.count(all) != before {
		t.Fatal("fenced original kept calling", err)
	}

	// The new owner deletes m2 only after committing it itself.
	revision := e.w.lastPut().Revision
	e.w.set(func() {
		e.w.onDelete = func(key string) {
			if d, err := restoredHolding.Get(ctx, cfGateway, key); err != nil || d.State != "archived" {
				t.Error("provider delete before the new owner committed", key, d.State, err)
			}
		}
	})
	// Scanner down: the restored ledger's "imported" row alone deletes nothing.
	e.scan.Store("fail")
	if err := restored.l.cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, kept := e.w.object(m2); !kept {
		t.Fatal("provider copy deleted on the restored ledger's word")
	}
	e.scan.Store("no action")
	e.clock.advance(2 * time.Minute)
	if err := restored.l.cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if e.w.waiting() != 0 || e.w.lastPut().Revision <= revision || cfStatus(t, restored).State != "running" {
		t.Fatal("new owner did not take over", e.w.waiting(), cfStatus(t, restored))
	}
	if got := cfFolder(t, e.r, e.users[0], "INBOX"); !slices.Equal(got, []string{"m1", "m2", "m3"}) {
		t.Fatal("mail lost or duplicated across takeover", got)
	}
	current, _, _, err := restored.keys.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{original.Token, current.Token, base64.StdEncoding.EncodeToString(original.Seed), base64.StdEncoding.EncodeToString(current.Seed)} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credential logged")
		}
	}
	if !strings.Contains(logs.String(), `"result":"fenced"`) {
		t.Fatal("fence not logged")
	}
}

func TestCloudflareContinuousRotationCrashWindows(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	e.cycle(t)
	// Pending persisted, then a crash: the retry sends the same material.
	if err := cfRotate(ctx, e.keys, e.w.client(), false, func(string) error { return errCrash }); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	_, pending, live, err := e.keys.Load()
	if epoch, _ := e.w.epochToken(); err != nil || pending == nil || !live || epoch != 1 {
		t.Fatal("pending not persisted before sending", err)
	}
	if err := cfRotate(ctx, e.keys, e.w.client(), false, nil); err != nil {
		t.Fatal(err)
	}
	cur, _, live, err := e.keys.Load()
	if epoch, token := e.w.epochToken(); err != nil || !live || cur.Token != pending.Token || epoch != 2 || token != pending.TokenSHA256() {
		t.Fatal("retry invented other material", err)
	}
	// Lost response: confirmed by the pending bearer.
	e.w.set(func() { e.w.abort["POST /rotate"] = true })
	if err := cfRotate(ctx, e.keys, e.w.client(), false, nil); err != nil {
		t.Fatal(err)
	}
	cur, _, live, err = e.keys.Load()
	if _, token := e.w.epochToken(); err != nil || !live || cur.Epoch != 3 || token != cur.TokenSHA256() {
		t.Fatal("lost rotation response not confirmed", err)
	}
	// Installed, then the process died before promoting: the daemon's 401
	// path confirms it with the pending bearer instead of fencing.
	next, err := cfreceiving.NewMaterial(4)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.keys.SavePending(next); err != nil {
		t.Fatal(err)
	}
	e.w.set(func() { e.w.install(next) })
	if err := e.l.cycle(ctx); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	if cur, _, live, err := e.keys.Load(); err != nil || !live || cur.Token != next.Token || cfStatus(t, e).State != "running" {
		t.Fatal("interrupted rotation not completed", err)
	}
	e.w.capture("one@example.test", "", "after-rotations", nil)
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[0], "INBOX"); !slices.Equal(got, []string{"after-rotations"}) {
		t.Fatal(got)
	}
}

func TestCloudflareContinuousRevisionsResignAndDomains(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	const issuer = "https://identity.example.test"
	if _, err := e.r.domains.ConfigureDomain(ctx, "xn--bcher-kva.test", issuer); err != nil {
		t.Fatal(err)
	}
	lapsed := map[string]bool{}
	var mu sync.Mutex
	e.r.domains.SetLookupForTest(func(_ context.Context, name string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		set, err := e.r.domains.ReadSet()
		for _, d := range set.Domains {
			if name == d.RecordName()+"." && !lapsed[d.Domain] {
				return []string{d.RecordValue()}, err
			}
		}
		return nil, errors.New("temporary DNS failure")
	})
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"three","externalId":"three","userName":"three","active":true,"emails":[{"value":"three@xn--bcher-kva.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "create-three", Type: "user.created", At: time.Now()}
	if _, err := e.r.life.ApplyDirectoryUser(issuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.life.AllocateNativeAccount(ctx, e.r.stateDir, issuer, "three", e.r.domains, e.r.accounts, mailbox.Limits{MessageBytes: 5 << 20, PayloadBytes: 32 << 20, Records: 10000}); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	first := e.w.lastPut()
	addresses := func(tb fakeTable) []string {
		out := []string{}
		for _, r := range tb.Routes {
			out = append(out, r.Address)
		}
		return out
	}
	if !slices.Equal(addresses(first), []string{"one@example.test", "three@xn--bcher-kva.test", "two@example.test"}) || first.Revision != e.clock.now().UnixMilli() {
		t.Fatal("A-label routes or millisecond revision", addresses(first), first.Revision)
	}
	// Hourly re-sign of an unchanged table, not before.
	e.clock.advance(50 * time.Minute)
	e.cycle(t)
	if e.w.putCount() != 1 {
		t.Fatal("re-signed before the hour")
	}
	e.clock.advance(11 * time.Minute)
	e.cycle(t)
	if e.w.putCount() != 2 || e.w.lastPut().Revision <= first.Revision || !slices.Equal(addresses(e.w.lastPut()), addresses(first)) || e.w.lastPut().IssuedAt != e.clock.now().UnixMilli() {
		t.Fatal("hourly re-sign", e.w.putCount())
	}
	// A transient DNS failure never prunes; a directory change still publishes.
	mu.Lock()
	lapsed["xn--bcher-kva.test"] = true
	mu.Unlock()
	if _, err := e.r.life.AddNativeAlias(ctx, e.r.stateDir, e.users[0].ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	if !slices.Equal(addresses(e.w.lastPut()), []string{"one@example.test", "sales@example.test", "three@xn--bcher-kva.test", "two@example.test"}) {
		t.Fatal("lapsed domain pruned", addresses(e.w.lastPut()))
	}
	// Restart with the clock behind: revisions still increase.
	last := e.w.lastPut().Revision
	e.clock.advance(-3 * time.Minute)
	e.l = e.restart()
	if _, err := e.r.life.ReleaseNativeAlias(ctx, e.r.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	if e.w.lastPut().Revision != last+1 || slices.Contains(addresses(e.w.lastPut()), "sales@example.test") {
		t.Fatal("revision after restart", e.w.lastPut().Revision, last)
	}
	// Lost PUT response: the next table goes above it.
	e.w.set(func() { e.w.abort["PUT /routes"] = true })
	e.clock.advance(3 * time.Hour)
	e.cycle(t)
	lost := e.w.lastPut().Revision
	e.cycle(t)
	if e.w.lastPut().Revision <= lost {
		t.Fatal("lost PUT not superseded")
	}
}

func TestCloudflareContinuousInitPrintsOnlyWorkerSecrets(t *testing.T) {
	secret, stateDir := t.TempDir(), t.TempDir()
	t.Setenv("SECRET_DIR", secret)
	t.Setenv("STATE_DIR", stateDir)
	var out bytes.Buffer
	if err := runCloudflareContinuous([]string{"init"}, &out); err != nil {
		t.Fatal(err)
	}
	m, _, live, err := cfreceiving.Keys{Dir: secret}.Load()
	if err != nil || !live {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), m.Token) || !strings.Contains(out.String(), "PICKUP_TOKEN_SHA256="+m.TokenSHA256()+"\n") || !strings.Contains(out.String(), "ROUTING_PUBLIC_KEY="+m.PublicKey()+"\n") {
		t.Fatal("init output", out.String())
	}
	if err := runCloudflareContinuous([]string{"init"}, io.Discard); err == nil {
		t.Fatal("second init replaced credentials")
	}
	if err := runCloudflareContinuous([]string{"takeover"}, io.Discard); err == nil || !strings.Contains(err.Error(), "current host stops receiving") {
		t.Fatal("takeover without naming its effect", err)
	}
	out.Reset()
	if err := runCloudflareContinuous([]string{"status"}, &out); err != nil || !strings.Contains(out.String(), `"state":"not-started"`) {
		t.Fatal("status", out.String(), err)
	}
	if strings.Contains(out.String(), m.Token) {
		t.Fatal("status printed the bearer")
	}
}

// Mail frozen under an unknown revision (after a restore) goes only to an
// address's current owner, and only by the explicit release.
func TestCloudflareContinuousUnresolvedReleaseToCurrentOwner(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	one, two := e.users[0], e.users[1]
	if _, err := e.r.life.AddNativeAlias(ctx, e.r.stateDir, one.ID, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	unknown := func(env *cfreceiving.Envelope) { env.TableRevision = 4242 }
	toOne := e.w.capture("one@example.test", "", "to-one", unknown)
	moved := e.w.capture("sales@example.test", "", "moved", unknown)
	gone := e.w.capture("sales@example.test", "", "gone", unknown)
	frozen := e.w.capture("sales@example.test", "", "frozen", nil)
	if _, err := e.r.life.ReleaseNativeAlias(ctx, e.r.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.life.ReassignNativeAddress(ctx, e.r.stateDir, "sales@example.test", two.ID); err != nil {
		t.Fatal(err)
	}
	e.cycle(t)
	release := func(id string, current bool) error {
		return e.r.life.ReleaseQuarantined(ctx, e.r.stateDir, "https://identity.example.test", e.r.accounts, e.r.holding, cfGateway, id, current)
	}
	if err := release(toOne, false); !errors.Is(err, sso.ErrQuarantineUnresolved) {
		t.Fatal("unresolved released without the flag", err)
	}
	if err := release(frozen, true); !errors.Is(err, sso.ErrQuarantineResolved) {
		t.Fatal("frozen-owner delivery redirected to the current owner", err)
	}
	listed, err := e.r.life.QuarantinedDeliveries(ctx, e.r.holding, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range listed {
		if d.ID == moved && (!d.Unresolved || d.Recipients[0].CurrentUser != two.ID || d.Recipients[0].User != "") {
			t.Fatalf("unresolved listing %+v", d)
		}
	}
	for _, id := range []string{toOne, moved} {
		if err := release(id, true); err != nil {
			t.Fatal(id, err)
		}
	}
	if got := cfFolder(t, e.r, one, "INBOX"); !slices.Equal(got, []string{"to-one"}) {
		t.Fatal("first owner", got)
	}
	if got := cfFolder(t, e.r, two, "INBOX"); !slices.Equal(got, []string{"moved"}) {
		t.Fatal("current owner of the reassigned address", got)
	}
	if d, err := e.r.holding.Get(ctx, cfGateway, moved); err != nil || d.State != "archived" || d.Disposition != "released" {
		t.Fatal("not archived as released", d.State, err)
	}
	if _, err := e.r.life.ReleaseNativeAlias(ctx, e.r.stateDir, "sales@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := release(gone, true); !errors.Is(err, sso.ErrQuarantineAddressInactive) {
		t.Fatal("released to an inactive address", err)
	}
	if d, err := e.r.holding.Get(ctx, cfGateway, gone); err != nil || d.State != "quarantined" || !d.Bindings[0].Unresolved() {
		t.Fatal("refused release changed the delivery", err)
	}
}

// Stuck mail backs off instead of starving newer mail; one message over its
// mailbox's limit is quarantined without halting pickup.
func TestCloudflareContinuousStuckItemsDoNotStarve(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	e.cycle(t)
	if _, err := e.r.accounts.Deactivate(e.users[0].ID); err != nil {
		t.Fatal(err)
	}
	for i := range 150 {
		e.w.capture("one@example.test", "", "stuck-"+strconv.Itoa(i), nil)
	}
	e.cycle(t)
	fresh := e.w.capture("two@example.test", "", "fresh", nil)
	e.cycle(t)
	if got := cfFolder(t, e.r, e.users[1], "INBOX"); !slices.Equal(got, []string{"fresh"}) {
		t.Fatal("stuck mail starved another user", got)
	}
	if _, kept := e.w.object(fresh); kept {
		t.Fatal("fresh mail not deleted")
	}
	fetches := e.w.count(func(c fakeCall) bool { return c.kind == "/messages/<id>" })
	e.cycle(t)
	if e.w.count(func(c fakeCall) bool { return c.kind == "/messages/<id>" }) != fetches {
		t.Fatal("backed-off mail downloaded again")
	}
	if s := cfStatus(t, e); s.Waiting != 150 || s.State != "running" {
		t.Fatalf("status %+v", s)
	}

	// A table record whose maxBytes the mailbox does not allow: the message
	// is quarantined with its bytes and the next one still arrives.
	if _, err := e.r.accounts.Reactivate(e.users[0].ID); err != nil {
		t.Fatal(err)
	}
	const issuer = "https://identity.example.test"
	raw := []byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"small","externalId":"small","userName":"small","active":true,"emails":[{"value":"small@example.test","primary":true}],"meta":{"version":"W/\"1\""}}`)
	var resource sso.DirectoryUser
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatal(err)
	}
	ev := syncauth.Event{ID: "create-small", Type: "user.created", At: time.Now()}
	if _, err := e.r.life.ApplyDirectoryUser(issuer, ev, resource, sso.EventDigest(ev.Type, raw), func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	small, err := e.r.life.AllocateNativeAccount(ctx, e.r.stateDir, issuer, "small", e.r.domains, e.r.accounts, mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 32 << 20, Records: 10000})
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := e.r.life.NativeAssignment(issuer, "small")
	if err != nil {
		t.Fatal(err)
	}
	e.cycle(t) // publishes small@ at maxBytes 1 MiB
	// A table record claiming more than the mailbox allows, as if its limit
	// shrank after capture.
	last, err := e.db.LastRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rev := last + 1
	if err := e.db.Record(ctx, rev, 1, "oversized-fixture", []cfreceiving.Route{{Address: "small@example.test", Generation: 1, MaxBytes: cfreceiving.MaxMessageBytes, Issuer: a.Owner.Issuer, Subject: a.Owner.Subject, Mailbox: a.Owner.Mailbox}}); err != nil {
		t.Fatal(err)
	}
	big := e.w.capture("small@example.test", "", strings.Repeat("x", 2<<20), func(env *cfreceiving.Envelope) { env.TableRevision = rev })
	e.w.capture("small@example.test", "", "after-big", nil)
	e.clock.advance(time.Hour)
	for range 2 { // the reactivated 150 drain at 100 fetches a cycle
		e.cycle(t)
	}
	if d, err := e.r.holding.Get(ctx, cfGateway, big); err != nil || d.State != "quarantined" || len(d.Raw) == 0 {
		t.Fatal("oversized message not quarantined with its bytes", d.State, err)
	}
	if got := cfFolder(t, e.r, small, "INBOX"); !slices.Equal(got, []string{"after-big"}) || cfStatus(t, e).State != "running" {
		t.Fatal("one oversized message halted pickup", got, cfStatus(t, e))
	}
}

// An empty truncated page stops the cycle; a junk verdict whose provider copy
// vanished before the bytes were accepted leaves the ledger.
func TestCloudflareContinuousListingAndStaleJunk(t *testing.T) {
	e := cfFixture(t)
	ctx := context.Background()
	e.cycle(t)
	e.w.set(func() { e.w.emptyTruncated = true })
	if err := e.l.cycle(ctx); err == nil || cfStatus(t, e).State != "error" {
		t.Fatal("empty truncated page accepted")
	}
	e.cycle(t)
	e.scan.Store("reject")
	key := e.w.capture("one@example.test", "", "spam", nil)
	e.l.crash = func(p string) error {
		if p == "junk" {
			return errCrash
		}
		return nil
	}
	_ = e.l.cycle(ctx)
	if entry, found, _ := e.db.Entry(ctx, key); !found || entry.State != "junk" {
		t.Fatal("fixture: junk verdict not recorded")
	}
	e.w.set(func() { delete(e.w.inbox, key) })
	e.l = e.restart()
	e.cycle(t)
	if _, found, _ := e.db.Entry(ctx, key); found {
		t.Fatal("junk row outlived its provider copy")
	}
}

// The Worker's sender fixture: everything it accepts, KyPost can hold.
func TestCloudflareContinuousSendersMatchWorker(t *testing.T) {
	raw, err := os.ReadFile("../../../receiving-worker/senders.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Accept, Reject []string }
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Accept) == 0 {
		t.Fatal(err)
	}
	for _, sender := range fixture.Accept {
		if !validEnvelopeSender(sender) {
			t.Error("Worker accepts a sender KyPost refuses", sender)
		}
	}
}
