package tenancy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// BindUserAccount is step 3 for table accounts. Cache deletion and account
// removal exclude creation of a new user_orgs row even if the org fence was
// lost. All stores share this PostgreSQL database; only the schema differs.
func BindUserAccount(ctx context.Context, db *sql.DB, stateDir, userID, org, staffOrg string) error {
	if db == nil || !config.ValidOrgID(org) {
		return ErrOrgMismatch
	}
	schema, _, err := dbbackend.SchemaName(filepath.Join(stateDir, volumes.File))
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT org_id FROM user_orgs WHERE user_id=?`, userID).Scan(&prior)
	if err == nil {
		if prior != org {
			return ErrOrgMismatch
		}
		return tx.Commit() // no new authority, including an inactive signed event
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if org != staffOrg {
		var status string
		if err = tx.QueryRowContext(ctx, `SELECT status FROM orgs WHERE id=? FOR SHARE`, org).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			return ErrNotServed
		} else if err != nil {
			return err
		}
		if status == "deleted" {
			return ErrNotServed
		}
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT record FROM "`+schema+`".accounts WHERE id=? FOR KEY SHARE`, userID).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return ErrNotServed
	} else if err != nil {
		return err
	}
	var u struct {
		ID      string `json:"id"`
		Org     string `json:"orgId"`
		Issuer  string `json:"ssoIssuer"`
		Subject string `json:"ssoSub"`
	}
	if json.Unmarshal(raw, &u) != nil || u.ID != userID || u.Org != org {
		return ErrOrgMismatch
	}
	var bound string
	if err = tx.QueryRowContext(ctx, `SELECT org_id FROM subject_orgs WHERE issuer=? AND subject=?`, u.Issuer, u.Subject).Scan(&bound); err != nil {
		return err
	}
	if bound != org {
		return ErrOrgMismatch
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_orgs(user_id,org_id) VALUES(?,?) ON CONFLICT DO NOTHING`, userID, org); err != nil {
		return err
	}
	return tx.Commit()
}
