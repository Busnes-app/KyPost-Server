package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tlsOrigin starts an HTTPS server and returns its URL, its leaf pin, and a
// pool that trusts it — a stand-in for the reverse proxy that terminates TLS in
// front of this server in the deployment docs/Reverse_Proxy_Networking.md
// describes.
func tlsOrigin(t *testing.T) (baseURL, pin string, roots *x509.CertPool) {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	roots = x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	return ts.URL, spkiPin(ts.Certificate()), roots
}

// closedHTTPSOrigin returns an https URL whose port has nothing listening, so
// the probe fails immediately with a connection refused rather than sitting out
// pinProbeTimeout. Binding and releasing a real port is what keeps it closed:
// a hardcoded one might be in use by something else on the machine.
func closedHTTPSOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "https://" + addr
}

// TestSPKIPinMatchesTheCertificatesPublicKey checks the published value against
// an independently marshalled SubjectPublicKeyInfo — the same bytes
// `openssl x509 -pubkey | openssl pkey -pubin -outform der` produces, which is
// what OkHttp's CertificatePinner.pin() hashes on the client side.
func TestSPKIPinMatchesTheCertificatesPublicKey(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeKeypair(t, certPath, keyPath, "relay.example.com")

	cfg, err := newTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("newTLSConfig: %v", err)
	}

	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	sum := sha256.Sum256(spki)
	want := "sha256/" + base64.StdEncoding.EncodeToString(sum[:])

	if got := spkiPin(servedLeaf(cfg)); got != want {
		t.Errorf("served pin = %q, want %q", got, want)
	}
}

// TestLeafSPKIPinFollowsARenewal is the whole reason the pin is read at
// link-generation time instead of cached: a renewal that mints a new key must
// change the pin, or every link generated after it pins a key the server no
// longer serves and pairing breaks until a restart.
func TestLeafSPKIPinFollowsARenewal(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeKeypair(t, certPath, keyPath, "relay.example.com")

	cfg, err := newTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("newTLSConfig: %v", err)
	}
	before := spkiPin(servedLeaf(cfg))

	// Same trick TestCertificateReloadsOnRenewal uses: the stamp is
	// (modtime, size), so a same-second rewrite needs a distinguishable modtime.
	writeKeypair(t, certPath, keyPath, "relay.example.com")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatalf("chtimes cert: %v", err)
	}
	if err := os.Chtimes(keyPath, future, future); err != nil {
		t.Fatalf("chtimes key: %v", err)
	}

	if after := spkiPin(servedLeaf(cfg)); after == before {
		t.Errorf("served pin = %q both before and after renewal; the pin is stale", after)
	}
}

// TestLeafSPKIPinEmptyWithoutTLS keeps the absent-pin path honest: no TLS
// termination here means no local pin, and with no probe either the app falls
// back to trust-on-first-use exactly as it did before pins existed.
func TestLeafSPKIPinEmptyWithoutTLS(t *testing.T) {
	if got := servedLeaf(nil); got != nil {
		t.Error("servedLeaf(nil) returned a certificate")
	}
	if got := servedLeaf(&tls.Config{MinVersion: tls.VersionTLS12}); got != nil {
		t.Error("servedLeaf(no GetCertificate) returned a certificate")
	}
}

func TestProbeSPKIPin(t *testing.T) {
	baseURL, want, roots := tlsOrigin(t)

	t.Run("reads the certificate the origin actually serves", func(t *testing.T) {
		got, err := probeSPKIPin(context.Background(), baseURL, roots)
		if err != nil {
			t.Fatalf("probeSPKIPin: %v", err)
		}
		if got != want {
			t.Errorf("probeSPKIPin() = %q, want %q", got, want)
		}
	})

	t.Run("refuses a chain it cannot verify", func(t *testing.T) {
		// The system pool does not trust the httptest CA. Publishing a pin means
		// telling the app "this key and nothing else" — the server must not
		// assert that about bytes nobody authenticated.
		if got, err := probeSPKIPin(context.Background(), baseURL, nil); err == nil {
			t.Errorf("probeSPKIPin() = %q with an untrusted chain, want an error", got)
		}
	})

	t.Run("refuses a non-https base URL without dialling", func(t *testing.T) {
		if _, err := probeSPKIPin(context.Background(), "http://relay.example.com", roots); err == nil {
			t.Error("probeSPKIPin() accepted an http origin")
		}
		if _, err := probeSPKIPin(context.Background(), "", roots); err == nil {
			t.Error("probeSPKIPin() accepted an empty origin")
		}
	})
}

// writeLeafPEM writes a self-signed certificate for hosts (DNS names or IPs),
// valid from notBefore to notAfter, followed by an unrelated second certificate
// the way a fullchain file carries an intermediate. Returns the leaf's pin.
func writeLeafPEM(t *testing.T, path string, notBefore, notAfter time.Time, hosts ...string) string {
	t.Helper()
	var out []byte
	var pin string
	for i := range 2 {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)), NotBefore: notBefore, NotAfter: notAfter}
		if i == 0 {
			for _, h := range hosts {
				if ip := net.ParseIP(h); ip != nil {
					tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
				} else {
					tmpl.DNSNames = append(tmpl.DNSNames, h)
				}
			}
		}
		der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			leaf, _ := x509.ParseCertificate(der)
			pin = spkiPin(leaf)
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return pin
}

func TestFilePin(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	valid := filepath.Join(dir, "valid.pem")
	want := writeLeafPEM(t, valid, now.Add(-time.Hour), now.Add(time.Hour), "relay.example.com")

	if got, err := filePin(valid, "relay.example.com", now); err != nil || got != want {
		t.Fatalf("filePin = %q, %v; want the leaf's %q", got, err, want)
	}

	expired := filepath.Join(dir, "expired.pem")
	writeLeafPEM(t, expired, now.Add(-48*time.Hour), now.Add(-time.Hour), "relay.example.com")
	notYet := filepath.Join(dir, "future.pem")
	writeLeafPEM(t, notYet, now.Add(time.Hour), now.Add(48*time.Hour), "relay.example.com")
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"wrong host": valid,
		"expired":    expired,
		"not yet":    notYet,
		"missing":    filepath.Join(dir, "absent.pem"),
		"no PEM":     garbage,
	} {
		t.Run(name, func(t *testing.T) {
			host := "relay.example.com"
			if name == "wrong host" {
				host = "other.example.com"
			}
			if got, err := filePin(path, host, now); err == nil || got != "" {
				t.Errorf("filePin = %q, %v; want a refusal", got, err)
			}
		})
	}
}

func TestPinSourcesFromEnv(t *testing.T) {
	t.Setenv("KYPOST_TLS_CERT_FILE", "certs/fullchain.pem")
	t.Setenv("KYPOST_TLS_PIN_PROBE", "")
	if _, _, err := pinSourcesFromEnv(); err == nil {
		t.Error("a relative KYPOST_TLS_CERT_FILE was accepted")
	}

	t.Setenv("KYPOST_TLS_CERT_FILE", "/kypost/tls/fullchain.pem")
	t.Setenv("KYPOST_TLS_PIN_PROBE", "yes")
	if _, _, err := pinSourcesFromEnv(); err == nil {
		t.Error("a non-boolean KYPOST_TLS_PIN_PROBE was accepted")
	}

	t.Setenv("KYPOST_TLS_PIN_PROBE", "")
	if file, probe, err := pinSourcesFromEnv(); err != nil || file != "/kypost/tls/fullchain.pem" || probe {
		t.Errorf("got (%q, %v, %v); the probe must default to off", file, probe, err)
	}

	t.Setenv("KYPOST_TLS_CERT_FILE", "")
	t.Setenv("KYPOST_TLS_PIN_PROBE", "true")
	if _, probe, err := pinSourcesFromEnv(); err != nil || !probe {
		t.Errorf("got (%v, %v), want the explicit opt-in", probe, err)
	}
}

func TestPrepareRefusesARelativePinCertFile(t *testing.T) {
	t.Setenv("KYPOST_TLS_CERT_FILE", "fullchain.pem")
	srv := newTestServer(t)
	srv.Prepare()
	if err := srv.Serve(); err == nil || !strings.Contains(err.Error(), "KYPOST_TLS_CERT_FILE") {
		t.Fatalf("Serve() = %v, want a startup refusal naming KYPOST_TLS_CERT_FILE", err)
	}
}

// TestPairingSPKIPinSources is the regression for signing an unvouched key. The
// probe here reaches a different leaf that is valid for the host under a CA
// this server trusts — what an interceptor of the server's own outbound path
// would present. Its key must be pinned only when the operator opted in.
func TestPairingSPKIPinSources(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "c.key")
	writeKeypair(t, certPath, keyPath, "relay.example.com")
	localCfg, err := newTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("newTLSConfig: %v", err)
	}
	localPin := spkiPin(servedLeaf(localCfg))

	probed := func(t *testing.T) (*Server, string) {
		baseURL, probedPin, roots := tlsOrigin(t)
		srv := newTestServer(t)
		srv.serverBaseURL = baseURL
		srv.pinProbeRoots = roots
		return srv, probedPin
	}

	t.Run("the file is pinned, never the probed leaf", func(t *testing.T) {
		srv, probedPin := probed(t)
		srv.pinCertFile = filepath.Join(dir, "front.pem")
		srv.pinProbe = true // the file outranks even an opt-in
		want := writeLeafPEM(t, srv.pinCertFile, now.Add(-time.Hour), now.Add(time.Hour), "127.0.0.1")
		got := srv.pairingSPKIPin()
		if got == probedPin {
			t.Fatal("pinned the probed leaf while a certificate file is configured")
		}
		if got != want {
			t.Errorf("pairingSPKIPin() = %q, want the file's %q", got, want)
		}
	})

	t.Run("a refused file means no pin, not the probe", func(t *testing.T) {
		srv, _ := probed(t)
		srv.pinCertFile = filepath.Join(dir, "other-host.pem")
		srv.pinProbe = true
		writeLeafPEM(t, srv.pinCertFile, now.Add(-time.Hour), now.Add(time.Hour), "other.example.com")
		if got := srv.pairingSPKIPin(); got != "" {
			t.Errorf("pairingSPKIPin() = %q, want empty", got)
		}
	})

	t.Run("without file or opt-in the probe is never used", func(t *testing.T) {
		srv, _ := probed(t)
		if got := srv.pairingSPKIPin(); got != "" {
			t.Errorf("pairingSPKIPin() = %q, want empty", got)
		}
	})

	t.Run("the opt-in uses the probed leaf", func(t *testing.T) {
		srv, probedPin := probed(t)
		srv.pinProbe = true
		srv.tlsConfig = localCfg
		if got := srv.pairingSPKIPin(); got != probedPin {
			t.Errorf("pairingSPKIPin() = %q, want the probed %q", got, probedPin)
		}
	})

	t.Run("the served certificate when it names the host", func(t *testing.T) {
		srv := newTestServer(t)
		srv.serverBaseURL = "https://relay.example.com"
		srv.tlsConfig = localCfg
		if got := srv.pairingSPKIPin(); got != localPin {
			t.Errorf("pairingSPKIPin() = %q, want the served %q", got, localPin)
		}
	})

	t.Run("not a served certificate for another host", func(t *testing.T) {
		// A self-signed hop behind a re-terminating front: devices never see it.
		srv := newTestServer(t)
		srv.serverBaseURL = "https://mail.example.com"
		srv.tlsConfig = localCfg
		if got := srv.pairingSPKIPin(); got != "" {
			t.Errorf("pairingSPKIPin() = %q, want empty", got)
		}
	})

	t.Run("an unset SERVER_BASE_URL is never dialled", func(t *testing.T) {
		// Dialling an address read off the request would let a caller aim our
		// outbound connections and choose the pin we publish.
		srv := newTestServer(t)
		srv.pinProbe = true
		srv.pinProbeRoots = x509.NewCertPool()
		if got := srv.pairingSPKIPin(); got != "" {
			t.Errorf("pairingSPKIPin() = %q with no configured base URL", got)
		}
		if srv.pinProbeHost != "" {
			t.Errorf("recorded a probe of %q", srv.pinProbeHost)
		}
	})
}

// TestProbeFailureIsCached keeps the pairing panel off the dial timeout. A
// consumer router that will not hairpin NAT makes every probe hang for
// pinProbeTimeout, and the panel re-mints its code every 90 seconds.
func TestProbeFailureIsCached(t *testing.T) {
	baseURL, want, roots := tlsOrigin(t)
	srv := newTestServer(t)
	srv.serverBaseURL = baseURL
	srv.pinProbeRoots = roots

	// A probe of this exact host failed a moment ago, so it must not be retried
	// even though it would now succeed.
	srv.pinProbeHost = baseURL
	srv.pinProbeFailedAt = time.Now()
	if got := srv.probedSPKIPin(); got != "" {
		t.Errorf("probedSPKIPin() = %q, want empty from the failure cache", got)
	}

	// Once the entry ages out the probe runs again — a cache that never expired
	// would strand a deployment on trust-on-first-use until a restart.
	srv.pinProbeFailedAt = time.Now().Add(-pinProbeFailureTTL - time.Second)
	if got := srv.probedSPKIPin(); got != want {
		t.Errorf("probedSPKIPin() = %q after the cache expired, want %q", got, want)
	}
}

func TestPairingResponsePublishesThePin(t *testing.T) {
	pairingPin := func(t *testing.T, configure func(*Server)) (string, bool) {
		t.Helper()
		srv := newTestServer(t)
		configure(srv)
		all, _ := srv.users.List()
		req := httptest.NewRequest(http.MethodPost, "/api/notifications/pairing", strings.NewReader(pairingStepUpBody(t, srv, all[0].ID)))
		authRequest(srv, req)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("pairing status %d: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode pairing response: %v", err)
		}
		pin, ok := body["tlsPin"].(string)
		return pin, ok
	}

	t.Run("from the operator's certificate file, not the probe", func(t *testing.T) {
		baseURL, probedPin, roots := tlsOrigin(t)
		file := filepath.Join(t.TempDir(), "front.pem")
		want := writeLeafPEM(t, file, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "127.0.0.1")
		pin, _ := pairingPin(t, func(s *Server) {
			s.serverBaseURL, s.pinProbeRoots, s.pinCertFile = baseURL, roots, file
		})
		if pin == probedPin || pin != want {
			t.Errorf("tlsPin = %q, want the file's %q (probed %q)", pin, want, probedPin)
		}
	})

	t.Run("absent without a certificate file or the probe opt-in", func(t *testing.T) {
		baseURL, _, roots := tlsOrigin(t)
		if pin, ok := pairingPin(t, func(s *Server) {
			s.serverBaseURL, s.pinProbeRoots = baseURL, roots
		}); ok {
			t.Errorf("tlsPin = %q from a probe nobody opted into", pin)
		}
	})

	t.Run("from the proxy that terminates TLS, with the opt-in", func(t *testing.T) {
		baseURL, want, roots := tlsOrigin(t)
		pin, ok := pairingPin(t, func(s *Server) {
			s.serverBaseURL = baseURL
			s.pinProbeRoots = roots
			s.pinProbe = true
		})
		if !ok {
			t.Fatal("no tlsPin in the pairing response; the app would pair over TOFU")
		}
		if pin != want {
			t.Errorf("tlsPin = %q, want %q", pin, want)
		}
	})

	t.Run("absent when the certificate cannot be established", func(t *testing.T) {
		if pin, ok := pairingPin(t, func(s *Server) {
			s.serverBaseURL = closedHTTPSOrigin(t)
			s.pinProbe = true
		}); ok {
			t.Errorf("tlsPin = %q with no certificate to read", pin)
		}
	})

	t.Run("absent on a plain-http base URL", func(t *testing.T) {
		baseURL, _, roots := tlsOrigin(t)
		if pin, ok := pairingPin(t, func(s *Server) {
			s.serverBaseURL = strings.Replace(baseURL, "https://", "http://", 1)
			s.pinProbeRoots = roots
			s.pinProbe = true
		}); ok {
			t.Errorf("tlsPin = %q for an http register endpoint", pin)
		}
	})
}
