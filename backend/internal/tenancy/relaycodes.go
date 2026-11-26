package tenancy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// relayAlphabet is RFC 4648 base32, lowercase (masked addresses spec D3), so at most 32 cells.
const relayAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// relayCode is one cell's masked relay region code (multi-host spec, "Mail per region", MH9):
// the character right after `adjective.animal.` in a relay-domain masked address.
type relayCode struct {
	code byte
	cell string
}

// relayCodes is the masked relay region code table. Append only: an entry, once released,
// is never changed, removed or reused, because every address allocated under it keeps the
// code. A closed cell keeps its entry. No cell has one yet: the owner assigns the launch
// cells (multi-host spec, "MH9 region codes and recovery as built"), and until then no cell
// can start multi mode.
var relayCodes = []relayCode{}

var cellSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// checkRelayTable refuses a table with a code outside relayAlphabet, a cell that is not a
// KyIdentity home region slug, or a code or cell listed twice.
func checkRelayTable(table []relayCode) error {
	codes, cells := map[byte]bool{}, map[string]bool{}
	for _, e := range table {
		switch {
		case !strings.ContainsRune(relayAlphabet, rune(e.code)):
			return fmt.Errorf("tenancy: masked relay code %q for %s is not a lowercase base32 character", e.code, e.cell)
		case !cellSlug.MatchString(e.cell):
			return fmt.Errorf("tenancy: masked relay code %q names %q, not a KyIdentity home region slug", e.code, e.cell)
		case codes[e.code]:
			return fmt.Errorf("tenancy: masked relay code %q is assigned twice", e.code)
		case cells[e.cell]:
			return fmt.Errorf("tenancy: cell %s has two masked relay codes", e.cell)
		}
		codes[e.code], cells[e.cell] = true, true
	}
	return nil
}

// ErrNoRelayCode refuses a multi start serving a cell that has no masked relay region code.
var ErrNoRelayCode = errors.New("KY_CELLS names a cell with no masked relay region code")

// CheckRelayCodes is the multi start check: the table is well formed and every cell this
// deployment serves has a code, so K PR 7 can never allocate a relay address without one.
func CheckRelayCodes(cells []string) error {
	return checkCellCodes(relayCodes, cells)
}

func checkCellCodes(table []relayCode, cells []string) error {
	if err := checkRelayTable(table); err != nil {
		return err
	}
	for _, c := range cells {
		found := false
		for _, e := range table {
			found = found || e.cell == c
		}
		if !found {
			return fmt.Errorf("%w: %s; see docs/CELL-RECOVERY.md, \"Region codes\"", ErrNoRelayCode, c)
		}
	}
	return nil
}

// RelayCell is the cell whose masked relay region code is code, if the table has one. An MX
// host sends a relay address with no such cell to its own cell, which answers it as unknown
// (MH10).
func RelayCell(code byte) (string, bool) {
	for _, e := range relayCodes {
		if e.code == code {
			return e.cell, true
		}
	}
	return "", false
}
