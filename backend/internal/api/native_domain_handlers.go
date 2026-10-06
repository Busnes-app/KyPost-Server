package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
func (s *Server) nativeDomainGate(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil || json.Unmarshal(raw, into) != nil {
		http.Error(w, "invalid mail domain request", http.StatusBadRequest)
		return false
	}
	var credential struct {
		Password   string `json:"password"`
		AuthSecret string `json:"authSecret"`
	}
	if json.Unmarshal(raw, &credential) != nil {
		http.Error(w, "invalid mail domain request", http.StatusBadRequest)
		return false
	}
	ac, ok := authFromContext(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return s.confirmActor(w, r, ac.UserID, credential.Password, credential.AuthSecret)
}
