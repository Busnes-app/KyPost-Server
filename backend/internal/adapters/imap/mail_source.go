package imap

import (
	"errors"
	"strconv"
	"strings"
)

// MailSourceIdentity keeps legacy IMAP's existing configuration behavior while
// letting native storage expose its immutable owner/database namespace. It is
// an account-mode fence; IMAP endpoint/UIDVALIDITY migration is separate work.
func MailSourceIdentity(client Client) string {
	if native, ok := client.(interface{ MailSourceIdentity() string }); ok {
		return native.MailSourceIdentity()
	}
	return "imap"
}

// MessageReference translates internal IDs only at wire boundaries. The native
// generation is independent of the immutable namespace used for encryption.
func MessageReference(client Client, id string) string {
	if MailSourceIdentity(client) == "imap" {
		return id
	}
	if native, ok := client.(interface{ MessageReferenceGeneration() string }); ok && native.MessageReferenceGeneration() != "" {
		return "n1:" + native.MessageReferenceGeneration() + ":" + id
	}
	return "" // Unknown native capabilities must never fall back to numeric IDs.
}

// ResolveMessageReference rejects restored/foreign/bare native references before
// any message lookup. Legacy IMAP keeps its existing internal ID handling.
func ResolveMessageReference(client Client, reference string) (string, error) {
	if MailSourceIdentity(client) == "imap" {
		return reference, nil
	}
	native, ok := client.(interface{ MessageReferenceGeneration() string })
	if ok && native.MessageReferenceGeneration() != "" {
		if id, found := strings.CutPrefix(reference, "n1:"+native.MessageReferenceGeneration()+":"); found {
			uid, err := strconv.Atoi(id)
			if err == nil && uid > 0 && strconv.Itoa(uid) == id {
				return id, nil
			}
		}
	}
	return "", errors.New("stale or invalid message reference; refresh the mailbox")
}
