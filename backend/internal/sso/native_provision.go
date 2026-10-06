package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

var ErrNativeProvisioning = errors.New("native provisioning refused; preserve storage and reconcile verified identity, domain and address ownership")

// NativeAssignment is preparation evidence, never an access or receiving grant.
// Owner, address, root and limits stay reserved even after failure/offboarding.
type NativeAssignment struct {
	Owner         mailbox.Owner  `json:"owner"`
	Address       string         `json:"address"`
	StateRoot     string         `json:"stateRoot"`
	Limits        mailbox.Limits `json:"limits"`
	Revision      int64          `json:"revision"`
	Digest        string         `json:"digest"`
	DesiredActive bool           `json:"desiredActive"`
	Status        string         `json:"status"`
	Failure       string         `json:"failure,omitempty"`
	Source        string         `json:"source,omitempty"`
	// LegacyMixedUse exempts an administrator subject from the admission
	// refusal. Set only by migration; cleared when an active resource lacks
	// the administrator role. Hashed into the recovery authority digest.
	LegacyMixedUse bool `json:"legacyMixedUse,omitempty"`
}

// nativeAssignments is the single-mailbox view callers use. stored keeps the
// version-2 fields that view does not model (generations, history).
type nativeAssignments struct {
	Accounts map[string]NativeAssignment
	stored   nativeLedger
}

const nativeProvisioningFile = "native-provisioning.json"

// nativeLedger is native-provisioning.json version 2. Phase 1 has only primary
// mailboxes (ID = user ID) and their primary addresses.
type nativeLedger struct {
	Version   int                            `json:"version"`
	Accounts  map[string]nativeLedgerAccount `json:"accounts"`
	Mailboxes map[string]nativeLedgerMailbox `json:"mailboxes"`
	Addresses map[string]nativeLedgerAddress `json:"addresses"`
}

type nativeLedgerAccount struct {
	Revision       int64  `json:"revision"`
	Digest         string `json:"digest"`
	DesiredActive  bool   `json:"desiredActive"`
	Status         string `json:"status"`
	Failure        string `json:"failure,omitempty"`
	PrimaryMailbox string `json:"primaryMailbox"`
	LegacyMixedUse bool   `json:"legacyMixedUse"`
}

type nativeLedgerMailbox struct {
	Owner struct {
		Issuer  string `json:"issuer"`
		Subject string `json:"subject"`
	} `json:"owner"`
	Kind      string         `json:"kind"`
	State     string         `json:"state"`
	StateRoot string         `json:"stateRoot"`
	Limits    mailbox.Limits `json:"limits"`
	Source    string         `json:"source,omitempty"`
}

type nativeLedgerAddress struct {
	Mailbox    string                 `json:"mailbox"`
	Kind       string                 `json:"kind"`
	State      string                 `json:"state"`
	Generation int64                  `json:"generation"`
	History    []nativeAddressHistory `json:"history"`
}

type nativeAddressHistory struct {
	Mailbox    string `json:"mailbox"`
	Generation int64  `json:"generation"`
}

// parseNativeLedger returns the file's version. Version 1 is accepted only for
// historical snapshots and migration; the runtime reads version 2 alone.
func parseNativeLedger(raw []byte, historical bool) (nativeAssignments, int, error) {
	var head struct {
		Version  int                         `json:"version"`
		Accounts map[string]NativeAssignment `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nativeAssignments{}, 0, err
	}
	if head.Version == 1 && historical && head.Accounts != nil {
		return nativeAssignments{Accounts: head.Accounts}, 1, nil
	}
	var l nativeLedger
	if head.Version != 2 || json.Unmarshal(raw, &l) != nil || l.Accounts == nil || l.Mailboxes == nil || l.Addresses == nil || len(l.Mailboxes) != len(l.Accounts) {
		return nativeAssignments{}, head.Version, ErrNativeProvisioning
	}
	addressOf := map[string]string{}
	for address, x := range l.Addresses {
		if _, ok := l.Mailboxes[x.Mailbox]; !ok || x.Kind != "primary" || addressOf[x.Mailbox] != "" || x.Generation < 1 || len(x.History) == 0 {
			return nativeAssignments{}, 2, ErrNativeProvisioning
		}
		addressOf[x.Mailbox] = address
	}
	f := nativeAssignments{Accounts: map[string]NativeAssignment{}, stored: l}
	for key, a := range l.Accounts {
		m, ok := l.Mailboxes[a.PrimaryMailbox]
		if !ok || m.Kind != "primary" || key != directoryKey(m.Owner.Issuer, m.Owner.Subject) {
			return nativeAssignments{}, 2, ErrNativeProvisioning
		}
		f.Accounts[key] = NativeAssignment{
			Owner:   mailbox.Owner{Issuer: m.Owner.Issuer, Subject: m.Owner.Subject, Mailbox: a.PrimaryMailbox},
			Address: addressOf[a.PrimaryMailbox], StateRoot: m.StateRoot, Limits: m.Limits,
			Revision: a.Revision, Digest: a.Digest, DesiredActive: a.DesiredActive, Status: a.Status, Failure: a.Failure, Source: m.Source,
			LegacyMixedUse: a.LegacyMixedUse,
		}
	}
	return f, 2, nil
}

// ledger renders version 2. Address generations never decrease and stay at
// least the subject's directory revision, which is what phase-1 routes and
// bindings carry; history starts at generation 1.
func (f nativeAssignments) ledger(directory map[string]DirectoryState) nativeLedger {
	l := nativeLedger{Version: 2, Accounts: map[string]nativeLedgerAccount{}, Mailboxes: map[string]nativeLedgerMailbox{}, Addresses: map[string]nativeLedgerAddress{}}
	for address, x := range f.stored.Addresses {
		l.Addresses[address] = x
	}
	for key, a := range f.Accounts {
		state := "disabled"
		if a.DesiredActive {
			state = "active"
		}
		l.Accounts[key] = nativeLedgerAccount{Revision: a.Revision, Digest: a.Digest, DesiredActive: a.DesiredActive, Status: a.Status, Failure: a.Failure, PrimaryMailbox: a.Owner.Mailbox, LegacyMixedUse: a.LegacyMixedUse}
		m := nativeLedgerMailbox{Kind: "primary", State: state, StateRoot: a.StateRoot, Limits: a.Limits, Source: a.Source}
		m.Owner.Issuer, m.Owner.Subject = a.Owner.Issuer, a.Owner.Subject
		l.Mailboxes[a.Owner.Mailbox] = m
		if a.Address == "" {
			continue
		}
		x, ok := l.Addresses[a.Address]
		if !ok {
			x.History = []nativeAddressHistory{{Mailbox: a.Owner.Mailbox, Generation: 1}}
		}
		x.Mailbox, x.Kind, x.State = a.Owner.Mailbox, "primary", state
		x.Generation = max(x.Generation, directory[key].Revision, 1)
		l.Addresses[a.Address] = x
	}
	return l
}

func (s *LifecycleStore) nativePath() string {
	return filepath.Join(filepath.Dir(s.path), nativeProvisioningFile)
}

func (s *LifecycleStore) loadNative() (nativeAssignments, error) {
	if err := checkNativeFormat(filepath.Dir(s.path)); err != nil {
		return nativeAssignments{}, err
	}
	f, _, err := s.loadNativeLedger(false)
	return f, err
}

// loadNativeLedger also reports the file's version (0 when absent).
func (s *LifecycleStore) loadNativeLedger(historical bool) (nativeAssignments, int, error) {
	lifecycle, err := s.load()
	if err != nil {
		return nativeAssignments{}, 0, err
	}
	f := nativeAssignments{Accounts: map[string]NativeAssignment{}}
	loaded, version := false, 0
	var parseErr error
	err = fsutil.LoadJSONFile(s.nativePath(), func(raw json.RawMessage) {
		loaded = true
		f, version, parseErr = parseNativeLedger(raw, historical)
	}, func() error {
		if lifecycle.NativeProvisioningInitialized {
			return ErrNativeProvisioning
		}
		return nil
	})
	if err == nil && (parseErr != nil || loaded && !lifecycle.NativeProvisioningInitialized) {
		err = ErrNativeProvisioning
	}
	return f, version, err
}

// Initialization fences missing-ledger repair. A crash after the fence but
// before the first ledger requires explicit recovery; it never frees addresses.
func (s *LifecycleStore) saveNative(f nativeAssignments) error {
	lifecycle, err := s.load()
	if err != nil {
		return err
	}
	if !lifecycle.NativeProvisioningInitialized {
		lifecycle.NativeProvisioningInitialized = true
		if err = fsutil.PersistJSONFile(s.path, lifecycle); err != nil {
			return err
		}
	}
	return fsutil.PersistJSONFile(s.nativePath(), f.ledger(lifecycle.Directory))
}

// ensureNativeLedger creates an empty version-2 ledger when none exists. A
// fenced (initialized) deployment with a lost ledger still refuses.
func (s *LifecycleStore) ensureNativeLedger(ctx context.Context) error {
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	return s.ensureNativeLedgerLocked()
}

// An existing ledger counts only when it is version 2; a version-1 one means
// migration has not finished and must never be adopted behind its back.
func (s *LifecycleStore) ensureNativeLedgerLocked() error {
	f, version, err := s.loadNativeLedger(false)
	if version != 0 && version != 2 {
		return ErrNativeMigration
	}
	if err != nil || version == 2 {
		return err
	}
	return s.saveNative(f)
}

// NativeAssignment reads persisted status only. Consumers must recheck the live
// directory fence: this snapshot may precede a newer deactivation.
func (s *LifecycleStore) NativeAssignment(issuer, subject string) (NativeAssignment, bool, error) {
	f, err := s.loadNative()
	a, ok := f.Accounts[directoryKey(issuer, subject)]
	return a, ok, err
}

// NativeAssignmentForAddress locates immutable ownership, not permission.
// Receiving callers must still admit the current account before binding mail.
func (s *LifecycleStore) NativeAssignmentForAddress(issuer, address string) (NativeAssignment, bool, error) {
	f, err := s.loadNative()
	if err != nil {
		return NativeAssignment{}, false, err
	}
	for _, a := range f.Accounts {
		if a.Owner.Issuer == issuer && a.Address == address {
			return a, true, nil
		}
	}
	return NativeAssignment{}, false, nil
}

// ReconcileNativeMailbox is the blocking internal compatibility wrapper.
// The caller proves domain authority and that localID is the NEW account bound
// to this issuer/subject, before publishing that account to ordinary state users.
// SCIM email is only a requested primary address, never domain proof or aliases.
func (s *LifecycleStore) ReconcileNativeMailbox(stateRoot, issuer, subject, localID, verifiedDomain string, limits mailbox.Limits) (NativeAssignment, error) {
	return s.reconcileNativeMailbox(stateRoot, issuer, subject, localID, verifiedDomain, limits, mailbox.PrepareAccount)
}

func (s *LifecycleStore) ReconcileNativeMailboxContext(ctx context.Context, stateRoot, issuer, subject, localID, verifiedDomain string, limits mailbox.Limits) (NativeAssignment, error) {
	return s.reconcileNativeMailboxContext(ctx, stateRoot, issuer, subject, localID, verifiedDomain, limits, func(root string, owner mailbox.Owner, address string, limits mailbox.Limits) (string, error) {
		return mailbox.PrepareAccountContext(ctx, root, owner, address, limits)
	})
}

func (s *LifecycleStore) reconcileNativeMailbox(stateRoot, issuer, subject, localID, domain string, limits mailbox.Limits, prepare func(string, mailbox.Owner, string, mailbox.Limits) (string, error)) (NativeAssignment, error) {
	ctx := context.Background()
	return s.reconcileNativeMailboxContext(ctx, stateRoot, issuer, subject, localID, domain, limits, prepare)
}

func (s *LifecycleStore) reconcileNativeMailboxContext(ctx context.Context, stateRoot, issuer, subject, localID, domain string, limits mailbox.Limits, prepare func(string, mailbox.Owner, string, mailbox.Limits) (string, error)) (NativeAssignment, error) {
	var result NativeAssignment
	if stateRoot == "" || !fsutil.SafePathComponent(localID) || !directoryIdentifier(issuer) || !directoryIdentifier(subject) || !nativeDomain(domain) || limits.MessageBytes <= 0 || limits.MessageBytes > mailmsg.MaxInboundMessageBytes || limits.PayloadBytes < limits.MessageBytes || limits.Records <= 0 {
		return result, ErrNativeProvisioning
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return result, err
	}
	// ponytail: instance-wide directory lock covers local preparation,
	// never network work. Split locks only after measuring contention and keeping
	// the same revision/revocation fence across publication.
	release, err := fsutil.LockFileContext(ctx, s.path)
	if err != nil {
		return result, err
	}
	defer release()
	return s.reconcileNativeMailboxLocked(ctx, root, issuer, subject, localID, domain, limits, prepare)
}

func (s *LifecycleStore) reconcileNativeMailboxLocked(ctx context.Context, root, issuer, subject, localID, domain string, limits mailbox.Limits, prepare func(string, mailbox.Owner, string, mailbox.Limits) (string, error)) (NativeAssignment, error) {
	var result NativeAssignment
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !filepath.IsAbs(root) || !fsutil.SafePathComponent(localID) || !directoryIdentifier(issuer) || !directoryIdentifier(subject) || !nativeDomain(domain) || limits.MessageBytes <= 0 || limits.MessageBytes > mailmsg.MaxInboundMessageBytes || limits.PayloadBytes < limits.MessageBytes || limits.Records <= 0 {
		return result, ErrNativeProvisioning
	}
	err := func() error {
		d, known, e := s.Directory(issuer, subject)
		if e != nil {
			return e
		}
		if !known || d.Resource == nil || d.Resource.ID != subject || d.Resource.Active == nil || *d.Resource.Active != d.Active {
			return ErrNativeProvisioning
		}
		revision, e := d.Resource.Revision("user.updated")
		if e != nil || revision != d.Revision {
			return ErrNativeProvisioning
		}
		f, e := s.loadNative()
		if e != nil {
			return e
		}
		key := directoryKey(issuer, subject)
		a, exists := f.Accounts[key]
		owner := mailbox.Owner{Issuer: issuer, Subject: subject, Mailbox: localID}
		if exists && (a.Owner != owner || a.StateRoot != root || a.Limits != limits || a.Revision > d.Revision || (a.Revision == d.Revision && a.Digest != d.Digest)) {
			return ErrNativeProvisioning
		}
		// Administrator subjects get a mailbox-less account; never reserve one.
		if !exists && (!d.Active || HasAdminRole(d.Resource.Roles)) {
			return ErrNativeProvisioning
		}
		for otherKey, other := range f.Accounts {
			if otherKey != key && other.Owner.Mailbox == localID {
				return ErrNativeProvisioning
			}
		}
		a.Owner = owner
		a.StateRoot = root
		a.Limits = limits
		a.Revision = d.Revision
		a.Digest = d.Digest
		a.DesiredActive = d.Active
		if directoryDemoted(*d.Resource) {
			a.LegacyMixedUse = false
		}
		fail := func(code string) error {
			a.Status = "failed"
			a.Failure = code
			result = a
			f.Accounts[key] = a
			if err := s.saveNative(f); err != nil {
				return err
			}
			return ErrNativeProvisioning
		}
		address := a.Address
		if d.Active {
			address, e = nativePrimary(*d.Resource, domain)
			if e != nil {
				return fail("invalid_primary_address")
			}
			if exists && a.Address != "" && a.Address != address {
				return fail("primary_address_change")
			}
		} else if !strings.HasSuffix(address, "@"+domain) {
			return fail("domain_change")
		}
		for otherKey, other := range f.Accounts {
			if otherKey != key && other.Address == address {
				return fail("address_conflict")
			}
		}
		a.Address = address
		a.Status = "pending"
		a.Failure = ""
		f.Accounts[key] = a
		if e = ctx.Err(); e != nil {
			return e
		}
		// Reserve durably BEFORE touching account files; a killed writer is repairable.
		if e = s.saveNative(f); e != nil {
			return e
		}
		if a.Source != "" {
			result.Source, e = mailbox.ValidatePreparedAccount(root, owner, address, limits)
			if e == nil && result.Source != a.Source {
				e = mailbox.ErrPreparation
			}
		} else if d.Active {
			result.Source, e = prepare(root, owner, address, limits)
		}
		if e != nil {
			a.Status = "failed"
			a.Failure = "storage_conflict_or_unavailable"
		} else {
			a.Status = "applied"
			if result.Source != "" {
				a.Source = result.Source
			}
		}
		result = a
		f.Accounts[key] = a
		if saveErr := s.saveNative(f); saveErr != nil {
			return saveErr
		}
		return e
	}()
	return result, err
}

// Conservative ASCII DNS names and case-insensitive bare dot-atom mailboxes.
// Refuse quoted/SMTPUTF8 addresses until an end-to-end transport contract exists.
func nativeDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func nativePrimary(u DirectoryUser, domain string) (string, error) {
	address := ""
	count := 0
	for _, email := range u.Emails {
		if email.Primary {
			count++
			address = email.Value
		}
	}
	a, e := mail.ParseAddress(address)
	local, host, ok := strings.Cut(address, "@")
	if count != 1 || e != nil || a.Address != address || !ok || len(address) > 254 || len(local) == 0 || len(local) > 64 || strings.ToLower(host) != domain || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", ErrNativeProvisioning
	}
	for _, r := range local {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-/=?^_`{|}~.", r)
		if !valid {
			return "", fmt.Errorf("%w: unsupported primary address", ErrNativeProvisioning)
		}
	}
	return strings.ToLower(address), nil
}
