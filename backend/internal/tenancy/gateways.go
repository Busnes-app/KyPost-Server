package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"time"
)

// Receiving gateway pins (hosted receiving tier, RX "Keys"). Each MX host is one gateway
// ID with at most two live pins: SHA-256 of a key's SubjectPublicKeyInfo, lowercase hex.

// PinLifetime is a pin's life from enrolment (RX open question 3); PinWarning is when a
// connection with that pin starts logging that it expires soon.
const (
	PinLifetime = 90 * 24 * time.Hour
	PinWarning  = 14 * 24 * time.Hour
	livePins    = 2
)

// ReservedGateways are the single-tenant ingress gateway IDs. They can never be enrolled,
// and a pin row naming one is refused.
var ReservedGateways = []string{"maddy-local", "cloudflare-worker", "cloudflare-continuous"}

var (
	gatewayIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	spkiPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Errors of the gateway functions. None names a key.
var (
	ErrGatewayID       = errors.New("a gateway ID is 1-63 lowercase letters, digits and hyphens, and maddy-local, cloudflare-worker and cloudflare-continuous are reserved")
	ErrGatewaySPKI     = errors.New("a pin is the key's SPKI SHA-256 as 64 lowercase hex digits")
	ErrGatewayRetired  = errors.New("this gateway ID is retired and takes no new pins")
	ErrGatewayKeyUsed  = errors.New("this key was enrolled before; generate a new key")
	ErrGatewayFull     = errors.New("this gateway ID already has two live pins; revoke one first")
	ErrGatewayNoPin    = errors.New("no such live pin")
	ErrGatewayUnpinned = errors.New("the key is not a live gateway pin")
)

// ValidGatewayID reports whether id may be enrolled.
func ValidGatewayID(id string) bool {
	return gatewayIDPattern.MatchString(id) && !slices.Contains(ReservedGateways, id)
}

func checkPin(id, spki string) error {
	if !ValidGatewayID(id) {
		return ErrGatewayID
	}
	if !ValidPin(spki) {
		return ErrGatewaySPKI
	}
	return nil
}

// gatewayTx runs fn serializably: PostgreSQL then refuses a concurrent writer that read
// the same rows, so two enrolments cannot both pass the two-live-pins check.
func gatewayTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AddGateway enrols spki under id until now+PinLifetime. A key seen before, under any ID
// and in any state, is refused, as is a retired ID or a third live pin.
func AddGateway(ctx context.Context, db *sql.DB, id, spki string, now time.Time) (expires time.Time, err error) {
	if err := checkPin(id, spki); err != nil {
		return time.Time{}, err
	}
	expires = now.Add(PinLifetime)
	return expires, gatewayTx(ctx, db, func(tx *sql.Tx) error {
		var used, retired, live int
		err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM receiving_gateways WHERE spki_sha256 = ?),
 (SELECT COUNT(*) FROM receiving_gateways WHERE id = ? AND retired_at IS NOT NULL),
 (SELECT COUNT(*) FROM receiving_gateways WHERE id = ? AND revoked_at IS NULL AND expires_at > ?)`,
			spki, id, id, now.Unix()).Scan(&used, &retired, &live)
		switch {
		case err != nil:
			return err
		case used > 0:
			return ErrGatewayKeyUsed
		case retired > 0:
			return ErrGatewayRetired
		case live >= livePins:
			return ErrGatewayFull
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO receiving_gateways(id, spki_sha256, created_at, expires_at) VALUES(?, ?, ?, ?)`,
			id, spki, now.Unix(), expires.Unix())
		return err
	})
}

// RevokeGateway revokes one pin of id. Revoking a pin twice is ErrGatewayNoPin.
func RevokeGateway(ctx context.Context, db *sql.DB, id, spki string, now time.Time) error {
	if err := checkPin(id, spki); err != nil {
		return err
	}
	return gatewayTx(ctx, db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE receiving_gateways SET revoked_at = ? WHERE id = ? AND spki_sha256 = ? AND revoked_at IS NULL`, now.Unix(), id, spki)
		return changed(res, err)
	})
}

// RetireGateway revokes every pin of id and retires the ID for good. An ID with no pins,
// or one already retired, is ErrGatewayNoPin.
func RetireGateway(ctx context.Context, db *sql.DB, id string, now time.Time) error {
	if !ValidGatewayID(id) {
		return ErrGatewayID
	}
	return gatewayTx(ctx, db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE receiving_gateways SET revoked_at = COALESCE(revoked_at, ?), retired_at = ? WHERE id = ? AND retired_at IS NULL`, now.Unix(), now.Unix(), id)
		return changed(res, err)
	})
}

func changed(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		err = ErrGatewayNoPin
	}
	return err
}

// GatewayFor returns the gateway ID of a live pin and when it expires. A revoked, expired
// or retired pin, an unknown key and a reserved ID are all ErrGatewayUnpinned.
func GatewayFor(ctx context.Context, db *sql.DB, spki string, now time.Time) (string, time.Time, error) {
	var id string
	var expires int64
	err := db.QueryRowContext(ctx, `SELECT id, expires_at FROM receiving_gateways WHERE spki_sha256 = ? AND revoked_at IS NULL AND retired_at IS NULL AND expires_at > ?`,
		spki, now.Unix()).Scan(&id, &expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !ValidGatewayID(id) {
		return "", time.Time{}, ErrGatewayUnpinned
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return id, time.Unix(expires, 0), nil
}

// ValidPin reports whether spki is a pin's form: 64 lowercase hex digits.
func ValidPin(spki string) bool { return spkiPattern.MatchString(spki) }
