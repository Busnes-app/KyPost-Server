package pgpmailtest

import (
	"bytes"
	"io"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// PolicyKey is a public key the key allowlist must accept or refuse.
type PolicyKey struct {
	Name    string
	Armored string
	Allowed bool
}

// PolicyKeys returns one public key per allowlist row, each carrying email as
// its User ID, so every entry point can be tested against the same set.
func PolicyKeys(email string) ([]PolicyKey, error) {
	gen := func(v6 bool, algo packet.PublicKeyAlgorithm, curve packet.Curve, bits int) (*openpgp.Entity, error) {
		return openpgp.NewEntity("Policy", "", email, &packet.Config{V6Keys: v6, Algorithm: algo, Curve: curve, RSABits: bits})
	}
	var out []PolicyKey
	add := func(name string, allowed bool, e *openpgp.Entity, err error, edit func([]*packet.OpaquePacket) []*packet.OpaquePacket) error {
		if err != nil {
			return err
		}
		armored, err := armorPackets(e, edit)
		out = append(out, PolicyKey{name, armored, allowed})
		return err
	}
	// withSubkey adds a subkey to a fresh v4 Ed25519 key.
	withSubkey := func(sub *packet.Config, signing bool) (*openpgp.Entity, error) {
		e, err := gen(false, packet.PubKeyAlgoEdDSA, packet.Curve25519, 0)
		if err != nil {
			return nil, err
		}
		if signing {
			return e, e.AddSigningSubkey(sub)
		}
		return e, e.AddEncryptionSubkey(sub)
	}
	retag := func(primary, subkey packet.PublicKeyAlgorithm) func([]*packet.OpaquePacket) []*packet.OpaquePacket {
		return func(ps []*packet.OpaquePacket) []*packet.OpaquePacket {
			for _, p := range ps {
				switch {
				case p.Tag == 6 && primary != 0:
					p.Contents[5] = byte(primary)
				case p.Tag == 14 && subkey != 0:
					p.Contents[5] = byte(subkey)
				}
			}
			return ps
		}
	}
	v4, err := gen(false, packet.PubKeyAlgoEdDSA, packet.Curve25519, 0)
	if err != nil {
		return nil, err
	}
	// A v4 subkey and its binding signature, spliced under a v6 primary.
	var v4Subkey []*packet.OpaquePacket
	if _, err := armorPackets(v4, func(ps []*packet.OpaquePacket) []*packet.OpaquePacket {
		for i, p := range ps {
			if p.Tag == 14 {
				v4Subkey = ps[i:]
			}
		}
		return ps
	}); err != nil {
		return nil, err
	}
	steps := []func() error{
		func() error { return add("v4 Ed25519+cv25519", true, v4, nil, nil) },
		func() error {
			e, err := gen(false, packet.PubKeyAlgoRSA, "", 2048)
			return add("v4 RSA-2048", true, e, err, nil)
		},
		func() error {
			e, err := withSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}, true)
			return add("v4 Ed25519 signing subkey", true, e, err, nil)
		},
		func() error {
			e, err := withSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoMlkem768X25519}, false)
			return add("v4 with ML-KEM-768+X25519 subkey", true, e, err, nil)
		},
		func() error {
			e, err := gen(true, packet.PubKeyAlgoEd25519, "", 0)
			return add("v6 Ed25519+X25519", true, e, err, nil)
		},
		func() error {
			e, err := gen(true, packet.PubKeyAlgoMldsa65Ed25519, "", 0)
			return add("v6 ML-DSA-65+Ed25519 / ML-KEM-768+X25519", true, e, err, nil)
		},
		func() error {
			e, err := gen(false, packet.PubKeyAlgoRSA, "", 1024)
			return add("v4 RSA-1024", false, e, err, nil)
		},
		func() error {
			e, err := gen(false, packet.PubKeyAlgoECDSA, packet.CurveNistP256, 0)
			return add("v4 ECDSA P-256", false, e, err, nil)
		},
		func() error {
			e, err := withSubkey(&packet.Config{Algorithm: packet.PubKeyAlgoECDH, Curve: packet.CurveNistP256}, false)
			return add("v4 with ECDH P-256 subkey", false, e, err, nil)
		},
		func() error {
			e, err := gen(false, packet.PubKeyAlgoEd25519, "", 0)
			return add("v4 Ed25519 (algorithm 27)", false, e, err, nil)
		},
		func() error {
			e, err := gen(true, packet.PubKeyAlgoRSA, "", 2048)
			return add("v6 RSA-2048", false, e, err, nil)
		},
		func() error {
			e, err := gen(true, packet.PubKeyAlgoMldsa87Ed448, "", 0)
			return add("v6 ML-DSA-87+Ed448 / ML-KEM-1024+X448", false, e, err, nil)
		},
		// openpgp.js 6.x's pre-RFC 9980 code points. go-crypto silently skips
		// the unknown subkey, so only the raw packet scan sees it.
		func() error { return add("v4 with pre-RFC 105 subkey", false, v4, nil, retag(0, 105)) },
		func() error {
			e, err := gen(true, packet.PubKeyAlgoMldsa65Ed25519, "", 0)
			return add("v6 pre-RFC 107 / 105", false, e, err, retag(107, 105))
		},
		func() error {
			e, err := gen(true, packet.PubKeyAlgoEd25519, "", 0)
			return add("v6 with a v4 subkey", false, e, err, func(ps []*packet.OpaquePacket) []*packet.OpaquePacket {
				return append(ps, v4Subkey...)
			})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// armorPackets serializes e's public key, lets edit rewrite its raw packets,
// and armors the result.
func armorPackets(e *openpgp.Entity, edit func([]*packet.OpaquePacket) []*packet.OpaquePacket) (string, error) {
	var raw bytes.Buffer
	if err := e.Serialize(&raw); err != nil {
		return "", err
	}
	var packets []*packet.OpaquePacket
	r := packet.NewOpaqueReader(&raw)
	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		packets = append(packets, p)
	}
	if edit != nil {
		packets = edit(packets)
	}
	var out bytes.Buffer
	w, err := armor.Encode(&out, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", err
	}
	for _, p := range packets {
		if err := p.Serialize(w); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}
