package users

import (
	"context"
	"crypto/hkdf"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
)

// authSecretInfo is the HKDF label the browser uses, and is what
// domain-separates the authentication half from anything else ever derived from
// the same PBKDF2 stretch. It MUST match AUTH_INFO in
// frontend/src/lib/authSecret.ts.
const authSecretInfo = "kypost/auth/v1"

// authSecretStretchBytes and authSecretBytes mirror STRETCH_BYTES and
// AUTH_SECRET_BYTES in frontend/src/lib/authSecret.ts.
const (
	authSecretStretchBytes = 32
	authSecretBytes        = 32
)

// DefaultLoginIterations is the PBKDF2 work factor for new credentials. It
// matches keyVault.ts's DEFAULT_ITERATIONS so one stretch serves both halves.
const DefaultLoginIterations = 600_000

// loginIterations is DefaultLoginIterations outside tests; see
// SetLoginIterationsForTest.
var loginIterations = DefaultLoginIterations

// LoginIterations is the work factor issued to clients and used for
// credentials derived server-side.
func LoginIterations() int { return loginIterations }

// SetLoginIterationsForTest lowers the PBKDF2 work factor, and the accepted
// floor with it (see validateLoginIterations), so a test binary that mints
// hundreds of accounts finishes under -race. Panics outside a test binary.
func SetLoginIterationsForTest(n int) (restore func()) {
	if !testing.Testing() {
		panic("users.SetLoginIterationsForTest called outside a test binary")
	}
	if n < 1 || n > MaxLoginIterations {
		panic(fmt.Sprintf("users.SetLoginIterationsForTest: %d out of range", n))
	}
	previous := loginIterations
	loginIterations = n
	return func() { loginIterations = previous }
}

// DeriveAuthSecret reproduces, server-side, the value a browser derives from a
// password and the login parameters this server served it:
//
//	hex( HKDF-SHA256( PBKDF2-SHA256(password, base64decode(salt), iterations, 32),
//	                  salt = "", info = "kypost/auth/v1", 32 ) )
//
// Used where the server legitimately holds a plaintext password (admin create
// and reset, first-run bootstrap) so the account is stored derived from the
// start. Must stay byte-for-byte in step with frontend/src/lib/authSecret.ts;
// pinned by TestDeriveAuthSecretMatchesTheBrowser.
func DeriveAuthSecret(password, loginSalt string, iterations int) (string, error) {
	if err := validateLoginSalt(loginSalt); err != nil {
		return "", err
	}
	if err := validateLoginIterations(iterations); err != nil {
		return "", err
	}
	salt, err := base64.StdEncoding.DecodeString(loginSalt)
	if err != nil {
		return "", fmt.Errorf("decode login salt: %w", err)
	}

	stretch, err := pbkdf2.Key(sha256.New, password, salt, iterations, authSecretStretchBytes)
	if err != nil {
		return "", fmt.Errorf("stretch password: %w", err)
	}
	// Empty HKDF salt, matching the browser: the stretch is already
	// salted by PBKDF2, and the label is what provides separation.
	out, err := hkdf.Key(sha256.New, stretch, nil, authSecretInfo, authSecretBytes)
	if err != nil {
		return "", fmt.Errorf("derive auth secret: %w", err)
	}
	return hex.EncodeToString(out), nil
}

// DerivedCredential is the stored form of a password the server was handed:
// the Argon2id hash of its auth secret, plus the parameters a client needs to
// reproduce that secret.
type DerivedCredential struct {
	Hash       string
	Salt       string
	Iterations int
}

// DeriveCredential stores a server-known password in derived form under a
// fresh random salt, so the account never needs the plaintext again.
func DeriveCredential(ctx context.Context, password string) (DerivedCredential, error) {
	raw := make([]byte, MinLoginSaltBytes)
	if _, err := rand.Read(raw); err != nil {
		return DerivedCredential{}, err
	}
	c := DerivedCredential{Salt: base64.StdEncoding.EncodeToString(raw), Iterations: loginIterations}
	secret, err := DeriveAuthSecret(password, c.Salt, c.Iterations)
	if err != nil {
		return DerivedCredential{}, err
	}
	if c.Hash, err = HashPassword(ctx, secret); err != nil {
		return DerivedCredential{}, err
	}
	return c, nil
}

// apply stores c on u as its derived-auth credential.
func (c DerivedCredential) apply(u *User) {
	u.PasswordHash = c.Hash
	u.AuthDerivation = AuthDerivationPBKDF2
	u.LoginSalt = c.Salt
	u.LoginIterations = c.Iterations
}
