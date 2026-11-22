package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
)

// In multi mode the staff console's sender blocks live in tenancy.db (receiving tier R7):
// add, list and remove work with both file names taken by unreadable directories, which a
// file read or write would trip on.
func TestStaffSenderBlocksUseTenancyTables(t *testing.T) {
	fx, _, staff := staffFixture(t)
	dir := filepath.Join(fx.srv.stateDir, "receiving")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ingress.BlocksFile, ingress.EvidenceFile} {
		if err := os.Mkdir(filepath.Join(dir, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	const base = "/api/admin/receiving/blocks"
	if w := fx.staffCall(staff, http.MethodPost, base, `{"kind":"domain","value":"evil.test"}`, ""); w.Code != http.StatusOK {
		t.Fatal("add", w.Code, w.Body)
	}
	var n int
	if err := fx.srv.tenancy.QueryRow(`SELECT count(*) FROM sender_blocks WHERE id = ?`, ingress.BlockID("domain", "evil.test")).Scan(&n); err != nil || n != 1 {
		t.Fatal("block not in tenancy.db", n, err)
	}
	if w := fx.staffCall(staff, http.MethodGet, base, "", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"value":"evil.test"`) || !strings.Contains(w.Body.String(), `"damaged":false`) {
		t.Fatal("list", w.Code, w.Body)
	}
	if w := fx.staffCall(staff, http.MethodDelete, base+"/"+ingress.BlockID("domain", "evil.test"), `{}`, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"result":"unblocked"`) || strings.Contains(w.Body.String(), "warning") {
		t.Fatal("remove", w.Code, w.Body)
	}
	for _, name := range []string{ingress.BlocksFile, ingress.EvidenceFile} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || !info.IsDir() || info.Mode().Perm() != 0 {
			t.Fatal("multi mode touched", name, err)
		}
	}
}
