package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

func TestDeviceOpenPGPReportOnBothEndpoints(t *testing.T) {
	s, owner, device, auth := newPairedDeviceForTest(t)
	other, _ := pairNativeDevice(t, s, owner, "other")
	store, err := s.userStore(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetNativeDeviceEncryptionEnrolled(device, true); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"enrollment-key", "enrollment-state"} {
		handler := s.handlePGPPublishEnrollmentKey
		fields := `"publicKey":"KEY","envelopeVersions":[4]`
		if endpoint == "enrollment-state" {
			handler = s.handlePGPDeviceEnrollmentState
			fields = `"encryptionEnrolled":true`
		}
		for _, report := range []string{`,"openpgp":["rfc9980","V6","v4","v6"]`, ""} {
			req := httptest.NewRequest("POST", "/api/pgp/device/"+endpoint, strings.NewReader("{"+fields+report+`,"deviceId":"`+other+`"}`))
			auth(req)
			w := httptest.NewRecorder()
			handler(w, req)
			if w.Code != http.StatusOK {
				t.Fatal(endpoint, w.Code, w.Body.String())
			}
			want := []string{"v4"}
			if report != "" {
				want = []string{"v4", "v6", "rfc9980"}
			}
			if got := deviceByID(t, s, owner, device); !slices.Equal(got.OpenPGP, want) {
				t.Fatal(endpoint, got)
			}
			if got := deviceByID(t, s, owner, other); !slices.Equal(got.OpenPGP, []string{"v4"}) {
				t.Fatal("foreign device mutated", got)
			}
			// This is the browser/P4 inventory, including already-enrolled upgrades.
			req = httptest.NewRequest("GET", "/api/notifications/native/devices", nil)
			authRequestAs(s, req, owner)
			w = httptest.NewRecorder()
			s.withAuth(s.handleNotificationNativeDevices).ServeHTTP(w, req)
			var listed struct {
				Devices []state.NativeDevice `json:"devices"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			found := false
			for _, d := range listed.Devices {
				if d.DeviceID == device {
					found = slices.Equal(d.OpenPGP, want) && d.SecretHash == ""
				}
			}
			if !found {
				t.Fatal("listing lost codecs")
			}
		}
	}
}

func TestDeviceOpenPGPRejectsMalformedBodiesBeforeMutation(t *testing.T) {
	s, owner, device, auth := newPairedDeviceForTest(t)
	for _, endpoint := range []string{"enrollment-key", "enrollment-state"} {
		handler := s.handlePGPPublishEnrollmentKey
		fields := `"publicKey":"NEW","envelopeVersions":[4]`
		if endpoint == "enrollment-state" {
			handler = s.handlePGPDeviceEnrollmentState
			fields = `"encryptionEnrolled":true`
		}
		for _, report := range []string{`[]`, `["v7"]`, `[null]`, `[4]`, `"v6"`, `["v4",false]`} {
			req := httptest.NewRequest("POST", "/api/pgp/device/"+endpoint, strings.NewReader("{"+fields+`,"openpgp":`+report+"}"))
			auth(req)
			w := httptest.NewRecorder()
			handler(w, req)
			if w.Code != 400 {
				t.Fatal(endpoint, report, w.Code, w.Body.String())
			}
			d := deviceByID(t, s, owner, device)
			if d.EnrollmentPublicKey != "" || d.EncryptionEnrolled || !slices.Equal(d.OpenPGP, []string{"v4"}) {
				t.Fatal("invalid request mutated device", d)
			}
		}
	}
}
