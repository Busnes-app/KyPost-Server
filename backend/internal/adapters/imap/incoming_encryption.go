package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	goimap "github.com/BrianLeishman/go-imap"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
)

// IncomingSource binds a replacement to one immutable message and mailbox UID
// namespace. Raw is used only in memory; journals retain Hash and ciphertext.
type IncomingSource struct {
	UID         int
	UIDValidity uint64
	AccountHash string
	Hash        string
	Raw         []byte `json:"-"`
	Flags       []string
	Date        time.Time
}

var uidValidityResponse = regexp.MustCompile(`(?i)UIDVALIDITY\s+(\d+)`)

func incomingMailbox(d *goimap.Dialer) (uint64, error) {
	// No reconnect/retry: a changed UID namespace must never reuse a saved UID.
	if _, err := d.Exec(`SELECT "INBOX"`, false, 0, nil); err != nil {
		return 0, errors.New("cannot select inbox for encryption; check IMAP connection")
	}
	d.Folder, d.ReadOnly = "INBOX", false
	capability, err := d.Exec("CAPABILITY", true, 0, nil)
	if err != nil {
		return 0, errors.New("cannot check IMAP capabilities; retry after connection recovers")
	}
	supports := false
	for _, atom := range strings.Fields(strings.ToUpper(capability)) {
		if atom == "UIDPLUS" || atom == "IMAP4REV2" {
			supports = true
		}
	}
	if !supports {
		return 0, errors.New("incoming encryption requires IMAP UIDPLUS or IMAP4rev2 for targeted deletion")
	}
	response, err := d.Exec(`STATUS "INBOX" (UIDVALIDITY)`, true, 0, nil)
	if err != nil {
		return 0, errors.New("cannot read inbox UIDVALIDITY; retry after connection recovers")
	}
	match := uidValidityResponse.FindStringSubmatch(response)
	if len(match) != 2 {
		return 0, errors.New("IMAP omitted UIDVALIDITY; original mail has been preserved")
	}
	validity, err := strconv.ParseUint(match[1], 10, 32)
	if err != nil || validity == 0 {
		return 0, errors.New("invalid inbox UIDVALIDITY; original mail has been preserved")
	}
	return validity, nil
}

func incomingHash(raw []byte) string { hash := sha256.Sum256(raw); return hex.EncodeToString(hash[:]) }

// PrepareIncoming captures the exact original, flags and INTERNALDATE before
// any rule can mark it read or move it out of the inbox.
func (c *APIClient) PrepareIncoming(ctx context.Context, uid int, requireMove bool) (IncomingSource, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return IncomingSource{}, err
	}
	if uid <= 0 {
		return IncomingSource{}, errors.New("invalid incoming UID")
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return IncomingSource{}, errors.New("cannot connect to IMAP for encryption")
	}
	if !strings.EqualFold(strings.TrimSpace(c.mailbox), "INBOX") {
		return IncomingSource{}, errors.New("incoming encryption requires the configured polling mailbox to be INBOX; original mail preserved")
	}
	validity, err := incomingMailbox(d)
	if err != nil {
		return IncomingSource{}, err
	}
	if requireMove {
		caps, err := d.Exec("CAPABILITY", true, 0, nil)
		supportsMove := false
		for _, atom := range strings.Fields(strings.ToUpper(caps)) {
			if atom == "MOVE" || atom == "IMAP4REV2" {
				supportsMove = true
			}
		}
		if err != nil || !supportsMove {
			return IncomingSource{}, errors.New("incoming encryption with moving rules requires IMAP MOVE; original mail preserved")
		}
	}
	raw, err := incomingRaw(d, uid)
	if err != nil {
		return IncomingSource{}, errors.New("cannot fetch original mail for encryption; original preserved")
	}
	ov, err := incomingMetadata(d, uid)
	if err != nil || ov == nil {
		return IncomingSource{}, errors.New("cannot read original flags and arrival date; original preserved")
	}
	if ov.Received.IsZero() {
		return IncomingSource{}, errors.New("IMAP omitted original arrival date; original preserved")
	}
	flags, err := incomingFlags(ov.Flags)
	if err != nil {
		return IncomingSource{}, err
	}
	return IncomingSource{AccountHash: incomingAccountHash(d), UID: uid, UIDValidity: validity, Hash: incomingHash(raw), Raw: raw, Flags: flags, Date: ov.Received}, nil
}

func incomingFlags(flags []string) ([]string, error) {
	out := make([]string, 0, len(flags))
	if len(flags) > 100 {
		return nil, errors.New("too many IMAP flags; original preserved")
	}
	for _, flag := range flags {
		switch strings.ToLower(flag) {
		case `\recent`, `\deleted`:
			continue
		case `\seen`, `\answered`, `\flagged`, `\draft`:
			out = append(out, flag)
		default:
			if err := ValidateKeyword(flag); err != nil {
				return nil, errors.New("unsafe IMAP flag; original preserved")
			}
			out = append(out, flag)
		}
	}
	return out, nil
}

// ReplaceIncoming is recoverable after an uncertain APPEND result. The durable
// job's random marker locates candidates, and byte equality authenticates the
// candidate before deletion. Header search alone never authorizes deletion.
func (c *APIClient) ReplaceIncoming(ctx context.Context, source IncomingSource, marker string, encrypted []byte) (int, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	token, err := hex.DecodeString(marker)
	if err != nil || len(token) != 32 || source.UID <= 0 || len(encrypted) == 0 || int64(len(encrypted)) > mailmsg.MaxInboundMessageBytes {
		return 0, errors.New("invalid incoming replacement")
	}
	if !bytes.HasPrefix(encrypted, []byte("X-KyPost-Incoming: "+marker+"\r\n")) {
		return 0, errors.New("incoming replacement marker mismatch")
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return 0, errors.New("cannot connect to IMAP; original preserved")
	}
	validity, err := incomingMailbox(d)
	if err != nil {
		return 0, err
	}
	if validity != source.UIDValidity || incomingAccountHash(d) != source.AccountHash {
		return 0, errors.New("inbox UIDVALIDITY changed; encryption paused to avoid deleting unrelated mail")
	}
	findCopy := func() (int, error) {
		uids, err := incomingSearch(d, `HEADER X-KyPost-Incoming "`+marker+`"`)
		if err != nil {
			return 0, errors.New("cannot locate encrypted copy; original preserved")
		}
		if len(uids) > 20 {
			return 0, errors.New("too many replacement candidates; original preserved")
		}
		var found int
		for _, uid := range uids {
			if uid == source.UID {
				continue
			}
			raw, err := incomingRaw(d, uid)
			if err != nil {
				return 0, errors.New("cannot verify encrypted copy; original preserved")
			}
			if bytes.Equal(raw, encrypted) {
				if found != 0 {
					return 0, errors.New("multiple verified encrypted copies; original preserved for reconciliation")
				}
				found = uid
			}
		}
		return found, nil
	}
	copyUID, err := findCopy()
	if err != nil {
		return 0, err
	}
	originals, err := incomingSearch(d, "UID "+strconv.Itoa(source.UID))
	if err != nil {
		return 0, errors.New("cannot check original UID; original preserved")
	}
	if len(originals) == 0 {
		if copyUID > 0 {
			return copyUID, nil
		}
		return 0, errors.New("original and encrypted copy are absent; encryption paused for reconciliation")
	}
	raw, err := incomingRaw(d, source.UID)
	if err != nil || incomingHash(raw) != source.Hash {
		return 0, errors.New("original message changed; encryption paused without deleting it")
	}
	ov, err := incomingMetadata(d, source.UID)
	if err != nil || ov == nil {
		return 0, errors.New("cannot refresh original flags; original preserved")
	}
	flags, err := incomingFlags(ov.Flags)
	if err != nil {
		return 0, err
	}
	if copyUID == 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := d.Append("INBOX", flags, source.Date, encrypted); err != nil {
			return 0, errors.New("encrypted upload was not confirmed; retry will check for a stored copy before uploading again")
		}
		copyUID, err = findCopy()
		if err != nil {
			return 0, err
		}
		if copyUID == 0 {
			return 0, errors.New("encrypted upload could not be verified; original preserved")
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Copy current flags even after recovery of an earlier APPEND. A mailbox-wide
	// EXPUNGE would delete mail marked by another client; never use it here.
	if _, err := d.Exec(fmt.Sprintf("UID STORE %d FLAGS.SILENT (%s)", copyUID, strings.Join(flags, " ")), false, 0, nil); err != nil {
		return 0, errors.New("cannot preserve flags on encrypted copy; original preserved")
	}
	if _, err := d.Exec(fmt.Sprintf(`UID STORE %d +FLAGS.SILENT (\Deleted)`, source.UID), false, 0, nil); err != nil {
		return 0, errors.New("cannot mark original for targeted removal; verified encrypted copy retained")
	}
	if _, err := d.Exec(fmt.Sprintf("UID EXPUNGE %d", source.UID), false, 0, nil); err != nil {
		return 0, errors.New("original removal was not confirmed; verified encrypted copy retained and cleanup will retry")
	}
	return copyUID, nil
}

// Bind to the authenticated connection rather than the credential file, which
// can be replaced concurrently and uses randomized encryption on each save.
func incomingAccountHash(d *goimap.Dialer) string {
	data, _ := json.Marshal([]any{d.Host, d.Port, d.Username})
	return incomingHash(data)
}

func incomingSearch(d *goimap.Dialer, criteria string) ([]int, error) {
	response, err := d.Exec("UID SEARCH "+criteria, true, 0, nil)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(response, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "*" || !strings.EqualFold(fields[1], "SEARCH") {
			continue
		}
		out := make([]int, 0, len(fields)-2)
		for _, value := range fields[2:] {
			uid, err := strconv.ParseUint(value, 10, 32)
			if err != nil || uid == 0 {
				return nil, errors.New("invalid IMAP search result")
			}
			out = append(out, int(uid))
		}
		return out, nil
	}
	return nil, errors.New("IMAP omitted search result")
}

func incomingRaw(d *goimap.Dialer, uid int) ([]byte, error) {
	large, err := incomingSearch(d, fmt.Sprintf("UID %d LARGER %d", uid, mailmsg.MaxInboundMessageBytes))
	if err != nil {
		return nil, err
	}
	if len(large) > 0 {
		return nil, mailmsg.ErrMessageTooLarge
	}
	response, err := d.Exec(fmt.Sprintf("UID FETCH %d BODY.PEEK[]", uid), true, 0, nil)
	if err != nil {
		return nil, err
	}
	records, err := d.ParseFetchResponse(response)
	if err != nil {
		return nil, err
	}
	return parseRawMessageRecord(records, uid)
}

func incomingMetadata(d *goimap.Dialer, uid int) (*goimap.Email, error) {
	response, err := d.Exec(fmt.Sprintf("UID FETCH %d (UID FLAGS INTERNALDATE)", uid), true, 0, nil)
	if err != nil {
		return nil, err
	}
	records, err := d.ParseFetchResponse(response)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		tokens := unwrapTokens(record)
		ov := &goimap.Email{}
		flagsFound := false
		for i := 0; i+1 < len(tokens); i++ {
			next := tokens[i+1]
			switch strings.ToUpper(tokens[i].Str) {
			case "UID":
				if next.Type != goimap.TNumber {
					return nil, errors.New("invalid UID metadata")
				}
				ov.UID = next.Num
				i++
			case "FLAGS":
				if next.Type != goimap.TContainer {
					return nil, errors.New("invalid FLAGS metadata")
				}
				for _, flag := range next.Tokens {
					ov.Flags = append(ov.Flags, flag.Str)
				}
				flagsFound = true
				i++
			case "INTERNALDATE":
				ov.Received, err = time.Parse(goimap.TimeFormat, next.Str)
				if err != nil {
					return nil, err
				}
				i++
			}
		}
		if ov.UID == uid && flagsFound && !ov.Received.IsZero() {
			return ov, nil
		}
	}
	return nil, errors.New("original metadata is absent")
}

// ApplyIncomingAction verifies the encrypted message on the same connection as
// the mutation, including after restart. A lost MOVE acknowledgement is resolved
// by locating the exact ciphertext in its destination, never by guessing a UID.
func (c *APIClient) ApplyIncomingAction(ctx context.Context, source IncomingSource, uid int, marker string, encrypted []byte, action, value string) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := hex.DecodeString(marker)
	if uid <= 0 || err != nil || len(token) != 32 || !bytes.HasPrefix(encrypted, []byte("X-KyPost-Incoming: "+marker+"\r\n")) {
		return errors.New("invalid encrypted replacement action")
	}
	d, err := c.ensureConnectedLocked()
	if err != nil {
		return errors.New("cannot connect to apply encrypted-mail action")
	}
	if incomingAccountHash(d) != source.AccountHash {
		return errors.New("IMAP account changed; restore its credentials before resuming encryption")
	}
	command, destination := "", ""
	switch action {
	case "keyword", "unkeyword":
		if err := ValidateKeyword(value); err != nil {
			return err
		}
		sign := "+"
		if action == "unkeyword" {
			sign = "-"
		}
		command = fmt.Sprintf("UID STORE %d %sFLAGS.SILENT (%s)", uid, sign, value)
	case "read":
		command = fmt.Sprintf(`UID STORE %d +FLAGS.SILENT (\Seen)`, uid)
	case "stop":
		return nil
	case "move":
		if err := ValidateMailboxName(value); err != nil {
			return err
		}
		folders, listErr := d.GetFolders()
		if listErr != nil {
			return errors.New("cannot list encrypted-mail action destinations")
		}
		if !containsMailboxPath(folders, value) {
			c.invalidateFolderIndexLocked()
			if err := d.CreateFolder(value); err != nil {
				return errors.New("cannot create encrypted-mail action destination")
			}
		}
		destination = value
	case "archive":
		destination, err = c.childOfSpecialFolderLocked(d, useArchive, strconv.Itoa(source.Date.UTC().Year()))
	case "spam":
		destination, err = c.specialFolderLocked(d, useJunk)
	case "delete":
		destination, err = c.specialFolderLocked(d, useTrash)
	default:
		return errors.New("unsupported incoming encrypted-mail action")
	}
	if err != nil {
		return errors.New("cannot resolve encrypted-mail action destination")
	}
	if destination != "" {
		if err := ValidateMailboxName(destination); err != nil {
			return err
		}
		command = fmt.Sprintf(`UID MOVE %d "%s"`, uid, destination)
	}
	// Folder discovery may reconnect. All identity checks follow it, and no
	// subsequent command reconnects automatically.
	validity, err := incomingMailbox(d)
	if err != nil {
		return err
	}
	if validity != source.UIDValidity || incomingAccountHash(d) != source.AccountHash {
		return errors.New("IMAP account or UIDVALIDITY changed; encrypted-mail action paused")
	}
	existing, err := incomingSearch(d, "UID "+strconv.Itoa(uid))
	if err != nil {
		return errors.New("cannot verify encrypted-mail UID before action")
	}
	if len(existing) == 0 && destination != "" {
		if _, err := d.Exec(`SELECT "`+destination+`"`, false, 0, nil); err != nil {
			return errors.New("cannot reconcile encrypted-mail move destination")
		}
		d.Folder, d.ReadOnly = destination, false
		candidates, err := incomingSearch(d, `HEADER X-KyPost-Incoming "`+marker+`"`)
		if err != nil || len(candidates) > 20 {
			return errors.New("cannot reconcile encrypted-mail move")
		}
		for _, candidate := range candidates {
			raw, err := incomingRaw(d, candidate)
			if err != nil {
				return errors.New("cannot verify encrypted-mail move")
			}
			if bytes.Equal(raw, encrypted) {
				return nil
			}
		}
		return errors.New("encrypted copy absent from inbox and action destination; preserve journal for reconciliation")
	}
	raw, err := incomingRaw(d, uid)
	if err != nil || !bytes.Equal(raw, encrypted) {
		return errors.New("encrypted replacement UID no longer matches; action paused without modifying mail")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.EqualFold(destination, "INBOX") {
		return nil
	}
	if _, err := d.Exec(command, false, 0, nil); err != nil {
		return errors.New("encrypted-mail action acknowledgement missing; verified copy retained for retry")
	}
	return nil
}
