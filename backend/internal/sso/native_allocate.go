package sso

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// AllocateNativeAccount prepares new accounts for the opt-in native runtime.
// DNS happens before locks. Lock order: domain -> directory -> users -> account.
// The prepared mailbox and acknowledged assignment exist before users.json can
// expose this ID to lazy state creators. Existing IMAP accounts are never adopted.
// org is the account's write-once OrgID, "" in a single-tenant install.
func (s *LifecycleStore) AllocateNativeAccount(ctx context.Context, stateRoot, issuer, subject string, domains *NativeDomainStore, accounts *users.Store, limits mailbox.Limits, org string) (users.User, error) {
	if stateRoot == "" || domains == nil || accounts == nil {
		return users.User{}, ErrNativeProvisioning
	}
	if err := RequireNativeRestoreReleased(stateRoot); err != nil {
		return users.User{}, err
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return users.User{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	set, err := domains.ReadSet()
	if err != nil {
		return users.User{}, err
	}
	if set.Issuer != issuer {
		return users.User{}, ErrNativeDomain
	}
	// Prove the requested primary address's domain; reconcile rechecks the
	// address against it under the directory fence.
	requested, _, err := s.Directory(issuer, subject)
	if err != nil {
		return users.User{}, err
	}
	domain := ""
	if requested.Resource != nil {
		for _, email := range requested.Resource.Emails {
			if org == "" && email.Primary || org != "" && email.Type == "mailbox" {
				domain = AddressDomain(email.Value)
			}
		}
	}
	// A primary missing or off the configured domains is recorded as a durable
	// failure below, without DNS, instead of being retried as a lapsed proof.
	_, configured := set.Domains[domain]
	eligible := configured
	if org != "" && domain != "" {
		allowed, err := s.hostedMailDomains(ctx, issuer)
		if err != nil {
			return users.User{}, err
		}
		eligible = configured && slices.Contains(allowed, domain)
	}
	var proof NativeDomain
	if eligible {
		if proof, err = domains.VerifyDomain(ctx, domain); err != nil {
			return users.User{}, err
		}
		var proofCancel context.CancelFunc
		ctx, proofCancel = context.WithDeadline(ctx, time.Unix(proof.VerifiedUntil, 0))
		defer proofCancel()
	}
	ctx, release, err := vol.Lock(ctx, domains.path, vol.FenceNativeDomains, vol.Exclusive)
	if err != nil {
		return users.User{}, err
	}
	defer release()
	current, err := domains.ReadSet()
	if err != nil {
		return users.User{}, err
	}
	if _, nowConfigured := current.Domains[domain]; configured != nowConfigured || eligible && !current.CurrentProof(proof) {
		return users.User{}, ErrNativeDomain
	}
	ctx, releaseDirectory, err := s.LockDirectoryContext(ctx)
	if err != nil {
		return users.User{}, err
	}
	defer releaseDirectory()
	d, found, err := s.Directory(issuer, subject)
	if err != nil {
		return users.User{}, err
	}
	if !found || !d.Active || d.Resource == nil || d.Resource.Active == nil || !*d.Resource.Active || d.Resource.ID != subject {
		return users.User{}, ErrNativeProvisioning
	}
	f, err := s.loadNative()
	if err != nil {
		return users.User{}, err
	}
	id := f.Accounts[directoryKey(issuer, subject)].Owner.Mailbox
	if id == "" {
		id, err = fsutil.NewUUIDv4()
		if err != nil {
			return users.User{}, err
		}
	}
	role := users.RoleUser
	if HasAdminRole(d.Resource.Roles) {
		role = users.RoleAdmin
	}
	username := d.Resource.UserName
	if users.ValidateUsername(username) != nil {
		username = "native-" + id
	}
	if !eligible {
		_, err = s.reconcileNativeMailboxLocked(ctx, root, issuer, subject, id, "", limits, false, org != "", false, nil)
		return users.User{}, err
	}
	tableManifest := vol.OpenDocument(filepath.Join(root, "users", id, "native-mailbox.json")).IsTable()
	prepare := func() (string, error) {
		if tableManifest {
			a, e := s.reconcileNativeMailboxLocked(ctx, root, issuer, subject, id, proof.Domain, limits, false, org != "", true, func(root string, o mailbox.Owner, address string, l mailbox.Limits) (string, error) {
				return mailbox.PrepareAccountStorageContext(ctx, root, o, address, l)
			})
			return a.Source, e
		}
		a, e := s.reconcileNativeMailboxLocked(ctx, root, issuer, subject, id, proof.Domain, limits, false, org != "", false, func(root string, o mailbox.Owner, address string, l mailbox.Limits) (string, error) {
			return mailbox.PrepareAccountContext(ctx, root, o, address, l)
		})
		return a.Source, e
	}
	user, err := accounts.PublishPreparedSSOUser(ctx, id, username, role, issuer, subject, d.Resource.UserName, d.Resource.Email(), org, prepare)
	if errors.Is(err, users.ErrUsernameTaken) {
		user, err = accounts.PublishPreparedSSOUser(ctx, id, "native-"+id, role, issuer, subject, d.Resource.UserName, d.Resource.Email(), org, prepare)
	}
	if err != nil {
		return users.User{}, err
	}
	if tableManifest {
		if _, err = s.reconcileNativeMailboxLocked(ctx, root, issuer, subject, id, proof.Domain, limits, false, org != "", false, func(root string, o mailbox.Owner, address string, l mailbox.Limits) (string, error) {
			return mailbox.PrepareAccountContext(ctx, root, o, address, l)
		}); err != nil {
			return users.User{}, err
		}
	}
	return user, nil
}

// hostedMailDomains reads the suite allowlist only for hosted allocation. DNS proof
// remains independently required. No cached answer can outlive a domain removal.
// ponytail: suitedir has no email-domains reader yet; reuse the SSO transport policy.
func (s *LifecycleStore) hostedMailDomains(ctx context.Context, issuer string) ([]string, error) {
	settings := NewStore(filepath.Dir(s.path)).Load()
	if dir, ok := settings.SuiteDirectory(); !ok || dir.Issuer != issuer {
		return nil, ErrNativeDomain
	}
	base, err := url.Parse(issuer)
	if err != nil || requireSafeURL("suite issuer", issuer, false) != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrNativeDomain
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath("api", "suite", "email-domains").String(), nil)
	if err != nil {
		return nil, ErrNativeDomain
	}
	req.SetBasicAuth(settings.ClientID, settings.ClientSecret)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, ErrNativeDomain
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	var answer struct {
		Domains []string `json:"domains"`
	}
	if err != nil || resp.StatusCode != http.StatusOK || len(raw) > 64<<10 || json.Unmarshal(raw, &answer) != nil || answer.Domains == nil {
		return nil, ErrNativeDomain
	}
	for _, domain := range answer.Domains {
		if !nativeDomain(domain) || strings.ToLower(domain) != domain {
			return nil, ErrNativeDomain
		}
	}
	return answer.Domains, nil
}
