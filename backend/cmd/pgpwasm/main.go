//go:build js && wasm

// Command pgpwasm is gopenpgp compiled to WebAssembly for KyPost's browser
// client (frontend/src/lib/pgpEngineWasm.ts). It registers globalThis.kypostPGP;
// each function takes one object and returns one object, or {error}.
// Build: scripts/build-pgp-wasm.sh.
package main

import (
	"fmt"
	"syscall/js"

	"github.com/Busnes-app/ky-primitives/xwing"
)

var (
	uint8Array = js.Global().Get("Uint8Array")
	textEnc    = js.Global().Get("TextEncoder").New()
)

func bytesOf(v js.Value) []byte {
	if v.IsUndefined() || v.IsNull() {
		return nil
	}
	if v.Type() == js.TypeString {
		v = textEnc.Call("encode", v)
	}
	if !v.InstanceOf(uint8Array) {
		// Typed arrays from another realm (for example a browser frame) have
		// a different constructor. Copy a genuine Uint8Array into this realm
		// before CopyBytesToGo, whose runtime uses instanceof too.
		if !js.Global().Get("ArrayBuffer").Call("isView", v).Bool() ||
			js.Global().Get("Object").Get("prototype").Get("toString").Call("call", v).String() != "[object Uint8Array]" {
			panic("expected Uint8Array")
		}
		v = uint8Array.New(v)
		defer v.Call("fill", 0) // this normalization copy is ours, not the caller’s
	}
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
}

func bytesList(v js.Value) [][]byte {
	if v.IsUndefined() || v.IsNull() {
		return nil
	}
	out := make([][]byte, v.Length())
	for i := range out {
		out[i] = bytesOf(v.Index(i))
	}
	return out
}

func toJS(b []byte) js.Value {
	u := uint8Array.New(len(b))
	js.CopyBytesToJS(u, b)
	return u
}

func anyList(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func packetList(list [][]byte) []any {
	out := make([]any, len(list))
	for i, p := range list {
		out[i] = toJS(p)
	}
	return out
}

func checks(list []signatureCheck) []any {
	out := make([]any, len(list))
	for i, c := range list {
		out[i] = map[string]any{"keyID": c.KeyID, "fingerprint": c.Fingerprint, "valid": c.Valid}
	}
	return out
}

func zero(list ...[]byte) {
	for _, b := range list {
		clear(b)
	}
}

// op wraps one bridge call: a panic or error becomes {error}, never a crash
// of the Go runtime the page shares across calls.
func op(fn func(req js.Value) (map[string]any, error)) js.Func {
	return js.FuncOf(func(_ js.Value, args []js.Value) (result any) {
		defer func() {
			if r := recover(); r != nil {
				result = map[string]any{"error": fmt.Sprint(r)}
			}
		}()
		req := js.Undefined()
		if len(args) > 0 {
			req = args[0]
		}
		out, err := fn(req)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return out
	})
}

func keyResult(priv, pub, fp string, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"privateKey": priv, "publicKey": pub, "fingerprint": fp}, nil
}

func main() {
	api := map[string]any{
		"xwingGenerate": op(func(req js.Value) (map[string]any, error) {
			key, err := xwing.Generate()
			if err != nil {
				return nil, err
			}
			seed := key.Seed()
			defer zero(seed)
			return map[string]any{"seed": toJS(seed), "publicKey": toJS(key.Public().Bytes())}, nil
		}),
		"xwingSeal": op(func(req js.Value) (map[string]any, error) {
			pub, err := xwing.ParsePublicKey(bytesOf(req.Get("publicKey")))
			if err != nil {
				return nil, err
			}
			plaintext := bytesOf(req.Get("plaintext"))
			defer zero(plaintext)
			sealed, err := xwing.Seal(pub, bytesOf(req.Get("info")), nil, plaintext)
			if err != nil {
				return nil, err
			}
			return map[string]any{"enc": toJS(sealed[1 : 1+xwing.EncapsulationBytes]), "ct": toJS(sealed[1+xwing.EncapsulationBytes:])}, nil
		}),
		"xwingOpen": op(func(req js.Value) (map[string]any, error) {
			seed := bytesOf(req.Get("seed"))
			defer zero(seed)
			key, err := xwing.FromSeed(seed)
			if err != nil {
				return nil, err
			}
			enc, ct := bytesOf(req.Get("enc")), bytesOf(req.Get("ct"))
			if len(enc) != xwing.EncapsulationBytes || len(ct) < 16 {
				return nil, fmt.Errorf("invalid v6 envelope framing")
			}
			sealed := append([]byte{xwing.Version}, enc...)
			sealed = append(sealed, ct...)
			plaintext, err := xwing.Open(key, bytesOf(req.Get("info")), nil, sealed)
			if err != nil {
				return nil, err
			}
			defer zero(plaintext)
			return map[string]any{"plaintext": toJS(plaintext)}, nil
		}),
		"generate": op(func(req js.Value) (map[string]any, error) {
			var ids []userID
			list := req.Get("userIDs")
			for i := 0; i < list.Length(); i++ {
				ids = append(ids, userID{list.Index(i).Get("name").String(), list.Index(i).Get("email").String()})
			}
			return keyResult(generate(req.Get("profile").String(), ids))
		}),
		"importPrivate": op(func(req js.Value) (map[string]any, error) {
			key, pass := bytesOf(req.Get("key")), bytesOf(req.Get("passphrase"))
			defer zero(key, pass)
			return keyResult(importPrivate(key, pass))
		}),
		"inspect": op(func(req js.Value) (map[string]any, error) {
			key := bytesOf(req.Get("key"))
			defer zero(key)
			info, err := inspect(key)
			if err != nil {
				return nil, err
			}
			algos := make([]any, len(info.Algorithms))
			for i, a := range info.Algorithms {
				algos[i] = a
			}
			return map[string]any{"fingerprint": info.Fingerprint, "version": info.Version, "isPrivate": info.IsPrivate,
				"locked": info.Locked, "count": info.Count, "expires": info.Expires, "publicKey": toJS(info.PublicKey), "packets": packetList(info.Packets),
				"keyIDs": anyList(info.KeyIDs), "fingerprints": anyList(info.Fingerprints), "algorithms": algos, "encryptionAlgorithm": info.EncryptionAlgorithm}, nil
		}),
		"encrypt": op(func(req js.Value) (map[string]any, error) {
			data, signer := bytesOf(req.Get("data")), bytesOf(req.Get("signingKey"))
			defer zero(data, signer)
			msg, err := encrypt(data, bytesList(req.Get("encryptionKeys")), signer)
			if err != nil {
				return nil, err
			}
			return map[string]any{"message": msg}, nil
		}),
		"signDetached": op(func(req js.Value) (map[string]any, error) {
			data, signer := bytesOf(req.Get("data")), bytesOf(req.Get("signingKey"))
			defer zero(data, signer)
			sig, hash, err := signDetached(data, signer)
			if err != nil {
				return nil, err
			}
			return map[string]any{"signature": sig, "hashAlgorithm": hash}, nil
		}),
		"decrypt": op(func(req js.Value) (map[string]any, error) {
			keys := bytesList(req.Get("decryptionKeys"))
			defer zero(keys...)
			data, sigs, err := decrypt(bytesOf(req.Get("message")), keys, bytesList(req.Get("verificationKeys")), int64(req.Get("maxBytes").Int()))
			if err != nil {
				return nil, err
			}
			defer zero(data)
			return map[string]any{"data": toJS(data), "signatures": checks(sigs), "quantumSafe": quantumSafeMessage(bytesOf(req.Get("message")))}, nil
		}),
		"reencrypt": op(func(req js.Value) (map[string]any, error) {
			keys := bytesList(req.Get("decryptionKeys"))
			defer zero(keys...)
			msg, err := reencrypt(bytesOf(req.Get("message")), keys, bytesOf(req.Get("encryptionKey")), int64(req.Get("maxBytes").Int()))
			if err != nil {
				return nil, err
			}
			return map[string]any{"message": msg}, nil
		}),
		"verifyDetached": op(func(req js.Value) (map[string]any, error) {
			sigs, err := verifyDetached(bytesOf(req.Get("data")), bytesOf(req.Get("signature")), bytesList(req.Get("verificationKeys")))
			if err != nil {
				return nil, err
			}
			return map[string]any{"signatures": checks(sigs)}, nil
		}),
		"armor": op(func(req js.Value) (map[string]any, error) {
			s, err := armorBytes(bytesOf(req.Get("data")), req.Get("type").String())
			if err != nil {
				return nil, err
			}
			return map[string]any{"armored": s}, nil
		}),
		"dearmor": op(func(req js.Value) (map[string]any, error) {
			b, err := dearmor(bytesOf(req.Get("armored")))
			if err != nil {
				return nil, err
			}
			return map[string]any{"data": toJS(b)}, nil
		}),
	}
	js.Global().Set("kypostPGP", js.ValueOf(api))
	select {}
}
