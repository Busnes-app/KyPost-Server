package users

import (
	"errors"
	"math"
	"os"
)

// This comparable snapshot excludes correspondence, key envelopes and routine
// profile updates. PGPRevision separately fences prepared cryptographic material.
type nativeSendAuthority struct {
	active, forced, totp, push                                      bool
	role                                                            Role
	password, derivation, salt, subject, issuer, source, totpSecret string
	iterations                                                      int
	revoked                                                         int64
}

func nativeSendState(u User) nativeSendAuthority {
	return nativeSendAuthority{u.Active, u.MustChangePassword, u.TOTPEnabled, u.PushMFAEnabled, u.Role, u.PasswordHash, u.AuthDerivation, u.LoginSalt, u.SSOSub, u.NativeMailboxIssuer, u.NativeMailboxSource, u.TOTPSecretEnc, u.LoginIterations, u.SSOLinkRevokedAt}
}

// Every existing-user writer holds both account locks at this shared sink.
// Comparing disk state also covers SSO writers outside mutateGuarded. A local
// deactivate/reactivate cycle must invalidate jobs even within one clock second.
func (s *Store) advanceNativeSendEpochs(f *usersFile) error {
	prior, err := s.readFileUnlocked()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	byID := make(map[string]User, len(prior.Users))
	for _, u := range prior.Users {
		byID[u.ID] = u
	}
	for i := range f.Users {
		u := &f.Users[i]
		old, exists := byID[u.ID]
		if !exists {
			u.NativeSendEpoch = 0
			continue
		}
		u.NativeSendEpoch = old.NativeSendEpoch
		if nativeSendState(*u) != nativeSendState(old) {
			if u.NativeSendEpoch == math.MaxUint64 {
				return errors.New("account authority exhausted; update refused without changing data")
			}
			u.NativeSendEpoch++
		}
	}
	return nil
}
