package config

import (
	"errors"
	"os"
	"strings"
)

// NativeMailEnabled is an explicit operator opt-in. A typo cannot select IMAP
// or silently enable a native mailbox stack in only one process.
func NativeMailEnabled() (bool, error) {
	switch strings.TrimSpace(os.Getenv("KYPOST_NATIVE_MAIL")) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("KYPOST_NATIVE_MAIL must be true or false")
	}
}
