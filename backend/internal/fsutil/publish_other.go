//go:build !linux

package fsutil

import "errors"

func PublishDirectory(_, _ string) error {
	return errors.New("atomic directory publication requires Linux; preserve existing data")
}
