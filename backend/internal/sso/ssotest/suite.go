package ssotest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type suiteTransport func(*http.Request) (*http.Response, error)

func (f suiteTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// SuiteEmailDomains serves only the fixture's authenticated suite-domain request.
// Discovery and any other network access fail immediately.
func SuiteEmailDomains(t *testing.T, issuer, clientID, secret string, domains ...string) http.RoundTripper {
	t.Helper()
	body, err := json.Marshal(map[string]any{"domains": domains})
	if err != nil {
		t.Fatal(err)
	}
	return suiteTransport(func(r *http.Request) (*http.Response, error) {
		id, password, ok := r.BasicAuth()
		if r.Method != http.MethodGet || r.URL.String() != issuer+"/api/suite/email-domains" || !ok || id != clientID || password != secret {
			return nil, errors.New("test suite: unexpected request")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
}
