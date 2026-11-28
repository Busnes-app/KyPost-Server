//go:build kye2e

package tenancy

// TEST-ONLY, throwaway: the suite end-to-end cells' relay codes, in kye2e builds only, until
// the system goes live. Never copy these into relayCodes (the live table, relaycodes.go): they
// are taken from the end of the alphabet so the live allocation, from its start, never
// collides with them, and a release build has neither entry.
func init() {
	relayCodes = append(relayCodes,
		relayCode{'7', "e2e-1"},
		relayCode{'6', "e2e-2"},
	)
}
