package sso

import (
	"bytes"
	"encoding/json"
	"slices"
	"sync"
)

// These are reuse limits, not file-format limits. Larger inputs still use the
// complete decoder on every read. Each process retains one view of each kind.
const nativeViewBytes = 8 << 20
const nativeViewAccounts = 4096

// nativeContentView keeps decoded contents, never a permission decision. The
// callback may only copy its selected value: maps stay private to this cache.
// ponytail: one entry per kind; other configurations replace it, without a registry.
type nativeContentView[T any] struct {
	mu    sync.Mutex
	raw   []byte
	value T
}

func (c *nativeContentView[T]) with(raw []byte, decode func([]byte) (T, error), eligible func(T) bool, copyValue func(T)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raw != nil && bytes.Equal(raw, c.raw) {
		copyValue(c.value)
		return nil
	}
	v, err := decode(raw)
	if err != nil {
		return err
	}
	if len(raw) <= nativeViewBytes && eligible(v) {
		c.raw, c.value = bytes.Clone(raw), v
	}
	copyValue(v)
	return nil
}

type nativeDirectoryContents struct {
	directory   map[string]DirectoryState
	initialized bool
}

var nativeDirectoryView nativeContentView[nativeDirectoryContents]
var nativeAssignmentView nativeContentView[map[string]NativeAssignment]

// nativeAssignmentsAlreadyDecoded proves only that these exact current bytes
// passed the complete runtime v2 decoder. It grants no access or migration state.
func nativeAssignmentsAlreadyDecoded(raw []byte) bool {
	nativeAssignmentView.mu.Lock()
	defer nativeAssignmentView.mu.Unlock()
	return nativeAssignmentView.raw != nil && bytes.Equal(raw, nativeAssignmentView.raw)
}

func decodeNativeDirectory(raw []byte) (nativeDirectoryContents, error) {
	var f lifecycleFile
	// Decode the complete original type, including fields admission does not
	// retain, so malformed unrelated fields still refuse exactly as load does.
	if err := json.Unmarshal(raw, &f); err != nil {
		return nativeDirectoryContents{}, err
	}
	return nativeDirectoryContents{f.Directory, f.NativeProvisioningInitialized}, nil
}

func retainNativeDirectory(v nativeDirectoryContents) bool {
	if len(v.directory) > nativeViewAccounts {
		return false
	}
	for _, d := range v.directory {
		if r := d.Resource; r != nil && (len(r.Schemas) > 8 || len(r.Emails) > 32 || len(r.Roles) > 4096) {
			return false
		}
	}
	return true
}

// nativeDirectoryFromLifecycle decodes lifecycle bytes the caller read for
// this operation; the caller maps a missing file to the empty, uninitialized
// result, as LifecycleStore.load does.
func nativeDirectoryFromLifecycle(raw []byte, key string) (d DirectoryState, known, initialized bool, err error) {
	err = nativeDirectoryView.with(raw, decodeNativeDirectory, retainNativeDirectory, func(v nativeDirectoryContents) {
		d, known = v.directory[key]
		initialized = v.initialized
		if d.Resource != nil {
			r := *d.Resource
			r.Schemas = slices.Clone(r.Schemas)
			r.Emails = slices.Clone(r.Emails)
			r.Roles = bytes.Clone(r.Roles)
			if r.Active != nil {
				active := *r.Active
				r.Active = &active
			}
			d.Resource = &r
		}
	})
	return
}

func decodeNativeAssignments(raw []byte) (map[string]NativeAssignment, error) {
	f, _, err := parseNativeLedger(raw, false)
	if err != nil {
		// Match readNativeLedger's syntax-error identity before native refusal.
		var syntax json.RawMessage
		if syntaxErr := json.Unmarshal(raw, &syntax); syntaxErr != nil {
			return nil, syntaxErr
		}
		return nil, ErrNativeProvisioning
	}
	// Only primary assignment values escape validation. The mutable ledger,
	// history and generation maps are neither retained nor shared with writers.
	return f.Accounts, nil
}

func retainNativeAssignments(v map[string]NativeAssignment) bool {
	return len(v) <= nativeViewAccounts
}

// nativeAssignmentFromLedger decodes ledger bytes the caller read for this
// operation. A missing ledger is valid only before initialization.
func nativeAssignmentFromLedger(raw []byte, exists bool, key string, initialized bool) (a NativeAssignment, known bool, err error) {
	if !exists {
		if initialized {
			err = ErrNativeProvisioning
		}
		return
	}
	err = nativeAssignmentView.with(raw, decodeNativeAssignments, retainNativeAssignments, func(v map[string]NativeAssignment) {
		a, known = v[key] // NativeAssignment contains values and immutable strings only.
	})
	if err == nil && !initialized {
		err = ErrNativeProvisioning
	}
	return
}
