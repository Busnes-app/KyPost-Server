# Native authority per account

Status: PR 1 (the `authorityStore` seam), PR 2 (the read-only v3 reader for
backups) and PR 3 (the SQLite runtime backend, `MigrateAuthority` in
`migrate-native`, `backend/internal/sso/authority_sqlite.go` and
`authority_migrate.go`) are implemented, 2026-11-11. The runtime reads and writes
only v3; the JSON backend remains for offline v1/v2 stores (restore staging,
the v1 → v2 step) until PR 4. It replaces
the whole-file reads of `sso-lifecycle.json` and `native-provisioning.json` on every
mail-client operation with reads of single records, so that admission costs O(1) in
the number of accounts. [NATIVE_ADDRESSING_V2.md](NATIVE_ADDRESSING_V2.md) and
[NATIVE_PROVISIONING.md](NATIVE_PROVISIONING.md) remain the record semantics;
section 11 lists where PR 3 departs from this design.

## Problem and decision summary

The busnes.app profile (`performance/post-ledger-profile-2026-11-10/REPORT.md`, numbers
quoted from the brief, not re-measured here) has 3,000 accounts and 3 active clients.
The daemon uses about 1.3 of 2 cores, mostly on fresh admission. Each operation reads
and byte-compares `native-provisioning.json` (about 2.9 MB, about 970 B per account)
and `sso-lifecycle.json` (the directory map plus the initialization flag). That is
about 3 MB per operation and about 9 GB per full scan, so O(accounts²) per scan. #571
removed the per-operation reopening of mailbox databases and gained only 4–7%.

There is a second cliff. The decoded-view reuse in `native_admission_view.go` keeps a
view only up to 4,096 entries or 8 MiB. At 10,000 accounts both files are over those
limits, so every operation does a full JSON decode and full ledger validation, not
just a read and a byte compare.

| Decision | Choice |
|---|---|
| Record store | One SQLite database, `$CONFIG_DIR/native-authority.db`. Each account, mailbox, address and directory subject is a row. |
| What moves | Everything in `sso-lifecycle.json` and `native-provisioning.json`. Both files become small type-incompatible stubs. |
| What stays a file | `native-domains.json`, `native-domain.json` (tombstone), `native-relay.json`, `sso.json`, `users.json`, `state/native-restore-hold.json`. All are O(1) in accounts except `users.json` (see open question Q3). |
| Interface | `authorityStore` with `View`/`Update` transactions (section 2). The JSON files sit behind it first, then SQLite. A later Postgres backend needs the same tables. |
| Freshness | Every operation runs its own read transaction against current committed rows. Nothing decoded survives a call. `native_admission_view.go` is deleted. |
| Fence | The directory flock (`sso-lifecycle.json.lock`) and the lock order domains → directory → users stay as they are. |
| Migration | `migrate-native` gains an offline v2 → v3 step. It keeps `*.v2-migrated` copies, builds the database by rename, writes the stubs last, and is idempotent and crash-convergent. |
| Not done | No admission cache, no TTL, no stat-only freshness, no batching, no Postgres. |

Why not one file per account: sealed capsules allow at most 4,096 files
(`ky-primitives` `capsule.MaxFiles`), so 10,000 account files cannot be backed up in a
capsule. Multi-account writes (reassigning an address, uniqueness checks) would need a
hand-written journal, and every account ID would become a path component. See
section 10.

## 1. Data model

### What admission needs per operation

`admitNativeMailUser` → `nativeUserStorageForAdmission` → `admitNativeMailbox` (and
`nativeUserOwnership` for state opens) read the following today:

| Input | Today | v3 | Scales with |
|---|---|---|---|
| Issuer, settings | `sso.json` read (`Store.Load`) | unchanged | O(1) |
| User (active, role, link, native fields) | `users.json`, cached by size and mtime (`users.Store.load`) | unchanged (Q3) | file O(N), read only after a change |
| Restore hold | `Lstat state/native-restore-hold.json` | unchanged | O(1) |
| Format and tombstone | reads `native-domain.json` and the whole ledger header, `Lstat`s required files | reads the tombstone, the ledger **stub** (about 60 B) and `meta.format`; `Lstat`s the required files | O(1) |
| Initialization flag | whole `sso-lifecycle.json` | `meta.initialized` row | O(1) |
| Directory entry | whole `sso-lifecycle.json` (map lookup after the full read and compare) | `directory` row by (issuer, subject) | O(log N) |
| Assignment (owner, root, limits, source, status) | whole ledger (full v2 validation on a view miss) | `accounts` row plus primary `mailboxes` row | O(log N) |
| Primary address | whole ledger | `addresses` via the partial index `(mailbox) WHERE kind='primary'` | O(log N) |
| Extra mailbox | whole ledger, read a **second** time (`loadNative`) | `mailboxes` row by ID plus its primary address | O(log N) |
| Domain set | `native-domains.json` | unchanged | O(domains) |
| Prepared storage | `mailbox.Prepared` held handles (#571) | unchanged; admissions without a client use a per-process pool of `Prepared` handles (section 11) | O(1) |

### Records and where they live

| Record | Kind | v3 table | Today |
|---|---|---|---|
| Format (3), initialization, change counter | install-wide, small | `meta` | ledger `version`, lifecycle flag, ledger mtime |
| Directory state per subject (resource, revision, digest, active, eventId, revokedBefore) | per account | `directory` | lifecycle `directory` |
| Account (revision, digest, desiredActive, status, failure, primaryMailbox, legacyMixedUse) | per account | `accounts` | ledger `accounts` |
| Mailbox (owner, kind, state, stateRoot, limits, source) | per mailbox | `mailboxes` | ledger `mailboxes` |
| Address (mailbox, kind, state, generation) | per address | `addresses` | ledger `addresses` |
| Address history `(mailbox, generation)` | per address, append-only | `address_history` | ledger `history` |
| Directory replay events | windowed by time (5 min, `syncauth.DefaultWindow`) | `directory_events` | lifecycle `events` |
| Logouts, spent sign-on `jti`s | windowed by retention | `logouts`, `sign_ons` | lifecycle |
| Release floors, recovery floors, repair barriers | per subject, present only around a restore | `release_floors`, `recovery_floors`, `recovery_barriers` | lifecycle |
| Recovery repair, challenge, receipt; restore release | install-wide singletons | `lifecycle_records(name, json)` | lifecycle |
| Domain set, relay, settings, users, restore hold | install-wide | unchanged files | unchanged |

Install-wide state that stays small: format and initialization (`meta`), the domain set
(O(domains)), the relay, the settings and the restore hold. The domain set stays a file
because its lock is the outermost fence and its size does not depend on accounts.

### Schema (SQLite, `PRAGMA user_version = 3`)

```sql
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
-- format='3', initialized='0'|'1', change=<int, +1 per mailbox/address commit>
CREATE TABLE directory(
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision >= 0), digest TEXT NOT NULL,
  active INTEGER NOT NULL CHECK(active IN (0,1)), event_id TEXT NOT NULL,
  revoked_before INTEGER NOT NULL DEFAULT 0,
  resource TEXT,            -- the retained DirectoryUser JSON, NULL for legacy rows
  PRIMARY KEY(issuer, subject)) STRICT, WITHOUT ROWID;
CREATE TABLE directory_primary_emails(   -- every emails[] entry with primary=true
  fold TEXT NOT NULL, issuer TEXT NOT NULL, subject TEXT NOT NULL,  -- fold = Go caseFold(TrimSpace(value))
  PRIMARY KEY(fold, issuer, subject)) STRICT, WITHOUT ROWID;
CREATE TABLE accounts(
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  revision INTEGER NOT NULL, digest TEXT NOT NULL, desired_active INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending','applied','failed')), failure TEXT NOT NULL DEFAULT '',
  primary_mailbox TEXT NOT NULL UNIQUE,                 -- local ID uniqueness
  legacy_mixed_use INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(issuer, subject)) STRICT, WITHOUT ROWID;
CREATE TABLE mailboxes(
  id TEXT PRIMARY KEY,                                  -- primaries = user ID; extras mbx-<uuid>
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('primary','extra')),
  state TEXT NOT NULL CHECK(state IN ('active','disabled')),
  state_root TEXT NOT NULL, message_bytes INTEGER NOT NULL, payload_bytes INTEGER NOT NULL,
  records INTEGER NOT NULL, source TEXT NOT NULL DEFAULT '',
  FOREIGN KEY(issuer, subject) REFERENCES accounts DEFERRABLE INITIALLY DEFERRED) STRICT;
CREATE INDEX mailboxes_owner ON mailboxes(issuer, subject);
CREATE TABLE addresses(
  address TEXT PRIMARY KEY CHECK(address = lower(address)),   -- global uniqueness
  mailbox TEXT NOT NULL REFERENCES mailboxes(id),
  kind TEXT NOT NULL CHECK(kind IN ('primary','alias')),
  state TEXT NOT NULL CHECK(state IN ('active','disabled','reserved') AND (kind = 'alias' OR state <> 'reserved')),
  generation INTEGER NOT NULL CHECK(generation >= 1)) STRICT, WITHOUT ROWID;
CREATE UNIQUE INDEX addresses_one_primary ON addresses(mailbox) WHERE kind = 'primary';
CREATE INDEX addresses_mailbox ON addresses(mailbox);
CREATE TABLE address_history(
  address TEXT NOT NULL REFERENCES addresses, generation INTEGER NOT NULL CHECK(generation >= 1),
  mailbox TEXT NOT NULL, PRIMARY KEY(address, generation)) STRICT, WITHOUT ROWID;
CREATE TABLE routes_pending(address TEXT PRIMARY KEY, generation INTEGER NOT NULL) STRICT;
-- directory_events, logouts, sign_ons (key, value, expiry index), release_floors,
-- recovery_floors, recovery_barriers (key -> existing JSON type), lifecycle_records.
```

The triggers record the never-delete and never-decrease rules, which the code enforces
today. `BEFORE DELETE` on `accounts`, `mailboxes`, `addresses` and `address_history`
raises an error. `BEFORE UPDATE OF generation ON addresses WHEN NEW.generation <
OLD.generation` raises an error. `BEFORE INSERT ON address_history` requires a
generation above the address's current maximum. `BEFORE UPDATE ON address_history`
always raises, because history rows are only appended and never changed: route
quarantine trusts (address, generation) → mailbox. The identity columns the code treats
as immutable are guarded by `BEFORE UPDATE` triggers that raise when the value changes:
- `mailboxes`: `issuer`, `subject`, `kind`, `state_root`, and `source` once non-empty;
- `accounts`: `issuer`, `subject`, `primary_mailbox`.

The columns that legitimately change stay writable: limits, mailbox and address
`state`, `addresses.mailbox` with a raised generation, and account status and
revision. PR 3 checks this list against every current writer. `accounts.primary_mailbox` must name a
`kind='primary'` mailbox with the same owner (a trigger, since the foreign key is
deferred). The per-record validity rules that today run inside
`validateNativeLedger` (`validHistory`, primary-address presence for extras) are
checked by the writer on the rows it touches, before commit.

Triggers only bind writes that fire them. SQLite's REPLACE conflict resolution
(`INSERT OR REPLACE`, `REPLACE INTO`, `ON CONFLICT REPLACE`) deletes the conflicting row
without firing delete triggers unless `recursive_triggers` is on, then inserts a new
row, so no update trigger fires either. Therefore:
- Writers never use REPLACE. They use plain `UPDATE`, or `INSERT ... ON CONFLICT DO
  UPDATE`, which fires the update triggers.
- Every handle sets `PRAGMA recursive_triggers=ON`, so a REPLACE that slips through
  hits `BEFORE DELETE` and aborts.
- The per-open checks (section 4) verify it alongside `trusted_schema=OFF`.

### Cost per operation, with numbers

Assumed row sizes, to be checked in PR 3: directory about 0.8 KB (resource JSON),
account about 0.25 KB, mailbox about 0.3 KB, address about 0.1 KB. With 4 KiB pages
and text keys under 100 B, every table at 10,000 rows has a B-tree of depth 3 or less.
That means at most 4 page visits per lookup.

| | 3,000 accounts | 10,000 accounts |
|---|---|---|
| Today, bytes read per admission | ≈ 3 MB read and byte-compared (2.9 MB ledger plus lifecycle; the brief's 9 GB / 3,000) | ≈ 10 MB ledger plus ≈ 8 MB lifecycle (estimated), above the view limits, so full JSON decode and full ledger validation every time |
| Today, per full scan (one admission per account) | ≈ 9 GB | ≈ 180 GB plus 10,000 full decodes |
| v3, lookups per admission | 6 (meta, directory, account, mailbox, primary-address index, address row); extras add 2 | same |
| v3, pages visited per admission | ≤ 24 (≤ 96 KiB of page cache), about 2 KB of row data decoded | ≤ 24; one more B-tree level would appear only above roughly 250,000 rows |
| v3, per full scan | ≤ 0.3 GB of page-cache visits, mostly the same interior pages | ≤ 1 GB, about 20 MB decoded |

The work per lookup is O(log N) with depth ≤ 3 across the whole supported range,
which is constant in practice. The O(1) claim is checked by measurement: per-operation
admission CPU at 10,000 accounts must stay within 1.2× of the value at 1,000 accounts
(section 9).

## 2. The interface

```go
// authorityStore holds native authority: the directory lifecycle and the
// native ledger. Every call reads current committed state; nothing decoded
// survives a call. Write callers hold the directory fence.
type authorityStore interface {
	// View runs fn on one consistent read snapshot.
	View(ctx context.Context, fn func(authorityReader) error) error
	// Update runs fn in one atomic write; the writes become visible together or not at all.
	Update(ctx context.Context, fn func(authorityWriter) error) error
}

type authorityReader interface {
	Initialized() (bool, error)
	Directory(issuer, subject string) (DirectoryState, bool, error)
	Account(issuer, subject string) (nativeLedgerAccount, bool, error)
	Mailbox(id string) (nativeLedgerMailbox, bool, error)
	Address(address string) (nativeLedgerAddress, bool, error) // with history
	PrimaryAddress(mailboxID string) (string, bool, error)
	MailboxesOwnedBy(issuer, subject string) ([]string, error) // extras of one owner
	DirectoryPrimaryTaken(address string) (bool, error)         // alias rule
	Event(id string) (directoryEvent, bool, error)
	Lifecycle() (lifecycleRecords, error) // logouts, sign-ons, floors, recovery, release
	Snapshot() (nativeAssignments, map[string]DirectoryState, error) // cold paths only
	Change() (int64, error) // publication mark for the Cloudflare routing table
}

type authorityWriter interface {
	authorityReader
	SetInitialized() error
	PutDirectory(issuer, subject string, d DirectoryState) error
	PutAccount(issuer, subject string, a nativeLedgerAccount) error
	PutMailbox(id string, m nativeLedgerMailbox) error
	PutAddress(address string, x nativeLedgerAddress) error // appends new history only
	PutEvent(id string, e directoryEvent) error
	PutLifecycle(l lifecycleRecords) error // writes changed rows, prunes expired ones
	MarkRoutesPending(inactive map[string]int64) error
}
```

Every method exists because a current caller needs it. Nothing is added for an
imagined backend. `Snapshot` returns the current in-memory types, so the cold paths
(recovery digest, restore validation, workers, admin lists) keep their algorithms
unchanged.

| Backend | `View` | `Update` | Lock |
|---|---|---|---|
| JSON (PR 1, existing files) | read `sso-lifecycle.json` and `native-provisioning.json` once each and serve from them; O(N), same as today | load both, apply the changes, persist in today's order: initialization flag first, then the ledger, then the lifecycle | existing flock |
| SQLite v3 (PR 3) | `BEGIN DEFERRED` … `COMMIT` on a pooled handle; `os.SameFile` identity check after the queries (section 4) | `BEGIN IMMEDIATE` … `COMMIT`, `synchronous=FULL`, `foreign_keys=ON` | existing flock, plus SQLite's own write lock |
| Postgres (later, not built) | `BEGIN READ ONLY ISOLATION LEVEL REPEATABLE READ` | `BEGIN ISOLATION LEVEL SERIALIZABLE` with retry on 40001 | flock while `users.json` is a file; `pg_advisory_xact_lock` once users move too |

Postgres tables match the SQLite schema, with `TEXT`/`BIGINT`/`BOOLEAN`, the same
`CHECK`, `UNIQUE` and partial unique index, and append-only rules enforced by `GRANT`
(no `DELETE`) plus triggers. Row-level security is covered in section 8.

## 3. Consistency and atomicity

| Invariant | Today | v3 |
|---|---|---|
| Writers serialized | directory flock | the same flock, plus a single SQLite writer (`BEGIN IMMEDIATE`) |
| Initialization before any ledger | `persistNative` writes the lifecycle flag, then the ledger; `nativeUserOwnership` depends on this order | the flag and the first ledger rows commit in **one** transaction, so the order dependency goes away. Readers read both in one `View`. |
| Global address uniqueness | `map` key, re-validated on every read | `addresses.address PRIMARY KEY`; a conflicting `INSERT` fails the transaction |
| Local ID / mailbox ID uniqueness, extras disjoint from primaries | loops over `f.Accounts`, re-validated on read | `mailboxes.id PRIMARY KEY` (one namespace for both kinds), `accounts.primary_mailbox UNIQUE` |
| A primary is never an alias or reserved address of anyone | `f.stored.Addresses` lookup | the same, by `Address(address)` |
| An alias never takes a directory primary | scan of every resource's `primary` emails with `strings.EqualFold` | `DirectoryPrimaryTaken`: lookup in `directory_primary_emails` by a Go-computed Unicode simple-fold key, so it matches exactly what `EqualFold` matched (finding H2) |
| Generations only increase and are never reused; history is append-only | Go code (`syncAddressStates`, `validHistory`) | the same Go code, plus triggers: no delete on any authority table, no generation decrease on `addresses`, no update at all on `address_history`, immutable identity columns (section 1); `PutAddress` writes only the new history rows |
| One primary address per mailbox | `addressOf` check | partial unique index |

**Directory event (`applyDirectory`).** Today this is three separate writes:
`users.json` (apply), the ledger (`syncNativeDirectory`), then the lifecycle. In v3,
`apply()` still writes `users.json` first. Then **one** `Update` commits the directory
row, the event record, the cleared recovery records and the ledger's desired-state
changes. Then routes are deactivated. The ledger-written-but-revision-unrecorded
window goes away. A crash after `users.json` is the same as today: the event is not
recorded, so the sender retries and `apply` is idempotent.

**Desired-state rule.** Today every commit re-evaluates every address and rewrites
every non-active route. That is O(N) per write, which makes a 10,000-account bulk
provisioning O(N²). In v3, a commit evaluates the touched owner's addresses, plus any
address in `routes_pending` (failed route deactivations). The minute pass
(`ReconcileNativeAddresses`) and domain-set changes still evaluate all addresses.
`ErrNativeRoutesPending` keeps its meaning: the ledger change is durable, and the
route is retried on the next commit and by the minute pass.

**Reconcile and allocation.** The order is unchanged: a durable `pending` reservation
is committed, then storage is prepared, then `applied` is committed. Each commit is
one transaction.

**Crash windows and what readers see.**

| Crash point | State on disk | Readers |
|---|---|---|
| Inside an `Update` | WAL rollback when the next connection opens | last committed state, never a partial one |
| After `Update`, before route deactivation | ledger changed, route active | import quarantines on the ledger generation, as today; `routes_pending` drives the retry |
| After `users.json`, before `Update` (directory) | user changed, revision unrecorded | admission compares the user role with the directory role and refuses on mismatch, as today; the retry converges |
| Reserve committed, storage half-prepared | `pending` | refused (`status` is not `applied`, no source); retry reuses the ID |

## 4. Freshness

- Every admission, ownership check, outbound claim and receiving bind runs its own
  `View`. Every value it uses comes from rows committed before that transaction
  started. No decoded value, view or decision lives longer than the call. PR 3
  deletes `native_admission_view.go` and `nativeAssignmentsAlreadyDecoded`.
- SQLite read transactions in WAL mode check the WAL index header under the shared
  lock when they start. The page cache is invalidated by the database's own change
  counter, not by mtime or size, so this is not stat-only freshness. Owner
  confirmation is requested in Q1.
- Replacing the file behind a held handle is detected with the #571 rule
  (`mailbox.Prepared.check`). The store records the database file's `Lstat` when it
  opens. After each `View`'s queries it requires `os.SameFile(Lstat(path), recorded)`.
  On a mismatch it refuses the operation, drops the handle and reopens on the next
  call. Supported paths never replace the file while services run: restore and
  migration are offline. This check covers unsupported paths too.
- Fail closed: any open, query, scan, schema or decode error, `meta.format != 3`, a
  `user_version` mismatch, a missing required table or a row that breaks a Go-side
  rule gives `ErrNativeProvisioning`, or `ErrNativeMigration` for format errors. Older
  contents are never used. On every handle open (runtime, migration and the
  historical restore opener) the store opens with `PRAGMA trusted_schema=OFF`, `PRAGMA recursive_triggers=ON` and
  defensive mode on. It requires `user_version` 3 and an **exact** match between
  `sqlite_schema` and the built-in schema: every table, index and trigger,
  compared after whitespace normalisation, with no extra objects. Any difference
  refuses with `ErrNativeMigration`. This matters because the constraints and
  triggers carry the global invariants, and `Snapshot` loads into maps that would
  silently collapse duplicate rows. `meta.format` is read in every `View`.
- Changed scope of validation: today every read validates the **whole** ledger, so
  damage to an unrelated record refuses everyone. In v3, the constraints and triggers
  make the global invariants structurally impossible to break through SQL writes
  (given the exact-schema check above),
  and each read validates the rows it uses. A whole-store validation
  (`validateNativeLedger` over `Snapshot`, plus `PRAGMA quick_check`) runs at
  migration, at startup (`migrate-native`), in every backup, and in the minute
  reconcile pass. That pass logs and refuses its own writes on failure. See finding H3 and Q5.

## 5. Migration (v2 → v3)

The format is decided by three files: L = `sso-lifecycle.json`, P =
`native-provisioning.json`, D = `native-authority.db`.

| L | P | D | Meaning | Runtime |
|---|---|---|---|---|
| absent or v2 JSON | absent, v1 or v2 | absent | not migrated | refuse (`ErrNativeMigration`) once the binary is v3, except a fresh install (all absent) |
| stub | stub v3 | present | v3 | serve |
| absent | absent | absent | fresh install | empty; the first write creates D, then L, then P under the flock |
| absent | absent | present | fresh-create crash | the writer under the flock finishes the stubs |
| any other mix (v2 L with D, stub L with v2 P, stub P without D, …) | | | half-migrated or damaged | refuse (`ErrNativeMigration`); `migrate-native` converges or reports |

The stubs make older binaries fail closed:

- P stub: `{"version":3,"authority":"native-authority.db"}`. A v2 binary's
  `currentNativeFormat` refuses `version != 2`, so domain setup, allocation,
  admission, receiving and sending refuse.
- L stub: `{"movedTo":"native-authority.db","directory":"native-authority.db","logouts":"moved"}`.
  A v2 binary's `LifecycleStore.load` fails to unmarshal a string into a map, so
  logins, logouts, directory events and every lifecycle write refuse. The stub is
  never rewritten, so the lock path `sso-lifecycle.json.lock` stays stable.

`kypost-server migrate-native` (entrypoint, runtime user, after the v1 → v2 step and
before `MigrateNativeLimits`) runs `MigrateAuthority` under domains → directory →
users locks:

1. **Preserve.** Copy L and P to `*.v2-migrated` while each still holds v2 data.
   Re-copy them on every run until a source is a stub. After that, keep the copy. Like
   the v1 rule, a missing source with a leftover copy refuses.
2. **Validate the copies before any output.** Run the full lifecycle decode, the full
   `parseNativeLedger` plus `validateNativeLedger`, `seedGenerations` for ledgers
   without the marker, and the initialization rule (a ledger behind an unset flag is
   refused, as for v1).
3. **Build D.** Write `native-authority.db.tmp` from scratch, removing any stale temp
   file first. Insert every row in one transaction, then run `PRAGMA integrity_check`,
   the whole-store validator and a **round-trip check**: `Snapshot` must equal the
   copies' decoded values, and the recovery digest must be identical. Then fsync,
   `rename` to D, and fsync the directory.
4. Write the L stub.
5. Write the P stub **last**.

Every output is derived from the copies and overwritten, so a crash at any step
converges on re-run. A v2 binary run between steps 1 and 3 rewrites v2 sources, which
are re-copied. After step 4 it cannot write (its `load` fails). Running the migration
twice is a no-op once P is a stub and D validates. A v1 install runs v1 → v2 → v3 in
one boot. Like v1 → v2, migration grants no authority and keeps a restore hold.

On failure: log the cause with a remediation and keep booting. Native and SSO paths
refuse, while password logins and external IMAP keep working. See Q2: this is wider
than v1 → v2, because L moves for SSO-only installs too.

**Rollback** keeps the existing policy: restore the backup taken before the upgrade.
The `*.v2-migrated` copies are also a valid v2 state *as of the migration*. An
operator may restore them by hand only if no v3 writes happened, because they lack
later reservations and generations and must never be used after v3 traffic. Whether
to build a reverse `migrate-native --to-v2` (from D) is Q4.

## 6. Backup, restore and recovery

| Area | Change |
|---|---|
| Collection (`backup/collect.go`) | Add `native-authority.db` to `snapshotDatabase`, so it is copied with `VACUUM INTO`. `-wal`/`-shm` and `*.v2-migrated` are already skipped (the skip list gains `.v2-migrated`). One consistent snapshot of lifecycle plus ledger replaces two separately read files, which removes a torn-pair window. The file stays far below `MaxCapsuleFileBytes` (64 MiB): an estimated 20 MB at 10,000 accounts. |
| Native detection (`validateNativePayload`) | Treats `native-authority.db` as native. Treats a P stub as native only when D's `meta.initialized` is set. The allowlist gains D. |
| Snapshot validator (`nativeSnapshot`, `ValidateNativeSnapshot`) | `loadNativeLedger(historical)` reads v1 or v2 files, or v3 through a **read-only** historical opener (`mode=ro`, `immutable=1` on staging, `Lstat` regular file, owner-only mode). Every ownership, address-history, route/binding, domain-set and release-floor check runs unchanged on `Snapshot`. A mixed format in a snapshot (the table in section 5) is refused. |
| Restore of an old-format (v1/v2) backup | Staged and qualified as today. The hold is persisted. The next boot's `migrate-native` converts while the hold stays. Nothing changes for operators. |
| Restore of a v3 backup on an old binary | Its validator rejects ledger `version` 3, so the restore fails closed and nothing is published. |
| Restore fencing (`FenceRestoredNativeTokens`, release floors, qualification) | Writes go through `Update` on the staging store (offline, exclusive). |
| Recovery inputs and digest (`nativeRecoveryInputs`, repair plan/intent/complete) | Built from one `Snapshot`, now one consistent read of lifecycle plus ledger. The digest bytes must not change across migration (round-trip test), so outstanding challenges stay valid. |
| Hold release (`PlanNativeRestoreReleaseHeld`, `Commit`) | Whole-snapshot reads and one `Update` per step. Floors become rows. |
| Offboarding | Unchanged semantics: directory deactivation plus the desired-state rule. Rows are never deleted (triggers), reservations are kept and addresses are not freed. |

## 7. Other readers and writers

All file access to L and P happens in `internal/sso`. Outside that package, two places
touch the files: `backup/native.go` (payload detection and the allowlist) and
`app/receiving_cfcontinuous.go` (`cfChangeMark` uses the ledger mtime and inode as a
republish trigger).

**Hot path. These become point reads (a single `View`):**

| Reader (callers) | Today reads | v3 |
|---|---|---|
| `AdmitOpenMailbox`/`AdmitUserMailbox` (api `withMailbox`/`admitMailbox`, processor ticks, held clients) | lifecycle whole, ledger whole (twice for extras), domains | `Directory`, `Initialized`, `Account`, `Mailbox`, `PrimaryAddress`; domains file |
| `ValidateNativeUserStorage`/`Ownership` (api `server_userscope.go` state open and cache hit, `processor/poller.go:263`, outbound, recovery repair) | ledger whole, lifecycle whole | `Initialized`, `Account`, `Mailbox`, `PrimaryAddress` |
| `WithNativeMailAccess` (receiving import, quarantine release) | ledger whole, plus k admissions | one `View`, k × (`Mailbox`, `Account`, `Directory`, `PrimaryAddress`) |
| `NativeOutbound.withAuthority` (Queue/Send/Submit/Status/Pending/FileSent/Quarantine/Recover) | `NativeMailboxAssignment`, admission, `NativeAddresses`, `Directory`, all whole | admission plus `Address(from)`, `Directory` |
| `NativeAssignmentForAddress` (`app/receiving.go`, `receiving_cloudflare.go`, per RCPT) | ledger whole | `Address`, `Mailbox`, `Account` |
| `NativeAddresses()` in `app/receiving.go` `activeAddress`, `api/native_outbound.go` `nativeFrom`, `api/scheduled_send.go`, `receiving_cfcontinuous.go:392` | ledger whole | new `NativeAddress(address)` point method; the callers change by one line each |
| `NativeMailboxAssignment` (receiving, quarantine, outbound) | ledger whole | `Mailbox`, `Account`, `PrimaryAddress` |
| `cfChangeMark` | ledger `stat` | `Change()` (`meta.change`, +1 per mailbox/address commit), plus the blocks file `stat` as today |
| `Directory()` (`api/native_signon.go`, `sso_handlers.go`, provisioning worker) | lifecycle whole | `Directory` row |
| `LoggedOut`, `RecordLogout`, `RecordSignOnJTI` (login, logout) | lifecycle whole | `logouts` scan (bounded by retention), keyed `sign_ons` |
| api `extraMailboxOwner` (`withMailbox` with `X-KyPost-Mailbox`, every extra-mailbox request), `handleMailboxes` (`GET /api/mailboxes`), `extraMailboxIDs` (contacts), `ownsActiveExtraMailbox` (PGP incoming) | `NativeMailboxes()` whole, per request | `Mailbox(id)` / `MailboxesOwnedBy` (finding H1) |

**Discovery passes** (`processor/poller.go` `activeExtraMailboxes` once per tick
round, `app/native_outbound.go` outbox discovery every 30 s) read
`NativeMailboxesWithoutAddresses`: the `Mailboxes` reader, one query over the
mailbox rows joined to their accounts, without addresses or whole-ledger
validation. It is discovery, never admission: every send, recovery and poll still
admits afresh. `BenchmarkNativeOutboxDiscovery` measures it at 1k/3k/10k.

**Cold paths. They keep whole reads through `Snapshot`, at most once per pass:**
`NativeMailboxes` (admin lists; linear in mailboxes plus addresses),
`NativeDirectoryRepairSubjects` and `ReconcileNativeAddresses` (minute), the
`NativeAddresses` admin handler, `NativeQueuedFromDomains`, `RetireDomain`, recovery
and release planning, `ValidateNativeSnapshot`, `NativeRestoreMailboxes`, migration
and `MigrateNativeLimits`.

**Writers (all under the directory flock, each becoming one `Update`):**
`applyDirectory`/`syncNativeDirectory`, `reconcileNativeMailboxLocked`,
`AllocateNativeAccount`, `CreateNativeMailbox`/`SetNativeMailboxState`,
`AddNativeAlias`/`ReleaseNativeAlias`/`ReassignNativeAddress` (`changeNativeAddress`),
`ReconcileNativeAddresses`, `RetireDomain`/`ConfigureDomain` (desired-state pass),
`ensureNativeLedger`, `RecordLogout`, `RecordSignOnJTI`, `RecordNativeReleaseFloors`,
`CompleteNativeRestoreRelease`, `NativeRestoreReleasePlan.RecordIntent`/`Commit`,
`Begin`/`AcceptNativeRecoveryHeld`, the `NativeRecoveryRepair*` intent/complete
writers, `FenceRestoredNativeTokens`, `RecordNativeRestoreQualification`,
`MigrateNative`/`MigrateNativeLimits` and `WriteNativeV1ForTest` (tests only).

Short-lived processes (`kypost-server receiving bind|accept`, run per transaction by
the receiver) open the database per invocation: one open, one schema check and point
reads. That is O(1); it replaces today's two whole-file reads per invocation.

The relay key and version check (`mailmsg.ReadDomainRelay`, runtime v2 only) is
unchanged. The relay stays a sealed file, and its `Generation` is not authority
stored here.

## 8. Security

- **Isolation between accounts.** It is logical, within one process that is trusted
  with every account, as today. The hot paths use a fixed set of parameterized
  statements keyed by (issuer, subject), mailbox ID or address. SQL is never built
  from strings. Each `View` returns only the rows for the requested key.
  `ErrNativeMailboxUnknown` still covers unknown, foreign and disabled mailboxes alike,
  so row existence does not leak through different errors.
- **Files.** D, `-wal` and `-shm` are 0600 under the 0700 config root, owned by the
  runtime user. The store creates D with `O_CREAT|O_EXCL` and mode 0600 before SQLite
  opens it. SQLite gives `-wal`/`-shm` the database's mode. Opening `Lstat`s D and
  requires a regular file (not a symlink) with mode `&0077 == 0`. After each `View` it
  requires `os.SameFile` (section 4). The migration temp file is created the same way
  and renamed within the directory.
- **Paths.** v3 adds **no** per-account path. D has a fixed name, and account IDs never
  become path components of authority storage. That removes the traversal surface a
  per-account-file design would have. Mailbox storage paths keep
  `fsutil.SafePathComponent`.
- **Integrity.** Constraints and triggers enforce uniqueness, the never-delete and
  never-decrease rules, append-only history (`address_history` refuses every update)
  and the immutable identity columns listed in section 1, even if Go code is wrong. That holds because writers never use
  REPLACE and every handle runs with `recursive_triggers=ON` (section 1). `quick_check` and whole-store
  validation run at startup, at migration and in backups. A process able to write the
  config root could rewrite D. That is also true of today's JSON files and is not a
  new boundary.
- **Postgres (later).** One database per install. Separate roles: `kypost_runtime`
  (SELECT, INSERT, UPDATE; no DELETE on authority tables, no DDL) and
  `kypost_migrate` (DDL, used only by `migrate-native`). TLS with `verify-full`. The
  credential lives in `SECRET_DIR` at 0600, not in environment or config. Per-account
  RLS (`USING (issuer = current_setting('kypost.issuer') AND subject =
  current_setting('kypost.subject'))` with `SET LOCAL` in hot `View`s) is defence in
  depth only. Writers and cold paths need every account, and the same process holds
  both roles. If several installs ever share a cluster, install-level RLS (or
  separate databases) is the real boundary. Backups would need a `pg_dump`-style
  consistent export; that is a design item for that phase.

## 9. Rollout plan

| PR | Content | Safe because | Tests | Size (estimate) |
|---|---|---|---|---|
| 1. Seam | `authorityStore` interface and **JSON backend**. Every `LifecycleStore` method listed in section 7 is rewritten on `View`/`Update` with today's write order. New `NativeAddress(address)` and `NativeMailboxesOf(userID)` point methods for the hot callers in section 7 (receiving, send `From`, scheduled send, `withMailbox`, `GET /api/mailboxes`, contacts, PGP incoming). Test fixture helpers replace the 37 direct file references in 15 test files. | No format change, the same bytes on disk, the same complexity | the whole existing `sso`/`api`/`app`/`backup`/`processor` suite under race; a golden test that the JSON backend writes identical files | +900 / −500 (sso, plus about 250 lines of test fixtures) |
| 2. v3 reader for backups | Historical read-only v3 opener, schema, the pure converter `v2 → rows` (used by tests and PR 3), snapshot validator/collector/detection support for D and the stubs | No binary writes v3 yet, so v3 exists only in fixtures | v3 fixture backups validate; mixed formats refuse; capsule round-trip; digest equal across conversion | +600 |
| 3. SQLite runtime and migration (**the gain**) | SQLite backend, `MigrateAuthority` in `migrate-native`, the format table and stubs, deletion of `native_admission_view.go`, per-touched-owner desired-state rule plus `routes_pending`, `cfChangeMark` → `Change()`, docs (SECURITY.md lines on bounded views, backend/AGENTS.md, NATIVE_ADDRESSING_V2/PROVISIONING, RESTORE.md) | Older binaries fail closed on the stubs; migration is crash-convergent (step-kill tests as for v1); backups already understand v3 (PR 2) | migration kill at each step and re-run; double run; v2 binary against v3 files (fixture) refuses; `BenchmarkAdmitOpenMailbox` at 1k/3k/10k (ns/op and B/op within 1.2×); every existing native test on the SQLite backend | +1,100 / −500 |
| 4. Remove the JSON runtime backend | Only the historical v1/v2 readers stay, for restore and migration | runtime has been v3 since PR 3 | existing suite | −400 |

**Measurement of PR 3** uses the busnes.app `performance/` matched-pair harness on
K80–K82. A = `main` at PR 3's merge-base image, B = PR 3 image, the same 3,000-account
/ 3-client workload, alternating A/B pairs on the same hosts. Then a 10,000-account
run of B only (A is expected to be unusable there). Acceptance:

- admission frames (`AdmitOpenMailbox` and below, plus `nativeUserOwnership`) are below
  5% of daemon CPU in B's profile;
- daemon CPU per completed client operation in B is lower than in A, with a
  matched-pair confidence interval that excludes zero;
- B's per-operation admission CPU at 10,000 accounts is within 1.2× of B at 3,000.

Total daemon cores are reported, not promised. Other costs (#571 residue, `users.json`)
may now dominate.

## 10. Rejected alternatives

| Alternative | Why rejected |
|---|---|
| One JSON file per account (plus index files) | 10,000+ files exceed the capsule `MaxFiles` 4,096. Reassignment and uniqueness span accounts and need a hand-written journal. The address index is itself install-wide and O(N) to rewrite. Account IDs become path components. Thousands of fsyncs and inodes. |
| Hash-sharded JSON (for example 256 shards) | Still O(N/shards) per operation, not O(1). Multi-shard writes need a journal. Backups still have many files. |
| Bigger decoded-view cache (raise 4,096 / 8 MiB) | Still reads and compares O(N) bytes per operation, so a scan stays O(N²). Memory grows with N. |
| Admission-decision cache, TTL, stat-only freshness, batching | Ruled out by the owner on 2026-11-10. |
| Append-only log with an in-memory index | Freshness would mean a tail-offset cache (a stat-like rule), plus compaction and crash repair. That is a hand-built database. |
| bbolt or another embedded KV | A new dependency. SQLite (`modernc.org/sqlite`) is already used, snapshotted by backups (`VACUUM INTO`) and maps directly to Postgres tables. |
| Move only `directory` out of the lifecycle file | Splits `applyDirectory`'s single write (events, recovery clears) across two stores and adds crash-ordering windows (stale recovery challenge, lost event dedupe). Moving the whole lifecycle keeps it one transaction. |
| Postgres now | Ruled out by the owner. Adds operations burden and a second backup path. |
| Per-account flock instead of the install-wide directory lock | No contention has been measured. With O(1) reads the lock is held for microseconds. It would change the fence semantics that `users.json` writes depend on. Revisit only with a profile. |
| Fresh SQLite connection per operation | O(1), but it pays schema parse and WAL setup each time, and the last-close checkpoint churns. The held handle with the `os.SameFile` check is the #571 precedent. Kept as a Q1 fallback. |

## Owner decisions (2026-11-10)

The owner accepted every recommended answer below and approved starting PR 1:

- **Q1:** (a) a held handle, with SQLite's change-counter protocol plus the `os.SameFile` check.
- **Q2:** (a) every SSO install migrates.
- **Q3:** (a) leave `users.json` as is and measure after PR 3.
- **Q4:** (a) the only rollback is restoring the pre-upgrade backup.
- **Q5:** (a) on whole-store validation failure, log, alert, and refuse that pass's writes only.

## Open questions for the owner (answered above)

- **Q1. Is the SQLite page cache "fresh"?** Options: (a) a held handle; each
  transaction sees the latest commit through SQLite's change-counter protocol, plus the
  `os.SameFile` check (recommended); (b) open a new connection per operation (also
  O(1), costs some CPU per operation; measure in PR 3); (c) (a) with
  `PRAGMA cache_size=0`-style limits (little benefit).
- **Q2. Scope of the lifecycle move.** (a) Every SSO install migrates; a failed
  migration then also refuses SSO logins until fixed, while password logins keep
  working (recommended: one code path). (b) Only installs with native initialized
  move; SSO-only installs keep the JSON backend for good (two live backends).
- **Q3. `users.json`.** It is install-wide, O(N), and is re-read after every write
  using size+mtime caching. That is an existing stat-based freshness rule inside
  admission. Options: (a) leave it and measure after PR 3 (recommended); (b) a follow-up
  design that moves users into the same store; (c) accept as is permanently.
- **Q4. Reverse migration.** (a) Keep "restore the pre-upgrade backup" as the only
  rollback (recommended, matches v1 → v2); (b) add `migrate-native --to-v2`, which
  writes v2 files from D, for a supported binary rollback after v3 traffic.
- **Q5. Whole-store validation failure in the minute pass.** (a) Log, alert, and refuse
  that pass's writes only (recommended); (b) also refuse all admission until repaired,
  which matches today's global fail-closed behaviour at the cost of one bad row
  blocking everyone.

## Review findings and answers

The hostile review and a design-scope security audit were run on this document and
checked against the code. The audit found no exploitable findings. These findings
survived and are answered above, or here:

| # | Finding (verified) | Answer |
|---|---|---|
| H1 | The first draft's hot-path list missed per-request whole-ledger reads in the API: `extraMailboxOwner` (every `X-KyPost-Mailbox` request through `withMailbox`), `handleMailboxes` (`GET /api/mailboxes`), `extraMailboxIDs` (contacts) and `ownsActiveExtraMailbox` (PGP incoming) all call `NativeMailboxes()`. Without fixing them, the O(N) cost per request stays. | Added to section 7. `MailboxesOwnedBy` was added to the reader, backed by the `mailboxes_owner` index. PR 1 converts these callers. |
| H2 | The draft had one `primary_email` column with SQL `lower()`. Today's alias rule (`everydayOwner`) refuses an alias equal to **any** `primary: true` entry, after `TrimSpace`, with Unicode `EqualFold`. One column loses resources with several primary entries, and `lower()` is ASCII-only. | Replaced by `directory_primary_emails` (every primary entry) keyed by a Go-computed simple-fold key. A parity test runs both rules over the same fixtures. |
| H3 | Validation scope moves from "whole ledger on every read" to "rows used plus constraints". Damage to a record no longer refuses unrelated accounts, and corruption that constraints cannot express (`validHistory` order, extra-mailbox primary presence) is caught only by the periodic whole-store validation. | Accepted as a deliberate change; it isolates one account's damage from the others. Triggers express the history rules. Whole-store validation runs at startup, migration, backup and the minute pass. What a failure there refuses is Q5. |
| H4 | Admission still takes the install-wide exclusive directory flock and validates prepared storage (`mailbox.Prepared` queries) while holding it. Removing the O(N) reads makes CPU O(1), but every operation in every process still serializes on one lock. | Not changed here; the fence semantics depend on it. PR 3 measures lock wait (time in `LockFileContext`) in the matched pair. A narrower lock is a separate design, needed only if wait becomes significant. |
| H5 | The test suite writes the two files directly (37 references in 15 test files). The draft's PR size ignored this churn. | PR 1 introduces fixture helpers on the interface. Size estimates were raised. |
| H6 | `cfChangeMark` uses the ledger file's mtime and inode to republish the Cloudflare routing table. A stub that never changes would silently stop republication. | `meta.change` counter, read by `Change()` (section 7). PR 3 test: an address change republishes within one tick. |
| H7 | Short-lived `receiving` processes open the database on every invocation. | O(1) per invocation (section 7). The WAL is shared with the long-running api and daemon handles. |
| S1 | `*.v2-migrated` copies keep stale authority and directory PII indefinitely. Adopting them by hand after v3 traffic could reuse generations. | Section 5 warns against this. PR 3 has `migrate-native` refuse a v2 adoption while D exists, and RESTORE.md tells operators to delete the copies after the first successful v3 backup. |
| S2 | A staged v3 database is trusted row by row during restore validation. | The historical opener runs `PRAGMA integrity_check` before reading rows (PR 2). |
| S5 | PR #574 security re-review: `address_history` was guarded against DELETE and INSERT but not UPDATE. Since writers use plain `UPDATE`, a bug could rewrite a history row's mailbox or generation unnoticed until the minute pass, and route quarantine trusts that history. | `BEFORE UPDATE ON address_history` always raises. Immutable identity columns on `mailboxes` and `accounts` get value-change triggers, and all of them join the exact-schema check (section 1). PR 3 extends `TestAuthorityReplaceCannotBypassTriggers`: plain `UPDATE`s of a history row's `mailbox` and `generation`, and of each immutable column, fail and leave the row unchanged. It also extends `TestAuthoritySchemaMismatchRefuses`: a fixture missing any of these triggers is refused. |
| S4 | PR #574 security re-review: REPLACE conflict resolution deletes and re-inserts a row without firing delete triggers (unless `recursive_triggers` is on) or update triggers. A writer bug could then lower an address generation or rewrite an account unnoticed until the minute pass, and route quarantine relies on generations. | Writers never use REPLACE (plain `UPDATE` or `ON CONFLICT DO UPDATE`). Every handle sets and verifies `recursive_triggers=ON` (sections 1 and 4). PR 3 test `TestAuthorityReplaceCannotBypassTriggers`: a REPLACE on a store-opened handle against an existing `addresses` row with a lower generation, and against an `accounts` row, both fail and leave the rows unchanged. |
| S3 | PR #574 security review: the open-time check covered only `user_version` and table presence, so a database missing the address primary key, the one-primary index or a never-delete trigger would pass. Duplicates would then go unseen, because `Snapshot` collapses them into maps and `integrity_check` checks storage, not schema meaning. | Every open now requires an exact `sqlite_schema` match with the built-in schema, under `trusted_schema=OFF` and defensive mode (section 4). PR 2/3 test `TestAuthoritySchemaMismatchRefuses`: a fixture with the right tables and `user_version` but a dropped key, index or trigger is refused by both the runtime and historical openers. |
| S3 | Existing, outside this design: the `users.json` cache is keyed on size+mtime, not inode. | Recorded under Q3. Adding the inode to the key is a one-line follow-up, independent of this rollout. |

Claims checked and confirmed: every v2 `LifecycleStore.load` caller refuses on a
decode error, so the lifecycle stub makes older binaries fail closed. Older binaries
also refuse ledger `version` 3 in `currentNativeFormat`. The capsule cap is 4,096
files (`ky-primitives` `capsule.MaxFiles`).

Still unproven, owned by PR 3's measurements: the row sizes, the ~8 MB lifecycle
estimate at 10,000 accounts, the claim that page visits stay ≤ 24, and the total
daemon CPU after the change.

## 11. Where PR 3 departs from this design

PR 3 is implemented in `backend/internal/sso/authority_sqlite.go` (runtime
backend, format table) and `authority_migrate.go` (`MigrateAuthority`). It
departs from the sections above in these places:

| Area | Design | PR 3 | Why |
|---|---|---|---|
| Interface | Section 2's per-record writer (`PutDirectory`, `PutEvent`, `DirectoryPrimaryTaken`, `MailboxesOwnedBy`) | PR 1's interface kept (`Lifecycle`/`PutLifecycle` whole, `PutAccount` by key), plus `OwnerMailboxes`, `Mailboxes`, `Logouts`, `Change` and `Check` | Every caller keeps its algorithm; the reads on the hot paths (section 7) are point reads. `PutLifecycle` and the ledger puts write only the rows that differ from the stored ones. Ceiling: lifecycle writers (logout, sign-on, directory events) and ledger writers still read the whole lifecycle or ledger, O(N) per write; the alias rule still scans the directory in admin writes. |
| Desired-state rule | Evaluate the touched owner plus `routes_pending` | Evaluated in memory over the whole ledger the writer already read; only changed rows are written and only their routes (plus `routes_pending`) are deactivated | Restricting the evaluation saves nothing while writers read the whole ledger; the expensive parts (row writes, route writes) are bounded. Bulk provisioning at N accounts still reads O(N) per commit. |
| Route failure in a directory event | (section 3) one `Update`, then routes | The same; unlike the JSON backend, the event is recorded and the retry is answered `already_applied`, the route is retried from `routes_pending` | Follows section 3. |
| Directory sync while v1 data awaits migration | (not covered) | Refused with `ErrNativeMigration`, like every authority access before `migrate-native` | Owner decision Q2: the lifecycle moves too. |
| Fresh-create crash with a domain tombstone | Served | Refused (the tombstone requires the ledger stub) | A tombstone means native mail was configured, so a missing ledger stub is damage, not a first write. |
| Replaced file | `os.SameFile` after each `View` | Also checked when the handle is taken: a file replaced between calls is reopened (with every open-time check) before any query | Never queries a stale file. A replacement during a call still refuses that call. |
| Open-time checks | On every handle open | On every pooled connection (a `driver.Connector` wrapper) | A pool opens connections lazily. |
| Statements | Not specified | Each pooled connection prepares a query once and finalizes its statements when it closes (`stmtConn`) | Parsing was about a third of the authority reads. Only the parse is reused; each run reads in its own transaction. |
| Storage check without a client | Full reopening path | api and daemon: a pool of at most 1,024 mailboxes' `Prepared` handles, each check query prepared once per handle (`mailbox.HoldAdmissionHandles`), the same checks plus `os.SameFile`, closed on a refused check and after 5–10 minutes idle; other processes reopen | Reopening and re-parsing both schemas was most of an admission. The bound keeps memory near 470 MiB and 10,500 descriptors per process (about 0.46 MiB and 10 descriptors measured per held mailbox in a 3,000-account profile; at 4,096 the daemon filled a 2 GiB container); with more accounts than the bound, the share past it still reopens. |
| Restore staging writes | `Update` on the staging store | The same, offline: each write checkpoints and closes, leaving no WAL; the historical opener refuses a non-empty WAL, and a staging database with a WAL is read through the runtime-verified opener | `immutable=1` ignores the WAL, so fenced rows would be unseen. The runtime DSN sets `journal_mode=WAL`, which converts a restored `VACUUM INTO` copy on its first open. |
| `S1` (no v2 adoption while D exists) | Refuse | Refuse when the database differs from the conversion of the existing copies; a database equal to it (a crash after step 3) is rebuilt | Distinguishes a crash from a hand adoption without a new meta key. |
| Migration output | Rows of the converter | Plus a `routes_pending` row for every non-active address | v2 rewrote every inactive route on each commit; the first v3 write converges them once. |
| `meta.change` | +1 per mailbox/address commit | The same; account-only and lifecycle-only commits leave it | |
| Fixture | `WriteNativeAuthorityV3ForTest` (PR 2) | Removed; tests reach v3 through the runtime or `MigrateAuthority` | The runtime writes v3. |

Measured on one host (medians of 5 × 3 s, Go 1.27.4; `BenchmarkAdmitOpenMailbox`
is one admission among n accounts without a held client, so it reopens both
mailbox databases; `BenchmarkAdmissionAuthorityReads` is the authority reads
alone):

| | 1,000 | 3,000 | 10,000 | 10k / 3k |
|---|---|---|---|---|
| main 7ff87f1a, admission | 1.21 ms, 1.84 MB | 2.11 ms, 5.45 MB | 94.0 ms, 44.6 MB | 44.5×, 8.2× |
| PR 3, admission | 1.003 ms, 37.7 KB | 1.008 ms, 37.8 KB | 1.008 ms, 37.8 KB | 1.00×, 1.00× |
| main, authority reads | 0.45 ms, 1.82 MB | 1.21 ms, 5.45 MB | 92.7 ms, 44.7 MB | 76.8×, 8.2× |
| PR 3, authority reads | 127 µs, 13.2 KB | 128 µs, 13.2 KB | 128 µs, 13.2 KB | 1.00×, 1.00× |

About a third of the 127 µs was SQLite re-parsing each statement. Held
admission handles and per-connection statements (0c1a1b20, against main
9395bed3, the same host, medians of 5 × 3 s; flat across 1k/3k/10k):

| | 1,000 | 3,000 | 10,000 |
|---|---|---|---|
| main, admission | 967 µs, 36.6 KB | 970 µs, 36.7 KB | 969 µs, 36.8 KB |
| held handles, admission (api/daemon) | 320 µs, 30.1 KB | 320 µs, 30.1 KB | 321 µs, 30.1 KB |
| no pool, admission (other processes) | 901 µs, 37.3 KB | 898 µs, 37.4 KB | 897 µs, 37.4 KB |
| main, authority reads | 124 µs, 13.2 KB | 125 µs, 13.2 KB | 125 µs, 13.2 KB |
| prepared statements, authority reads | 74 µs, 13.3 KB | 73 µs, 13.3 KB | 74 µs, 13.3 KB |

In the held admission, re-preparing the mailbox check's own queries is now
about a fifth of its CPU (not cached there). Still unproven here: the busnes.app matched-pair
measurement (section 9), lock wait (H4) and the row-size and page-visit
estimates of section 1.
