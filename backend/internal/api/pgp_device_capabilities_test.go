package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Busness-app/kypost-server/backend/internal/state"
)

func TestEnrollmentCapabilitiesPublishAndListForOwner(t *testing.T) {
	srv, userID, deviceID, authDevice := newPairedDeviceForTest(t)
	otherID, _ := pairNativeDevice(t, srv, userID, "other-device")
	req := httptest.NewRequest(http.MethodPost, "/api/pgp/device/enrollment-key", strings.NewReader(`{"publicKey":"KEY","envelopeVersions":[4,3,2],"deviceId":"other-device"}`))
	authDevice(req)
	rec := httptest.NewRecorder()
	srv.handlePGPPublishEnrollmentKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	own := deviceByID(t, srv, userID, deviceID)
	other := deviceByID(t, srv, userID, otherID)
	if own.EnrollmentPublicKey != "KEY" || !slices.Equal(own.EnrollmentEnvelopeVersions, []int{2, 3, 4}) {
		t.Fatal("own claim missing")
	}
	if other.EnrollmentPublicKey != "" || !slices.Equal(other.EnrollmentEnvelopeVersions, []int{2}) {
		t.Fatal("claim reached other device")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/notifications/native/devices", nil)
	authRequestAs(srv, req, userID)
	rec = httptest.NewRecorder()
	srv.withAuth(srv.handleNotificationNativeDevices).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var listed struct {
		Devices []state.NativeDevice `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range listed.Devices {
		if d.SecretHash != "" {
			t.Fatal("secret hash leaked")
		}
		if d.DeviceID == deviceID {
			found = slices.Equal(d.EnrollmentEnvelopeVersions, []int{2, 3, 4})
		}
	}
	if !found {
		t.Fatal("owner listing omitted capabilities")
	}
	// Omitted and explicit-null claims are legacy publications, not preservation requests.
	for _, body := range []string{`{"publicKey":"SAME","envelopeVersions":null}`, `{"publicKey":"OLD"}`} {
		req = httptest.NewRequest(http.MethodPost, "/api/pgp/device/enrollment-key", strings.NewReader(body))
		authDevice(req)
		rec = httptest.NewRecorder()
		srv.handlePGPPublishEnrollmentKey(rec, req)
		if rec.Code != http.StatusOK || !slices.Equal(deviceByID(t, srv, userID, deviceID).EnrollmentEnvelopeVersions, []int{2}) {
			t.Fatal("legacy publication failed", rec.Code)
		}
	}
}

func TestEnrollmentCapabilitiesRejectInvalidRequestsWithoutMutation(t *testing.T) {
	srv, userID, deviceID, authDevice := newPairedDeviceForTest(t)
	before := deviceByID(t, srv, userID, deviceID)
	bodies := []string{
		`{"publicKey":"NEW","envelopeVersions":[]}`,
		`{"publicKey":"NEW","envelopeVersions":[2,2]}`,
		`{"publicKey":"NEW","envelopeVersions":[1]}`,
		`{"publicKey":"NEW","envelopeVersions":[65536]}`,
		`{"publicKey":"NEW","envelopeVersions":[2.5]}`,
		`{"publicKey":"NEW","envelopeVersions":["3"]}`,
		`{"publicKey":"NEW","envelopeVersions":[null]}`,
		`{"publicKey":"NEW","envelopeVersions":false}`,
		`{"publicKey":"NEW","envelopeVersions":[2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18]}`,
		`{"publicKey":"NEW","envelopeVersions":[2]} {}`,
		`{"publicKey":"NEW","envelopeVersions":[2]}` + strings.Repeat(" ", 4096),
	}
	for _, body := range bodies {
		req := httptest.NewRequest(http.MethodPost, "/api/pgp/device/enrollment-key", strings.NewReader(body))
		authDevice(req)
		rec := httptest.NewRecorder()
		srv.handlePGPPublishEnrollmentKey(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		after := deviceByID(t, srv, userID, deviceID)
		// Credential verification touches last-seen state; the key/capability fields must not change.
		if after.EnrollmentPublicKey != before.EnrollmentPublicKey || after.EnrollmentKeyAt != before.EnrollmentKeyAt || !reflect.DeepEqual(after.EnrollmentEnvelopeVersions, before.EnrollmentEnvelopeVersions) {
			t.Fatal("invalid request changed enrollment")
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/pgp/device/enrollment-key", strings.NewReader(`{"publicKey":"NEW","envelopeVersions":[3]}`))
	authRequestAs(srv, req, userID)
	rec := httptest.NewRecorder()
	srv.handlePGPPublishEnrollmentKey(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("session credential published device capabilities")
	}
}
