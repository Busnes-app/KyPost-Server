package users

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Busnes-app/ky-primitives/password"
)

func fastParams() password.Params { return password.Params{Memory: 8 * 1024, Time: 1, Threads: 1} }

func TestHashPasswordIsArgon2idAndVerifies(t *testing.T) {
	defer SetHashParamsForTest(fastParams())()
	ctx := context.Background()
	h, err := HashPassword(ctx, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash %q is not Argon2id PHC", h)
	}
	if ok, err := VerifySecretHash(ctx, h, "correct horse"); err != nil || !ok {
		t.Fatalf("verify: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifySecretHash(ctx, h, "wrong"); ok {
		t.Fatal("wrong password verified")
	}
	if NeedsRehash(h) {
		t.Fatal("fresh hash reports NeedsRehash")
	}
}

// retiredScryptHash has the shape of a pre-Argon2id hash (scrypt$N$r$p$salt$key)
// at the last production cost. Nothing verifies it any more.
const retiredScryptHash = "scrypt$131072$8$1$c2FsdHNhbHRzYWx0c2FsdA==$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g="

func TestRetiredScryptHashFailsLikeAWrongPasswordAndWarns(t *testing.T) {
	defer SetHashParamsForTest(fastParams())()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	u := User{ID: "user-123", PasswordHash: retiredScryptHash, AuthDerivation: AuthDerivationPBKDF2}
	ok, err := VerifyAuthSecret(context.Background(), u, "any password")
	if ok || err != nil {
		t.Fatalf("retired hash: ok=%v err=%v, want false,nil", ok, err)
	}
	if NeedsRehash(retiredScryptHash) {
		t.Fatal("a retired hash cannot be rehashed: it never verifies")
	}
	out := logs.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, `"user_id":"user-123"`) {
		t.Fatalf("want one warning naming the user, got %q", out)
	}
	if strings.Contains(out, "scrypt$") {
		t.Fatalf("warning leaked the stored hash: %q", out)
	}
}

func TestMalformedHashIsFalseNotError(t *testing.T) {
	defer SetHashParamsForTest(fastParams())()
	ctx := context.Background()
	for _, h := range []string{"", "$argon2id$garbage", "scrypt$1$2", retiredScryptHash, "sha256:abc"} {
		ok, err := VerifySecretHash(ctx, h, "x")
		if ok || err != nil {
			t.Fatalf("%q: ok=%v err=%v, want false,nil", h, ok, err)
		}
	}
}

func TestBusyIsKDFBusy(t *testing.T) {
	// password.ErrBusy from the library must surface as ErrKDFBusy so callers
	// answer 503 and spend no lockout strike.
	if !errors.Is(mapBusy(password.ErrBusy), ErrKDFBusy) {
		t.Fatal("password.ErrBusy not mapped to ErrKDFBusy")
	}
	if mapBusy(nil) != nil {
		t.Fatal("nil error changed")
	}
}
