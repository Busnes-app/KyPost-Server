package state

import (
	"slices"
	"strings"
)

// PGPV6Ready is computed on each use, never persisted as a rollout flag. A
// codec report alone is not possession: every paired device must have confirmed
// the current complete ring over PQ transport. Unknown and failed reads close
// the gate, including devices whose previous enrollment was cleared by growth.
func (s *Store) PGPV6Ready(generation uint64, fingerprint string) (bool, error) {
	devices, err := s.ListNativeDevicesStrict()
	if err != nil {
		return false, err
	}
	for _, d := range devices {
		if !slices.Contains(d.OpenPGP, "v6") || !slices.Contains(d.OpenPGP, "rfc9980") ||
			!d.EncryptionEnrolled || d.EnrolledVersion != 6 || d.EnrolledGeneration != generation ||
			!strings.EqualFold(d.EnrolledFingerprint, fingerprint) {
			return false, nil
		}
	}
	return true, nil
}
