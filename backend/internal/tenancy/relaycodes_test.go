package tenancy

import (
	"errors"
	"slices"
	"testing"
)

// releasedRelayCodes is every entry any release has carried. relayCodes must start with it,
// unchanged: a released code stays its cell's forever. Append here when you append there.
var releasedRelayCodes = []relayCode{}

func TestRelayCodeTableIsStableAndUnique(t *testing.T) {
	if err := checkRelayTable(relayCodes); err != nil {
		t.Fatal(err)
	}
	if len(relayCodes) < len(releasedRelayCodes) || !slices.Equal(relayCodes[:len(releasedRelayCodes)], releasedRelayCodes) {
		t.Fatalf("a released masked relay code was changed or removed: %v, released %v", relayCodes, releasedRelayCodes)
	}
	// A full table: every base32 character, one cell each.
	full := make([]relayCode, 0, len(relayAlphabet))
	for i := range len(relayAlphabet) {
		full = append(full, relayCode{relayAlphabet[i], "cell-" + string(relayAlphabet[i])})
	}
	if err := checkRelayTable(full); err != nil {
		t.Fatalf("a full table: %v", err)
	}
	for i, e := range full {
		if err := checkCellCodes(full, []string{e.cell}); err != nil || full[i].code != relayAlphabet[i] {
			t.Fatalf("%s: %v", e.cell, err)
		}
	}
}

// A code that is not a lowercase base32 character, a cell that is not a home region slug,
// and a code or cell listed twice are refused, and so is a cell with no code.
func TestRelayCodeTableRefuses(t *testing.T) {
	ok := relayCode{'a', "us-east-1"}
	for name, table := range map[string][]relayCode{
		"uppercase":  {{'A', "us-east-1"}},
		"digit 1":    {{'1', "us-east-1"}},
		"digit 8":    {{'8', "us-east-1"}},
		"digit 0":    {{'0', "us-east-1"}},
		"hyphen":     {{'-', "us-east-1"}},
		"dot":        {{'.', "us-east-1"}},
		"no code":    {{0, "us-east-1"}},
		"high byte":  {{0xe9, "us-east-1"}},
		"bad cell":   {{'a', "US-EAST-1"}},
		"empty cell": {{'a', ""}},
		"code twice": {ok, {'a', "us-east-2"}},
		"cell twice": {ok, {'b', "us-east-1"}},
	} {
		if err := checkRelayTable(table); err == nil {
			t.Errorf("%s: accepted %v", name, table)
		}
		if err := checkCellCodes(table, []string{"us-east-1"}); err == nil {
			t.Errorf("%s: start check accepted %v", name, table)
		}
	}
	table := []relayCode{ok, {'7', "eu-central-1"}}
	if err := checkCellCodes(table, []string{"eu-central-1", "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if err := checkCellCodes(table, []string{"us-east-1", "us-east-2"}); !errors.Is(err, ErrNoRelayCode) {
		t.Fatalf("a cell with no code: %v", err)
	}
	// Today no cell has a code, so no multi start passes (the owner assigns the launch cells).
	if err := CheckRelayCodes([]string{"e2e-1"}); len(relayCodes) == 0 && !errors.Is(err, ErrNoRelayCode) {
		t.Fatalf("an unassigned cell started: %v", err)
	}
}

func TestRelayCodeTableBuildGate(t *testing.T) {
	if taggedBuild {
		if err := CheckRelayCodes([]string{"e2e-1", "e2e-2"}); err != nil {
			t.Fatalf("tagged build CheckRelayCodes(e2e-1, e2e-2): %v", err)
		}
		if cell, ok := RelayCell('7'); !ok || cell != "e2e-1" {
			t.Fatalf("tagged build RelayCell('7') = (%q, %v), want (\"e2e-1\", true)", cell, ok)
		}
		if cell, ok := RelayCell('6'); !ok || cell != "e2e-2" {
			t.Fatalf("tagged build RelayCell('6') = (%q, %v), want (\"e2e-2\", true)", cell, ok)
		}
		if code, ok := RelayCode("e2e-2"); !ok || code != '6' {
			t.Fatalf("tagged build RelayCode(e2e-2) = (%q, %v), want '6'", code, ok)
		}
		if err := CheckRelayCodes([]string{"unassigned-1"}); !errors.Is(err, ErrNoRelayCode) {
			t.Fatalf("tagged build unassigned cell: %v, want ErrNoRelayCode", err)
		}
		return
	}
	if len(relayCodes) != 0 {
		t.Fatalf("release build relayCodes = %v, want empty", relayCodes)
	}
	if err := CheckRelayCodes([]string{"e2e-1"}); !errors.Is(err, ErrNoRelayCode) {
		t.Fatalf("release build CheckRelayCodes(e2e-1): %v, want ErrNoRelayCode", err)
	}
	if code, ok := RelayCode("e2e-1"); ok || code != 0 {
		t.Fatalf("release build RelayCode(e2e-1) = (%q, %v), want none", code, ok)
	}
	if cell, ok := RelayCell('7'); ok || cell != "" {
		t.Fatalf("release build RelayCell('7') = (%q, %v), want (\"\", false)", cell, ok)
	}
}
