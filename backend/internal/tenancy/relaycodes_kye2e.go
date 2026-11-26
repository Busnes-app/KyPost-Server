//go:build kye2e

package tenancy

// init registers the suite end-to-end test cells in kye2e builds only.
// Release builds keep relayCodes empty until launch cells are assigned.
func init() {
	relayCodes = append(relayCodes,
		relayCode{'a', "e2e-1"},
		relayCode{'b', "e2e-2"},
	)
}
