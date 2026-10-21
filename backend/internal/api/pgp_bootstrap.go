package api

import (
	"context"
	"net/http"
	"net/mail"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sendas"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

// handlePGPBootstrap returns everything a client needs to bring its PGP
// state up from nothing, in one round trip.
//
// GET /api/pgp/bootstrap
//
// A client cannot keep the unwrapped private key across restarts — the web
// vault holds it in page memory only, and the mobile apps are told not to
// put it in the Keystore/Keychain, because anything that survives a restart
// without the password is recoverable without the password. So every cold
// start is a full reload: the app comes up knowing nothing and has to
// rediscover whether this account even has a key, which protection mode it
// is under, whether it must prompt for an unlock, and what public keys it
// needs to verify signatures with.
//
// Doing that as four separate calls (identity, wrapped key, contacts,
// protection mode) means four chances to get a partial answer and render a
// half-initialized UI — the state where an app shows "no PGP identity" to
// someone who has one, or silently treats encrypted mail as unreadable
// because the wrapped-key call was the one that failed. One endpoint gives
// the client a single consistent snapshot to act on.
//
// It is withMailAuth so a paired device can call it with per-device
// credentials; it returns nothing a session-authenticated caller could not
// already obtain.
func (s *Server) handlePGPBootstrap(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		http.Error(w, "user unavailable", http.StatusInternalServerError)
		return
	}

	protection := u.PGPProtection()
	resp := map[string]any{
		// hasIdentity is deliberately separate from protection: an account
		// with no key at all and an account whose mode the client does not
		// recognize are different situations, and conflating them is how a
		// client ends up offering "unlock" for a key that does not exist.
		"hasIdentity": u.PGPFingerprint != "",
		"pgpRevision": u.PGPRevision,
		"keyring":     u.PGPKeyring,
		"protection":  protection,
		"fingerprint": u.PGPFingerprint,
		"keyId":       u.PGPKeyID,
		"publicKey":   u.PGPPublicKey,
		"keySource":   u.PGPKeySource,
		"createdAt":   u.PGPKeyCreatedAt,
	}

	switch protection {
	case users.PGPProtectionClient:
		// The wrapped envelope is self-describing (kdf, iterations, salt,
		// iv), so the client derives the unwrapping key from what is in the
		// blob rather than from hardcoded parameters. That is what lets the
		// KDF change later without stranding existing clients.
		resp["wrappedPrivateKey"] = u.PGPPrivateKeyWrapped
		resp["unlockRequired"] = true
		resp["canDecryptServerSide"] = false
		resp["migrationAvailable"] = false
		// Slot NAMES only. The browser needs to know a recovery envelope exists
		// to decide between "recover with your code" and "set one up"; it does
		// not need the sealed bytes until it is actually unlocking, and putting a
		// second brute-forceable envelope in every cold-start response would be a
		// cost with no matching benefit.
		slots := []string{}
		for _, e := range u.WrappedEnvelopes() {
			slots = append(slots, e.Slot)
		}
		resp["envelopeSlots"] = slots
	case users.PGPProtectionServer:
		// Legacy: the server still decrypts, so the client has nothing to
		// unlock — but it should offer the one-time migration.
		resp["wrappedPrivateKey"] = ""
		resp["unlockRequired"] = false
		resp["canDecryptServerSide"] = true
		resp["migrationAvailable"] = true
		resp["envelopeSlots"] = []string{}
	default:
		resp["wrappedPrivateKey"] = ""
		resp["unlockRequired"] = false
		resp["canDecryptServerSide"] = false
		resp["migrationAvailable"] = false
		resp["envelopeSlots"] = []string{}
	}

	// Contact public keys, so a freshly-started client can verify inbound
	// signatures immediately instead of waiting on a contacts sync. Public
	// key material only, each labelled with the addresses the address book
	// binds it to — the client must not re-derive that binding from the keys
	// themselves. See boundSignerKeys.
	//
	// An address book this process cannot read yields NO keys rather than the
	// last set it managed to load: an empty list can only ever produce "signed,
	// but not by a key you hold", never a green badge for a key the user
	// removed. The client stays usable and no verdict is invented.
	signerKeys := []boundSignerKey{}
	if contactsStore, cerr := s.userContactsStore(ac.UserID); cerr == nil {
		keys, kerr := boundSignerKeys(contactsStore)
		if kerr != nil {
			s.logger.Error("could not read contact signer keys", "user_id", ac.UserID, "error", kerr.Error())
		} else {
			signerKeys = keys
		}
	}
	resp["signerKeys"] = signerKeys

	// The addresses a newly generated key must carry as User IDs: the IMAP
	// account address plus every verified send-as alias. Both WKD serving
	// and Autocrypt advertising refuse a key that does not carry the address
	// in question, so a client generating a key needs this and must not
	// guess — the login name is frequently not an email address at all.
	// Mirrors handlePGPIdentityGenerate's own derivation.
	resp["suggestedUserIDs"] = s.suggestedKeyUserIDs(ac.UserID)
	// The addresses this account sends as, so a client can leave its own
	// addresses out of Reply All. Separate from suggestedUserIDs, which has
	// key-generation semantics and keeps a login that is not an address.
	resp["accountAddresses"] = s.accountAddresses(ac.UserID)
	resp["displayName"] = u.Username

	// Tell the client where to go for ciphertext rather than making it infer
	// the route, so an older server and a newer app disagree loudly (absent
	// field) instead of silently 404ing every decrypt.
	resp["payloadEndpoint"] = "/api/mail/pgp-payload"

	writeJSON(w, http.StatusOK, resp)
}

// suggestedKeyUserIDs returns the addresses a key generated for userID should
// carry, primary address first. Empty when no mail account is configured —
// the caller surfaces that rather than minting a key with no usable User ID.
func (s *Server) suggestedKeyUserIDs(userID string) []string {
	out := []string{}
	if s.users != nil {
		a, native, err := s.nativeMailAssignment(context.Background(), userID)
		if native || err != nil {
			if err == nil {
				return []string{a.Address}
			}
			return out
		}
	}
	payload, exists, err := mailmsg.ReadIMAPConfigPayload(s.userIMAPConfigPath(userID), s.imapConfigKeyPath)
	if err != nil || !exists || strings.TrimSpace(payload.Username) == "" {
		return out
	}
	out = append(out, strings.TrimSpace(payload.Username))

	return s.appendVerifiedAliases(userID, out, sendas.Alias.DomainProven)
}

// accountAddresses returns the addresses this account sends as, primary
// first: the From the relay uses when the client names none (the native
// address, or the IMAP login — see resolveMailFrom), then every verified
// send-as alias. Unlike suggestedKeyUserIDs it keeps code-verified aliases:
// they authorize From, and this list claims nothing to anyone else. A login
// that is not an address is not a sending address, so it is left out rather
// than guessed at. Never fails: a read error logs and yields what is known,
// possibly nothing.
func (s *Server) accountAddresses(userID string) []string {
	out := []string{}
	if s.users != nil {
		a, native, err := s.nativeMailAssignment(context.Background(), userID)
		if err != nil {
			s.logger.Error("failed to resolve native mailbox for account addresses", "user_id", userID, "error", err.Error())
			return out
		}
		if native {
			return []string{a.Address}
		}
	}
	payload, exists, err := mailmsg.ReadIMAPConfigPayload(s.userIMAPConfigPath(userID), s.imapConfigKeyPath)
	if err != nil {
		s.logger.Error("failed to read imap config for account addresses", "user_id", userID, "error", err.Error())
		return out
	}
	if !exists {
		return out
	}
	if addr, perr := mail.ParseAddress(strings.TrimSpace(payload.Username)); perr == nil {
		out = append(out, addr.Address)
	}
	return s.appendVerifiedAliases(userID, out, func(sendas.Alias) bool { return true })
}

// appendVerifiedAliases appends every verified send-as alias that keep accepts
// and that is not already in out, compared case-insensitively.
func (s *Server) appendVerifiedAliases(userID string, out []string, keep func(sendas.Alias) bool) []string {
	seen := map[string]bool{}
	for _, addr := range out {
		seen[strings.ToLower(addr)] = true
	}
	store, err := s.userSendAsStore(userID)
	if err != nil {
		// A key missing its alias User IDs is only fixable by regenerating,
		// and a client missing its own aliases puts them back in Reply All,
		// so this is worth a log line rather than a silent partial answer.
		s.logger.Error("failed to open send-as store", "user_id", userID, "error", err.Error())
		return out
	}
	verified, err := store.ListVerified()
	if err != nil {
		s.logger.Error("failed to read send-as aliases", "user_id", userID, "error", err.Error())
		return out
	}
	for _, alias := range verified {
		addr := strings.TrimSpace(alias.Email)
		if !keep(alias) || addr == "" || seen[strings.ToLower(addr)] {
			continue
		}
		seen[strings.ToLower(addr)] = true
		out = append(out, addr)
	}
	return out
}
