package pgpautocrypt

import (
	"strings"
	"testing"
)

// header builds an Autocrypt value whose whitespace-free length is exactly n
// (n a multiple of 4, so the keydata stays valid base64).
func header(n int) string {
	prefix := "addr=ab@example.org;keydata=" // 28 bytes
	return prefix + strings.Repeat("A", n-len(prefix))
}

func TestParseAutocryptHeaderBounds(t *testing.T) {
	// Exactly at the cap, folded with whitespace that does not count: accepted.
	at := strings.Replace(header(MaxHeaderBytes), "keydata=", "keydata=\r\n ", 1)
	if _, _, err := ParseAutocryptHeader(at); err != nil {
		t.Fatalf("header at the cap refused: %v", err)
	}
	if _, _, err := ParseAutocryptHeader(header(MaxHeaderBytes + 4)); err == nil {
		t.Fatal("header over the cap accepted")
	}
	if _, _, err := ParseAutocryptHeader(header(11 << 10)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("11 KiB header: %v", err)
	}
}
