package cryptutil

import (
	"crypto/rsa"
	"crypto/tls"
	"fmt"
)

// RequireStrongCertificateKey refuses a TLS certificate whose RSA key is below
// 2048 bits. Go serves one even with GODEBUG=fips140=on.
func RequireStrongCertificateKey(pair tls.Certificate) error {
	if key, ok := pair.PrivateKey.(*rsa.PrivateKey); ok && key.N.BitLen() < 2048 {
		return fmt.Errorf("certificate uses RSA-%d; at least 2048 bits required", key.N.BitLen())
	}
	return nil
}
