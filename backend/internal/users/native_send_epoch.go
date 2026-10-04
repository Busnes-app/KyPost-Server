package users

import (
	"errors"
	"math"
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
	native := false
	for _, u := range f.Users {
		native = native || u.NativeMailboxSource != "" || u.NativeMailboxIssuer != ""
	}
	if !native {
		return nil
	}
	prior, err := s.readFileUnlocked()
	if err != nil {
		return err
	}
	byID := make(map[string]User, len(prior.Users))
	for _, u := range prior.Users {
		byID[u.ID] = u
	}
	for i := range f.Users {
		u := &f.Users[i]
		if u.NativeMailboxSource == "" && u.NativeMailboxIssuer == "" {
			continue
		}
		old, exists := byID[u.ID]
		if !exists {
			u.NativeSendEpoch = 0
			continue
		}
		u.NativeSendEpoch = old.NativeSendEpoch
		if nativeSendState(*u) != nativeSendState(old) {
			if u.NativeSendEpoch == math.MaxUint64 {
				return errors.New("native send authority exhausted; update refused without changing data")
			}
			u.NativeSendEpoch++
		}
	}
	return nil
}
