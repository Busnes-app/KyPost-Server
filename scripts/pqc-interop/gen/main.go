// Command gen writes public certificates for the OpenPGP v4/v6 interop check
// (docs/superpowers/specs/2026-11-25-pqc-openpgp-v6-design.md, P5 interop matrix).
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/ProtonMail/gopenpgp/v3/profile"
)

func cert(p *crypto.PGPHandle) []byte {
	k, err := p.KeyGeneration().AddUserId("Alice", "alice@example.test").New().GenerateKey()
	if err != nil {
		log.Fatal(err)
	}
	pub, err := k.ToPublic()
	if err != nil {
		log.Fatal(err)
	}
	b, err := pub.Serialize()
	if err != nil {
		log.Fatal(err)
	}
	return b
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: gen <out-dir>")
	}
	v4 := cert(crypto.PGPWithProfile(profile.Default()))
	v6 := cert(crypto.PGPWithProfile(profile.RFC9580()))
	pq := cert(crypto.PGPWithProfile(profile.PQC()))
	cat := func(a, b []byte) []byte { return append(append([]byte{}, a...), b...) }
	for name, b := range map[string][]byte{
		"v4.pgp": v4, "v6-ed25519.pgp": v6, "v6-pqc.pgp": pq,
		"bundle-v4-v6pqc.pgp": cat(v4, pq), "bundle-v6pqc-v4.pgp": cat(pq, v4), "bundle-v4-v6.pgp": cat(v4, v6),
	} {
		if err := os.WriteFile(filepath.Join(os.Args[1], name), b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
