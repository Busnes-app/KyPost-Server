package cfreceiving

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The contract: body {"<field>": "<json>", "signature": "<b64>"} with exactly
// those keys; the signature covers UTF-8(context) || UTF-8(payload string).
func verifyContract(t *testing.T, body []byte, field, context string, key ed25519.PublicKey) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]string
	if err := json.Unmarshal(body, &doc); err != nil || len(doc) != 2 {
		t.Fatal("body is not exactly two string keys", string(body))
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(doc["signature"])
	if err != nil || len(signature) != 64 || base64.StdEncoding.EncodeToString(signature) != doc["signature"] {
		t.Fatal("signature encoding")
	}
	if !ed25519.Verify(key, []byte(context+doc[field]), signature) {
		t.Fatal("signature does not cover context || payload")
	}
	if ed25519.Verify(key, []byte(doc[field]), signature) {
		t.Fatal("signature verified without its context")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc[field]), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func keysOf(m map[string]json.RawMessage) string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(sortStrings(out), ",")
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

func TestSignedDocumentsMatchContract(t *testing.T) {
	m, err := NewMaterial(1)
	if err != nil {
		t.Fatal(err)
	}
	public, err := base64.StdEncoding.DecodeString(m.PublicKey())
	if err != nil || len(public) != 32 {
		t.Fatal("public key encoding", err)
	}
	sum := sha256.Sum256([]byte(m.Token))
	if m.TokenSHA256() != hex.EncodeToString(sum[:]) || len(m.Token) != 64 || strings.ToLower(m.Token) != m.Token {
		t.Fatal("bearer hash")
	}
	routes := []Route{{Address: "a@xn--bcher-kva.test", Generation: 3, MaxBytes: 4 << 20, Issuer: "i", Subject: "s", Mailbox: "m"}}
	body, err := SignTable(m, 1790000000000, 1790000000000, routes, nil)
	if err != nil {
		t.Fatal(err)
	}
	table := verifyContract(t, body, "table", "kypost-cf-routes/1\n", public)
	if keysOf(table) != "blockedSenders,issuedAt,revision,routes" || string(table["blockedSenders"]) != "[]" {
		t.Fatal("table keys", keysOf(table), string(table["blockedSenders"]))
	}
	var wire []map[string]json.RawMessage
	if err := json.Unmarshal(table["routes"], &wire); err != nil || len(wire) != 1 || keysOf(wire[0]) != "address,generation,maxBytes" || string(wire[0]["address"]) != `"a@xn--bcher-kva.test"` {
		t.Fatal("owner leaked to the Worker or route shape wrong", string(table["routes"]))
	}
	next, err := NewMaterial(2)
	if err != nil {
		t.Fatal(err)
	}
	rotation := verifyContract(t, mustRotation(t, m, next), "rotation", "kypost-cf-rotate/1\n", public)
	if keysOf(rotation) != "epoch,publicKey,tokenSha256" || string(rotation["epoch"]) != "2" || string(rotation["tokenSha256"]) != `"`+next.TokenSHA256()+`"` || string(rotation["publicKey"]) != `"`+next.PublicKey()+`"` {
		t.Fatal("rotation payload", rotation)
	}
	for _, bad := range []Route{{Address: "Upper@example.test", Generation: 1, MaxBytes: 1}, {Address: "bücher@example.test", Generation: 1, MaxBytes: 1}, {Address: "a@bücher.test", Generation: 1, MaxBytes: 1}, {Address: "a@example.test", Generation: 0, MaxBytes: 1}, {Address: "a@example.test", Generation: 1, MaxBytes: MaxMessageBytes + 1}} {
		if _, err := SignTable(m, 1, 1, []Route{bad}, nil); err == nil {
			t.Fatal("invalid route signed", bad)
		}
	}
	if _, err := SignTable(m, 1, 1, []Route{routes[0], routes[0]}, nil); err == nil {
		t.Fatal("duplicate address signed")
	}
	if !ValidKey("01890a5d-ac96-774b-bcce-b302099a8057") || ValidKey("01890A5D-AC96-774B-BCCE-B302099A8057") || ValidKey("01890a5d-ac96-474b-bcce-b302099a8057") {
		t.Fatal("key pattern")
	}
	if KeyTime("01890a5d-ac96-774b-bcce-b302099a8057") != 0x01890a5dac96 {
		t.Fatal("key time")
	}
}

func mustRotation(t *testing.T, current, next Material) []byte {
	t.Helper()
	body, err := SignRotation(current, next)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestKeysHostRecordAndPromotionWindow(t *testing.T) {
	dir := t.TempDir()
	keys := Keys{Dir: dir}
	if _, _, _, err := keys.Load(); err != ErrNoCredentials {
		t.Fatal("missing credentials", err)
	}
	m, err := keys.Init()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Init(); err == nil {
		t.Fatal("init replaced credentials")
	}
	for _, name := range []string{CredentialsFile, HostFile} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal(name, "not owner-only", err)
		}
	}
	if cur, pending, live, err := keys.Load(); err != nil || !live || pending != nil || cur.Token != m.Token {
		t.Fatal("initialized host not live", err)
	}
	// A restore carries the credentials, never the host record.
	restored := Keys{Dir: t.TempDir()}
	raw, err := os.ReadFile(filepath.Join(dir, CredentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored.Dir, CredentialsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, live, err := restored.Load(); err != nil || live {
		t.Fatal("restored credentials started live", err)
	}
	next, err := NewMaterial(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.SavePending(next); err != nil {
		t.Fatal(err)
	}
	if _, pending, live, err := keys.Load(); err != nil || !live || pending == nil || pending.Token != next.Token {
		t.Fatal("pending not persisted beside a live current", err)
	}
	// Crash between Promote's writes: credentials moved, host record not.
	if err := keys.save(CredentialsFile, next); err != nil {
		t.Fatal(err)
	}
	if cur, pending, live, err := keys.Load(); err != nil || !live || pending != nil || cur.Token != next.Token {
		t.Fatal("interrupted promotion not recognized", err)
	}
	if err := keys.Fence(); err != nil {
		t.Fatal(err)
	}
	if _, _, live, err := keys.Load(); err != nil || live {
		t.Fatal("fence did not stop the host", err)
	}
	if err := os.WriteFile(filepath.Join(dir, CredentialsFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := CurrentStatus(t.Context(), keys, t.TempDir()); err != nil || s.State != "error" {
		t.Fatal("corrupt credentials not reported", s, err)
	}
}
