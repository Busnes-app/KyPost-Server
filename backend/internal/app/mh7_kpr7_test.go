package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/ky-primitives/suitedir"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/dbbackend"
	"github.com/Busnes-app/kypost-server/backend/internal/mailbox"
	"github.com/Busnes-app/kypost-server/backend/internal/tenancy"
	"github.com/Busnes-app/kypost-server/backend/internal/users"
	"github.com/Busnes-app/kypost-server/backend/internal/vol"
)

// MH7: BootstrapAdmin in table mode creates neither admin.env nor first-run-password.txt.
func TestBootstrapAdminTableModeWritesNothing(t *testing.T) {
	dbbackend.RequirePostgres(t)
	d := newGracefulShutdownTestDeps(t)
	secret := os.Getenv("SECRET_DIR")
	enterTableMode(t, d.store, d.configDir, d.stateDir, secret)

	t.Setenv("CONFIG_DIR", d.configDir)
	t.Setenv("STATE_DIR", d.stateDir)
	t.Setenv("SECRET_DIR", secret)
	t.Setenv("KY_TENANCY", config.TenancySingle)

	if err := BootstrapAdmin(); err != nil {
		t.Fatalf("BootstrapAdmin in table mode: %v", err)
	}
	for _, name := range []string{"admin.env", users.BootstrapPasswordFile} {
		if _, err := os.Stat(filepath.Join(d.configDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("table-mode BootstrapAdmin wrote %s: %v", name, err)
		}
	}
}

// MH7: migrateLegacySingleUserData in table mode copies tuning.md into the documents table
// without writing to the host's config or state directories, and skips legacy SQLite files.
func TestMigrateLegacySingleUserDataTableMode(t *testing.T) {
	dbbackend.RequirePostgres(t)
	d := newGracefulShutdownTestDeps(t)
	secret := os.Getenv("SECRET_DIR")
	enterTableMode(t, d.store, d.configDir, d.stateDir, secret)

	accounts, err := users.OpenExisting(context.Background(), d.configDir)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := accounts.FirstAdmin()
	if err != nil {
		t.Fatal(err)
	}

	tuningSrc := filepath.Join(t.TempDir(), "TUNING.md")
	const wantTuning = "custom tuning prompt\n"
	if err := os.WriteFile(tuningSrc, []byte(wantTuning), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TUNING_FILE", tuningSrc)

	if err := migrateLegacySingleUserData(d.logger, accounts, d.configDir, d.stateDir, config.UserNotificationSettings{}, false); err != nil {
		t.Fatalf("migrateLegacySingleUserData in table mode: %v", err)
	}

	tuningDst := filepath.Join(d.configDir, "users", admin.ID, "tuning.md")
	got, err := vol.OpenDocument(tuningDst).Read()
	if err != nil {
		t.Fatalf("tuning.md missing from documents table: %v", err)
	}
	if string(got) != wantTuning {
		t.Fatalf("tuning.md = %q, want %q", got, wantTuning)
	}
	if _, err := os.Stat(tuningDst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("table-mode tuning.md written to host disk: %v", err)
	}
}

// K PR 7: newAPIServer wires UseTenancy when d.tenancy is set (StoragePoolCheck enforces
// org grants and exempts the staff org) and leaves single-tenant mode a no-op when nil.
func TestNewAPIServerTenancyAndOrgPoolCheck(t *testing.T) {
	d := newGracefulShutdownTestDeps(t)

	// Single-tenant: d.tenancy == nil -> StoragePoolCheck is a no-op.
	singleSrv := newAPIServer(d, nil)
	owner := mailbox.Owner{Issuer: "https://id.example.test", Subject: "sub-1", Mailbox: "mb-1"}
	if err := singleSrv.StoragePoolCheck(context.Background(), owner, 1024); err != nil {
		t.Fatalf("single-tenant StoragePoolCheck refused: %v", err)
	}

	// Multi-tenant: wire tenancy.db, staff org, and cells.
	db, err := tenancy.Open(d.stateDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	const (
		orgA     = "1a2b3c4d-0000-4000-8000-00000000000a"
		staffOrg = "1a2b3c4d-0000-4000-8000-0000000000ff"
	)
	ctx := context.Background()
	if err := tenancy.BindSubject(ctx, db, owner.Issuer, owner.Subject, orgA); err != nil {
		t.Fatal(err)
	}
	staffOwner := mailbox.Owner{Issuer: owner.Issuer, Subject: "sub-staff", Mailbox: "mb-staff"}
	if err := tenancy.BindSubject(ctx, db, staffOwner.Issuer, staffOwner.Subject, staffOrg); err != nil {
		t.Fatal(err)
	}
	if err := tenancy.ApplyOrg(ctx, db, suitedir.Org{ID: orgA, Status: "active", Revision: 1, Enabled: true, Quotas: json.RawMessage(`{"storage_bytes":4096}`)}, time.Now()); err != nil {
		t.Fatal(err)
	}

	d.tenancy = db
	d.staffOrg = staffOrg
	d.cells = []string{"cell-1"}
	multiSrv := newAPIServer(d, nil)

	// No storage grant yet -> refused for orgA, allowed for staffOrg.
	for name, check := range map[string]mailbox.PoolCheck{
		"Server.StoragePoolCheck": multiSrv.StoragePoolCheck,
		"app.orgPoolCheck":        orgPoolCheck(db, staffOrg),
	} {
		if err := check(ctx, owner, 512); !errors.Is(err, mailbox.ErrPoolFull) {
			t.Fatalf("%s without grant: got %v, want ErrPoolFull", name, err)
		}
		if err := check(ctx, staffOwner, 512); err != nil {
			t.Fatalf("%s for staff org: %v", name, err)
		}
		if err := check(ctx, owner, 0); err != nil {
			t.Fatalf("%s for add=0: %v", name, err)
		}
	}

	// Grant 1024 bytes -> 512 succeeds via StoragePoolCheck, next 512 succeeds via orgPoolCheck,
	// next 1 byte fails with ErrPoolFull.
	seq, last, err := tenancy.NextStorageSeq(ctx, db, orgA)
	if err != nil {
		t.Fatal(err)
	}
	if err := tenancy.SetGrant(ctx, db, orgA, seq, tenancy.Grant{
		Admitted:  last.Admitted,
		Ceiling:   1024,
		Expires:   time.Now().Add(time.Hour),
		PoolBytes: 4096,
	}); err != nil {
		t.Fatal(err)
	}
	if err := multiSrv.StoragePoolCheck(ctx, owner, 512); err != nil {
		t.Fatalf("StoragePoolCheck within grant: %v", err)
	}
	if err := orgPoolCheck(db, staffOrg)(ctx, owner, 512); err != nil {
		t.Fatalf("orgPoolCheck within grant: %v", err)
	}
	if err := multiSrv.StoragePoolCheck(ctx, owner, 1); !errors.Is(err, mailbox.ErrPoolFull) {
		t.Fatalf("StoragePoolCheck past grant: got %v, want ErrPoolFull", err)
	}
}
