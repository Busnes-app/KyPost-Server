package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

var ErrNativeSendDevice = errors.New("queued device authority changed; retain the job and reconcile before sending")

// NativeDeviceWitness identifies the verified credential, never its raw secret.
// Re-pairing under the same DeviceID cannot adopt its predecessor's queued work.
func NativeDeviceWitness(d NativeDevice) string {
	if d.SecretHash == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(d.SecretHash))
	return hex.EncodeToString(digest[:])
}

// WithNativeSendDevice fences all cooperating device mutations through a local
// outbox commit. The callback must not re-enter state or perform network I/O;
// caller holds directory/users before this immediate SQLite transaction.
func (s *Store) WithNativeSendDevice(ctx context.Context, deviceID string, action func(NativeDevice) error) error {
	if deviceID == "" || action == nil {
		return ErrNativeSendDevice
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	d, err := scanDevice(tx.QueryRowContext(ctx, `SELECT `+deviceColumns+` FROM native_devices WHERE device_id=?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNativeSendDevice
	}
	if err != nil {
		return err
	}
	if err = action(d); err != nil {
		return err
	}
	return tx.Commit()
}
