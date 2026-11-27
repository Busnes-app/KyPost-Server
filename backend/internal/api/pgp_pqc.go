package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

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
	if u.PGPPublicKeyV6 == "" || u.PGPKeyring == nil || u.PGPKeyring.Version != 2 {
		return false
	}
	store, err := s.userStore(u.ID)
	if err != nil {
		return false
	}
	ready, err := store.PGPV6Ready(u.PGPKeyring.MaterialGeneration, u.PGPFingerprint)
	return err == nil && ready
}

// Internal discovery uses only the sender's own organization. Existing contact
// pins still veto a changed certificate; adding v6 never replaces the v4 pin.
func (s *Server) localPQRecipient(sender users.User, address string, resolver *keyResolver) (resolvedKey, bool, error) {
	accounts, err := s.users.List()
	if err != nil {
		return resolvedKey{}, false, err
	}
	for _, u := range accounts {
		if !u.Active || u.OrgID != sender.OrgID || u.PGPProtection() != users.PGPProtectionClient || !s.pgpV6Ready(u) {
			continue
		}
		owned := false
		for _, addr := range s.accountAddresses(u.ID) {
			if strings.EqualFold(addr, address) {
				owned = true
			}
		}
		if !owned {
			continue
		}
		c, found, err := findContact(resolver.store, address)
		if err != nil {
			return resolvedKey{}, false, err
		}
		if found && ((c.PGPKeyFingerprint != "" && !strings.EqualFold(c.PGPKeyFingerprint, func() string {
			if keyVersionOf(c.PGPKey) == 6 {
				return u.PGPFingerprintV6
			}
			return u.PGPFingerprint
		}())) ||
			(c.PGPKeyV6Fingerprint != "" && !strings.EqualFold(c.PGPKeyV6Fingerprint, u.PGPFingerprintV6))) {
			return resolvedKey{Tier: tierKeyChanged}, true, nil
		}
		return resolvedKey{Armored: u.PGPPublicKeyV6, Fingerprint: u.PGPFingerprintV6, Tier: tierContactVerified, Usable: true, Capability: "pq"}, true, nil
	}
	return resolvedKey{}, false, nil
}
