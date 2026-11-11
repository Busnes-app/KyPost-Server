package sso

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
)

// authorityStore holds native authority: the directory lifecycle and the
// native ledger. Every call reads current committed state; nothing decoded
// survives a call. Write callers hold the directory fence.
type authorityStore interface {
	// View runs fn on one consistent read snapshot.
	View(ctx context.Context, fn func(authorityReader) error) error
	// Update runs fn as one write; see jsonAuthority for its file order.
	Update(ctx context.Context, fn func(authorityWriter) error) error
}

type authorityReader interface {
	Initialized() (bool, error)
	Directory(issuer, subject string) (DirectoryState, bool, error)
	// Account is the subject's primary mailbox: account, mailbox and primary address.
	Account(issuer, subject string) (NativeAssignment, bool, error)
	// Mailbox resolves a primary or extra mailbox; see nativeAssignments.mailbox.
	Mailbox(id string) (NativeAssignment, nativeLedgerMailbox, bool, error)
	Address(address string) (nativeLedgerAddress, bool, error) // with history
	// Lifecycle is the whole lifecycle record: logouts, sign-ons, directory,
	// events, floors, recovery and release. Each call returns a copy (see
	// lifecycleFile.clone), so a caller's edits reach later reads only by
	// PutLifecycle.
	Lifecycle() (lifecycleFile, error)
	Snapshot() (nativeAssignments, map[string]DirectoryState, error) // cold paths only
}

type authorityWriter interface {
	authorityReader
	// SetInitialized starts every ledger write: the flag precedes the ledger.
	// It refuses after PutLifecycle, which would be written after it.
	SetInitialized() error
	// PutAccount takes the stored ledger key, directoryKey(issuer, subject)
	// for a v2 ledger; a migrating v1 ledger keeps its legacy keys byte for byte.
	PutAccount(key string, a nativeLedgerAccount) error
	PutMailbox(id string, m nativeLedgerMailbox) error
	PutAddress(address string, x nativeLedgerAddress) error
	PutLifecycle(l lifecycleFile) error
	// MarkRoutesPending writes these addresses' routes inactive after the ledger.
	// It needs the ledger: one put in this Update, or the stored one already read.
	MarkRoutesPending(inactive map[string]int64) error
}

func (s *LifecycleStore) authority() authorityStore { return jsonAuthority{s.path} }

// update is one authority write under the directory lock.
func (s *LifecycleStore) update(fn func(authorityWriter) error) error {
	return fsutil.WithFileLock(s.path, func() error {
		return s.authority().Update(context.Background(), fn)
	})
}

// jsonAuthority serves authority from sso-lifecycle.json and, beside it,
// native-provisioning.json. A transaction reads each file at most once, on
// first use, so a lifecycle-only call never fails on the ledger. Readers on
// the admission path decode through the bounded content views.
//
// Update writes in persistNative's order, under the caller's flock: the
// initialization flag (at once, so a later failure leaves the fence set),
// then the ledger, then inactive routes, then the lifecycle. A route failure
// skips the lifecycle write, as applyDirectory did.
// ponytail: the ledger written is exactly the records put in this Update, as
// persistNative wrote its whole rendered ledger, so writers put every record
// (writeNative). Reads inside an Update see the committed files, except that
// after PutLifecycle the lifecycle reads (Lifecycle, Directory, Initialized,
// Snapshot's directory) see the put record. Ledger puts are never read back.
//
// Reads without the flock may interleave with a writer. loadNative reads the
// lifecycle bytes before the ledger, so the decoded ledger is never older than
// the lifecycle it is checked against, as the old second ledger read was.
// Account (admission) reads the ledger first and the flag after it; under the
// flag-first write order a reader that sees a ledger sees the flag.
type jsonAuthority struct{ path string }

type jsonAuthorityTx struct {
	path string

	lifeRead, lifeAbsent bool
	lifeRaw              []byte
	lifeReadErr          error
	initRead, init       bool

	lifeLoaded bool
	life       lifecycleFile
	lifeErr    error

	ledgerRead, ledgerExists bool
	ledgerRaw                []byte
	ledgerErr                error

	nativeLoaded bool
	native       nativeAssignments
	nativeErr    error

	ledger    *nativeLedger
	routes    bool
	inactive  map[string]int64
	lifeDirty bool
}

func (a jsonAuthority) View(_ context.Context, fn func(authorityReader) error) error {
	return fn(&jsonAuthorityTx{path: a.path})
}

func (a jsonAuthority) Update(_ context.Context, fn func(authorityWriter) error) error {
	t := &jsonAuthorityTx{path: a.path}
	if err := fn(t); err != nil {
		return err
	}
	if t.ledger != nil {
		if err := fsutil.PersistJSONFile(filepath.Join(filepath.Dir(a.path), nativeProvisioningFile), *t.ledger); err != nil {
			return err
		}
	}
	if t.routes {
		l := t.ledger
		if l == nil {
			l = &t.native.stored
		}
		if err := deactivateRoutes(*l, t.inactive); err != nil {
			return err
		}
	}
	if t.lifeDirty {
		return fsutil.PersistJSONFile(a.path, t.life)
	}
	return nil
}

func (t *jsonAuthorityTx) readLifecycle() error {
	if !t.lifeRead {
		t.lifeRead = true
		t.lifeRaw, t.lifeReadErr = os.ReadFile(t.path)
		if errors.Is(t.lifeReadErr, os.ErrNotExist) {
			t.lifeAbsent, t.lifeReadErr = true, nil
		}
	}
	return t.lifeReadErr
}

// Lifecycle decodes the whole file as LifecycleStore.load always did, once,
// and hands out copies.
func (t *jsonAuthorityTx) Lifecycle() (lifecycleFile, error) {
	if !t.lifeLoaded {
		t.lifeLoaded = true
		f := lifecycleFile{}
		err := t.readLifecycle()
		if err == nil && !t.lifeAbsent {
			err = decodeLifecycle(t.lifeRaw, &f)
		}
		f.ensureMaps()
		t.life, t.lifeErr = f, err
	}
	return t.life.clone(), t.lifeErr
}

// directoryView answers from the bounded view unless the whole file is decoded.
func (t *jsonAuthorityTx) directoryView(key string) (d DirectoryState, known, initialized bool, err error) {
	if t.lifeLoaded {
		d, known = t.life.Directory[key]
		return d, known, t.life.NativeProvisioningInitialized, t.lifeErr
	}
	if err = t.readLifecycle(); err != nil || t.lifeAbsent {
		return d, false, false, err
	}
	d, known, initialized, err = nativeDirectoryFromLifecycle(t.lifeRaw, key)
	if err == nil {
		t.initRead, t.init = true, initialized
	}
	return d, known, initialized, err
}

func (t *jsonAuthorityTx) Initialized() (bool, error) {
	if t.initRead && !t.lifeLoaded {
		return t.init, nil
	}
	_, _, initialized, err := t.directoryView("")
	return initialized, err
}

func (t *jsonAuthorityTx) Directory(issuer, subject string) (DirectoryState, bool, error) {
	d, known, _, err := t.directoryView(directoryKey(issuer, subject))
	return d, known, err
}

// readLedger is the format check and the ledger bytes it read.
func (t *jsonAuthorityTx) readLedger() error {
	if !t.ledgerRead {
		t.ledgerRead = true
		t.ledgerRaw, t.ledgerExists, t.ledgerErr = currentNativeFormat(filepath.Dir(t.path))
	}
	return t.ledgerErr
}

// loadNative is the old LifecycleStore.loadNative. It reads the lifecycle
// bytes, then the ledger once, and reports errors in the old order: format,
// lifecycle, ledger.
func (t *jsonAuthorityTx) loadNative() (nativeAssignments, error) {
	if !t.nativeLoaded {
		t.nativeLoaded = true
		_ = t.readLifecycle() // its error is reported by Lifecycle below
		if t.nativeErr = t.readLedger(); t.nativeErr == nil {
			var life lifecycleFile
			if life, t.nativeErr = t.Lifecycle(); t.nativeErr == nil {
				readErr := error(nil)
				if !t.ledgerExists {
					readErr = os.ErrNotExist
				}
				t.native, _, t.nativeErr = decodeNativeLedger(t.ledgerRaw, readErr, life, false)
			}
		}
	}
	return t.native, t.nativeErr
}

func (t *jsonAuthorityTx) Account(issuer, subject string) (NativeAssignment, bool, error) {
	key := directoryKey(issuer, subject)
	if t.nativeLoaded {
		a, ok := t.native.Accounts[key]
		return a, ok, t.nativeErr
	}
	if err := t.readLedger(); err != nil {
		return NativeAssignment{}, false, err
	}
	initialized, err := t.Initialized()
	if err != nil {
		return NativeAssignment{}, false, err
	}
	return nativeAssignmentFromLedger(t.ledgerRaw, t.ledgerExists, key, initialized)
}

func (t *jsonAuthorityTx) Mailbox(id string) (NativeAssignment, nativeLedgerMailbox, bool, error) {
	f, err := t.loadNative()
	if err != nil {
		return NativeAssignment{}, nativeLedgerMailbox{}, false, err
	}
	a, m, ok := f.mailbox(id)
	return a, m, ok, nil
}

func (t *jsonAuthorityTx) Address(address string) (nativeLedgerAddress, bool, error) {
	f, err := t.loadNative()
	x, ok := f.stored.Addresses[address]
	return x, ok, err
}

// Snapshot copies the maps, so a caller's edits reach the files only by puts.
func (t *jsonAuthorityTx) Snapshot() (nativeAssignments, map[string]DirectoryState, error) {
	f, err := t.loadNative()
	f.Accounts = maps.Clone(f.Accounts)
	f.stored.Accounts = maps.Clone(f.stored.Accounts)
	f.stored.Mailboxes = maps.Clone(f.stored.Mailboxes)
	f.stored.Addresses = maps.Clone(f.stored.Addresses)
	return f, maps.Clone(t.life.Directory), err
}

// errAuthorityOrder is a writer calling the transaction out of its order.
var errAuthorityOrder = errors.New("sso: authority transaction used out of order")

func (t *jsonAuthorityTx) SetInitialized() error {
	if t.lifeDirty {
		return errAuthorityOrder // the flag would be written before the put lifecycle
	}
	life, err := t.Lifecycle()
	if err != nil {
		return err
	}
	if t.ledger == nil {
		t.ledger = &nativeLedger{Version: 2, AddressGenerations: true, Accounts: map[string]nativeLedgerAccount{}, Mailboxes: map[string]nativeLedgerMailbox{}, Addresses: map[string]nativeLedgerAddress{}}
	}
	if life.NativeProvisioningInitialized {
		return nil
	}
	t.life.NativeProvisioningInitialized = true
	// Write the file as read plus the flag, never edits not yet put.
	fresh := lifecycleFile{}
	if !t.lifeAbsent {
		if err = decodeLifecycle(t.lifeRaw, &fresh); err != nil {
			return err
		}
	}
	fresh.ensureMaps()
	fresh.NativeProvisioningInitialized = true
	return fsutil.PersistJSONFile(t.path, fresh)
}

func (t *jsonAuthorityTx) pending() (*nativeLedger, error) {
	if t.ledger == nil {
		return nil, ErrNativeProvisioning // a ledger write starts with SetInitialized
	}
	return t.ledger, nil
}

func (t *jsonAuthorityTx) PutAccount(key string, a nativeLedgerAccount) error {
	l, err := t.pending()
	if err == nil {
		l.Accounts[key] = a
	}
	return err
}

func (t *jsonAuthorityTx) PutMailbox(id string, m nativeLedgerMailbox) error {
	l, err := t.pending()
	if err == nil {
		l.Mailboxes[id] = m
	}
	return err
}

func (t *jsonAuthorityTx) PutAddress(address string, x nativeLedgerAddress) error {
	l, err := t.pending()
	if err == nil {
		l.Addresses[address] = x
	}
	return err
}

// PutLifecycle replaces the whole record, flag included, as the old writers did.
func (t *jsonAuthorityTx) PutLifecycle(l lifecycleFile) error {
	t.lifeRead, t.lifeLoaded, t.lifeErr = true, true, nil
	t.life, t.lifeDirty = l.clone(), true
	return nil
}

func (t *jsonAuthorityTx) MarkRoutesPending(inactive map[string]int64) error {
	if t.ledger == nil && (!t.nativeLoaded || t.nativeErr != nil) {
		return errAuthorityOrder // no ledger to find the routes' state root in
	}
	t.routes, t.inactive = true, inactive
	return nil
}
