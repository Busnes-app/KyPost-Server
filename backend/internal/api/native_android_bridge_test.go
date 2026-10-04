//go:build linux

package api

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicitly opt-in: installed test APKs and a dedicated disposable emulator.
// Reuse the actual native receiving/TLS SMTP fixture; no exported test API.
func qualifyNativeAndroidMail(t *testing.T, s *Server, userID string) *httptest.ResponseRecorder {
	t.Helper()
	serial := os.Getenv("KYPOST_NATIVE_ANDROID_TEST_SERIAL")
	if !strings.HasPrefix(serial, "emulator-") {
		t.Fatal("Android qualification requires a dedicated disposable emulator")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adb := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "adb", append([]string{"-s", serial}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("disposable emulator command failed: %v\n%s", err, output)
		}
		return string(output)
	}
	if strings.TrimSpace(adb("shell", "getprop", "sys.boot_completed")) != "1" {
		t.Fatal("disposable emulator is not booted")
	}
	// Observe the real send response for the existing durable outbox/Sent checks.
	// The handler still authenticates and processes every request normally.
	sent := make(chan *httptest.ResponseRecorder, 1)
	routes := s.routes()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mail/send" || r.Method != "POST" {
			routes.ServeHTTP(w, r)
			return
		}
		reply := httptest.NewRecorder()
		routes.ServeHTTP(reply, r)
		for name, values := range reply.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(reply.Code)
		_, _ = w.Write(reply.Body.Bytes())
		select {
		case sent <- reply:
		default:
		}
	}))
	defer server.Close()
	s.serverBaseURL = server.URL
	port := server.Listener.Addr().(*net.TCPAddr).Port
	address := strings.TrimPrefix(server.URL, "https://127.0.0.1:")
	if address == server.URL || port == 0 {
		t.Fatal("qualification server must bind loopback")
	}
	certificate := server.TLS.Certificates[0].Certificate[0]
	leaf, err := x509.ParseCertificate(certificate)
	if err != nil || leaf.VerifyHostname("127.0.0.1") != nil {
		t.Fatal("qualification certificate must verify the loopback host")
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	pin := "sha256/" + base64.StdEncoding.EncodeToString(digest[:])
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600); err != nil {
		t.Fatal(err)
	}
	deviceCA := "/data/local/tmp/kypost-native-" + address + ".pem"
	for _, line := range strings.Split(adb("reverse", "--list"), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "tcp:"+address {
			t.Fatal("disposable reverse port is already owned by another test")
		}
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, args := range [][]string{{"shell", "rm", "-f", deviceCA}, {"reverse", "--remove", "tcp:" + address}} {
			if err := exec.CommandContext(cleanup, "adb", append([]string{"-s", serial}, args...)...).Run(); err != nil {
				t.Errorf("disposable Android cleanup failed: %v", err)
			}
		}
	}()
	adb("push", ca, deviceCA)
	adb("reverse", "tcp:"+address, "tcp:"+address)
	var proof struct {
		SubscriberID string `json:"subscriberId"`
		Token        string `json:"pairingToken"`
	}
	pairing := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/notifications/pairing", nil)
	authRequestAs(s, r, userID)
	routes.ServeHTTP(pairing, r)
	if pairing.Code != 200 || json.Unmarshal(pairing.Body.Bytes(), &proof) != nil || proof.SubscriberID == "" || proof.Token == "" {
		t.Fatal("native Android pairing proof unavailable")
	}
	output := adb("shell", "am", "instrument", "-w", "-r", "-e", "class", "org.kysecurity.mail.mail.NativeMailboxRoundtripTest",
		"-e", "nativeServer", server.URL, "-e", "nativeSubscriber", proof.SubscriberID, "-e", "nativePairingToken", proof.Token,
		"-e", "nativePin", pin, "-e", "nativeCA", deviceCA, "org.kysecurity.mail.test/androidx.test.runner.AndroidJUnitRunner")
	if !strings.Contains(output, "OK (1 test)") || !strings.Contains(output, "INSTRUMENTATION_CODE: -1") {
		t.Fatalf("Android native-mail assertions failed:\n%s", output)
	}
	t.Log("Android native pairing, pinned HTTPS, Room, body, labels, attachment and ordinary send passed")
	select {
	case reply := <-sent:
		return reply
	default:
		t.Fatal("Android test did not submit mail")
		return nil
	}
}
