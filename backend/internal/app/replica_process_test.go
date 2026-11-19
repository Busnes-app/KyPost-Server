package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend/pgtest"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

type child struct {
	name string
	cmd  *exec.Cmd
	out  *syncBuffer
	port int
	done chan error
}

// buildServer builds the kypost-server binary for the process tests.
func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kypost-server")
	build := exec.Command("go", "build", "-o", bin, "../../cmd")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// replicaEnv is one deployment's environment: shared volumes and a database URL, and
// nothing that reaches a Ky service, a push relay or a model.
func replicaEnv(dataRoot, dbURL string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "KYPOST_") && k != "STATE_DIR" && k != "CONFIG_DIR" && k != "SECRET_DIR" && k != "LOG_DIR" && k != "WEB_PORT" {
			env = append(env, kv)
		}
	}
	return append(env,
		"CONFIG_DIR="+filepath.Join(dataRoot, "config"), "SECRET_DIR="+filepath.Join(dataRoot, "private"),
		"STATE_DIR="+filepath.Join(dataRoot, "state"), "LOG_DIR="+filepath.Join(dataRoot, "logs"),
		"KYPOST_DATABASE_URL="+dbURL, "KYPOST_BIND=127.0.0.1", "KYPOST_DISABLE_AI_SORTING=true",
		"CAPTCHA_PROVIDER=none", "PUSH_RELAY_URL=", "APNS_RELAY_URL=", "OLLAMA_BASE_URL=http://127.0.0.1:1",
		"BOOTSTRAP_ADMIN_USER=admin", "BOOTSTRAP_ADMIN_PASS=replica-admin-password",
	)
}

func startChild(t *testing.T, bin, name string, env []string, args ...string) *child {
	t.Helper()
	c := &child{name: name, out: &syncBuffer{}, done: make(chan error, 1)}
	if args[len(args)-1] == "server" {
		c.port = freeTCPPort(t)
		env = append(env, "WEB_PORT="+strconv.Itoa(c.port))
	}
	c.cmd = exec.Command(bin, args...)
	c.cmd.Env = env
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- c.cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			_ = c.cmd.Process.Kill() // this test's own child
			<-c.done
		}
		if t.Failed() {
			t.Logf("%s output:\n%s", c.name, c.out.String())
		}
	})
	return c
}

func (c *child) url(path string) string { return "http://127.0.0.1:" + strconv.Itoa(c.port) + path }

func (c *child) running() bool {
	select {
	case err := <-c.done:
		c.done <- err
		return false
	default:
		return true
	}
}

// status is one request's status code, 0 when nothing answered.
func status(client *http.Client, method, url string) int {
	req, _ := http.NewRequest(method, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, within time.Duration, what string, check func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !check() {
		if time.Since(start) > within {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return time.Since(start)
}

func signIn(t *testing.T, c *child) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	resp, err := client.Get(c.url("/api/auth/login-params?username=admin"))
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Salt       string `json:"salt"`
		Iterations int    `json:"iterations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&params); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	secret, err := users.DeriveAuthSecret("replica-admin-password", params.Salt, params.Iterations)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"username": "admin", "authSecret": secret})
	resp, err = client.Post(c.url("/api/auth/login"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	return client
}

// signedIn is /api/auth/me's verdict on client's cookie at c: 1 signed in, 0 signed out,
// minus the status otherwise (-1: no answer).
func signedIn(client *http.Client, c *child) int {
	resp, err := client.Get(c.url("/api/auth/me"))
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -resp.StatusCode
	}
	var me struct {
		Authenticated bool `json:"authenticated"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if me.Authenticated {
		return 1
	}
	return 0
}

func elected(c *child, role string) bool {
	for _, line := range strings.Split(c.out.String(), "\n") {
		if strings.Contains(line, `"message":"elected"`) && strings.Contains(line, `"role":"`+role+`"`) {
			return true
		}
	}
	return false
}

// AC27: two api and two daemon processes of the real binary share one volume set and one
// PostgreSQL through a freezable relay. A frozen database turns both /readyz to 503 within
// 4 s while /livez stays 200, an authenticated GET answers 503 within the deadline + 1 s and
// nobody exits; after a thaw both are ready within 15 s and the cookie still works. SIGTERM
// to one api turns its /readyz 503 before its listener closes, it exits 0, and the other
// keeps the cookie; SIGTERM to the owning daemon hands the daemon role to the other.
func TestReplicaProcessesSurviveFreezeAndStop(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dbURL := dbbackend.FreshDatabaseForTest(t)
	relay := pgtest.NewRelay(t, pgtest.Target(t, dbURL))
	bin := buildServer(t)
	root := t.TempDir()
	env := replicaEnv(root, relay.URL(t, dbURL))
	boot := exec.Command(bin, "--mode", "bootstrap-admin")
	boot.Env = env
	if out, err := boot.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap-admin: %v\n%s", err, out)
	}
	apiA := startChild(t, bin, "api-a", env, "--mode", "server")
	apiB := startChild(t, bin, "api-b", env, "--mode", "server")
	daemonA := startChild(t, bin, "daemon-a", env, "--mode", "daemon")
	daemonB := startChild(t, bin, "daemon-b", env, "--mode", "daemon")
	probe := &http.Client{Timeout: 3 * time.Second}
	for _, c := range []*child{apiA, apiB} {
		waitFor(t, 90*time.Second, c.name+" ready", func() bool { return status(probe, "GET", c.url("/readyz")) == 200 })
	}
	client := signIn(t, apiA)
	if got := signedIn(client, apiB); got != 1 {
		t.Fatalf("A's session on B: %d", got)
	}
	waitFor(t, 30*time.Second, "a daemon elected", func() bool { return elected(daemonA, "daemon") || elected(daemonB, "daemon") })
	if elected(daemonA, "daemon") && elected(daemonB, "daemon") {
		t.Fatal("both daemons own the daemon role")
	}

	// A frozen database.
	relay.Freeze()
	for _, c := range []*child{apiA, apiB} {
		waitFor(t, 4*time.Second, c.name+" unready while frozen", func() bool { return status(probe, "GET", c.url("/readyz")) == 503 })
		if got := status(probe, "GET", c.url("/livez")); got != 200 {
			t.Fatalf("%s /livez while frozen: %d", c.name, got)
		}
	}
	start := time.Now()
	if got := signedIn(client, apiB); got != -503 {
		t.Fatalf("an authenticated GET on a frozen database: %d, want -503", got)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("the frozen authenticated GET took %v, beyond the deadline + 1 s", took)
	}
	for _, c := range []*child{apiA, apiB, daemonA, daemonB} {
		if !c.running() {
			t.Fatalf("%s exited while the database was frozen", c.name)
		}
	}
	relay.Thaw()
	for _, c := range []*child{apiA, apiB} {
		waitFor(t, 15*time.Second, c.name+" ready after the thaw", func() bool { return status(probe, "GET", c.url("/readyz")) == 200 })
	}
	waitFor(t, 10*time.Second, "the cookie after the thaw", func() bool { return signedIn(client, apiB) == 1 })

	// SIGTERM to one api.
	if err := apiA.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	sawUnready := false
	waitFor(t, 15*time.Second, "api-a's listener closes", func() bool {
		switch status(probe, "GET", apiA.url("/readyz")) {
		case 503:
			sawUnready = true
		case 0:
			return true
		}
		return false
	})
	if !sawUnready {
		t.Fatal("api-a closed its listener without /readyz answering 503 first")
	}
	select {
	case err := <-apiA.done:
		apiA.done <- err
		if err != nil {
			t.Fatalf("api-a exited with %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("api-a did not exit within its budget")
	}
	if got := signedIn(client, apiB); got != 1 {
		t.Fatalf("api-b after api-a stopped: %d", got)
	}

	// SIGTERM to the owning daemon.
	owner, standby := daemonA, daemonB
	if elected(daemonB, "daemon") {
		owner, standby = daemonB, daemonA
	}
	if err := owner.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the standby takes the daemon role", func() bool { return elected(standby, "daemon") })
	select {
	case err := <-owner.done:
		owner.done <- err
		if err != nil {
			t.Fatalf("%s exited with %v", owner.name, err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("%s did not exit", owner.name)
	}
}

// AC26: three processes start together on an empty database and fresh volumes: one admin,
// one first-run password that signs in, each key once with no temporary file left, each
// schema once and one volume ID in every root.
func TestConcurrentFirstStart(t *testing.T) {
	dbbackend.RequirePostgres(t)
	dbURL := dbbackend.FreshDatabaseForTest(t)
	bin := buildServer(t)
	root := t.TempDir()
	var env []string
	for _, kv := range replicaEnv(root, dbURL) {
		if !strings.HasPrefix(kv, "BOOTSTRAP_ADMIN_PASS=") {
			env = append(env, kv) // a generated password, written once
		}
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			boot := exec.Command(bin, "--mode", "bootstrap-admin")
			boot.Env = env
			if out, err := boot.CombinedOutput(); err != nil {
				t.Errorf("bootstrap-admin: %v\n%s", err, out)
			}
		})
	}
	wg.Wait()
	var apis []*child
	for i := range 3 {
		apis = append(apis, startChild(t, bin, "api-"+strconv.Itoa(i), env, "--mode", "server"))
	}
	probe := &http.Client{Timeout: 3 * time.Second}
	for _, c := range apis {
		waitFor(t, 90*time.Second, c.name+" ready", func() bool { return status(probe, "GET", c.url("/readyz")) == 200 })
	}

	raw, err := os.ReadFile(filepath.Join(root, "config", "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Users []struct {
			Username, Role string
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Users) != 1 || doc.Users[0].Role != "admin" {
		t.Fatalf("users.json: %+v %v", doc.Users, err)
	}
	pw, err := os.ReadFile(filepath.Join(root, "config", users.BootstrapPasswordFile))
	if err != nil {
		t.Fatal(err)
	}
	password := ""
	for _, line := range strings.Split(string(pw), "\n") {
		if v, ok := strings.CutPrefix(line, "password: "); ok {
			password = v
		}
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	resp, err := client.Get(apis[2].url("/api/auth/login-params?username=admin"))
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Salt       string `json:"salt"`
		Iterations int    `json:"iterations"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&params)
	resp.Body.Close()
	secret, _ := users.DeriveAuthSecret(password, params.Salt, params.Iterations)
	body, _ := json.Marshal(map[string]string{"username": "admin", "authSecret": secret})
	resp, err = client.Post(apis[2].url("/api/auth/login"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the one first-run password does not sign in: %d", resp.StatusCode)
	}

	for _, dir := range []string{"config", "private", "state"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp.") {
				t.Errorf("%s/%s: a temporary file was left", dir, e.Name())
			}
		}
	}
	for _, key := range []string{"totp-secret.key", "pairing.key", "relay-identity.key"} {
		if _, err := os.Stat(filepath.Join(root, "private", key)); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	var ids []string
	for _, dir := range []string{"config", "private", "state"} {
		b, err := os.ReadFile(filepath.Join(root, dir, "replica-volume.id"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, strings.TrimSpace(string(b)))
	}
	if ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("volume IDs %v", ids)
	}
	tx, err := dbbackend.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var rows, schemas, connections int
	if err := tx.QueryRow(`SELECT count(*), count(DISTINCT schema) FROM kypost_catalog.stores`).Scan(&rows, &schemas); err != nil || rows == 0 || rows != schemas {
		t.Fatalf("catalog: %d rows for %d schemas (%v)", rows, schemas, err)
	}
	var extra int
	if err := tx.QueryRow(`SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'kypost\_%' AND nspname <> 'kypost_catalog' AND nspname NOT IN (SELECT schema FROM kypost_catalog.stores)`).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("%d schemas outside the catalog (%v)", extra, err)
	}
	if err := tx.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()`).Scan(&connections); err == nil {
		t.Logf("connections with three idle api processes: %d (this test's own included)", connections)
	}
}

// A clean stop leaves no non-empty -wal: the stores keep their handles until exit, and
// migrate-to-postgres refuses a log beside a database.
func TestCleanStopFoldsSQLiteLogs(t *testing.T) {
	dbbackend.SkipOnPostgres(t, "SQLite write-ahead logs")
	dir := t.TempDir()
	st, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("left", "open"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "state.db-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("fixture has no log to fold: %v", err)
	}
	checkpointSQLite(newTestLogger(t), dir) // the store is still open, as at a real stop
	if info, err := os.Stat(filepath.Join(dir, "state.db-wal")); err == nil && info.Size() != 0 {
		t.Fatalf("a %d-byte log is left beside state.db", info.Size())
	}
	if v, err := st.Setting("left"); err != nil || v != "open" {
		t.Fatalf("the folded write is lost: %q %v", v, err)
	}
	_ = st.Close()
}
