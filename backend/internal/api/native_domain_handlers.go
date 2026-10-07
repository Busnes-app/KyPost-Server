package api

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

// nativeMigrationRefused answers a pending/failed storage migration with its
// own remediation instead of a generic domain error.
func nativeMigrationRefused(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, sso.ErrNativeMigration) {
		return false
	}
	http.Error(w, "native mail storage migration is pending or failed, so native mail is refused; read the kypost-server migrate-native error in the container log, fix it and restart, or restore the pre-upgrade backup (docs/RESTORE.md#storage-format-migration)", http.StatusServiceUnavailable)
	return true
}

func (s *Server) handleNativeMailDomain(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	store := s.nativeDomains
	if r.Method == http.MethodGet {
		d, err := store.Read()
		if nativeMigrationRefused(w, err) {
			return
		}
		if err != nil {
			http.Error(w, "mail domain state unreadable; preserve configuration and restore it", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, 200, map[string]any{"configured": d.Domain != "", "claim": d, "recordName": d.RecordName(), "recordValue": d.RecordValue(), "receivingEnabled": false})
		return
	}
	var body struct {
		Domain     string `json:"domain"`
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	issuer := s.ssoStore.Load().IssuerURL
	if issuer == "" {
		http.Error(w, "configure KyIdentity before claiming a mail domain", http.StatusServiceUnavailable)
		return
	}
	if s.refuseBlockedDomain(w, body.Domain) {
		return
	}
	d, err := store.Configure(r.Context(), body.Domain, issuer)
	if nativeMigrationRefused(w, err) {
		return
	}
	if err != nil {
		http.Error(w, "mail domain configuration refused; preserve the existing domain, use an issuer without a trailing slash and retry a valid challenge", http.StatusConflict)
		return
	}
	writeJSON(w, 200, map[string]any{"claim": d, "recordName": d.RecordName(), "recordValue": d.RecordValue(), "receivingEnabled": false})
}
func (s *Server) handleNativeMailDomainVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	store := s.nativeDomains
	prior, err := store.Read()
	if nativeMigrationRefused(w, err) {
		return
	}
	if err != nil || prior.Issuer == "" || prior.Issuer != s.ssoStore.Load().IssuerURL {
		http.Error(w, "mail domain issuer missing or changed; restore matching configuration", http.StatusConflict)
		return
	}
	d, err := store.Verify(r.Context())
	if err != nil {
		http.Error(w, "mail domain verification failed; check the exact TXT record, challenge expiry and DNS availability", http.StatusConflict)
		return
	}
	writeJSON(w, 200, map[string]any{"claim": d, "receivingEnabled": false})
}

// nativeDomainStatus is one entry of the domain-set API. Retired domains have
// no proof, so their record fields are empty.
func nativeDomainStatus(d sso.NativeDomain, retired bool) map[string]any {
	recordName := d.RecordName()
	if retired {
		recordName = ""
	}
	return map[string]any{"domain": d.Domain, "configured": !retired, "retired": retired, "recordName": recordName, "recordValue": d.RecordValue(), "established": d.Established, "expiresAt": d.ExpiresAt, "verifiedUntil": d.VerifiedUntil}
}

// handleNativeMailDomains lists (GET) or adds/re-challenges (POST) domains.
func (s *Server) handleNativeMailDomains(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		set, err := s.nativeDomains.ReadSet()
		if nativeMigrationRefused(w, err) {
			return
		}
		if err != nil {
			http.Error(w, "mail domain state unreadable; preserve configuration and restore it", http.StatusServiceUnavailable)
			return
		}
		list := []map[string]any{}
		for _, domain := range slices.Sorted(maps.Keys(set.Domains)) {
			list = append(list, nativeDomainStatus(set.Domains[domain], false))
		}
		for _, domain := range slices.Sorted(slices.Values(set.Retired)) {
			list = append(list, nativeDomainStatus(sso.NativeDomain{Domain: domain}, true))
		}
		writeJSON(w, http.StatusOK, map[string]any{"issuer": set.Issuer, "founding": set.Founding, "domains": list, "receivingEnabled": false})
		return
	}
	var body struct {
		Domain     string `json:"domain"`
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	issuer := s.ssoStore.Load().IssuerURL
	if issuer == "" {
		http.Error(w, "configure KyIdentity before claiming a mail domain", http.StatusServiceUnavailable)
		return
	}
	if s.refuseBlockedDomain(w, body.Domain) {
		return
	}
	d, err := s.nativeDomains.ConfigureDomain(r.Context(), body.Domain, issuer)
	if nativeMigrationRefused(w, err) {
		return
	}
	if err != nil {
		http.Error(w, "mail domain refused; use a lowercase DNS name that is not retired, under the same KyIdentity issuer", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, nativeDomainStatus(d, false))
}

func (s *Server) handleNativeMailDomainsVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	set, err := s.nativeDomains.ReadSet()
	if nativeMigrationRefused(w, err) {
		return
	}
	if err != nil || set.Issuer == "" || set.Issuer != s.ssoStore.Load().IssuerURL {
		http.Error(w, "mail domain issuer missing or changed; restore matching configuration", http.StatusConflict)
		return
	}
	d, err := s.nativeDomains.VerifyDomain(r.Context(), r.PathValue("domain"))
	if err != nil {
		http.Error(w, "mail domain verification failed; check the domain is configured, the exact TXT record, challenge expiry and DNS availability", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, nativeDomainStatus(d, false))
}

// handleNativeMailDomainsRetire never deletes address records or mail.
func (s *Server) handleNativeMailDomainsRetire(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if !s.nativeDomainGate(w, r, &body) {
		return
	}
	domain := r.PathValue("domain")
	err := s.nativeDomains.RetireDomain(r.Context(), domain, s.stateDir, filepath.Join(config.SecretDir(), "native-relay.key"))
	if nativeMigrationRefused(w, err) {
		return
	}
	if errors.Is(err, sso.ErrNativeDomainInUse) || errors.Is(err, sso.ErrNativeRestoreHold) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "mail domain retirement refused; only a configured domain can be retired, and storage must be readable", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, nativeDomainStatus(sso.NativeDomain{Domain: domain}, true))
}

func (s *Server) nativeDomainGate(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil || json.Unmarshal(raw, into) != nil {
		http.Error(w, "invalid mail administration request", http.StatusBadRequest)
		return false
	}
	var credential struct {
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if json.Unmarshal(raw, &credential) != nil {
		http.Error(w, "invalid mail administration request", http.StatusBadRequest)
		return false
	}
	ac, ok := authFromContext(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return s.confirmActor(w, r, ac.UserID, credential.Password, credential.AuthSecret)
}
