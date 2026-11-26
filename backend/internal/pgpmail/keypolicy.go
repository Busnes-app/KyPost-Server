package pgpmail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// ErrKeyPolicy refuses a public key whose OpenPGP version or algorithm is not
// on KyPost's allowlist (docs/STRONG-CRYPTO.md).
var ErrKeyPolicy = errors.New("KyPost accepts only v4 keys (Ed25519, Curve25519, RSA-2048 or larger, ML-KEM-768+X25519 subkeys) and v6 keys (Ed25519, X25519, ML-DSA-65+Ed25519, ML-KEM-768+X25519)")

// keyPolicy lists the allowed algorithms per key version and role. A v4
// EdDSA signing subkey is allowed: GnuPG smartcard setups make one, and it
// adds no algorithm the primary list lacks. Level-5 PQC (31, 36) waits for
// the owner's answer to spec Q7; pre-RFC 9980 code points (105, 107) never.
var keyPolicy = map[byte]struct{ primary, subkey []packet.PublicKeyAlgorithm }{
	4: {
		primary: []packet.PublicKeyAlgorithm{packet.PubKeyAlgoRSA, packet.PubKeyAlgoEdDSA},
		subkey:  []packet.PublicKeyAlgorithm{packet.PubKeyAlgoRSA, packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoECDH, packet.PubKeyAlgoMlkem768X25519},
	},
	6: {
		primary: []packet.PublicKeyAlgorithm{packet.PubKeyAlgoEd25519, packet.PubKeyAlgoMldsa65Ed25519},
		subkey:  []packet.PublicKeyAlgorithm{packet.PubKeyAlgoEd25519, packet.PubKeyAlgoX25519, packet.PubKeyAlgoMldsa65Ed25519, packet.PubKeyAlgoMlkem768X25519},
	},
}

// CheckArmoredKeyPolicy is CheckKeyPolicy for an armored key.
func CheckArmoredKeyPolicy(armored string) error {
	block, err := armor.Decode(strings.NewReader(strings.TrimSpace(armored)))
	if err != nil {
		return fmt.Errorf("pgpmail: dearmor key: %w", err)
	}
	binary, err := io.ReadAll(block.Body)
	if err != nil {
		return fmt.Errorf("pgpmail: dearmor key: %w", err)
	}
	return CheckKeyPolicy(binary)
}

// CheckKeyPolicy refuses a binary key unless every key packet in it is on the
// allowlist. It reads the raw packets because go-crypto silently skips a
// subkey whose algorithm it does not know, so a parsed key cannot show a
// pre-RFC 105/107 subkey that another client would still use.
func CheckKeyPolicy(binary []byte) error {
	reader := packet.NewOpaqueReader(bytes.NewReader(binary))
	var primaryVersion byte
	for {
		op, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("pgpmail: read key packets: %w", err)
		}
		subkey := false
		switch op.Tag {
		case 6, 5: // public key, secret key
		case 14, 7: // public subkey, secret subkey
			subkey = true
		default:
			continue
		}
		// Every key version keeps the algorithm at byte 5: version, a 4-byte
		// creation time, then the algorithm.
		if len(op.Contents) < 6 {
			return fmt.Errorf("pgpmail: truncated key packet: %w", ErrKeyPolicy)
		}
		version, algo := op.Contents[0], packet.PublicKeyAlgorithm(op.Contents[5])
		if !subkey {
			primaryVersion = version
		} else if version != primaryVersion {
			return fmt.Errorf("pgpmail: v%d subkey under a v%d primary key: %w", version, primaryVersion, ErrKeyPolicy)
		}
		allowed, ok := keyPolicy[version]
		if !ok {
			return fmt.Errorf("pgpmail: OpenPGP v%d key: %w", version, ErrKeyPolicy)
		}
		list, role := allowed.primary, "primary key"
		if subkey {
			list, role = allowed.subkey, "subkey"
		}
		if !slices.Contains(list, algo) {
			return fmt.Errorf("pgpmail: v%d %s uses public-key algorithm %d: %w", version, role, algo, ErrKeyPolicy)
		}
		if err := checkKeyParameters(op, algo); err != nil {
			return err
		}
	}
	if primaryVersion == 0 {
		return fmt.Errorf("pgpmail: no key packet: %w", ErrKeyPolicy)
	}
	return nil
}

// checkKeyParameters enforces the RSA-2048 floor and limits the v4 elliptic
// algorithms to Curve25519; the v6 and PQC algorithms fix their own curves.
func checkKeyParameters(op *packet.OpaquePacket, algo packet.PublicKeyAlgorithm) error {
	if algo != packet.PubKeyAlgoRSA && algo != packet.PubKeyAlgoEdDSA && algo != packet.PubKeyAlgoECDH {
		return nil
	}
	parsed, err := op.Parse()
	if err != nil {
		return fmt.Errorf("pgpmail: parse key packet: %w", err)
	}
	var pub *packet.PublicKey
	switch p := parsed.(type) {
	case *packet.PublicKey:
		pub = p
	case *packet.PrivateKey:
		pub = &p.PublicKey
	default:
		return fmt.Errorf("pgpmail: unexpected key packet: %w", ErrKeyPolicy)
	}
	if algo == packet.PubKeyAlgoRSA {
		bits, err := pub.BitLength()
		if err != nil || bits < 2048 {
			return fmt.Errorf("pgpmail: key %X uses RSA-%d: %w: %w", pub.Fingerprint, bits, ErrKeyPolicy, ErrWeakRSAKey)
		}
		return nil
	}
	if curve, err := pub.Curve(); err != nil || curve != packet.Curve25519 {
		return fmt.Errorf("pgpmail: key %X uses curve %q: %w", pub.Fingerprint, curve, ErrKeyPolicy)
	}
	return nil
}
