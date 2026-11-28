package app

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/Busnes-app/ky-primitives/s3"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// probeObjectStorage refuses a multi start whose bucket refuses the credentials or does not
// refuse an existing key on a conditional create (MH11), before any store opens.
func probeObjectStorage(cells []string) error {
	c, err := config.ObjectStorage()
	if err != nil {
		return err
	}
	o, err := vol.NewObject(c, cells, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return o.Probe(ctx)
}

var errBodyOrg = errors.New("a mailbox's bodies open only under its own org's key")

// useObjectBodies places every mailbox this multi process opens in object storage (MH11):
// under its org's cell, which must be served, and sealed with its org's doc_keys data key,
// created only under the owner guard (vol.OrgKey). Each store gets an Object of its one cell
// whose key opens only its own org. It returns the deployment's Object (every served cell, no
// key: the purge's lists and deletes) and its release. The hook is per process: the first
// caller installs it and the last release removes it, so a daemon's short-lived receiving
// runtime never uninstalls the api's. The hook opens its own tenancy.db handle, closed with
// the last release, so no caller's handle outlives its owner inside it.
func useObjectBodies(client *s3.Client, staffOrg string, cells []string, stateDir string) (*vol.Object, func(), error) {
	placing.Lock()
	defer placing.Unlock()
	if placing.n > 0 {
		placing.n++
		return placing.all, releaseObjectBodies, nil
	}
	all, err := vol.NewObject(client, cells, nil)
	if err != nil {
		return nil, nil, err
	}
	db, err := tenancy.Open(stateDir, false)
	if err != nil {
		return nil, nil, err
	}
	mailbox.SetObjectBodies(placeBodies(client, db, staffOrg, cells, stateDir))
	placing.n, placing.all, placing.db = 1, all, db
	return all, releaseObjectBodies, nil
}

// placeBodies is the mailbox placement useObjectBodies installs.
func placeBodies(client *s3.Client, db *sql.DB, staffOrg string, cells []string, stateDir string) func(mailbox.Owner) (*vol.Object, mailbox.BodyScope, error) {
	return func(owner mailbox.Owner) (*vol.Object, mailbox.BodyScope, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		org, err := tenancy.SubjectOrg(ctx, db, owner.Issuer, owner.Subject)
		if err != nil {
			return nil, mailbox.BodyScope{}, err
		}
		cell, err := tenancy.OrgCell(ctx, db, org, staffOrg, cells)
		if err != nil {
			return nil, mailbox.BodyScope{}, err
		}
		o, err := vol.NewObject(client, []string{cell}, func(ctx context.Context, keyOrg string) ([]byte, error) {
			if keyOrg != org {
				return nil, errBodyOrg
			}
			return vol.OrgKey(ctx, stateDir, owner.Issuer, owner.Subject, org)
		})
		return o, mailbox.BodyScope{Cell: cell, Org: org}, err
	}
}

var placing struct {
	sync.Mutex
	n   int
	all *vol.Object
	db  *sql.DB
}

func releaseObjectBodies() {
	placing.Lock()
	defer placing.Unlock()
	if placing.n--; placing.n == 0 {
		mailbox.SetObjectBodies(nil)
		_ = placing.db.Close()
		placing.all, placing.db = nil, nil
	}
}
