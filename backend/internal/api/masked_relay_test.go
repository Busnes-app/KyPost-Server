package api

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
)

// testRelayCode is the throwaway relay code of the test servers' cell; tenancy's live table
// stays empty (tenancy.relayCodes).
const testRelayCode = 'q'

func init() {
	relayCode = func(cell string) (byte, bool) {
		if cell == testCell {
			return testRelayCode, true
		}
		return tenancy.RelayCode(cell)
	}
}

var (
	relayShape  = regexp.MustCompile(`^[a-z]+\.[a-z]+\.q[a-z2-7]{6}@relay\.test$`)
	singleShape = regexp.MustCompile(`^[a-z]+\.[a-z]+\.[1-9][0-9]{3}@relay\.test$`)
)

// D3 with the cell's code (MH10 routes on it): the code is the seventh-from-last character,
// outside the 6 random ones, and single mode keeps 4 digits.
func TestMaskedRelayLocalPart(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		a, err := randomMaskedAddress("relay.test", testRelayCode)
		if err != nil || !relayShape.MatchString(a) {
			t.Fatalf("relay address %q: %v", a, err)
		}
		seen[a[strings.LastIndexByte(a, '.')+2:strings.IndexByte(a, '@')]] = true
		s, err := randomMaskedAddress("relay.test", 0)
		if err != nil || !singleShape.MatchString(s) {
			t.Fatalf("single-tenant address %q: %v", s, err)
		}
	}
	if len(seen) < 190 {
		t.Fatalf("6 random characters drew only %d distinct tails in 200", len(seen))
	}
}

// Through the route in multi mode: a member's masked address carries its org's cell's code;
// an org of a cell with no code, or of a cell not served, gets no address at all.
func TestMaskedCreateUsesTheOrgsCellCode(t *testing.T) {
	fx, _ := newOrgFixture(t)
	create := func() (int, string) {
		w := fx.call(fx.a, "POST", "/api/addresses/masked", `{"domain":"example.test"}`, nil)
		var m config.MaskedAddress
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m.Address
	}
	code, address := create()
	if code != 200 || !regexp.MustCompile(`^[a-z]+\.[a-z]+\.q[a-z2-7]{6}@example\.test$`).MatchString(address) {
		t.Fatalf("multi-mode masked address %d %q, want cell %s's code", code, address, testCell)
	}
	for name, cells := range map[string][]string{"cell without a code": {"nocode-1", testCell}, "cell not served": {"other-1"}} {
		fx.srv.cells = cells
		for _, org := range []string{orgA, orgB} {
			if _, err := fx.srv.tenancy.Exec(`UPDATE orgs SET home_region=? WHERE id=?`, cells[0], org); err != nil {
				t.Fatal(err)
			}
		}
		if name == "cell not served" {
			if _, err := fx.srv.tenancy.Exec(`UPDATE orgs SET home_region='elsewhere-1'`); err != nil {
				t.Fatal(err)
			}
		}
		if code, address := create(); code != 503 || address != "" {
			t.Fatalf("%s: %d %q, want 503 and no address", name, code, address)
		}
	}
	if _, err := fx.srv.maskedRelayCode(context.Background(), fx.a.ID); !errors.Is(err, tenancy.ErrOrgCell) {
		t.Fatalf("an org outside the served cells: %v", err)
	}
}
