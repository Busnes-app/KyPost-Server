package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/gopenpgp/v3/crypto"

	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpprofile"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

func encryptArmoredForTest(t *testing.T, v6 bool, pubKeys []string, plaintext []byte) string {
	t.Helper()
	var handle *crypto.PGPHandle
	if v6 {
		handle = pgpprofile.V6()
	} else {
		handle = crypto.PGP()
	}
	ring, err := crypto.NewKeyRing(nil)
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	for _, armored := range pubKeys {
		k, err := crypto.NewKeyFromArmored(armored)
		if err != nil {
			t.Fatalf("parse pubkey: %v", err)
		}
		if err := ring.AddKey(k); err != nil {
			t.Fatalf("add pubkey: %v", err)
		}
	}
	enc, err := handle.Encryption().Recipients(ring).New()
	if err != nil {
		t.Fatalf("encryption builder: %v", err)
	}
	msg, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	armored, err := msg.Armor()
	if err != nil {
		t.Fatalf("armor: %v", err)
	}
	return armored
}

func makeIdentityForTest(t *testing.T, v6 bool, name, email string) (armoredPub, armoredPriv string) {
	t.Helper()
	var handle *crypto.PGPHandle
	if v6 {
		handle = pgpprofile.V6()
	} else {
		handle = crypto.PGP()
	}
	key, err := handle.KeyGeneration().AddUserId(name, email).New().GenerateKey()
	if err != nil {
		t.Fatalf("generate key (v6=%v): %v", v6, err)
	}
	pub, err := key.GetArmoredPublicKey()
	if err != nil {
		t.Fatalf("armor pub: %v", err)
	}
	priv, err := key.Armor()
	if err != nil {
		t.Fatalf("armor priv: %v", err)
	}
	return pub, priv
}

func buildPGPMIMEMsgForTest(extraHeaders []string, armoredCiphertext string) []byte {
	boundary := "kypost-test-p6-boundary"
	var b strings.Builder
	for _, h := range extraHeaders {
		b.WriteString(h)
		b.WriteString("\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: application/pgp-encrypted\r\n\r\n")
	b.WriteString("Version: 1\r\n\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: application/octet-stream; name=\"encrypted.asc\"\r\n")
	b.WriteString("Content-Disposition: inline; filename=\"encrypted.asc\"\r\n\r\n")
	b.WriteString(armoredCiphertext)
	if !strings.HasSuffix(armoredCiphertext, "\n") {
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return []byte(b.String())
}

func TestPGPArchiveReencryptEndToEndAndSecurityProperties(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	u := clientProtectedUser(t, s)
	const addr = "archive@example.invalid"

	// Set up native mailbox store + client attached to the user.
	store, err := mailbox.Open(
		filepath.Join(t.TempDir(), "mailbox"),
		mailbox.Owner{Issuer: "https://identity.example.test", Subject: u.ID, Mailbox: u.ID},
		mailbox.Limits{MessageBytes: 1 << 20, PayloadBytes: 8 << 20, Records: 200},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client, err := mailbox.NewClient(store, addr)
	if err != nil {
		t.Fatal(err)
	}
	nativeState, err := state.NewNative(filepath.Join(t.TempDir(), "native-state"), client.MailSourceIdentity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nativeState.Close() })
	s.imapConfigKeyPath = filepath.Join(t.TempDir(), "imap-config.key")
	if err := writeIMAPConfigPayload(s.userIMAPConfigPath(u.ID), s.imapConfigKeyPath, imapConfigPayload{
		Host: "imap.example.invalid", Port: 993, Username: addr, Password: "pw",
		Mailbox: "INBOX", UpdatedAt: "test",
	}); err != nil {
		t.Fatal(err)
	}
	s.userMu.Lock()
	s.userStores[u.ID] = nativeState
	s.userMail[u.ID] = &serverMailEntry{client: client, updatedAt: "test"}
	s.userMu.Unlock()

	// Create v4 and v6 keys for the user, plus an outsider's v4 and v6 keys.
	v4Pub, v4Priv := makeIdentityForTest(t, false, "Archive", addr)
	v4Info, err := pgpmail.InspectPublicKey(v4Pub)
	if err != nil {
		t.Fatal(err)
	}
	v6Pub, v6Priv := makeIdentityForTest(t, true, "Archive", addr)
	v6Info, err := pgpmail.InspectPQCKey(v6Pub)
	if err != nil {
		t.Fatal(err)
	}
	otherV4Pub, _ := makeIdentityForTest(t, false, "Outsider", "outsider@example.invalid")
	otherV6Pub, _ := makeIdentityForTest(t, true, "OtherV6", "otherv6@example.invalid")

	// Start with v4-only client-protected account (v6Ready == false).
	u, err = s.users.SetPGPIdentityClientProtected(u.ID, v4Info.Fingerprint, v4Info.KeyID, v4Info.ArmoredPublicKey, `{"v":1}`, "generated", "now", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Authenticate once and reuse session cookies + CSRF token across requests.
	sessReq := httptest.NewRequest("GET", "/api/mail/pgp-reencrypt/candidates", nil)
	authRequestAs(s, sessReq, u.ID)
	doReq := func(method, target string, body any) *httptest.ResponseRecorder {
		var rBody *bytes.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			rBody = bytes.NewReader(b)
		} else {
			rBody = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, target, rBody)
		for _, c := range sessReq.Cookies() {
			req.AddCookie(c)
		}
		req.Header.Set("X-CSRF-Token", sessReq.Header.Get("X-CSRF-Token"))
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}

	// 1. Before v6 conversion, endpoints MUST refuse with 409.
	if w := doReq("GET", "/api/mail/pgp-reencrypt/candidates", nil); w.Code != http.StatusConflict {
		t.Fatalf("candidates before v6Ready: got %d, want 409 (%s)", w.Code, w.Body.String())
	}

	// Enable v6 key, then add a legacy enrolled device without v6 -> still 409!
	u, err = s.users.CommitPGPKeyring(ctx, u.ID, users.PGPKeyringUpdate{
		ExpectedRevision:    u.PGPRevision,
		MaterialGeneration:  1,
		PrimaryFingerprints: []string{v4Info.Fingerprint, v6Info.Fingerprint},
		KeyFingerprints:     append(v4Info.KeyFingerprints, v6Info.KeyFingerprints...),
		PublicKey:           u.PGPPublicKey,
		PublicKeyV6:         v6Pub,
		PasswordEnvelope:    `{"v":2,"ring":"pw"}`,
		RecoveryEnvelope:    `{"v":2,"ring":"rec"}`,
		Source:              "generated",
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyDev, _ := pairNativeDevice(t, s, u.ID, "linux")
	if w := doReq("GET", "/api/mail/pgp-reencrypt/candidates", nil); w.Code != http.StatusConflict {
		t.Fatalf("candidates while legacy device blocks v6Ready: got %d, want 409 (%s)", w.Code, w.Body.String())
	}
	// Confirm v6 capability + ring delivery on the device -> v6Ready becomes true!
	caps := []string{"v4", "v6", "rfc9980"}
	if _, err := nativeState.SetNativeDeviceEnrollmentKey(legacyDev, "KEY", "now", []int{6}, caps); err != nil {
		t.Fatal(err)
	}
	delivery := state.DeviceEnrollment{Version: 6, Generation: 1, Fingerprint: u.PGPFingerprint}
	if err := nativeState.RecordNativeDeviceDelivery(legacyDev, delivery); err != nil {
		t.Fatal(err)
	}
	if ok, err := nativeState.ConfirmNativeDeviceEnrollment(legacyDev, delivery, caps); err != nil || !ok {
		t.Fatalf("confirm device enrollment: ok=%v err=%v", ok, err)
	}

	// 2. Seed mailbox across INBOX, Sent, Drafts, and Archive:
	//    - INBOX: v4-only (with custom headers + flags + inner MIME), outside multi-recipient v4, plaintext, already-v6
	//    - Sent: v4-only
	//    - Drafts: v4-only
	//    - Archive: v4-only
	innerMIME := []byte("Content-Type: multipart/signed; micalg=pgp-sha256; boundary=\"inner\"\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\nSecret signed archive text\r\n--inner\r\nContent-Type: application/pgp-signature\r\n\r\nsig-payload\r\n--inner--\r\n")
	inboxV4Raw := buildPGPMIMEMsgForTest([]string{
		"Received: from mx.outside.test ([198.51.100.1])",
		" by kypost.local with ESMTPS",
		"DKIM-Signature: v=1; a=ed25519-sha256; d=outside.test; s=k1; b=AAAA",
		"From: outside@example.invalid",
		"To: " + addr,
		"Subject: [Encrypted]",
		"Message-ID: <p6-inbox-1@outside.test>",
		"In-Reply-To: <parent@outside.test>",
		"References: <root@outside.test> <parent@outside.test>",
		"X-Custom-Preserve: keep-me-verbatim",
	}, encryptArmoredForTest(t, false, []string{v4Pub}, innerMIME))

	inboxOutsideMultiRaw := buildPGPMIMEMsgForTest([]string{
		"From: outside@example.invalid",
		"To: " + addr + ", outsider@example.invalid",
		"Subject: Group v4 mail",
	}, encryptArmoredForTest(t, false, []string{v4Pub, otherV4Pub}, []byte("group message")))

	inboxPlainRaw := []byte("From: news@example.invalid\r\nTo: " + addr + "\r\nSubject: Plain\r\n\r\nplain text\r\n")

	inboxAlreadyV6Raw := buildPGPMIMEMsgForTest([]string{
		"From: pq@example.invalid",
		"To: " + addr,
		"Subject: Already V6",
	}, encryptArmoredForTest(t, true, []string{v6Pub}, []byte("already pq")))

	sentV4Raw := buildPGPMIMEMsgForTest([]string{
		"From: " + addr,
		"To: peer@example.invalid",
		"Subject: Sent v4",
	}, encryptArmoredForTest(t, false, []string{v4Pub}, []byte("sent copy")))

	draftV4Raw := buildPGPMIMEMsgForTest([]string{
		"From: " + addr,
		"To: peer@example.invalid",
		"Subject: Draft v4",
	}, encryptArmoredForTest(t, false, []string{v4Pub}, []byte("draft body")))

	archiveV4Raw := buildPGPMIMEMsgForTest([]string{
		"From: old@example.invalid",
		"To: " + addr,
		"Subject: Archived v4",
	}, encryptArmoredForTest(t, false, []string{v4Pub}, []byte("archived body")))

	inboxV4ID, err := store.Append(ctx, "INBOX", bytes.NewReader(inboxV4Raw), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, "INBOX", inboxV4ID, true, true, []string{"vault"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "INBOX", bytes.NewReader(inboxOutsideMultiRaw), false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "INBOX", bytes.NewReader(inboxPlainRaw), false); err != nil {
		t.Fatal(err)
	}
	alreadyV6ID, err := store.Append(ctx, "INBOX", bytes.NewReader(inboxAlreadyV6Raw), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "Sent", bytes.NewReader(sentV4Raw), false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "Drafts", bytes.NewReader(draftV4Raw), true); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateFolder(ctx, "CustomFolder"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "CustomFolder", bytes.NewReader(archiveV4Raw), false); err != nil {
		t.Fatal(err)
	}

	// 3. Paginate candidates with limit=2 and verify all 5 v4 candidates are found across folders.
	var allCandidates []pgpReencryptCandidate
	cursorFolder := ""
	cursorAfter := 0
	for page := 0; page < 10; page++ {
		url := fmt.Sprintf("/api/mail/pgp-reencrypt/candidates?limit=2&folder=%s&afterUid=%d", cursorFolder, cursorAfter)
		w := doReq("GET", url, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("candidates page %d: got %d (%s)", page, w.Code, w.Body.String())
		}
		var resp pgpReencryptCandidatesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(resp.V6Fingerprint, v6Info.Fingerprint) {
			t.Fatalf("v6Fingerprint mismatch: got %q want %q", resp.V6Fingerprint, v6Info.Fingerprint)
		}
		allCandidates = append(allCandidates, resp.Candidates...)
		if resp.Done || resp.NextCursor == nil {
			break
		}
		cursorFolder = resp.NextCursor.Folder
		cursorAfter = resp.NextCursor.AfterUID
	}
	if len(allCandidates) != 5 {
		t.Fatalf("expected 5 v4 candidates across Archive/Drafts/INBOX/Sent, got %d: %+v", len(allCandidates), allCandidates)
	}

	// 4. Fetch raw ciphertext + CAS hash for inboxV4ID.
	rawW := doReq("GET", fmt.Sprintf("/api/mail/pgp-reencrypt/raw?folder=INBOX&messageId=%d", inboxV4ID), nil)
	if rawW.Code != http.StatusOK {
		t.Fatalf("raw fetch: got %d (%s)", rawW.Code, rawW.Body.String())
	}
	var rawResp pgpReencryptRawResponse
	if err := json.Unmarshal(rawW.Body.Bytes(), &rawResp); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(inboxV4Raw)
	if rawResp.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("raw sha256 mismatch: got %s", rawResp.SHA256)
	}

	// Decrypt with v4 key (simulating client) and verify exact inner MIME.
	v4PrivKey, err := crypto.NewKeyFromArmored(v4Priv)
	if err != nil {
		t.Fatal(err)
	}
	decHandle, err := crypto.PGP().Decryption().DecryptionKey(v4PrivKey).New()
	if err != nil {
		t.Fatal(err)
	}
	decRes, err := decHandle.Decrypt([]byte(rawResp.ArmoredCiphertext), crypto.Armor)
	if err != nil {
		t.Fatalf("client decrypt v4: %v", err)
	}
	reencryptedV6 := encryptArmoredForTest(t, true, []string{v6Pub}, decRes.Bytes())

	// 5. Security Property P6: server MUST refuse invalid replacements.
	badCases := []struct {
		name      string
		folder    string
		messageID int
		sha256    string
		cipher    string
		wantCode  int
	}{
		{
			name:      "v4 ciphertext refused",
			folder:    "INBOX",
			messageID: int(inboxV4ID),
			sha256:    rawResp.SHA256,
			cipher:    encryptArmoredForTest(t, false, []string{v4Pub}, decRes.Bytes()),
			wantCode:  http.StatusBadRequest,
		},
		{
			name:      "other user v6 ciphertext refused",
			folder:    "INBOX",
			messageID: int(inboxV4ID),
			sha256:    rawResp.SHA256,
			cipher:    encryptArmoredForTest(t, true, []string{otherV6Pub}, decRes.Bytes()),
			wantCode:  http.StatusBadRequest,
		},
		{
			name:      "multi-recipient v6 ciphertext refused",
			folder:    "INBOX",
			messageID: int(inboxV4ID),
			sha256:    rawResp.SHA256,
			cipher:    encryptArmoredForTest(t, true, []string{v6Pub, otherV6Pub}, decRes.Bytes()),
			wantCode:  http.StatusBadRequest,
		},
		{
			name:      "plaintext refused",
			folder:    "INBOX",
			messageID: int(inboxV4ID),
			sha256:    rawResp.SHA256,
			cipher:    "not armored pgp at all",
			wantCode:  http.StatusBadRequest,
		},
		{
			name:      "CAS hash mismatch refused",
			folder:    "INBOX",
			messageID: int(inboxV4ID),
			sha256:    strings.Repeat("0", 64),
			cipher:    reencryptedV6,
			wantCode:  http.StatusConflict,
		},
		{
			name:      "already-v6 message refused",
			folder:    "INBOX",
			messageID: int(alreadyV6ID),
			sha256:    sha256HexBytes(inboxAlreadyV6Raw),
			cipher:    reencryptedV6,
			wantCode:  http.StatusConflict,
		},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			w := doReq("POST", "/api/mail/pgp-reencrypt/replace", pgpReencryptReplaceRequest{
				Folder:            tc.folder,
				MessageID:         tc.messageID,
				ExpectedSHA256:    tc.sha256,
				ArmoredCiphertext: tc.cipher,
			})
			if w.Code != tc.wantCode {
				t.Fatalf("got status %d, want %d (body=%s)", w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
	// Stored bytes must remain untouched after every refusal.
	untouched, err := store.Raw(ctx, "INBOX", inboxV4ID)
	if err != nil || !bytes.Equal(untouched, inboxV4Raw) {
		t.Fatalf("stored message was mutated after refused replacement")
	}

	// 6. Valid replacement MUST succeed atomically, preserving ID, folder, flags, outer headers, and inner MIME.
	repW := doReq("POST", "/api/mail/pgp-reencrypt/replace", pgpReencryptReplaceRequest{
		Folder:            "INBOX",
		MessageID:         int(inboxV4ID),
		ExpectedSHA256:    rawResp.SHA256,
		ArmoredCiphertext: reencryptedV6,
	})
	if repW.Code != http.StatusOK {
		t.Fatalf("valid replace: got %d (%s)", repW.Code, repW.Body.String())
	}
	var repResp pgpReencryptReplaceResponse
	if err := json.Unmarshal(repW.Body.Bytes(), &repResp); err != nil {
		t.Fatal(err)
	}
	if repResp.OldMessageID != int(inboxV4ID) || repResp.NewMessageID != int(inboxV4ID) {
		t.Fatalf("native replace changed message ID: %+v", repResp)
	}

	// Replaying with stale hash must now fail with 409 Conflict.
	staleW := doReq("POST", "/api/mail/pgp-reencrypt/replace", pgpReencryptReplaceRequest{
		Folder:            "INBOX",
		MessageID:         int(inboxV4ID),
		ExpectedSHA256:    rawResp.SHA256,
		ArmoredCiphertext: reencryptedV6,
	})
	if staleW.Code != http.StatusConflict {
		t.Fatalf("stale replay: got %d, want 409", staleW.Code)
	}

	// Verify flags & labels survived.
	listed, err := store.List(ctx, "INBOX", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var foundMeta *mailbox.Message
	for i := range listed {
		if listed[i].ID == inboxV4ID {
			foundMeta = &listed[i]
			break
		}
	}
	if foundMeta == nil || !foundMeta.Seen || !foundMeta.Starred || len(foundMeta.Labels) != 1 || foundMeta.Labels[0] != "vault" {
		t.Fatalf("flags/labels corrupted after replace: %+v", foundMeta)
	}

	// Verify outer headers survived verbatim and inner MIME decrypts ONLY with v6 private key.
	storedNewRaw, err := store.Raw(ctx, "INBOX", inboxV4ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, wantHeader := range []string{
		"DKIM-Signature: v=1; a=ed25519-sha256; d=outside.test; s=k1; b=AAAA",
		"Message-ID: <p6-inbox-1@outside.test>",
		"In-Reply-To: <parent@outside.test>",
		"References: <root@outside.test> <parent@outside.test>",
		"X-Custom-Preserve: keep-me-verbatim",
	} {
		if !strings.Contains(string(storedNewRaw), wantHeader) {
			t.Fatalf("outer header lost after replace: missing %q", wantHeader)
		}
	}
	newArmored, err := pgpmail.ExtractPGPMIMEEncryptedPayload(storedNewRaw)
	if err != nil {
		t.Fatalf("extract new armored: %v", err)
	}
	// Old v4 key must NOT decrypt the replaced message.
	if _, err := decHandle.Decrypt([]byte(newArmored), crypto.Armor); err == nil {
		t.Fatalf("replaced v6 message was still decryptable by old v4 key")
	}
	// New v6 key MUST decrypt the replaced message and yield the exact inner MIME.
	v6PrivKey, err := crypto.NewKeyFromArmored(v6Priv)
	if err != nil {
		t.Fatal(err)
	}
	v6Dec, err := pgpprofile.V6().Decryption().DecryptionKey(v6PrivKey).New()
	if err != nil {
		t.Fatal(err)
	}
	v6Plain, err := v6Dec.Decrypt([]byte(newArmored), crypto.Armor)
	if err != nil {
		t.Fatalf("decrypt replaced v6 message: %v", err)
	}
	if !bytes.Equal(v6Plain.Bytes(), innerMIME) {
		t.Fatalf("inner MIME mismatch after v6 re-encryption:\ngot:  %q\nwant: %q", v6Plain.Bytes(), innerMIME)
	}

	// Re-listing candidates must now return only the remaining 4 v4 messages.
	afterW := doReq("GET", "/api/mail/pgp-reencrypt/candidates?limit=50", nil)
	if afterW.Code != http.StatusOK {
		t.Fatalf("candidates after replace: got %d (%s)", afterW.Code, afterW.Body.String())
	}
	var afterResp pgpReencryptCandidatesResponse
	if err := json.Unmarshal(afterW.Body.Bytes(), &afterResp); err != nil {
		t.Fatal(err)
	}
	if len(afterResp.Candidates) != 4 {
		t.Fatalf("expected 4 remaining candidates after replacing 1, got %d", len(afterResp.Candidates))
	}
}
