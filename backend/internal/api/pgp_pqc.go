package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// This is an additive conversion only. Retirement, publication and history
// merging deliberately have no HTTP writer here.
func (s *Server) handlePGPUpgradePQC(w http.ResponseWriter, r *http.Request) {
	ac, ok := authFromContext(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		ExpectedRevision    *uint64               `json:"expectedRevision"`
		ExpectedFingerprint string                `json:"expectedFingerprint"`
		PublicKeyV6         string                `json:"publicKeyV6"`
		Wrapped             string                `json:"wrapped"`
		RecoveryEnvelope    string                `json:"recoveryEnvelope"`
		Keyring             users.PGPKeyringState `json:"keyring"`
		AuthSecret          string                `json:"authSecret,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	u, err := s.users.Get(ac.UserID)
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	if !s.requirePGPStepUp(w, r, ac.UserID, req.AuthSecret) {
		return
	}
	if req.ExpectedRevision == nil || req.Keyring.Version != 2 || req.PublicKeyV6 == "" ||
		!strings.EqualFold(req.ExpectedFingerprint, u.PGPFingerprint) {
		http.Error(w, "v2 keyring, v6 certificate, expected revision and fingerprint required", http.StatusBadRequest)
		return
	}
	// One UID per current account address; never accept someone else's address
	// in the internal certificate. The store independently enforces the profile.
	key, err := crypto.NewKeyFromArmored(req.PublicKeyV6)
	if err != nil {
		http.Error(w, "invalid v6 public key", http.StatusBadRequest)
		return
	}
	addresses := s.suggestedKeyUserIDs(u.ID)
	ids := key.GetEntity().Identities
	if len(addresses) == 0 || len(ids) != len(addresses) {
		http.Error(w, "certificate must carry every account address exactly once", http.StatusBadRequest)
		return
	}
	seen := map[string]bool{}
	for _, id := range ids {
		address := strings.ToLower(id.UserId.Email)
		owned := false
		for _, addr := range addresses {
			if strings.EqualFold(addr, address) {
				owned = true
			}
		}
		if !owned || seen[address] {
			http.Error(w, "certificate must carry every account address exactly once", http.StatusBadRequest)
			return
		}
		seen[address] = true
	}
	u, err = s.users.CommitPGPKeyring(r.Context(), u.ID, users.PGPKeyringUpdate{
		ExpectedRevision: *req.ExpectedRevision, MaterialGeneration: req.Keyring.MaterialGeneration,
		PrimaryFingerprints: req.Keyring.PrimaryFingerprints, KeyFingerprints: req.Keyring.KeyFingerprints,
		PublicKey: u.PGPPublicKey, PublicKeyV6: req.PublicKeyV6, PasswordEnvelope: req.Wrapped,
		RecoveryEnvelope: req.RecoveryEnvelope, Source: "generated",
	})
	if err != nil {
		writeUserStoreError(w, err)
		return
	}
	// Keep previous device records until they acknowledge their replacement ring.
	// The material-generation comparison closes their gates immediately.
	s.logger.Info("pgp PQ certificate added", "user_id", u.ID, "fingerprint", u.PGPFingerprintV6)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pgpRevision": u.PGPRevision})
}

func (s *Server) pgpV6Ready(u users.User) bool {
	if u.PGPPublicKeyV6 == "" || u.PGPKeyring == nil || u.PGPKeyring.Version != 2 || !pgpmail.EncryptsNow(u.PGPPublicKeyV6) {
		return false
	}
	store, err := s.userStore(u.ID)
	if err != nil {
		return false
	}
	ready, err := store.PGPV6Ready(u.PGPKeyring.MaterialGeneration, u.PGPFingerprint)
	return err == nil && ready
}

// preferV6 is false only for a signed send by a v4-only sender: a v6 delivery
// needs a v6 signing certificate, so that sender keeps the recipient's v4 key.
func preferV6(sender users.User, sign bool) bool {
	return !sign || sender.PGPPublicKeyV6 != ""
}

// resolveRecipient is the contact/WKD ladder, then internal PQ discovery when
// the resolver may choose v6. A broken pin is never overridden.
func (s *Server) resolveRecipient(ctx context.Context, sender users.User, address string, resolver *keyResolver) (resolvedKey, error) {
	rk := resolver.resolve(ctx, address)
	if rk.Err != nil || rk.Tier == tierKeyChanged || !resolver.preferV6 {
		return rk, rk.Err
	}
	local, found, err := s.localPQRecipient(sender, address, resolver)
	if err != nil {
		return resolvedKey{}, err
	}
	if found {
		return local, nil
	}
	return rk, nil
}

// provenAddresses are the addresses that stand for this account to other
// accounts: the native mailbox, else DKIM domain-proven send-as aliases. The
// IMAP login and code-confirmed aliases are self-declared and never count.
func (s *Server) provenAddresses(userID string) ([]string, error) {
	a, native, err := s.nativeMailAssignment(context.Background(), userID)
	if err != nil || native {
		return []string{a.Address}, err
	}
	store, err := s.userSendAsStore(userID)
	if err != nil {
		return nil, err
	}
	verified, err := store.ListVerified()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, alias := range verified {
		if alias.DomainProven() {
			out = append(out, strings.TrimSpace(alias.Email))
		}
	}
	return out, nil
}

// Internal discovery uses only the sender's own organization and only proven
// address bindings; an address two accounts prove is ambiguous and yields no
// internal key. Existing contact pins still veto a changed certificate;
// adding v6 never replaces the v4 pin.
func (s *Server) localPQRecipient(sender users.User, address string, resolver *keyResolver) (resolvedKey, bool, error) {
	accounts, err := s.users.List()
	if err != nil {
		return resolvedKey{}, false, err
	}
	var owner *users.User
	claims := 0
	for i, u := range accounts {
		if !u.Active || u.OrgID != sender.OrgID || !s.orgAdmit(AuthContext{OrgID: sender.OrgID}, u) {
			continue
		}
		addresses, err := s.provenAddresses(u.ID)
		if err != nil {
			// Unknown ownership closes internal discovery, not the whole resolve.
			s.logger.Error("internal PQ discovery could not read proven addresses", "user_id", u.ID, "error", err.Error())
			return resolvedKey{}, false, nil
		}
		if !slices.ContainsFunc(addresses, func(a string) bool { return strings.EqualFold(a, address) }) {
			continue
		}
		if claims++; u.PGPProtection() == users.PGPProtectionClient && s.pgpV6Ready(u) {
			owner = &accounts[i]
		}
	}
	if claims > 1 {
		s.logger.Warn("internal PQ discovery refused an address proven by two accounts", "address", address)
	}
	if claims != 1 || owner == nil {
		return resolvedKey{}, false, nil
	}
	u := *owner
	c, found, err := findContact(resolver.store, address)
	if err != nil {
		return resolvedKey{}, false, err
	}
	v4Pin := u.PGPFingerprint
	if keyVersionOf(c.PGPKey) == 6 {
		v4Pin = u.PGPFingerprintV6
	}
	if found && ((c.PGPKeyFingerprint != "" && !strings.EqualFold(c.PGPKeyFingerprint, v4Pin)) ||
		(c.PGPKeyV6Fingerprint != "" && !strings.EqualFold(c.PGPKeyV6Fingerprint, u.PGPFingerprintV6))) {
		return resolvedKey{Tier: tierKeyChanged}, true, nil
	}
	return resolvedKey{Armored: u.PGPPublicKeyV6, Fingerprint: u.PGPFingerprintV6, Tier: tierContactVerified, Usable: true, Capability: "pq"}, true, nil
}
