package vol

import (
	"context"
	"database/sql"
	"errors"
)

// VerifySnapshot authenticates every wrapped data key and document in an offline SQLite
// capsule copy. It shares the live reader, including path/owner binding and no fallback.
func VerifySnapshot(ctx context.Context, db *sql.DB, master []byte, orgFor func(context.Context, string) (string, error)) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM doc_keys`).Scan(&n); err != nil {
		return err
	}
	if n > 0 && len(master) != 32 {
		return errors.New("vol: documents.key is required to restore sealed documents")
	}
	t := &Table{db: db, master: master, orgFor: orgFor}
	rows, err := db.QueryContext(ctx, `SELECT org_id FROM doc_keys`)
	if err != nil {
		return err
	}
	var orgs []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, org)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, org := range orgs {
		if _, err := t.dataKey(ctx, db, org, false); err != nil {
			return err
		}
	}
	rows, err = db.QueryContext(ctx, `SELECT root,path FROM documents ORDER BY root,path`)
	if err != nil {
		return err
	}
	// Only paths are buffered. Bodies are opened one at a time through Read.
	var paths [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return err
		}
		paths = append(paths, p)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, p := range paths {
		if p[0] != "config" && p[0] != "state" {
			return errRoot
		}
		t.root = p[0]
		if _, _, err := t.Read(ctx, p[1]); err != nil {
			return err
		}
	}
	return nil
}
