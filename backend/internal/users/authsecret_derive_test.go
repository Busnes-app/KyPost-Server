package users

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMain lowers the server-side PBKDF2 cost to the floor so the many Create
// calls here finish; TestDeriveAuthSecretMatchesTheBrowser pins the algorithm.
func TestMain(m *testing.M) {
	restore := SetLoginIterationsForTest(MinLoginIterations)
	code := m.Run()
	restore()
	os.Exit(code)
}

// verifyPassword checks a typed password the way a browser would: derive the
// auth secret from the stored login parameters, then verify that.
func verifyPassword(ctx context.Context, u User, password string) (bool, error) {
	if !u.UsesDerivedAuth() {
		return false, nil
	}
	secret, err := DeriveAuthSecret(password, u.LoginSalt, u.LoginIterations)
	if err != nil {
		return false, err
	}
	return VerifyAuthSecret(ctx, u, secret)
}

// TestServerSetPasswordsAreStoredDerived: every path where the server is handed
// a plaintext password stores the derived form, so no new legacy account exists.
func TestServerSetPasswordsAreStoredDerived(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := LoadOrMigrate(ctx, dir, filepath.Join(dir, "admin.env"))
	if err != nil {
		t.Fatalf("LoadOrMigrate: %v", err)
	}
	first, err := store.FirstAdmin()
	if err != nil {
		t.Fatalf("FirstAdmin: %v", err)
	}
	created, err := store.Create(ctx, "created", "created-test-password", RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	reset, err := store.SetPassword(ctx, created.ID, "reset-test-password", true, nil)
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	for name, u := range map[string]User{"first-run": first, "create": created, "reset": reset} {
		if !u.UsesDerivedAuth() || u.NeedsPasswordReset() || u.LoginIterations != LoginIterations() {
			t.Errorf("%s: stored legacy or with unexpected parameters: derivation=%q iterations=%d", name, u.AuthDerivation, u.LoginIterations)
		}
		if validateLoginSalt(u.LoginSalt) != nil {
			t.Errorf("%s: invalid login salt %q", name, u.LoginSalt)
		}
	}
	if created.LoginSalt == reset.LoginSalt {
		t.Error("a reset reused the previous salt")
	}
	if ok, _ := verifyPassword(ctx, reset, "reset-test-password"); !ok {
		t.Error("the reset password does not verify through its derived secret")
	}
	if ok, _ := VerifyAuthSecret(ctx, reset, "reset-test-password"); ok {
		t.Error("the plaintext itself verified as the credential")
	}
}

// TestLegacyAccountNeedsPasswordReset: a stored plaintext hash never verifies
// and is flagged for the admin.
func TestLegacyAccountNeedsPasswordReset(t *testing.T) {
	ctx := context.Background()
	hash, err := HashPassword(ctx, "legacy-test-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	legacy := User{ID: "legacy", PasswordHash: hash}
	if !legacy.NeedsPasswordReset() || !legacy.Public().NeedsPasswordReset {
		t.Fatal("legacy account is not flagged for reset")
	}
	if ok, _ := VerifyAuthSecret(ctx, legacy, "legacy-test-password"); ok {
		t.Fatal("legacy plaintext hash verified")
	}
	if (User{ID: "sso-only"}).NeedsPasswordReset() {
		t.Fatal("an account with no local credential was flagged")
	}
}

// TestDeriveAuthSecretMatchesTheBrowser is the cross-implementation pin.
//
// The server's derivation only means anything if it produces the SAME bytes the
// browser produces — a mismatch would not fail closed, it would lock every
// admin-created or admin-reset account out of its first sign-in. The expected value below was
// computed with WebCrypto using exactly the algorithm in
// frontend/src/lib/authSecret.ts:
//
//	PBKDF2-SHA256(password, base64decode(salt), iterations, 32)
//	  -> HKDF-SHA256(stretch, salt="", info="kypost/auth/v1", 32) -> hex
//
// If this test fails, the two implementations have drifted and sign-in for
// server-set passwords is broken; do not "fix" it by changing the constant.
func TestDeriveAuthSecretMatchesTheBrowser(t *testing.T) {
	const (
		password = "correct-horse-battery-staple"
		salt     = "AAAAAAAAAAAAAAAAAAAAAA=="
		want     = "da9fc5d333fdab121f3c302b6191b68657bf806030a32c0cbd47b39b720dc860"
	)
	got, err := DeriveAuthSecret(password, salt, MinLoginIterations)
	if err != nil {
		t.Fatalf("DeriveAuthSecret: %v", err)
	}
	if got != want {
		t.Fatalf("server derivation disagrees with frontend/src/lib/authSecret.ts:\n got  %s\n want %s", got, want)
	}
}
