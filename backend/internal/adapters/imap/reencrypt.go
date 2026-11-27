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
			if uidNext, parseErr := strconv.Atoi(match[1]); parseErr == nil && uidNext > 0 {
				bound := uidNext
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

// ErrCondstoreUnsupported is returned when the IMAP server cannot perform
// race-safe conditional deletion (CONDSTORE / usable MODSEQ).
var ErrCondstoreUnsupported = errors.New("IMAP server lacks CONDSTORE/IMAP4rev2 conditional deletion; refusing archive replacement")

// Matches FLAGS (...) and MODSEQ (n) in a single UID FETCH response.
var (
	flagsResponse  = regexp.MustCompile(`(?i)\bFLAGS\s+\(([^)]*)\)`)
	modSeqResponse = regexp.MustCompile(`(?i)\bMODSEQ\s+\((\d+)\)`)
)

func supportsCondstore(d *goimap.Dialer) bool {
	resp, err := d.Exec("CAPABILITY", true, 0, nil)
	if err != nil {
		return false
	}
	for _, tok := range strings.Fields(strings.ToUpper(resp)) {
		if tok == "CONDSTORE" || tok == "IMAP4REV2" {
			return true
		}
	}
	return false
}

// fetchFlagsAndModSeq captures FLAGS and MODSEQ atomically in a single
// UID FETCH (FLAGS MODSEQ) snapshot so no flag mutation can slip between
// reading flags and obtaining the MODSEQ precondition.
func fetchFlagsAndModSeq(d *goimap.Dialer, uid int) ([]string, uint64, error) {
	resp, err := d.Exec(fmt.Sprintf("UID FETCH %d (FLAGS MODSEQ)", uid), true, 0, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch original flags and MODSEQ: %w", err)
	}
	modMatch := modSeqResponse.FindStringSubmatch(resp)
	if len(modMatch) != 2 {
		return nil, 0, ErrCondstoreUnsupported
	}
	modSeq, err := strconv.ParseUint(modMatch[1], 10, 64)
	if err != nil || modSeq == 0 {
		return nil, 0, ErrCondstoreUnsupported
	}
	flagMatch := flagsResponse.FindStringSubmatch(resp)
	if len(flagMatch) != 2 {
		return nil, 0, errors.New("cannot parse original message flags")
	}
	flags, err := incomingFlags(strings.Fields(flagMatch[1]))
	if err != nil {
		return nil, 0, err
	}
	return flags, modSeq, nil
}

func cleanupCopyUID(d *goimap.Dialer, copyUID int) {
	if copyUID <= 0 {
		return
	}
	_, _ = d.Exec(fmt.Sprintf(`UID STORE %d +FLAGS.SILENT (\Deleted)`, copyUID), false, 0, nil)
	_, _ = d.Exec(fmt.Sprintf("UID EXPUNGE %d", copyUID), false, 0, nil)
}

const uidScanCacheTTL = 60 * time.Second

type folderUIDScanCache struct {
	folder    string
	afterUID  int
	uids      []int
	fetchedAt time.Time
}

// ListFolderMessageUIDs returns up to limit message UIDs strictly greater than
// afterUID in ascending order for folder. Within a folder scan it reuses a
// short-lived sorted UID snapshot so paginating a large mailbox does not
// re-download the remaining UID suffix on every batch.
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

	var sortedUIDs []int
	if afterUID > 0 &&
		c.uidScanCache != nil &&
		c.uidScanCache.folder == folder &&
		afterUID >= c.uidScanCache.afterUID &&
		time.Since(c.uidScanCache.fetchedAt) < uidScanCacheTTL {
		sortedUIDs = c.uidScanCache.uids
	} else {
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
		c.uidScanCache = &folderUIDScanCache{
			folder:    folder,
			afterUID:  afterUID,
			uids:      filtered,
			fetchedAt: time.Now(),
		}
		sortedUIDs = filtered
	}

	idx := sort.SearchInts(sortedUIDs, afterUID+1)
	if idx >= len(sortedUIDs) {
		c.uidScanCache = nil
		return nil, nil
	}
	end := idx + limit
	if end > len(sortedUIDs) {
		end = len(sortedUIDs)
	}
	out := make([]int, end-idx)
	copy(out, sortedUIDs[idx:end])
	if end >= len(sortedUIDs) {
		c.uidScanCache = nil
	}
	return out, nil
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
	if !supportsCondstore(d) {
		return 0, ErrCondstoreUnsupported
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

	// Capture FLAGS and MODSEQ atomically in one UID FETCH snapshot so no
	// concurrent flag change can slip between reading flags and MODSEQ.
	flags, origModSeq, err := fetchFlagsAndModSeq(d, uid)
	if err != nil {
		return 0, err
	}
	ov, err := incomingMetadata(d, uid)
	if err != nil || ov == nil {
		return 0, errors.New("cannot read original message date")
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
			cleanupCopyUID(d, copyUID)
			return 0, fmt.Errorf("preserve flags on replacement copy: %w", err)
		}
	}

	// Require RFC 7162 conditional deletion against the atomically captured
	// MODSEQ so concurrent flag updates on the original are never silently lost.
	// Use non-silent +FLAGS with buildResponse=true because go-imap omits the
	// tagged OK [MODIFIED] line; the server emits an untagged FETCH carrying
	// \Deleted only when UNCHANGEDSINCE succeeded.
	storeResp, err := d.Exec(fmt.Sprintf(`UID STORE %d (UNCHANGEDSINCE %d) +FLAGS (\Deleted)`, uid, origModSeq), true, 0, nil)
	if err != nil {
		cleanupCopyUID(d, copyUID)
		return 0, fmt.Errorf("mark original message deleted: %w", err)
	}
	if !strings.Contains(strings.ToLower(storeResp), `\deleted`) {
		cleanupCopyUID(d, copyUID)
		return 0, ErrCASMismatch
	}
	if _, err := d.Exec(fmt.Sprintf("UID EXPUNGE %d", uid), false, 0, nil); err != nil {
		return 0, fmt.Errorf("expunge original message: %w", err)
	}
	return copyUID, nil
}
