package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	goimap "github.com/BrianLeishman/go-imap"
)

// Matches UIDNEXT inside the STATUS attribute list, which ends the line, so a
// mailbox name containing "UIDNEXT n" cannot supply the value.
var uidNextResponse = regexp.MustCompile(`(?im)\([^()]*\bUIDNEXT\s+(\d+)[^()]*\)\s*$`)

// folderMinAppendedUID returns a lower bound for the UID assigned to a message
// appended next to folder using O(1) IMAP commands (STATUS UIDNEXT, falling
// back to UID SEARCH RETURN (MAX) 1:*).
func folderMinAppendedUID(d *goimap.Dialer, folder string, origUID int) (int, error) {
	if resp, err := d.Exec(`STATUS "`+goimap.AddSlashes.Replace(folder)+`" (UIDNEXT)`, true, 0, nil); err == nil {
		if match := uidNextResponse.FindStringSubmatch(resp); len(match) == 2 {
			if uidNext, parseErr := strconv.ParseUint(match[1], 10, 32); parseErr == nil && uidNext > 0 {
				bound := int(uidNext)
				if bound <= origUID {
					bound = origUID + 1
				}
				return bound, nil
			}
		}
	}
	maxUID, err := d.GetMaxUID()
	if err != nil {
		return 0, err
	}
	if maxUID < origUID {
		maxUID = origUID
	}
	return maxUID + 1, nil
}

var (
	// ErrCASMismatch indicates the stored raw message bytes changed before
	// compare-and-swap replacement could commit.
	ErrCASMismatch = errors.New("stored message hash mismatch")
	// ErrMessageNotFound indicates the target message UID is absent in the folder.
	ErrMessageNotFound = errors.New("message not found")
)

// ArchiveReencryptClient is implemented by mail backends that support Spec A
// Phase P6 archive re-encryption: scanning folders, reading exact stored raw
// bytes, and replacing a message atomically with compare-and-swap.
type ArchiveReencryptClient interface {
	ListAllFolders(ctx context.Context) ([]string, error)
	ListFolderMessageUIDs(ctx context.Context, folder string, afterUID int, limit int) ([]int, error)
	GetRawMessage(ctx context.Context, folder string, uid int) ([]byte, error)
	ReplaceRawMessageCAS(ctx context.Context, folder string, uid int, expectedSHA256 string, newRaw []byte) (newUID int, err error)
}

var _ ArchiveReencryptClient = (*APIClient)(nil)

// ListAllFolders returns all selectable mailbox folder names in deterministic order.
func (c *APIClient) ListAllFolders(ctx context.Context) ([]string, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	folders, err := d.GetFolders()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(folders))
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		name := strings.TrimSpace(f)
		if name == "" || seen[name] {
			continue
		}
		if ValidateMailboxName(name) != nil {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// ListFolderMessageUIDs returns up to limit message UIDs strictly greater than
// afterUID in ascending order for folder.
func (c *APIClient) ListFolderMessageUIDs(ctx context.Context, folder string, afterUID int, limit int) ([]int, error) {
	if err := ValidateMailboxName(folder); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	if err := d.SelectFolder(folder); err != nil {
		return nil, err
	}
	startUID := afterUID + 1
	if startUID < 1 {
		startUID = 1
	}
	uids, err := incomingSearch(d, fmt.Sprintf("UID %d:*", startUID))
	if err != nil {
		return nil, err
	}
	filtered := make([]int, 0, len(uids))
	for _, uid := range uids {
		if uid > afterUID {
			filtered = append(filtered, uid)
		}
	}
	sort.Ints(filtered)
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

// GetRawMessage fetches the exact stored RFC 5322 bytes for (folder, uid).
func (c *APIClient) GetRawMessage(ctx context.Context, folder string, uid int) ([]byte, error) {
	if err := ValidateMailboxName(folder); err != nil {
		return nil, err
	}
	if uid <= 0 {
		return nil, ErrMessageNotFound
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return nil, err
	}
	if err := d.SelectFolder(folder); err != nil {
		return nil, err
	}
	found, err := incomingSearch(d, "UID "+strconv.Itoa(uid))
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrMessageNotFound
	}
	return incomingRaw(d, uid)
}

// ReplaceRawMessageCAS verifies that (folder, uid) still has SHA-256 equal to
// expectedSHA256, appends newRaw preserving flags and INTERNALDATE, verifies
// the appended copy byte-for-byte, and removes only the original UID with
// UIDPLUS UID EXPUNGE.
func (c *APIClient) ReplaceRawMessageCAS(ctx context.Context, folder string, uid int, expectedSHA256 string, newRaw []byte) (int, error) {
	if err := ValidateMailboxName(folder); err != nil {
		return 0, err
	}
	if uid <= 0 {
		return 0, ErrMessageNotFound
	}
	if len(newRaw) == 0 {
		return 0, errors.New("empty replacement message")
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return 0, err
	}
	if err := d.SelectFolder(folder); err != nil {
		return 0, err
	}
	supported, err := supportsUIDExpunge(d)
	if err != nil {
		return 0, fmt.Errorf("check UIDPLUS capability: %w", err)
	}
	if !supported {
		return 0, errors.New("IMAP server lacks UIDPLUS/IMAP4rev2 UID EXPUNGE; refusing archive replacement")
	}

	found, err := incomingSearch(d, "UID "+strconv.Itoa(uid))
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, ErrMessageNotFound
	}

	oldRaw, err := incomingRaw(d, uid)
	if err != nil {
		return 0, err
	}
	if !strings.EqualFold(incomingHash(oldRaw), strings.TrimSpace(expectedSHA256)) {
		return 0, ErrCASMismatch
	}

	ov, err := incomingMetadata(d, uid)
	if err != nil || ov == nil {
		return 0, errors.New("cannot read original message flags and date")
	}
	flags, err := incomingFlags(ov.Flags)
	if err != nil {
		return 0, err
	}
	date := ov.Received
	if date.IsZero() {
		date = time.Now().UTC()
	}

	minNewUID, err := folderMinAppendedUID(d, folder, uid)
	if err != nil {
		return 0, fmt.Errorf("read folder UIDNEXT before append: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := d.Append(folder, flags, date, newRaw); err != nil {
		return 0, fmt.Errorf("append replacement message: %w", err)
	}

	// Locate the newly appended message strictly by byte equality starting from
	// the pre-append UIDNEXT bound (O(1) instead of scanning the entire folder).
	candUIDs, err := incomingSearch(d, fmt.Sprintf("UID %d:*", minNewUID))
	if err != nil {
		return 0, fmt.Errorf("locate appended replacement message: %w", err)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(candUIDs)))
	copyUID := 0
	for _, cand := range candUIDs {
		if cand < minNewUID || cand == uid {
			continue
		}
		candRaw, err := incomingRaw(d, cand)
		if err != nil {
			continue
		}
		if bytes.Equal(candRaw, newRaw) {
			copyUID = cand
			break
		}
	}
	if copyUID == 0 {
		return 0, errors.New("appended replacement message could not be verified; original preserved")
	}

	if len(flags) > 0 {
		if _, err := d.Exec(fmt.Sprintf("UID STORE %d FLAGS.SILENT (%s)", copyUID, strings.Join(flags, " ")), false, 0, nil); err != nil {
			return 0, fmt.Errorf("preserve flags on replacement copy: %w", err)
		}
	}
	if _, err := d.Exec(fmt.Sprintf(`UID STORE %d +FLAGS.SILENT (\Deleted)`, uid), false, 0, nil); err != nil {
		return 0, fmt.Errorf("mark original message deleted: %w", err)
	}
	if _, err := d.Exec(fmt.Sprintf("UID EXPUNGE %d", uid), false, 0, nil); err != nil {
		return 0, fmt.Errorf("expunge original message: %w", err)
	}
	return copyUID, nil
}
