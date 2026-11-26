//go:build js && wasm

// Run under Node: PATH="$(go env GOROOT)/lib/wasm:$PATH" GOOS=js GOARCH=wasm go test ./cmd/pgpwasm
package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

type pair struct{ priv, pub string }

func gen(t *testing.T, profile, email string) pair {
	t.Helper()
	priv, pub, fp, err := generate(profile, []userID{{"Test", email}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"v4": 40, "v6": 64}[profile]
	if len(fp) != want {
		t.Fatalf("%s fingerprint %q, want %d hex", profile, fp, want)
	}
	return pair{priv, pub}
}

// packetsOf lists the packet types of an armored message, outer layer only.
func packetsOf(t *testing.T, armored string) []packet.Packet {
	t.Helper()
	block, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		t.Fatal(err)
	}
	var out []packet.Packet
	r := packet.NewReader(block.Body)
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
		if _, ok := p.(*packet.SymmetricallyEncrypted); ok {
			return out
		}
	}
}

// internal/pgpmail does not build for js/wasm (fsutil uses flock and
// statfs), so this uses the same calls its EncryptStoredMIME and the test
// decryptor make: crypto.PGP() encryption (mime.go) and go-crypto reading.
// ring reads armored keys one block at a time: go-crypto's armored reader
// stops after the first block.
func ring(t *testing.T, armored ...string) openpgp.EntityList {
	t.Helper()
	var out openpgp.EntityList
	for _, a := range armored {
		el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(a))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, el...)
	}
	return out
}

func TestV4InteropWithServerPath(t *testing.T) {
	me := gen(t, "v4", "me@example.test")
	pub, err := crypto.NewKeyFromArmored(me.pub)
	if err != nil {
		t.Fatal(err)
	}
	h, err := crypto.PGP().Encryption().Recipient(pub).New()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := h.Encrypt([]byte("server to browser"))
	if err != nil {
		t.Fatal(err)
	}
	armored, err := stored.Armor()
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := decrypt([]byte(armored), [][]byte{[]byte(me.priv)}, nil, 1<<20)
	if err != nil || string(data) != "server to browser" {
		t.Fatalf("bridge could not open server-style ciphertext: %v %q", err, data)
	}

	// Bridge v4 output: v3 PKESK + SEIPDv1, as the server writes today, and
	// go-crypto opens and verifies it.
	other := gen(t, "v4", "server@example.test")
	msg, err := encrypt([]byte("browser to server"), [][]byte{[]byte(other.pub)}, []byte(me.priv))
	if err != nil {
		t.Fatal(err)
	}
	pkts := packetsOf(t, msg)
	if ek, ok := pkts[0].(*packet.EncryptedKey); !ok || ek.Version != 3 {
		t.Fatalf("want v3 PKESK, got %#v", pkts[0])
	}
	if se, ok := pkts[len(pkts)-1].(*packet.SymmetricallyEncrypted); !ok || se.Version != 1 {
		t.Fatalf("want SEIPDv1, got %#v", pkts[len(pkts)-1])
	}
	keys := ring(t, other.priv, me.pub)
	block, err := armor.Decode(strings.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	md, err := openpgp.ReadMessage(block.Body, keys, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(md.UnverifiedBody)
	if err != nil || string(body) != "browser to server" || md.SignatureError != nil || md.Signature == nil {
		t.Fatalf("go-crypto: %v %q sigErr=%v", err, body, md.SignatureError)
	}
}

func TestV6IsPQAndOpensWithGoCrypto(t *testing.T) {
	alice, bob := gen(t, "v6", "alice@example.test"), gen(t, "v6", "bob@example.test")
	info, err := inspect([]byte(alice.pub))
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != 6 || info.Algorithms[0] != int(packet.PubKeyAlgoMldsa65Ed25519) || info.Algorithms[1] != int(packet.PubKeyAlgoMlkem768X25519) {
		t.Fatalf("v6 key: %+v", info)
	}
	msg, err := encrypt([]byte("post-quantum"), [][]byte{[]byte(bob.pub)}, []byte(alice.priv))
	if err != nil {
		t.Fatal(err)
	}
	// Only v6 PKESKs, to ML-KEM-768+X25519, then SEIPDv2 with OCB.
	pkts := packetsOf(t, msg)
	for _, p := range pkts[:len(pkts)-1] {
		ek, ok := p.(*packet.EncryptedKey)
		if !ok || ek.Version != 6 || ek.Algo != packet.PubKeyAlgoMlkem768X25519 {
			t.Fatalf("want only v6 ML-KEM PKESKs, got %#v", p)
		}
	}
	seipd, ok := pkts[len(pkts)-1].(*packet.SymmetricallyEncrypted)
	if !ok || seipd.Version != 2 || seipd.Mode != packet.AEADModeOCB {
		t.Fatalf("want SEIPDv2 OCB, got %#v", pkts[len(pkts)-1])
	}

	// go-crypto (the server's library), not gopenpgp, opens and verifies it.
	keys := ring(t, bob.priv, alice.pub)
	block, err := armor.Decode(strings.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	md, err := openpgp.ReadMessage(block.Body, keys, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(md.UnverifiedBody)
	if err != nil || string(body) != "post-quantum" || md.SignatureError != nil || md.Signature == nil ||
		md.Signature.PubKeyAlgo != packet.PubKeyAlgoMldsa65Ed25519 {
		t.Fatalf("go-crypto: %v %q sigErr=%v", err, body, md.SignatureError)
	}
}

func TestRefusals(t *testing.T) {
	v4, v6, other := gen(t, "v4", "a@example.test"), gen(t, "v6", "b@example.test"), gen(t, "v6", "c@example.test")
	if _, err := encrypt([]byte("x"), [][]byte{[]byte(v4.pub), []byte(v6.pub)}, nil); err == nil {
		t.Fatal("one message mixed v4 and v6 recipients")
	}
	if _, err := encrypt([]byte("x"), [][]byte{[]byte(v6.pub)}, []byte(v4.priv)); err == nil {
		t.Fatal("a v6 delivery was signed with the v4 key")
	}
	msg, err := encrypt([]byte("x"), [][]byte{[]byte(v6.pub)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decrypt([]byte(msg), [][]byte{[]byte(other.priv)}, nil, 1<<20); err == nil {
		t.Fatal("decrypted with a key the message was not encrypted to")
	}
	if _, _, err := decrypt([]byte(msg), [][]byte{[]byte(v6.pub)}, nil, 1<<20); err == nil {
		t.Fatal("accepted a public key as a decryption key")
	}
}

func TestDetachedSignatureAndArmor(t *testing.T) {
	for _, profile := range []string{"v4", "v6"} {
		k := gen(t, profile, "s@example.test")
		sig, hash, err := signDetached([]byte("signed part"), []byte(k.priv))
		if err != nil || hash == 0 {
			t.Fatalf("%s sign: %v hash=%d", profile, err, hash)
		}
		checks, err := verifyDetached([]byte("signed part"), []byte(sig), [][]byte{[]byte(k.pub)})
		if err != nil || len(checks) != 1 || !checks[0].Valid {
			t.Fatalf("%s verify: %v %+v", profile, err, checks)
		}
		checks, err = verifyDetached([]byte("signed parT"), []byte(sig), [][]byte{[]byte(k.pub)})
		if err != nil || len(checks) != 1 || checks[0].Valid {
			t.Fatalf("%s verified a changed byte: %v %+v", profile, err, checks)
		}
		raw, err := dearmor([]byte(sig))
		if err != nil {
			t.Fatal(err)
		}
		again, err := armorBytes(raw, "PGP SIGNATURE")
		if err != nil {
			t.Fatal(err)
		}
		if raw2, _ := dearmor([]byte(again)); !bytes.Equal(raw, raw2) {
			t.Fatal("armor round trip changed bytes")
		}
	}
}
