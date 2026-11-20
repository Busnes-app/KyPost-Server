package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// Two replicas behind a balancer that sends each request anywhere: an upload link minted on
// one is spent once on either, the job's status and cancel reach every replica, a stopped
// runner reads as interrupted, and a starting replica keeps a live replica's upload.
func TestImportIsSharedAcrossReplicas(t *testing.T) {
	dbbackend.RequirePostgres(t)
	ctx := context.Background()
	t.Setenv("SECRET_DIR", t.TempDir())
	a := newNativeRuntimeServer(t)
	a.configPath = filepath.Join(a.configDir, "config.yaml")
	if err := config.Save(a.configPath, a.cfg); err != nil {
		t.Fatal(err)
	}
	b := replicaOf(t, a)
	b.EnableNativeMail()
	b.nativeDomains.SetLookupForTest(func(context.Context, string) ([]string, error) {
		d, err := b.nativeDomains.Read()
		return []string{d.RecordValue()}, err
	})
	instance := func(srv *Server, id string) func() {
		release, err := srv.globalStore.RegisterInstance(ctx, state.Instance{ID: id, BootID: "boot", VolumeID: "vol", Role: "api", Started: 1})
		if err != nil {
			t.Fatal(err)
		}
		srv.SetReplicaInstance(id, srv.globalStore.InstanceLive)
		return release
	}
	defer instance(a, "import-a")()
	releaseB := instance(b, "import-b")
	a.UseSharedState(a.globalStore)
	b.UseSharedState(b.globalStore)

	directoryStatus(t, postDirectory(t, a, testSyncKey, "user.created", "import-replica", 1, runtimeDirectoryUser(true)))
	one, err := a.users.GetBySSOSubIssuer("https://idp.example", "native-runtime-one")
	if err != nil {
		t.Fatal(err)
	}
	const password = "long-password-for-import"
	if _, err = a.users.SetPassword(ctx, one.ID, password, false, nil); err != nil {
		t.Fatal(err)
	}
	token, csrf := mintSessionForTest(a, one.ID)
	call := func(srv *Server, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "kypost_session", Value: token})
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.routes().ServeHTTP(w, req)
		return w
	}
	link := func() string {
		t.Helper()
		w := call(a, "POST", "/api/import", `{"authSecret":"`+userAuthSecret(a, one.ID, password)+`"}`)
		var got struct{ URL string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatal("start on A", w.Code, w.Body)
		}
		return got.URL
	}
	status := func(srv *Server) importJob {
		t.Helper()
		var st importJob
		if w := call(srv, "GET", "/api/import", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil {
			t.Fatal("status", w.Code, w.Body)
		}
		return st
	}
	mbox := "From a@example.test Mon Jan  1 00:00:00 2024\r\nFrom: a@example.test\r\nSubject: one\r\n\r\nbody one\r\n"

	// A link minted on A uploads on B; A shows B's job through to its end.
	if w := call(b, "POST", link(), mbox); w.Code != http.StatusAccepted {
		t.Fatal("upload on B", w.Code, w.Body)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if st := status(a); st.State == "finished" && st.Imported == 1 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("A never saw B's import finish: %+v", st)
		}
	}

	// One link, uploaded on both at once: spent exactly once.
	url := link()
	// Bound to its user, not only to the session: another user with this session's cookie
	// does not hold it.
	linkToken := strings.TrimPrefix(url, "/api/import/")
	if _, ok := b.importGrantOf(linkToken, one.ID, token, false); !ok {
		t.Fatal("the link's own user and session do not hold it on B")
	}
	if _, ok := b.importGrantOf(linkToken, "another-user", token, false); ok {
		t.Fatal("a grant was held by another user with the same session")
	}
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, srv := range []*Server{a, b} {
		wg.Go(func() { codes <- call(srv, "POST", url, mbox).Code })
	}
	wg.Wait()
	close(codes)
	accepted := 0
	for c := range codes {
		if c == http.StatusAccepted {
			accepted++
		} else if c != http.StatusNotFound && c != http.StatusConflict {
			t.Fatal("racing upload answered", c)
		}
	}
	if accepted != 1 {
		t.Fatalf("one link was spent %d times", accepted)
	}
	for deadline := time.Now().Add(20 * time.Second); status(a).State != "finished"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the racing import never finished")
		}
	}
	// Nothing runs now, so only the spent grant can refuse a second upload, on either replica.
	for _, srv := range []*Server{a, b} {
		if w := call(srv, "POST", url, mbox); w.Code != http.StatusNotFound {
			t.Fatal("a spent link was accepted again", w.Code, w.Body)
		}
	}

	// A stand-in for a runner: claimed like a real job, ended when the test ends.
	running := func(srv *Server, user string) *importJob {
		t.Helper()
		job := &importJob{State: "running", user: user, correlation: "c"}
		if code, refusal, _ := srv.claimImport(job); code != 0 {
			t.Fatal("claim", user, code, refusal)
		}
		t.Cleanup(func() { srv.finishImport(job, "cancelled", "") })
		return job
	}

	// A running job on B: busy on A, cancelled from A, and B's runner stops.
	stopped := make(chan struct{})
	job := running(b, one.ID)
	b.importMu.Lock()
	job.cancel = func() {
		select {
		case <-stopped:
		default:
			close(stopped)
		}
	}
	b.importMu.Unlock()
	if code, _ := a.importRefusal(one.ID); code != http.StatusConflict {
		t.Fatal("A allowed a second import beside B's", code)
	}
	if st := status(a); st.State != "running" {
		t.Fatalf("A's view of B's running job: %+v", st)
	}
	if w := call(a, "POST", "/api/import/cancel", "{}"); w.Code != 200 {
		t.Fatal("cancel on A", w.Code, w.Body)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancel on A never reached B's runner")
	}

	// The server-wide slots (maxImports) count every replica's jobs: B's and one on A.
	running(a, "other")
	if code, refusal := a.importRefusal("third"); code != http.StatusConflict || !strings.Contains(refusal, "busy") {
		t.Fatal("slots taken on both replicas still admitted a third import", code, refusal)
	}

	// B's upload folder survives A's start while B lives, and goes once B has stopped.
	upload := filepath.Join(a.stateDir, importDir, "import-b", "upload-1")
	loose := filepath.Join(a.stateDir, importDir, "upload-old")
	dead := filepath.Join(a.stateDir, importDir, "import-gone", "upload-2")
	for _, p := range []string{upload, loose, dead} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a.sweepImportUploads()
	if _, err := os.Stat(upload); err != nil {
		t.Fatal("a starting replica removed a live replica's upload", err)
	}
	for _, p := range []string{loose, dead} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("a stopped replica's upload was kept", p, err)
		}
	}

	// B stops: its job reads as interrupted on A, frees the user and the slot, and its
	// upload folder is swept.
	releaseB()
	if st := status(a); st.State != "interrupted" || st.Error == "" {
		t.Fatalf("a stopped replica's job: %+v", st)
	}
	if code, refusal := a.importRefusal(one.ID); code != 0 {
		t.Fatal("an interrupted job still blocks the user", code, refusal)
	}
	a.sweepImportUploads()
	if _, err := os.Stat(upload); !os.IsNotExist(err) {
		t.Fatal("a stopped replica's upload folder was kept", err)
	}
}
