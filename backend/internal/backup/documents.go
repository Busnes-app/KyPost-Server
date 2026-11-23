package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kypost-server/backend/internal/pgpmail"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
	"github.com/Busnes-app/kypost-server/backend/internal/volumes"
)

// validateDocumentSnapshot examines the actual collected/staged SQLite bytes. The
// immutable user_orgs bindings supply hosted ownership; a migrated install has no tenancy.db.
func validateDocumentSnapshot(dir string) error {
	release, err := documentSnapshot(dir, false)
	if release != nil {
		release()
	}
	return err
}

func documentSnapshot(dir string, mount bool) (func(), error) {
	noop := func() {}
	p := filepath.Join(dir, "state", volumes.File)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return noop, nil
	} else if err != nil {
		return nil, err
	}
	mode := "ro"
	if mount {
		mode = "rw&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p}).String()+"?mode="+mode)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = db.Close()
		}
	}()
	orgs := map[string]string{}
	multi := false
	tp := filepath.Join(dir, "state", tenancy.File)
	if _, err := os.Stat(tp); err == nil {
		multi = true
		td, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: tp}).String()+"?mode=ro")
		if err != nil {
			return nil, err
		}
		defer td.Close()
		rows, err := td.Query(`SELECT user_id,org_id FROM user_orgs`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var own, org string
			if err := rows.Scan(&own, &org); err != nil {
				rows.Close()
				return nil, err
			}
			orgs[own] = org
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	orgFor := func(_ context.Context, owner string) (string, error) {
		org, ok := orgs[owner]
		if multi && !ok {
			return "", errors.New("sealed document has no org binding")
		}
		return org, nil
	}
	master, _ := cryptutil.LoadKey(filepath.Join(dir, "private", "documents.key"))
	if err := vol.VerifySnapshot(context.Background(), db, master, orgFor); err != nil {
		return nil, err
	}
	if err := documentSecrets(dir, db, master, orgFor); err != nil {
		return nil, err
	}
	if !mount {
		return noop, nil
	}
	// Early MH2 capsules may contain documents beside platform files. A full table
	// snapshot is identified by its root config; MH8 adds the completed-migration mark.
	var roots int
	if err := db.QueryRow(`SELECT count(*) FROM documents WHERE root='config' AND path='config.yaml'`).Scan(&roots); err != nil {
		return nil, err
	}
	if roots == 0 {
		return noop, nil
	}
	configVolume, err := vol.NewSnapshotTable(db, "config", vol.TableOptions{MasterKey: master, Org: orgFor})
	if err != nil {
		return nil, err
	}
	stateVolume, err := vol.NewSnapshotTable(db, "state", vol.TableOptions{MasterKey: master, Org: orgFor})
	if err != nil {
		return nil, err
	}
	configRelease, err := vol.Mount(filepath.Join(dir, "config"), configVolume)
	if err != nil {
		return nil, err
	}
	stateRelease, err := vol.Mount(filepath.Join(dir, "state"), stateVolume)
	if err != nil {
		configRelease()
		return nil, err
	}
	keep = true
	return func() { stateRelease(); configRelease(); _ = db.Close() }, nil
}

// The volume seal does not replace each credential store's own encryption. A
// capsule must carry the keys needed to open both layers after restore.
func documentSecrets(dir string, db *sql.DB, master []byte, orgFor func(context.Context, string) (string, error)) error {
	rows, err := db.Query(`SELECT root,path FROM documents WHERE path LIKE '%imap-config.json' OR path LIKE '%carddav-client.json' OR path LIKE 'pickup/%'`)
	if err != nil {
		return err
	}
	var paths [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return err
		}
		paths = append(paths, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, p := range paths {
		table, err := vol.NewSnapshotTable(db, p[0], vol.TableOptions{MasterKey: master, Org: orgFor})
		if err != nil {
			return err
		}
		body, _, err := table.Read(context.Background(), p[1])
		if err != nil {
			return err
		}
		switch filepath.Base(p[1]) {
		case "imap-config.json", "carddav-client.json":
			if _, err := cryptutil.OpenBytes(body, filepath.Join(dir, "private", "imap-config.key")); err != nil {
				return errors.New("document snapshot has unreadable mail/contact-sync credentials")
			}
		default:
			var record pgpmail.PickupRecord
			if err := json.Unmarshal(body, &record); err != nil {
				return errors.New("document snapshot has an unreadable pickup record")
			}
			if record.SubjectEnc != nil || record.BodyEnc != (cryptutil.EncryptedPayload{}) {
				key, err := cryptutil.LoadKey(filepath.Join(dir, "private", "pickup-store.key"))
				if err != nil {
					return errors.New("document snapshot requires pickup-store.key")
				}
				if _, err := cryptutil.Open(record.BodyEnc, key); err != nil {
					return errors.New("document snapshot has an unreadable pickup body")
				}
				if record.SubjectEnc != nil {
					if _, err := cryptutil.Open(*record.SubjectEnc, key); err != nil {
						return errors.New("document snapshot has an unreadable pickup subject")
					}
				}
			}
		}
	}
	return nil
}
