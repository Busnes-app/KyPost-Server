// Package objecttest connects tests to the disposable S3 fixture CI runs (SeaweedFS), through
// a verified TLS proxy, since the shared client accepts only HTTPS.
package objecttest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"testing"

	"github.com/Busnes-app/ky-primitives/s3"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// Key is a fixed test data key per org.
func Key(_ context.Context, org string) ([]byte, error) {
	k := sha256.Sum256([]byte("kypost-test-org-key:" + org))
	return k[:], nil
}

// Cell is a fresh cell slug, so concurrent runs never share keys.
func Cell() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "t" + hex.EncodeToString(b)
}

// Client is the fixture's client. It skips the test when KY_TEST_S3_ENDPOINT is unset and
// refuses anything but a loopback HTTP fixture.
func Client(t testing.TB) *s3.Client {
	t.Helper()
	endpoint := os.Getenv("KY_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("needs KY_TEST_S3_ENDPOINT, KY_TEST_S3_BUCKET, KY_TEST_S3_ACCESS_KEY and KY_TEST_S3_SECRET_KEY")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("KY_TEST_S3_ENDPOINT must be a loopback http fixture")
	}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(u))
	t.Cleanup(proxy.Close)
	c, err := s3.New(s3.Config{Endpoint: proxy.URL, Bucket: os.Getenv("KY_TEST_S3_BUCKET"), AccessKey: os.Getenv("KY_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("KY_TEST_S3_SECRET_KEY"), HTTP: proxy.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// New is an Object serving cells over the fixture, with Key as its key source.
func New(t testing.TB, cells ...string) (*vol.Object, *s3.Client) {
	t.Helper()
	c := Client(t)
	o, err := vol.NewObject(c, cells, Key)
	if err != nil {
		t.Fatal(err)
	}
	return o, c
}
