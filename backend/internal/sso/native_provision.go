package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"path/filepath"
	"strings"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/ingress"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

var ErrNativeProvisioning = errors.New("native provisioning refused; preserve storage and reconcile verified identity, domain and address ownership")

// ErrNativeAdministrator is the administrator-separation refusal. It wraps
// ErrNativeProvisioning, so every existing refusal path still applies.
var ErrNativeAdministrator = fmt.Errorf("%w: administrator identities have no mailbox; use your everyday identity", ErrNativeProvisioning)

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

// nativeLedger is native-provisioning.json version 2: primary mailboxes (ID =
// user ID), their primary addresses and administrator-managed aliases.
type nativeLedger struct {
	Version   int                            `json:"version"`
	Accounts  map[string]nativeLedgerAccount `json:"accounts"`
	Mailboxes map[string]nativeLedgerMailbox `json:"mailboxes"`
	Addresses map[string]nativeLedgerAddress `json:"addresses"`
	// AddressGenerations marks generations as authoritative: from the first
	// write with it set they change only on reassign, disable, re-enable and
	// release. Files without it were written while routes carried the
	// directory revision, so loading raises each generation to that revision.
	AddressGenerations bool `json:"addressGenerations,omitempty"`
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

// validHistory: strictly increasing generations, the last entry is the
// current mailbox and never above the current generation.
func (x nativeLedgerAddress) validHistory() bool {
	if len(x.History) == 0 || x.History[len(x.History)-1].Mailbox != x.Mailbox || x.History[len(x.History)-1].Generation > x.Generation {
		return false
	}
	for i, h := range x.History {
		if h.Generation < 1 || i > 0 && h.Generation <= x.History[i-1].Generation {
			return false
		}
	}
	return true
}

// heldBy reports whether mailbox held the address at generation: some
// history entry i names it and history[i] <= generation < history[i+1], or
// i is last and history[i] <= generation <= the current generation.
func (x nativeLedgerAddress) heldBy(mailbox string, generation int64) bool {
	for i, h := range x.History {
		upper := x.Generation + 1
		if i+1 < len(x.History) {
			upper = x.History[i+1].Generation
		}
		if h.Mailbox == mailbox && h.Generation <= generation && generation < upper {
			return true
		}
	}
	return false
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
		_, ok := l.Mailboxes[x.Mailbox]
		primary := x.Kind == "primary" && (x.State == "active" || x.State == "disabled") && addressOf[x.Mailbox] == ""
		alias := x.Kind == "alias" && (x.State == "active" || x.State == "disabled" || x.State == "reserved")
		if !ok || !primary && !alias || x.Generation < 1 || !x.validHistory() || address != strings.ToLower(address) {
			return nativeAssignments{}, 2, ErrNativeProvisioning
		}
		if primary {
			addressOf[x.Mailbox] = address
		}
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

// seedGenerations is the one-time switch to authoritative generations. Routes
// and bindings written before it carry a directory revision no newer than the
// subject's current one, so raising to it keeps every generation at least as
// high as anything already written; the next save freezes the result.
func (l *nativeLedger) seedGenerations(directory map[string]DirectoryState) {
	if l.AddressGenerations {
		return
	}
	for address, x := range l.Addresses {
		if m, ok := l.Mailboxes[x.Mailbox]; ok {
			x.Generation = max(x.Generation, directory[directoryKey(m.Owner.Issuer, m.Owner.Subject)].Revision)
			l.Addresses[address] = x
		}
	}
}

// ledger renders version 2. Existing address records are kept as they are
// (syncAddressStates owns their state); a new primary address starts at the
// subject's directory revision, which is what version-1 routes and bindings
// carry, with history from generation 1.
func (f nativeAssignments) ledger(directory map[string]DirectoryState) nativeLedger {
	l := nativeLedger{Version: 2, AddressGenerations: true, Accounts: map[string]nativeLedgerAccount{}, Mailboxes: map[string]nativeLedgerMailbox{}, Addresses: map[string]nativeLedgerAddress{}}
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
		if _, ok := l.Addresses[a.Address]; a.Address == "" || ok {
			continue
		}
		l.Addresses[a.Address] = nativeLedgerAddress{Mailbox: a.Owner.Mailbox, Kind: "primary", State: state, Generation: max(directory[key].Revision, 1), History: []nativeAddressHistory{{Mailbox: a.Owner.Mailbox, Generation: 1}}}
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
	f.stored.seedGenerations(lifecycle.Directory)
	return f, version, err
}

// saveNative is the format write (migration, empty ledger): no state rule.
func (s *LifecycleStore) saveNative(f nativeAssignments) error {
	return s.persistNative(f, nil, false)
}

// commitNative applies the desired-state rule, persists the ledger and then
// writes every non-active address's route inactive. The caller holds the
// directory lock; overlay carries a directory state not yet recorded.
func (s *LifecycleStore) commitNative(f nativeAssignments, overlay map[string]DirectoryState) error {
	return s.persistNative(f, overlay, true)
}

// Initialization fences missing-ledger repair. A crash after the fence but
// before the first ledger requires explicit recovery; it never frees addresses.
func (s *LifecycleStore) persistNative(f nativeAssignments, overlay map[string]DirectoryState, sync bool) error {
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
	directory := maps.Clone(lifecycle.Directory)
	maps.Copy(directory, overlay)
	l := f.ledger(directory)
	if !sync {
		return fsutil.PersistJSONFile(s.nativePath(), l)
	}
	inactive, err := s.syncAddressStates(&l, directory)
	if err != nil {
		return err
	}
	if err = fsutil.PersistJSONFile(s.nativePath(), l); err != nil {
		return err
	}
	root := ""
	for _, m := range l.Mailboxes {
		root = m.StateRoot
	}
	if root == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ingress.DeactivateRoutes(ctx, filepath.Join(root, "receiving"), inactive)
}

// syncAddressStates is the level-triggered desired-state rule: active iff the
// owner is active, has no administrator role (or is legacyMixedUse) and the
// domain is configured, not retired; reserved stays reserved. Every change
// bumps the generation. It returns each non-active address's generation.
// ponytail: every commit rewrites all non-active routes in one transaction;
// track changed addresses instead if ledgers grow to many thousands.
func (s *LifecycleStore) syncAddressStates(l *nativeLedger, directory map[string]DirectoryState) (map[string]int64, error) {
	inactive := map[string]int64{}
	if len(l.Addresses) == 0 {
		return inactive, nil
	}
	domains, err := NewNativeDomainStore(filepath.Dir(s.path)).ReadSet()
	if err != nil {
		return nil, err
	}
	for address, x := range l.Addresses {
		if x.State != "reserved" {
			state := "disabled"
			if l.desiredActive(address, x.Mailbox, directory, domains) {
				state = "active"
			}
			if x.State != state {
				x.State = state
				x.Generation++
				l.Addresses[address] = x
			}
		}
		if x.State != "active" {
			inactive[address] = x.Generation
		}
	}
	return inactive, nil
}

func (l nativeLedger) desiredActive(address, mailboxID string, directory map[string]DirectoryState, domains NativeDomainSet) bool {
	m := l.Mailboxes[mailboxID]
	key := directoryKey(m.Owner.Issuer, m.Owner.Subject)
	d := directory[key]
	_, configured := domains.Domains[AddressDomain(address)]
	return configured && d.Active && d.Resource != nil && (!HasAdminRole(d.Resource.Roles) || l.Accounts[key].LegacyMixedUse)
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

// NativeAssignmentForAddress locates the account whose mailbox currently
// records address (primary or alias, any state): ownership, not permission.
// Receiving callers must still admit the account and check the address under
// the directory fence before binding mail.
func (s *LifecycleStore) NativeAssignmentForAddress(issuer, address string) (NativeAssignment, bool, error) {
	f, err := s.loadNative()
	if err != nil {
		return NativeAssignment{}, false, err
	}
	x, ok := f.stored.Addresses[address]
	for _, a := range f.Accounts {
		if ok && a.Owner.Issuer == issuer && a.Owner.Mailbox == x.Mailbox {
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
	return s.reconcileNativeMailboxContext(ctx, stateRoot, issuer, subject, localID, verifiedDomain, limits, false, func(root string, owner mailbox.Owner, address string, limits mailbox.Limits) (string, error) {
		return mailbox.PrepareAccountContext(ctx, root, owner, address, limits)
	})
}

// DisableNativeMailboxContext records a deactivation without domain proof. It
// refuses, changing nothing, if the subject is active again by the time the
// directory fence is held: reactivation needs allocation's fresh proof.
func (s *LifecycleStore) DisableNativeMailboxContext(ctx context.Context, stateRoot, issuer, subject, localID, addressDomain string, limits mailbox.Limits) (NativeAssignment, error) {
	return s.reconcileNativeMailboxContext(ctx, stateRoot, issuer, subject, localID, addressDomain, limits, true, nil)
}

func (s *LifecycleStore) reconcileNativeMailbox(stateRoot, issuer, subject, localID, domain string, limits mailbox.Limits, prepare func(string, mailbox.Owner, string, mailbox.Limits) (string, error)) (NativeAssignment, error) {
	ctx := context.Background()
	return s.reconcileNativeMailboxContext(ctx, stateRoot, issuer, subject, localID, domain, limits, false, prepare)
}

func (s *LifecycleStore) reconcileNativeMailboxContext(ctx context.Context, stateRoot, issuer, subject, localID, domain string, limits mailbox.Limits, disableOnly bool, prepare func(string, mailbox.Owner, string, mailbox.Limits) (string, error)) (NativeAssignment, error) {
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
	return s.reconcileNativeMailboxLocked(ctx, root, issuer, subject, localID, domain, limits, disableOnly, prepare)
}

// domain is the freshly proven domain; "" means the requested primary is not on
// a configured domain, which is recorded as a failure for an active subject.
func (s *LifecycleStore) reconcileNativeMailboxLocked(ctx context.Context, root, issuer, subject, localID, domain string, limits mailbox.Limits, disableOnly bool, prepare func(string, mailbox.Owner, string, mailbox.Limits) (string, error)) (NativeAssignment, error) {
	var result NativeAssignment
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !filepath.IsAbs(root) || !fsutil.SafePathComponent(localID) || !directoryIdentifier(issuer) || !directoryIdentifier(subject) || domain != "" && !nativeDomain(domain) || limits.MessageBytes <= 0 || limits.MessageBytes > mailmsg.MaxInboundMessageBytes || limits.PayloadBytes < limits.MessageBytes || limits.Records <= 0 {
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
		if e != nil || revision != d.Revision || disableOnly && d.Active {
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
			if err := s.commitNative(f, nil); err != nil {
				return err
			}
			return ErrNativeProvisioning
		}
		address := a.Address
		if d.Active && domain == "" {
			return fail("primary_domain_unavailable")
		}
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
		// An alias or reserved address of any mailbox is never a new primary.
		if x, taken := f.stored.Addresses[address]; taken && (x.Mailbox != localID || x.Kind != "primary") {
			return fail("address_conflict")
		}
		a.Address = address
		a.Status = "pending"
		a.Failure = ""
		f.Accounts[key] = a
		if e = ctx.Err(); e != nil {
			return e
		}
		// Reserve durably BEFORE touching account files; a killed writer is repairable.
		if e = s.commitNative(f, nil); e != nil {
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
		if saveErr := s.commitNative(f, nil); saveErr != nil {
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
	if count != 1 {
		return "", ErrNativeProvisioning
	}
	return nativeAddress(address, domain)
}

// nativeAddress canonicalizes a bare ASCII dot-atom address on domain.
func nativeAddress(address, domain string) (string, error) {
	a, e := mail.ParseAddress(address)
	local, host, ok := strings.Cut(address, "@")
	if e != nil || a.Address != address || !ok || len(address) > 254 || len(local) == 0 || len(local) > 64 || strings.ToLower(host) != domain || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", ErrNativeProvisioning
	}
	for _, r := range local {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-/=?^_`{|}~.", r)
		if !valid {
			return "", fmt.Errorf("%w: unsupported address", ErrNativeProvisioning)
		}
	}
	return strings.ToLower(address), nil
}
