//go:build !linux

package mailbox

import "errors"

func publishPreparedAccount(_, _ string) error {
	return errors.New("atomic native account preparation requires Linux; preserve existing account state")
}
