//go:build kye2e

package sso

import "testing"

// The end-to-end test build neither saves nor loads an issuer outside .test, so a test image
// pointed at a real issuer by mistake has no sign-in, directory or webhook issuer at all.
func TestE2EBuildRefusesARealIssuer(t *testing.T) {
	if err := ValidateIssuerURL("https://auth.example.com", false); err == nil {
		t.Fatal("test build validated a real issuer")
	}
	if err := ValidateIssuerURL("https://id.e2e.test", false); err != nil {
		t.Fatalf("test build refused a .test issuer: %v", err)
	}
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(testSettings("https://auth.example.com")); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(dir).Load(); got.Enabled || got.IssuerURL != "" {
		t.Fatalf("test build loaded a real issuer: %+v", got)
	}
	if err := store.Save(testSettings("https://id.e2e.test")); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(dir).Load(); !got.Enabled || got.IssuerURL != "https://id.e2e.test" {
		t.Fatalf("test build did not load a .test issuer: %+v", got)
	}
}
