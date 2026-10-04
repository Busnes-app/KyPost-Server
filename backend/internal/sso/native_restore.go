package sso

import (
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
)

const NativeRestoreHoldFile = "native-restore-hold.json"

var ErrNativeRestoreHold = errors.New("native mail is held after restore; preserve storage and reconcile current identity, DNS and receiver evidence before resuming workers")

// RequireNativeRestoreReleased refuses even an unreadable or malformed hold.
// There is no automatic release: restored evidence cannot authorize live mail.
func RequireNativeRestoreReleased(stateRoot string) error {
	_, err := os.Lstat(filepath.Join(stateRoot, NativeRestoreHoldFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return ErrNativeRestoreHold
}

// FenceRestoredNativeTokens is only for exclusive, stopped restore staging.
// Qualification proves historical ownership, not current access or hold release.
func (s *LifecycleStore) FenceRestoredNativeTokens(stateRoot string, accounts []users.User) error {
	return fsutil.WithFileLock(s.path, func() error {
		if _, err := nativeRecoveryEpoch(stateRoot); err != nil {
			return err
		}
		if _, err := s.ValidateNativeSnapshot(stateRoot, accounts); err != nil {
			return err
		}
		f, err := s.load()
		if err != nil {
			return err
		}
		// Include every timestamp the verifier could have admitted before this
		// fence, including its 30-second future skew and the current second.
		cutoff := time.Now().Unix() + 31
		changed := false
		for _, u := range accounts {
			if u.NativeMailboxSource == "" {
				continue
			}
			key := directoryKey(u.NativeMailboxIssuer, u.SSOSub)
			d, ok := f.Directory[key]
			if !ok {
				return ErrNativeProvisioning
			}
			if d.RevokedBefore < cutoff {
				d.RevokedBefore = cutoff
				f.Directory[key] = d
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return fsutil.PersistJSONFile(s.path, f)
	})
}

// ValidateNativeSnapshot checks historical local consistency, never live access.
// All storage reads use stateRoot, not the ledger's original absolute StateRoot.
// It must not create storage, acknowledge preparations or rewrite reservations.
func (s *LifecycleStore) ValidateNativeSnapshot(stateRoot string, accounts []users.User) (bool, error) {
	lifecycle, err := s.load()
	if err != nil {
		return true, err
	}
	f, err := s.loadNative()
	if err != nil {
		return true, err
	}
	native := lifecycle.NativeProvisioningInitialized
	byID := map[string]users.User{}
	bySubject := map[string]users.User{}
	for _, u := range accounts {
		if u.NativeMailboxIssuer != "" || u.NativeMailboxSource != "" {
			native = true
			if !fsutil.SafePathComponent(u.ID) || u.NativeMailboxIssuer == "" || u.NativeMailboxSource == "" || u.SSOSub == "" {
				return true, ErrNativeProvisioning
			}
			a, ok := f.Accounts[directoryKey(u.NativeMailboxIssuer, u.SSOSub)]
			if !ok || a.Owner.Mailbox != u.ID || a.Owner.Issuer != u.NativeMailboxIssuer || a.Owner.Subject != u.SSOSub || a.Source != u.NativeMailboxSource {
				return true, ErrNativeProvisioning
			}
		}
		if prior, ok := byID[u.ID]; ok && (prior.NativeMailboxSource != "" || u.NativeMailboxSource != "") {
			return true, ErrNativeProvisioning
		}
		byID[u.ID] = u
		subject := strings.TrimSpace(u.SSOSub)
		if prior, ok := bySubject[subject]; subject != "" && ok && (prior.NativeMailboxSource != "" || u.NativeMailboxSource != "") {
			return true, ErrNativeProvisioning
		}
		if subject != "" {
			bySubject[subject] = u
		}
	}
	ids, addresses := map[string]bool{}, map[string]bool{}
	root := ""
	domain, err := NewNativeDomainStore(filepath.Dir(s.path)).Read()
	if err != nil {
		return native, err
	}
	for key, a := range f.Accounts {
		native = true
		if key != directoryKey(a.Owner.Issuer, a.Owner.Subject) || !directoryIdentifier(a.Owner.Issuer) || !directoryIdentifier(a.Owner.Subject) || !fsutil.SafePathComponent(a.Owner.Mailbox) || ids[a.Owner.Mailbox] || !filepath.IsAbs(a.StateRoot) || filepath.Clean(a.StateRoot) != a.StateRoot || root != "" && root != a.StateRoot || a.Limits.MessageBytes <= 0 || a.Limits.MessageBytes > mailmsg.MaxInboundMessageBytes || a.Limits.PayloadBytes < a.Limits.MessageBytes || a.Limits.Records <= 0 || a.Revision <= 0 || a.Digest == "" {
			return true, ErrNativeProvisioning
		}
		if a.Status != "pending" && a.Status != "applied" && a.Status != "failed" {
			return true, ErrNativeProvisioning
		}
		ids[a.Owner.Mailbox], root = true, a.StateRoot
		d, ok := lifecycle.Directory[key]
		if !ok || d.Resource == nil || d.Resource.ID != a.Owner.Subject || d.Resource.Active == nil || *d.Resource.Active != d.Active || a.Revision > d.Revision || a.Revision == d.Revision && (a.Digest != d.Digest || a.DesiredActive != d.Active) {
			return true, ErrNativeProvisioning
		}
		revision, err := d.Resource.Revision("user.updated")
		if err != nil || revision != d.Revision {
			return true, ErrNativeProvisioning
		}
		if domain.Issuer != a.Owner.Issuer || domain.Domain == "" {
			return true, ErrNativeProvisioning
		}
		if a.Address != "" {
			requested := DirectoryUser{}
			requested.Emails = append(requested.Emails, struct {
				Value   string `json:"value"`
				Primary bool   `json:"primary"`
			}{a.Address, true})
			address, err := nativePrimary(requested, domain.Domain)
			if err != nil || address != a.Address || addresses[address] {
				return true, ErrNativeProvisioning
			}
			addresses[address] = true
		}
		if u, ok := byID[a.Owner.Mailbox]; ok && a.Source != "" && (u.NativeMailboxIssuer != a.Owner.Issuer || u.SSOSub != a.Owner.Subject || u.NativeMailboxSource != a.Source) {
			return true, ErrNativeProvisioning
		}
		manifest := filepath.Join(stateRoot, "users", a.Owner.Mailbox, "native-mailbox.json")
		_, err = os.Lstat(manifest)
		if a.Source != "" || err == nil {
			source, err := mailbox.ValidatePreparedAccount(stateRoot, a.Owner, a.Address, a.Limits)
			if err != nil || a.Source != "" && source != a.Source {
				return true, ErrNativeProvisioning
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return true, err
		}
	}
	// Find complete orphan preparations, including a missing ledger/lifecycle pair.
	entries, err := os.ReadDir(filepath.Join(stateRoot, "users"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return native, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		_, manifestErr := os.Lstat(filepath.Join(stateRoot, "users", entry.Name(), "native-mailbox.json"))
		if manifestErr == nil && !ids[entry.Name()] {
			return true, ErrNativeProvisioning
		}
		if manifestErr != nil && !errors.Is(manifestErr, os.ErrNotExist) {
			return native, manifestErr
		}
		_, dbErr := os.Lstat(filepath.Join(stateRoot, "users", entry.Name(), "mailbox/mailbox.db"))
		if dbErr == nil && (!ids[entry.Name()] || manifestErr != nil) {
			return true, ErrNativeProvisioning
		}
		if dbErr != nil && !errors.Is(dbErr, os.ErrNotExist) {
			return native, dbErr
		}
	}
	err = filepath.WalkDir(stateRoot, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		switch entry.Name() {
		case "mailbox.db":
			native = true
			rel, err := filepath.Rel(stateRoot, path)
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if err != nil || len(parts) != 4 || parts[0] != "users" || parts[2] != "mailbox" || !ids[parts[1]] {
				return ErrNativeProvisioning
			}
		case "ingress.db":
			native = true
			if !lifecycle.NativeProvisioningInitialized {
				return ErrNativeProvisioning
			}
			return validateNativeReceiving(path, f.Accounts)
		}
		return nil
	})
	if err != nil {
		return native, err
	}
	return native, nil
}

// Receiving routes and frozen envelopes remain historical ownership evidence.
// Expired leases/routes are preserved; fresh authority is a hold-release gate.
func validateNativeReceiving(path string, assignments map[string]NativeAssignment) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ErrNativeProvisioning
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs}).String()+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var orphaned int
	if err := db.QueryRow(`SELECT count(*) FROM deliveries d WHERE d.state!='staged' AND NOT EXISTS(SELECT 1 FROM bindings b WHERE b.gateway=d.gateway AND b.id=d.id)`).Scan(&orphaned); err != nil {
		return err
	}
	if orphaned != 0 {
		return ErrNativeProvisioning
	}
	if err := db.QueryRow(`SELECT count(*) FROM bindings b WHERE NOT EXISTS(SELECT 1 FROM deliveries d WHERE b.gateway=d.gateway AND b.id=d.id)`).Scan(&orphaned); err != nil {
		return err
	}
	if orphaned != 0 {
		return ErrNativeProvisioning
	}
	rows, err := db.Query(`SELECT issuer,subject,mailbox,address,generation FROM routes UNION ALL SELECT issuer,subject,mailbox,address,generation FROM bindings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var issuer, subject, mailboxID, address string
		var generation int64
		if err := rows.Scan(&issuer, &subject, &mailboxID, &address, &generation); err != nil {
			return err
		}
		a, ok := assignments[directoryKey(issuer, subject)]
		if !ok || a.Owner.Issuer != issuer || a.Owner.Subject != subject || a.Owner.Mailbox != mailboxID || a.Address != address || a.Source == "" || generation <= 0 {
			return ErrNativeProvisioning
		}
	}
	return rows.Err()
}
