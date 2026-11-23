# KyPost multi-host replicas for multi-tenant mode, design

Date: 2026-11-22. Status: proposed design. MH1 is built ("MH1 as built"); nothing uses it in production yet.
Revised 2026-11-22 with the owner's answers to every open question. Two overturn a
recommendation and are redesigned here: self-hosted single-tenant installs get multi-host as an
opt-in migration ("Self-hosted multi-host"), and multi mode runs in several regions
("Regions"). The rest are approved as recommended ("Open questions").

**Owner decision, 2026-11-22: multi-tenant needs multi-server.** A multi-tenant install
(`KYPOST_TENANCY=multi`) must run as several replicas on several hosts. Today R ("Replicas on one
host", #677) refuses a replica on another host. K's "Replicas" section called R's follow-up, which
moves accounts, per-user files and the other volume files into PostgreSQL, a launch prerequisite
outside its build plan. This note makes it a hard requirement of multi mode and specifies it.

Sources: KyPost's tenancy design (`docs/superpowers/specs/2026-11-20-tenancy-design.md`, **K**,
including every "PR N as built" section), the hosted receiving tier
(`docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`, **RX**), R as merged
(`docs/RUNNING.md` "Replicas on one host", `backend/internal/replica/AGENTS.md`), and the hosted
masked-address note (`docs/superpowers/specs/2026-11-19-masked-addresses-hosted-design.md`, **M**).

Line references are to KyPost `origin/main` at `3e467f2b`, re-checked at `3d0465a2` (RX PR 4) for the files it changed, and every reference re-checked at `fb0d6aab` (MH1 merged) for the 2026-11-22 revision. Paths are relative to
`backend/internal/` unless they start with `docs/` or a top-level folder. The references were
checked by reading the code at that commit. "Unverified" marks a claim this note did not check in
code or by running it. **Nothing here was run.** Every statement about behaviour comes from reading
code.

**Reading rule (2026-11-22 revision).** Where the sections below say a volume, lock, key or
host-check change happens "in multi mode", it happens in **table mode**: multi mode, or a single
install migrated by `migrate-volumes-to-postgres`. The exceptions are the inventory rows that are
absent only because of multi mode's own rules (K6, RX R1). "Self-hosted multi-host" lists their
targets in a migrated install. Org, region and receiving-tier rules stay multi-only.

## Summary

- **What binds replicas to one host is small and countable.** Everything already in PostgreSQL
  (every `.db` store, sessions, windows, replay guards, elections) or in process memory works with
  any number of hosts. Only four things depend on the host:
  1. the files on the three shared volumes;
  2. the `flock`s that fence them;
  3. the boot-ID and volume-ID checks that refuse a replica on another host;
  4. the keys in `SECRET_DIR`, which are created lazily, on whichever replica gets there first.
- **In table mode (multi mode, or a migrated single install), every volume file that a replica
  writes or must share moves into PostgreSQL.**
  Nothing is left for a second host to see or lock. Two new tables go in a new store, `volumes.db`
  (they started in `tenancy.db`; see "Self-hosted multi-host").
  `documents` holds the JSON and YAML stores keyed by their root-relative path, so each store
  keeps its own format and rules. `accounts` holds `users.json` as one row per account. No object
  storage and no new dependency are added.
- **Locks give liveness; revisions give safety.** Every document write is a compare-and-swap on
  the document's revision inside one transaction, so a lost lock can cause a retry but never a lost
  update. The cross-store fences (the domain fence, the directory fence, the settings lock and the
  per-mailbox quota lock) become PostgreSQL advisory locks with R's ownership probe.
- **Keys are provisioned, not created.** In table mode every host mounts the same read-only
  `SECRET_DIR` from the orchestrator's secret store. A missing key refuses the start; it is never
  minted. A keyed digest of the key set in `meta` refuses a host with other keys.
- **The host checks change in table mode only.** The boot-ID check (`replica.ErrHost`) and the
  volume-ID files give way to one assertion: no store a replica writes is file-backed. Single-tenant
  installs that have not migrated, SQLite and R's single-host replicas, keep every check, file and
  lock byte for byte.
- **Self-hosted installs can opt in (owner, 2026-11-22).** A single-tenant R install moves its
  volume files into PostgreSQL only when an operator runs `migrate-volumes-to-postgres`, and
  `migrate-volumes-to-files` moves it back. Until then it upgrades byte for byte and keeps its
  files, `flock`s and host checks. The two tables move out of `tenancy.db` into a new store,
  `volumes.db`, so that `tenancy.db` still means multi mode (MH8).
- **Multi mode runs in several regions as cells (owner, 2026-11-22).** Each region is a complete
  multi install with its own PostgreSQL, and each org lives in one home region. Every fence, CAS,
  election and session stays inside one region, so the single-database design above holds per
  cell. Only routing crosses regions (MH9, MH10).
- **Every hop is encrypted** in multi mode and in a migrated install (OQ4 as answered; "Encryption
  in transit").
- **K PR 7 must not accept `multi` until multi-host works.** This note's PRs MH1–MH7 and MH9 come
  before it, as do RX PRs 1–5 (per-org staff step-up, the other new gate, merged in #703). K's
  "Phased build plan" holds the full gate.

## What binds a replica to one host today

### The checks that refuse another host

| Check | Where | What it refuses | In multi mode it becomes |
|---|---|---|---|
| Boot ID | `replica/topology.go:24-26` (`BootIDPath`), `:31-32` (`ErrHost`), `:79-81` (the comparison against every live instance); read at `app/replica.go:34` | A replica whose kernel differs from a live replica's kernel | **Removed in multi mode only** (MH7). It is replaced by the volume-free assertion (below). Single mode keeps it unchanged. |
| Volume ID | `replica/topology.go:17-19` (`replica-volume.id`), `:96-153` (`checkVolumes`, which writes the file with `CreateExclusive` at `:135`), `:82-84` | A replica that does not mount the deployment's three volumes | **Removed in multi mode only** (MH7). Multi mode shares no volume, so the file has nothing to prove. The deployment's identity is the database's `meta` record: K1's tenancy record and the key-set digest (MH5). |
| Maddy receiver | `replica/topology.go:33-34`, `:62-64` | `KYPOST_NATIVE_RECEIVER` on PostgreSQL | Unchanged. RX replaces it with MX hosts in multi mode. |
| Shared settings fingerprint | `app/replica.go:46-47` (`replica.CheckConfig`) | A replica whose shared settings differ | Unchanged. It is already a database record, so it works across hosts. |
| Proof-of-work key | `app/replica.go:50` (`api.CheckSharedPoWKey`, `api/pow_handlers.go:90`) | A `pow.key` that cannot be kept in `SECRET_DIR` | Covered by the key rule (MH5): provisioned and read-only. |
| Instance liveness | `state/instances.go:35-36` (`RegisterInstance`, a session advisory lock), `:107-135` (`LiveInstances`) | Nothing alone. It feeds the boot-ID and volume checks. | Unchanged. It is a PostgreSQL lock, so it works across hosts. The `boot_id` and `volume_id` columns are still written (multi mode writes the record's volume ID), so `replica_instances` keeps one schema. |

**The volume-free assertion** (MH7). At startup, before any listener or worker starts, a multi
replica checks that every store it opens for writing resolves to PostgreSQL. It checks this against
`TestEveryStoreIsClassified`'s list (`tenancy/inventory_test.go:75-109`) extended with a target
column (below). A store with a file target refuses the start unless its path matches the explicit
per-host pattern list that the volume-free test also uses ("Proof that single-tenant stays byte for byte", below). The test and the
assertion read one list, and an inventory label is not enough on its own. This is the check that makes removing `ErrHost` safe: a host can join only when nothing
it writes lives on its own disk.

### Every host-local lock

All volume locks go through `fsutil` (`fsutil/lock.go:29` `WithFileLock`, `:42` `LockFile`, `:48`
`LockFileContext`, `:54` `LockFileShared`, `:62` `LockFileGated`), except four direct `flock`s. A
`flock` holds on one kernel and one inode only (`docs/RUNNING.md:240-245`).

| Lock (file it fences) | Call sites | What it orders | Replacement in multi mode |
|---|---|---|---|
| `users.json` | `users/users.go:839` (shared reads), `:871` (gated writes), `:1396`; `users/native_recovery.go:35,120` | Every account write against every snapshot reader | The `accounts` table's own transactions (MH3). Writers lock the rows they change (`SELECT … FOR UPDATE`). A whole-set snapshot reads in one `REPEATABLE READ` transaction, so no writer waits for a reader. |
| **The domain fence** (`native-domains.json`) | `sso/native_domain.go:363,448,511`; `sso/native_mailbox.go:144`; `sso/native_address.go:318`; `sso/native_allocate.go:65`; `sso/hr_mailbox.go:28`; `sso/native_outbound.go:94` (shared); `api/native_relay_handlers.go:124,282`; `api/native_recovery_handlers.go:326`; `app/receiving.go:538` (shared); `app/receiving_config.go:236`; `app/migrate_native.go:75` | Domain proof, address and mailbox allocation, outbound admission and the receiving `config` fetch against one another. It spans the domains document, the ledger (`native-authority.db`) and `users.json`. | A **fence** (below): a session advisory lock named `fence:native-domains`, with shared and exclusive modes and R's probe. |
| **The directory fence** (`sso-lifecycle.json`) | `sso/directory.go:284` (gated), `:291` (shared) | Every lifecycle write in every process (`sso/directory.go:277-278`) | A fence named `fence:sso-lifecycle`. |
| The settings lock (per-user `config.yaml`) | `config/config.go:519`; `api/masked_addresses.go:176`; `api/native_mailbox_handlers.go:286` | Settings and masked records, before the domain fence and then the directory fence (`api/masked_addresses.go:166-168`, order settings → domains → directory) | A fence named `fence:settings:<user ID>`, taken in the same order. |
| `quota-<mailbox>` | `app/receiving.go:63` | The mailbox quota check against its write. RX PR 4 also uses it as the fence between an import write and the purge (RX "Import leased per mailbox"). | A fence named `fence:quota:<mailbox>`. The purge's safety must not depend on it alone: see "The purge and a lost fence". |
| `native-relay.json` (relay profile and every DKIM private key) | `api/native_relay_handlers.go:291`; `mailmsg/domain_relay.go:304,326` | Relay settings and key writes | The document's revision (CAS). DKIM rotation's fence is below. |
| DKIM rotation (`r.Fence` gated, then `r.Path`) | `mailmsg/dkim_rotation.go:47,52` | A rotation against the domain fence and the relay file | `r.Fence` is the domain fence above. `r.Path` is the relay document's CAS. |
| `sso.json`, the current-settings read | `sso/sso.go:123`; `sso/current_settings.go:16` | SSO settings writes | The document's CAS. `WithCurrentSettings` takes a fence named `fence:sso`, because its action runs under the lock. |
| Per-user JSON stores | `contacts/store.go:144,163,213,315,481,508,630,709,862,952`; `groups/store.go:104,188,264`; `rules/store.go:107,164,191`; `sendas/store.go:98`; `pgpdiscovery/atomic.go:58`, `pgpdiscovery/suppress.go:76,107`; `kycalendar/flags.go:54`, `kycalendar/invitations.go:53`; `api/dav_auth.go:353`, `api/contacts_handlers.go:532,543`, `api/server_userscope.go:1009` (CardDAV password); `api/contacts_photo.go:173` (the photo directory) | Read-modify-write of one file | The document's CAS in one transaction (MH2). There is no separate lock. |
| Per-mailbox `mailcache.json` | `mailcache/store.go:179,333,652,788,805,870,894`; `mailcache/keygen.go:23` | Read-modify-write of one file | The document's CAS (MH2). |
| Mailbox preparation (the target directory) | `mailbox/prepare.go:70,638` | Creating and converging one mailbox's storage and `native-mailbox.json` | A fence named `fence:mailbox:<mailbox>`, together with the document's CAS (MH4). |
| `outbox-idle` | `mailbox/outbox_idle.go:36` (direct `flock`) | The outbox idle mark against a transaction that makes outbound work due | **Dropped in multi mode** (MH4). The mark is a per-mailbox optimisation that saves a database read. In multi mode the recovery worker reads the outbox rows. RX "Outbound at hosted volume" already recommends an index of queued rows. |
| `wkd-domains.json` | `wkdpublish/store.go:137,180,208` | Read-modify-write | The document's CAS. |
| Sender blocks and evidence | `ingress/blocks.go:340`, `ingress/autoblocks.go:186` | The two files | Already absent in multi mode: RX PR 3 uses `tenancy.db` tables. |
| Cloudflare credentials | `cfreceiving/state.go:49` | The credentials file | Absent: Cloudflare is refused in multi mode (RX R1). |
| Migrations (`native-domains.json`, `sso-lifecycle.json`, `users.json`) | `sso/native_migrate.go:43,164`; `sso/authority_migrate.go:187` | Offline v2 → v3 migrations | Not reached: multi installs start empty on v3 (K2). |
| Installer setup | `app/setup.go:93,308` | The single-container installer | Not reached in multi mode. |
| `backup-operation.lock` | `backup/backup.go:524-535` (direct `flock`), taken by `Service.lock` at `backup/backup.go:190,245,291,373,390,400,443` and `backup/drill.go:22` | One backup operation at a time on one host | Every one of these operations takes a fence named `fence:backup-operation` (MH4). On PostgreSQL the run and export also hold `backup-run` (`backup/replica.go:95-101`), and pin, pairing and unpair hold `recovery-pin` (`:118`). Whether every other caller of `Service.lock` (for example `SetIntervalSeconds`, `:399`) is covered by a database lock today is unverified, so MH4 puts the fence on all of them. |
| `server-instance.lock`, `state.db.<hash>.lock` | `state/coordination.go:324-343` (`HoldInstance`), `:299-315` (`withFlock`) | SQLite only (`app/replica.go:123-137`; `state/coordination.go:172-174`) | Not reached: multi mode is PostgreSQL only (K2). |

**Fences.** A fence is R's existing session advisory lock (`state.Store.Hold`,
`state/coordination.go:281-297`), with four changes, all made only in multi mode:

1. **R's probe.** Today `Hold` and `Serialized` (`:270-279`) take the lock on a connection of their
   own and never probe it. Only `WithLock` (`:164-195`) probes, so a holder whose lock
   connection dies does not learn that it lost the lock. On one host that is rare. Across hosts a
   single TCP connection can drop on its own. A fence therefore runs under `WithLock`'s probe
   (`lockProbeInterval` and `lockProbeTimeout`, 1 s each, `:137-139`), which cancels the holder's
   context when the probe fails.
2. **A shared mode.** The domain and directory fences have shared readers
   (`fsutil.LockFileShared`). A fence uses `pg_advisory_lock_shared` for these readers.
   `LockFileGated` exists so that a stream of readers cannot starve a writer
   (`fsutil/lock.go:58-62`). PostgreSQL's lock manager queues a new shared request behind a waiting
   exclusive one, for advisory locks too: verified on PostgreSQL 17 by MH1's
   `TestFenceWriterNotStarvedByReaders` (`backend/internal/state/fence_test.go`).
3. **Revisions underneath.** A fence orders work but does not make it safe. Every document it
   covers is written with a CAS on its revision (below), so a holder that lost its fence and kept
   writing for up to the probe bound (2 s, `ProbeBound`, `:144`) fails its write instead of
   overwriting a newer one.

4. **Keys, sessions and waits.** R's lock keys are `(space, hashtext(current_schema() || ':' ||
   name))` (`state/coordination.go:154-162`), so the name part is a 32-bit hash. With one fence per
   user and per mailbox, collisions are expected at hosted scale. Fences are nested (settings →
   domains → directory), so a collision is not only a slowdown. A request holding fence A on one
   connection that asks for a colliding fence B on a second connection waits on itself. Two
   requests can also wait on each other in a cycle. PostgreSQL's deadlock detector cannot see
   either wait, because the holder is idle and the waiting happens in the application. So a fence:
   - **uses a 64-bit key**: `pg_advisory_lock(bigint)` on the first 8 bytes of
     SHA-256(schema, `:`, name). The bigint form is a separate key space from R's two-int locks
     (`state/coordination.go:153`), so a fence never meets them. It does share that space with the
     per-schema writer locks (`dbbackend/pgdialect/driver.go:120-123`, FNV-64 of the schema name).
     With 64-bit keys, a collision with one of those is negligible, but it is not impossible;
   - **holds every fence of one request on one session.** A fence set takes its fences in the fixed
     order on one dedicated connection. A session advisory lock is re-entrant within its session,
     so a collision inside one request is harmless;
   - **has a bounded wait** like R's `pairingLockWait` (10 s, `api/shared_state.go:63-65`). A wait
     that runs out fails the request with `503` and logs the fence's kind, never its name (a name
     carries a user or mailbox ID). A cycle between two requests therefore ends in one `503`, not
     a hang. MH1 tests a forced collision (a test hook that maps two names to one key) both inside
     one request and across two.

## Inventory of volume files, with targets

Classes are K's (O, D, P, `-`). **Target** is where the data lives in multi mode:
- **`documents`:** a row in the new `volumes.db` table (MH2);
- **`accounts`:** the new `volumes.db` table (MH3);
- **already PG:** the store is already on PostgreSQL in every R install;
- **absent:** not present in multi mode;
- **per host:** stays on the host's own disk, with the reason.

The file names are the ones `TestEveryStoreIsClassified` lists (`tenancy/inventory_test.go:75-109`).

| File | Defined | Class | Target | Why |
|---|---|---|---|---|
| `users.json` | `app/bootstrap.go:47`, `users/users.go:701,812` | O | `accounts` | One file holds every customer's account and is rewritten whole under one lock (`users/users.go:871`, gated writes of the whole file). One row per account lets writers lock one account. |
| `admin.env`, `first-run-password.txt` | `app/bootstrap.go:83` | – | absent | K6 |
| Root `config.yaml` | `config/config.go` (root config) | P | `documents` | Edited at runtime from the admin settings and re-read by every replica within 5 seconds (`docs/RUNNING.md:288`). The re-read becomes a revision check. |
| `mail-defaults.json` | `api/server_mail_defaults.go:26` | P | `documents` | Admin-edited at runtime |
| `sso.json` | `sso/sso.go:94` | P | `documents` | Admin-edited at runtime |
| `sso-lifecycle.json`, `native-provisioning.json` | `sso/lifecycle.go:97`, `sso/native_provision.go:73` | P | `documents` (stubs) | v3 leaves them as stubs (K inventory). The stubs are still read, and the directory fence names the lifecycle file. Whether a v3 multi install reads the stubs' contents is unverified, so MH2 moves them rather than dropping them. |
| `native-domains.json` | `sso/native_domain.go:27` | P | `documents` | Platform domains (K9). It is also the domain fence's name. |
| `native-relay.json` (relay credentials and **every DKIM private key**, sealed under `native-relay.key`) | `api/native_relay_handlers.go:47` | P | `documents` | The sealed bytes move unchanged. The sealing key stays in `SECRET_DIR` (MH5). |
| `wkd-domains.json` | `wkdpublish/store.go:74` | P | `documents` | |
| `receiving.conf` | `api/native_recovery_handlers.go:393` | P | absent on replicas (unverified) | In multi mode an MX host renders its own profile from the replica's `config` answer (RX PR 2 as built, "`config`"), on the MX host's own disk. Whether a multi replica still writes `receiving.conf` is unverified. It is not on the per-host pattern list, so the startup assertion and the volume-free test both fail if a replica writes it. The fix is then to stop writing it in multi mode. |
| `quota-<mailbox>` | `app/receiving.go:63` | D | fence (lock only, no data) | See the lock table above. |
| `sender-blocks.json`, `sender-evidence.json`, `sender-evidence.damaged.json` | `ingress/blocks.go`, `ingress/autoblocks.go` | – | absent (already PG tables) | RX PR 3 |
| Per-user `config.yaml` (settings, signatures, masked records) | `config/config.go:414-416` | D | `documents` | M D8 moves the masked records into their own table in hosted mode (K PR 7). The rest of the settings become one document per user. |
| Per-user `imap-config.json` | `config/config.go:407` | – | absent | K6: there is no external-IMAP source in multi mode. |
| Per-user `tuning.md` | `config/config.go:411` | D | `documents` | |
| Per-user `carddav-auth.json`, `carddav-client.json` | `api/server_userscope.go:246-255` | D | `documents` | `carddav-auth.json` is re-read on every use (`docs/RUNNING.md:286`). That becomes one indexed row read. |
| Per-user `contacts.json`, `groups.json`, `rules.json`, `send_as_aliases.json` | `contacts/store.go:64`, `groups/store.go:44`, `rules/store.go:43`, `sendas/store.go:62` | D | `documents` | |
| Per-user `pgp-discovery.json`, `pgp-discovery-suppressions.json` | `pgpdiscovery/settings.go:23`, `pgpdiscovery/suppress.go:28` | D | `documents` | |
| Per-user `kycalendar-invitations.json`, `kycalendar-flags.json` | `kycalendar/invitations.go:32`, `kycalendar/flags.go:35` | D | `documents` | |
| Per-user `incoming-encryption.json` | `processor/incoming_encryption.go:58` | D | `documents` | |
| **Per-user `contact-photos/`** (one file per photo, named by content hash) | `api/server_userscope.go:417-424`, locked at `api/contacts_photo.go:173` | D | `documents`, one row per photo | **Not in `TestEveryStoreIsClassified`**, because the name is a directory with no extension (`tenancy/inventory_test.go:121,124`). MH2 adds it to `extras` and classifies it as D. K PR 6's purge must cover it whichever PR lands first. |
| Per-mailbox `mailcache.json` | `mailcache/store.go:105` | D | `documents` | |
| Per-mailbox `native-mailbox.json` | `mailbox/prepare.go:26` | D | `documents` | |
| Per-mailbox `outbox-idle` | `mailbox/outbox_idle.go:22` | D | absent | Dropped in multi mode (lock table) |
| `$STATE/pickup/<id>.json` | `api/server.go:507` | D | `documents` | Public pickup links can be opened on any replica. |
| `$STATE/imports/upload-*` | `api/mail_import.go:38` | D | per host (scratch) | One streamed request writes it, and the job runs on the replica that received it (`api/mail_import.go:305-313`). Grants and job status are already in PostgreSQL. The upload is lost if that host dies, and the user starts again, as today when a replica stops. |
| Native restore hold, qualification and release files, `mfa-clear-all.*` | `sso/native_restore.go`, `sso/native_restore_qualification.go`, `sso/native_release.go`, `app/app.go` | P | `documents` | Whole-install recovery state that every replica must see. Restore runs offline. These files are read at start and by the release routes, so they must be shared. |
| `backups/` (local capsule copies) | `backup/backup.go:90-99` | P | absent in table mode (open question 3, approved 2026-11-22) | A local copy is useful only on the host that holds it. Table mode refuses local copies and requires a KyRecovery deposit (MH4 refuses the local write; MH8 refuses a migration without a pin). |
| `mail-bulk.json` | `backup/bulk.go:38-43` | P | per host (scratch for one run) | It is written into the capsule during a run (`bulkManifestPath`). Whether it outlives the run on disk is unverified. MH7's assertion decides. |
| `backup-operation.lock` | `backup/backup.go:524` | P | fence | Lock table |
| `replica-volume.id` | `replica/topology.go:19` | P | absent in multi mode | MH7 |
| `server-instance.lock` | `state/coordination.go:319` | P | absent (SQLite only) | |
| `tenancy.db` and every other `.db` | `dbbackend` | O/D/P | already PG | R and K PR 1 |
| `recovery.pub` | `STATE_DIR/recovery.pub`: `backup/replica.go:240,303` call `recoveryclient.RecoveryKeyPath(stateDir)`, which joins the data directory and the file name (ky-primitives `recoveryclient/pin.go:38-40` at the `backend/go.mod` pin) | P | per host, in `STATE_DIR` (never `SECRET_DIR`), rewritten from the sealed anchor | R rewrites a missing `recovery.pub` from the sealed record (`app/replica.go:52-58`). `SyncRecoveryKey` runs at start and before every run and export (`backup/backup.go:197,253`), and a file naming another key refuses with `ErrAnchor` (`backup/replica.go:240-243`). So a stale host fails closed and never deposits to an unpinned key. Across hosts, though, a re-pin on one replica would leave every other host's file stale, and that host would refuse every backup until the file was fixed by hand. **In multi mode** (MH5), `SyncRecoveryKey` therefore replaces a mismatching file with the key from the sealed record, after the record has opened under its AEAD. That record is the authority, so the file is only a cache of it. It writes an audit line naming the key ID only. A record that does not open still refuses. |
| TLS certificate files (`TLS_CERT_FILE`, `KYPOST_TLS_CERT_FILE`) | `api/tls.go:42`, `api/pairing_pin.go:180` | P | per host (read-only, provisioned) | HAProxy or the edge proxy ends TLS (`docs/RUNNING.md:257-258`). The pin source must be the same on every host. The shared-settings fingerprint already covers the path (`replica/config.go:48`). |
| Logs, supervisord socket and pid, Ollama models, backup scratch | `docs/RUNNING.md:292-293`; `replica/config.go:60-69` (`PerHost`) | P | per host | Already per container |

**Keys (`SECRET_DIR`).** All keys in `TestEveryStoreIsClassified` (`tenancy/inventory_test.go:99-105`)
are **per host, provisioned identically and read-only** (MH5). They are not moved into PostgreSQL,
because then a database dump would hold both the sealed records and the keys that open them. Today
they are created lazily by `cryptutil.LoadOrCreateKey` (`cryptutil/cryptutil.go:47-57`). The
callers include `app/replica.go:46` (`totp-secret.key`), `api/scheduled_send.go:162`,
`api/import_rows.go:331`, `api/relay_identity.go:41`, `api/pow_handlers.go:70,95`,
`api/server_imap_config.go:245`, `api/pickup_handlers.go:396`, `pgpmail/pickup.go:233`,
`mailmsg/domain_relay.go:362`, `mfa/mfa.go:475` and `backup/bulk.go:206`. Others use
`fsutil.CreateExclusive` (`config/config.go:862`, the VAPID key; `app/receiving_tls.go:174`,
`receiving-tls.key`). On one shared volume, an exclusive create makes concurrent first starts agree
(`docs/RUNNING.md:289`). On separate disks, each host would mint its own key, and every host would
then refuse the others' sealed records, sessions and links. In multi mode every one of these calls
**loads**, and a missing file refuses the start. This includes K PR 6's `address-tombstone.key`.

**Not files, unchanged by host count.** Sessions, CSRF, revocations, lockouts, challenges, replay
guards, import grants and job status, the orgs cache, the gateway pins and the elections are
already in PostgreSQL (`docs/RUNNING.md:281-284,287`; `api/shared_state.go:17-55`). In-process caches
over PostgreSQL rows, such as the per-user store handles and the device and subscriber indexes
(`api/server.go:361-362`), behave the same with N replicas on one host or on N hosts. R already
runs them with N > 1.

### The `documents` table

```
documents(root TEXT, path TEXT, owner TEXT, revision BIGINT, body BYTEA, updated_at BIGINT,
          PRIMARY KEY (root, path))      -- table mode only, in volumes.db (SQLite DDL for the capsule copy)
```

- **The key is the root and the path.** `root` is `config` or `state`. `path` is the same relative
  path the file store would use, built by the same helpers (`config.UserDir`,
  `config.SafeUserPathComponent`, `config/config.go:392-403`). A `CHECK` and a boundary validator
  refuse an absolute path, `..`, an empty segment, a NUL and anything over 512 bytes. A store
  therefore keeps its own code and format and changes only its I/O.
- **One seam.** A `vol` interface (Read, Write with an expected revision, Remove, List under a
  prefix) has a file implementation, which is today's `fsutil` calls unchanged, and a table
  implementation. The choice is made once, at open, by table mode (`KYPOST_TENANCY=multi`, or a single install whose `state.db` `meta` holds `volume_stores=postgres`), as RX PR 3 picks the
  sender tables (`app/receiving.go:483-497`). No store branches on the mode
  anywhere else.
- **CAS on a global revision.** Every write takes a fresh `revision` from one PostgreSQL
  sequence (`nextval`), so no two versions of any document, before or after a remove, ever share a
  revision. A write gives the revision it read:
  - an update is `UPDATE … SET body=?, revision=nextval(…) WHERE root=? AND path=? AND revision=?`;
  - a create (the document was absent) is `INSERT … ON CONFLICT (root, path) DO NOTHING`, so two
    replicas creating one document never see a key violation;
  - a remove is `DELETE … WHERE root=? AND path=? AND revision=?`.

  If any of these touches no row, the write returns `ErrStale`, and the store retries its
  read-modify-write. The retry is bounded. A store that exhausts the bound fails the request with
  `503`, never with a partial write.

  Because revisions never repeat, a writer that read a document before a remove and re-create
  cannot match the new row (the ABA case), however long it stalled. So a remove deletes the row,
  with no tombstone and no timing argument. A restore sets the sequence above the restored
  table's highest revision before any replica starts. The SQLite DDL exists only for the capsule
  copy (K "PR 1 as built"). Multi mode never writes through it.
- **Size.** Each document has a cap, refused at the write. **The figure is provisional**:
  16 MiB is a placeholder, not derived from anything. Before any store moves, MH2 measures each
  store's largest legitimate document: each store's own limits, and the worst case its existing
  tests build, as `TestEvidenceWorstCaseSize` does for evidence. The cap is then set at or above
  the largest of those, so the table never refuses a write that the file implementation accepts.
  Each store keeps its own caps.
- **Class and owner.** `owner` is the owning user's ID for a D row and NULL for a P row. The
  seam sets it on every write and never takes it from the request:
  - from the path for `users/<id>/…`;
  - from the ledger's owner for `mailboxes/<id>/…`;
  - for a D store outside those prefixes, from the record's own owner (today only
    `pickup/<id>.json`, whose record names its owner user, K inventory).

  A D store with no owner mapping fails `TestEveryStoreIsClassified`, which gains the per-store
  owner rule. K PR 6's purge deletes an org's rows `WHERE owner IN` the snapshot's user IDs, so
  pickup links go with the org.
- **No write after the owner is gone.** A purged user's documents must not come back. Otherwise
  a paused writer whose CAS failed would retry, read the document as absent, and re-create it.
  So every D write locks its owner's `accounts` row (`SELECT … FROM accounts WHERE id = owner FOR
  KEY SHARE`) in the same `volumes.db` transaction, and writes nothing if the row is missing.
  The purge deletes the snapshot's `accounts` rows **before** their documents. A concurrent write
  either commits first, and the document delete removes it, or it waits on the row lock and then
  finds no account. Both tables are in one store, so this is one database rule, not a lock that
  can be lost. The purge's final check also lists `documents WHERE owner IN` the snapshot, and an
  empty list is required before the purge is done. The guard needs the `accounts` table, so it
  ships in MH3. Between MH2 and MH3, only the final check guards this. That is safe, because
  multi mode cannot start before MH7 (K PR 7's gate), and MH8's migration needs MH1–MH7.
- **Expired rows.** The elected `api-sweepers` worker already sweeps expired pickup links
  (`docs/RUNNING.md:280`, "pickup and contact sweeps"). In multi mode it deletes their rows.
- **Backups.** `tenancy.db` joins the backup set in K PR 6 (K "PR 1 as built"), and MH2 adds
  `volumes.db` the same way. A table-mode install's documents and accounts are then in every
  capsule's database export. A restore loads them with the rest of `volumes.db`, so it writes no
  files. K PR 6 is therefore a hard dependency of MH2.

### Per-org encryption at rest (owner decision 2026-11-22)

Today contacts are plaintext JSON (`contacts/store.go:26`, `SECURITY.md` "Unencrypted"). In table
mode every org's contacts would sit in one database, so the seam seals what it stores.

- **What is sealed.** The table implementation seals the `body` of every D row (`owner` is set):
  `contacts.json` and each `contact-photos/` row included. Sealing at the seam is one code path, and
  no store changes. P rows and the bookkeeping columns (`root`, `path`, `owner`, `revision`) stay
  plaintext, because CAS and the purge read them.
- **Why the server holds the key, not the browser.** The server reads contacts with no browser
  present: pre-sort and out-of-office match senders (`processor/presort.go:60`), signature checks
  bind keys to addresses (`api/pgp_receive.go:294`), send-time key lookup, Autocrypt harvest and
  key discovery write contacts, and CardDAV clients need plaintext vCards. The armored public key
  also carries the addresses as User IDs. Browser-side encryption breaks those and still leaks
  most addresses, so it is not done.
- **Keys.** One random 256-bit data key per org, in a `volumes.db` table
  `doc_keys(org_id TEXT PRIMARY KEY, wrapped BYTEA, created_at BIGINT)`, wrapped with AES-256-GCM
  under `documents.key` in `SECRET_DIR`. A migrated single-tenant install has one row with an empty
  `org_id`. `documents.key` is provisioned and read-only like every MH5 key, and joins MH5's
  key-set digest. A data key is a database row, so creating one is an
  `INSERT … ON CONFLICT DO NOTHING` followed by a read, and two replicas agree on one key.
  Rotation is offline, as for MH5 keys.
- **Binding.** AES-256-GCM with a random nonce, as `cryptutil.Seal`, plus associated data of
  `root`, `path` and `owner`. A body copied to another path, user or org fails to open. `Seal`
  takes no associated data today (`cryptutil/cryptutil.go:91`), so MH2 adds that variant.
- **Failure.** A body that does not open is an error, never an empty document (the
  refresh-from-disk rule, `backend/AGENTS.md`). No plaintext, key or address is logged.
- **What it protects.** A copy of the database, a dump or a capsule's `volumes.db` without
  `SECRET_DIR`. It does not protect against a compromised replica, which holds the keys (threat
  model).
- **Creating a key is owner-guarded.** A data key is created only inside the D write's own
  transaction, after the owner guard (No write after the owner is gone) has locked the owner's
  `accounts` row and found it. A writer paused across the purge therefore finds no account and
  creates no key, as it writes no document. Between MH2 and MH3 only the final check guards this,
  as for documents.
- **The purge.** The purge deletes the org's `doc_keys` row after its documents, and its final
  check lists `doc_keys` for the org with the documents: a row left there fails the purge. Capsules made
  earlier still carry the wrapped key, so this is not a cryptographic erase of backups. K's restore
  release keeps governing those.
- **File mode is unchanged.** An un-migrated single-tenant install keeps plaintext files, byte for
  byte (open question 16).

### The `accounts` table

```
accounts(id TEXT PRIMARY KEY, username TEXT UNIQUE, issuer TEXT, subject TEXT,
         revision BIGINT, record BYTEA, UNIQUE (issuer, subject),
         CHECK ((issuer IS NULL) = (subject IS NULL)))   -- table mode only, volumes.db
```

`issuer` and `subject` are NULL for an account with no SSO binding, never an empty string. A
`UNIQUE` constraint admits any number of NULLs, so unbound accounts never collide. The writer maps
Go's empty strings to NULL at the boundary. Multi mode has no local accounts (K6), so every
customer account is bound. The column rule still keeps a staff break-glass or not-yet-linked
account from being misreported as `ErrSSOSubTaken`.

- `record` holds the same JSON object one `users.json` entry holds (`users/users.go:49-165`),
  including `OrgID`. The users store's invariants (K "PR 2 as built": `orgInvariants`,
  `writeFileUnlocked` comparing old and new, `users/users.go:570,995`) become
  per-row comparisons in the same transaction. The `UNIQUE` constraints enforce `ErrSSOSubTaken`
  and username uniqueness in the database rather than in a whole-file scan.
- `WithCurrentUsers` (`users/users.go:830-866`) reads every account under the shared lock, and
  today that lock also keeps writers out while a caller checks and then acts. Its replacement:
  - **A caller that only reads** gets one `REPEATABLE READ` scan.
  - **A write that checks a whole-set invariant runs `SERIALIZABLE`.** Examples are the
    last-administrator rule, and a staff demotion or deactivation checked against it. On
    serialization failure (`40001`) it retries within the CAS bound, then answers `503`.
    `REPEATABLE READ` would allow write skew here. Two demotions on two replicas each lock only
    their own row, each sees the other administrator still active, and both commit, which leaves
    no administrator. Every writer that can change an administrator runs `SERIALIZABLE`, so
    PostgreSQL's serializable snapshot isolation sees both sides of the conflict.
  - **The purge snapshot against provisioning** uses a fence, not isolation, because provisioning
    is several ordered writes in several stores (`subject_orgs`, `accounts`, `user_orgs`; K "PR 2
    as built"), which one `SERIALIZABLE` transaction cannot cover.
    - Provisioning an account holds `fence:org:<org_id>` shared from its first write to its last.
      It re-reads the org's cache row under the fence and refuses an org that is `deleted`.
    - The purge takes the same fence exclusively for its snapshot. It keeps K PR 2's order:
      `user_orgs` before accounts.
    - The fence can be lost. So the purge's final check (K step 6) lists `subject_orgs`,
      `user_orgs` and `accounts` for the org once more, and re-runs the member steps for any
      survivor before the purge is marked done.
- The stat-based read cache (`users/users.go:644-684`) becomes a per-account read that is
  revision-checked, so no replica serves an account it has not re-read this request
  (`currentUser` already re-reads, K "What KyPost has today").
- `user_orgs` stays as it is (K3). PR 2's three-record check (`checkAccountOrg`) is unchanged,
  because `accounts.record.OrgID` is the `users.json` `OrgID`.

## Per-replica meters

R keeps these in process memory, so with N replicas each one allows up to N times its limit
(`docs/RUNNING.md:285`, O4). They are built at `api/server.go:525-555`, and only the lockouts, the
login KDF budget and the office budgets are moved into PostgreSQL (`api/shared_state.go:41-55`).
Multi-host does not change the arithmetic, but hosted mode makes N larger and the caller untrusted.

| Meter | Where | Per-org quotas may rely on it? | In multi mode |
|---|---|---|---|
| MFA push (`mfaPushLimiter`) | `api/server.go:541`, `api/mfa_push_limiter.go:44` | No | **Shared window** (MH6): it sends pushes to a person's phone. N times the limit is harassment. |
| Send-as verification, KyPulse confirm, notification test cooldowns | `api/server.go:542-544` | No | **Shared window** (MH6): each one sends mail or a push to a third party. |
| Native relay check and deliverability cooldowns | `api/server.go:545-546` | No | **Shared window** (MH6): outbound DNS and SMTP probes |
| Proof-of-work challenge budget and escalation | `api/server.go:553-554` | No | **Shared window** (MH6): unauthenticated callers |
| `loginParamsLimiter` | `api/server.go:539` | No | **Shared window** (MH6): unauthenticated |
| MCP calls, WKD, account writes, people, KyDrive, KyMessages, push polls, TLS pin, device rescan | `api/server.go:525,531-537,540` | No | Per replica, documented. These shed load per account. N times the limit costs CPU, not other people, and the per-org send and storage quotas sit behind them. |
| Import slots (`maxImports = 2`, server-wide) | `api/mail_import.go:34-35` | No | Per replica. K's "one import or export per org at a time" (K PR 5 as built, "Not built") is a lease in PostgreSQL, as RX's per-mailbox leases are (MH6). |
| IMAP listing slots | `api/server.go:529` | No | Absent: K6 turns IMAP off. |

**Rule for per-org quotas** (K "Replicas", "Per-replica meters"): a per-org quota reads PostgreSQL
only. As built, it reads `send_ledger` (`sendPerHour`, `recipientsPerDay`), the ledger and mailbox
counts (`mailboxes`), and mailbox usage (`mailboxBytes`, `storageBytes`) (K "PR 5 as built",
"Quotas"). M's masked quotas (K PR 7) and the per-org import and export lease (MH6) must do the
same. `TestOrgQuotas` gains a two-replica variant on the PostgreSQL job that runs the same caps
through two servers sharing one database and asserts the total.

## Elected roles and split brain

The roles are `daemon`, `mail-runtime` and `api-sweepers` (`app/app.go:380-381,473`), each
elected by `worker.Run` (`worker/worker.go:44-60`) over `WithLock`. A new owner waits a `Grace`
longer than the probe bound before its first job (`app/replica.go:68-75`). The election promises
single ownership, not exactly-once delivery: "jobs keep their own fences (claims, leases, CAS
moves)" (`worker/worker.go:44-46`).

Across hosts, three things can happen that one host did not see often:

1. **A partition between a holder and PostgreSQL.** The holder's probe fails within 2 s and
   cancels its jobs. PostgreSQL releases the lock only when it notices the dead connection. That
   takes TCP keepalive time, which defaults to hours on Linux (unverified for the Compose
   PostgreSQL image). Until then no other replica can win the role. That is a stall, not a split
   brain. **Requirement (MH7):** the multi profile bounds both ways a dead connection can sit:
   - **an idle socket:** PostgreSQL's `tcp_keepalives_idle`, `tcp_keepalives_interval` and
     `tcp_keepalives_count`, about 30 s in total;
   - **a socket with unacknowledged data:** `tcp_user_timeout` (PostgreSQL 12 or later), 30 s.
     Keepalives do not run on such a socket, so without this Linux falls back to retransmission
     timeouts (`tcp_retries2`, about 15 minutes). The holder's 1 s probe makes this the likely
     case in a partition. (`client_connection_check_interval` does not help here: it checks only during a
   running query, and a lock holder's connection is idle.)
2. **A holder paused longer than `Grace`** (VM freeze or GC). PostgreSQL drops it, a new owner
   starts after `Grace`, and the old one resumes with a job mid-write before its next probe. This
   is the split brain. Safety then rests on each job's own fence. MH4 lists every elected job with
   its fence, and a job without one gets one:
   - `daemon`: receiving import (RX PR 4's per-mailbox leases, with `lease_until` on database
     time) and the poller and push (per-user rows). It also runs the WKD recheck, retention and the
     backup schedule (`backup-run`, a `WithLock` whose loss cancels the run).
   - `mail-runtime`: outbox recovery (the outbox claim token) and DKIM rotation (the domain fence
     and the relay document's CAS).
   - `api-sweepers`: scheduled sends (the `sending` row records its replica,
     `docs/RUNNING.md:280`), the orgs poller (`ApplyOrg`'s revision fence, K PR 5) and the purge
     (`org_purges` rows, K PR 6).
3. **A PostgreSQL failover.** Advisory locks are not replicated, so every holder loses every lock
   and every fence at once. Every probe fails, every role re-elects after `Grace`, and every fenced
   section in flight fails its CAS or its commit. With RX R5 (synchronous standby) nothing that
   committed is lost.

**Every lease or window compares database time** (`state/coordination.go:26-28` `nowMillis`).
Leases added by MH6 must too, so clock skew between hosts changes no answer. **RX PR 4's import
lease does not yet** (found on `origin/main` `3d0465a2`): `TakeImportLease` takes the caller's
`now time.Time` and writes `now+60 s` and `now` as Unix seconds (`tenancy/imports.go:18-30`), and its callers pass `time.Now()` (`app/receiving_imports.go:119,138,258`), so
the lease's expiry is decided by the replica's own clock. On one host every replica shares that
clock. Across hosts, a host whose clock runs 60 s fast takes a lease that is still live on
another host. Two importers then work one mailbox, and the arrival order RX R8 promises is lost
(the receipts keep it from writing twice). MH4 moves the lease's comparisons to database time,
as `nowMillis` does, and adds a test with two replicas whose clocks are skewed by more than the
lease.

## The purge and a lost fence

RX's import fence relies on `quota-<mailbox>`. The purge holds it while it removes a mailbox's
storage, and the import write re-reads the org's status after taking it (RX "Fence against the
purge"). With a fence that can be lost, a paused import worker could write after the purge removed
the schema. If the store's open recreates a missing schema, that write would bring the deleted
org's mail back.

**Requirement (MH4, before multi mode starts; RX PR 4 is built with the quota lock alone):** a purged mailbox's catalog entry
(`kypost_catalog.stores`, `dbbackend/dbbackend.go:205-207`) is tombstoned in the
same step that drops its schema, and opening a tombstoned store fails. The purge is then safe in
the database whatever happens to the lock. The fence only reduces wasted work. K PR 6's final check
(no mailbox, receipt or schema left) also asserts that the tombstone exists.

## Encryption in transit (open question 4, as answered)

The owner wants every hop encrypted. In multi mode and in a migrated install (table mode), the
hops are:

| Hop | Today | In table mode |
|---|---|---|
| Browser or app to the edge | TLS, ended in a proxy in front of HAProxy (`docs/RUNNING.md:257-258`) | Unchanged |
| Edge or HAProxy to a replica | Plain HTTP on the Docker network (`docs/RUNNING.md:257-258`) | **TLS.** The hop now crosses hosts. Each replica serves HTTPS with a provisioned per-host certificate, and the proxy verifies it. A replica refuses to start with a plain listener on a non-loopback address. (MH7) |
| Replica to replica | None found: replicas share state only through PostgreSQL (MH's inventory; unverified beyond it) | None allowed. A later direct call must be mutually authenticated TLS. |
| Replica to PostgreSQL, and to PgBouncer | `sslmode=disable` in the Compose URL (`docs/RUNNING.md:313`) | `sslmode=verify-full` unless the host is loopback (approved). With PgBouncer, both of its legs use `verify-full`. |
| PostgreSQL primary to standby | Not deployed (RX R5) | Replication connections use `verify-full` too (MH7 docs). |
| MX host to receiving server | TLS 1.3 with pinned keys both ways (RX R3) | Unchanged, also across regions (MH10) |
| Replica to Rspamd | Loopback inside one network namespace (RX R6) | Unchanged: not a network hop |
| Replica to Ollama | "One shared service" (`docs/RUNNING.md:293`) | TLS, or Ollama on each host's loopback. Which the code allows is unverified; MH7 decides. |
| Replica to KyIdentity, peer apps (KyCalendar, KyDrive, KyMessages-Native, KyPulse's replacement), KyRecovery and the push relays | HTTPS by configuration. Whether the code refuses an `http://` peer URL is unverified. | MH7 refuses a non-HTTPS URL for each of these unless the host is loopback. |
| Replica to the outbound SMTP relay | Unverified | MH7 requires TLS to the relay and documents it. |

## Regions (owner decision 2026-11-22: multi-region is required)

Open question 8 recommended one region, because every fence and CAS is a round trip to one
PostgreSQL. The owner overturned it. This section designs multi-region for multi mode.
Self-hosted single-tenant installs stay in one region: an operator who wants two regions runs two
installs.

### Where an org's data lives: the options

| Option | How | For | Against |
|---|---|---|---|
| **A. One global primary** | Replicas in every region, one PostgreSQL primary in one region | No new routing, no change to anything above | Every fence, CAS, session read and commit from a far region pays a cross-region round trip (tens to 150 ms each, unmeasured). A request makes many of them, so far regions are slow. No data residency. One region's outage takes every org down. |
| **B. A home region per org (cells)** | Each region is a complete multi install, a **cell**: its own PostgreSQL with an in-region synchronous standby, replicas, receiving servers, MX hosts, keys and backups. An org lives in exactly one cell. | Everything above holds unchanged per cell: no lock or CAS ever crosses a region. Residency per org. A region's outage affects only its orgs. | Routing: a sign-in, a sync event and a mail must reach the org's cell. Moving an org between regions is a project of its own. |
| **C. Distributed SQL** (a PostgreSQL-compatible multi-region database) | One logical database across regions | One database to run | A new dependency. MH's fences are session advisory locks, and support for them varies by product (unverified). Every commit pays consensus across regions. KyPost's schema per mailbox at hosted scale is unproven there. |
| **D. Multi-primary replication** | A writable primary per region, replicated both ways | Local writes | Conflicting writes break CAS, `UNIQUE` and the ledger's never-reuse rule. Advisory locks are not replicated. Rejected. |

**Recommended: B, cells.** It is the only option that keeps MH's safety argument (one database
orders every fence and revision) and gives residency, without a new dependency. The rest of this
section is B.

### What a cell is

- **A region ID**, `KYPOST_REGION` (a short lowercase label), recorded in `meta` beside
  `tenancy_mode` at the first multi start (K1). A replica with another or no region refuses to
  start, as it does for another staff org. Single mode refuses the variable, as it refuses
  `KYPOST_STAFF_ORG_ID` (`config/tenancy.go:31`). (MH9)
- **The region is logical.** It names the cell, not a data centre. A disaster recovery move
  (below) keeps the ID.
- **Each cell has its own public hostname** and its own OIDC client registration at KyIdentity.
  Cookies, sessions, device pairings and TLS pins are per hostname, so they are per cell.

### What stays inside one cell

| Thing | Across regions |
|---|---|
| Fences, CAS, revisions, the owner guard | Per cell. No lock ever spans two cells, so MH's design and its latency budget hold per cell. |
| Elected roles (`daemon`, `mail-runtime`, `api-sweepers`) | One election per cell. Two cells never share a job. |
| Sessions, CSRF, revocations, step-ups, lockouts, shared meters | Per cell, in its PostgreSQL. A session of cell A means nothing to cell B. |
| The `orgs` poll | Each cell's `api-sweepers` polls KyIdentity. It caches **only orgs whose home region is its own**, so an org of another region is "not in the cache" and refused as K's Lifecycle table refuses one (MH9). This needs a `region` field on each org in `/api/suite/orgs` (a T change). |
| Sync events and provisioning | KyIdentity sends an org's events to its home cell only (T change). A cell also refuses, without writing, an event for an org cached with another region. An event for an org not cached yet (a new org, before the next poll) gets a temporary refusal, and KyIdentity retries it, so a new org's first sign-in can wait up to one poll. So an account exists in one cell only. |
| The purge and Delete everything | In the org's home cell, unchanged. Its address tombstones are that cell's. |
| Backups and restore | Per cell. A capsule holds one cell, and a restore rolls back only that cell's orgs. Deposits go to a KyRecovery in the same residency zone (KyRecovery's own regional install, outside KyPost). No local copies (open question 3). |
| Staff | The staff org is the same in every cell (`KYPOST_STAFF_ORG_ID`), and each cell is exempt for it (K8). Staff accounts are provisioned in each cell from that cell's events, and each cell has its own staff console. The per-org step-up (K "PR 4 as built", owner answer) is per cell. |

### Mail per region

- **Primary suite domains are homed in one region each.** KyIdentity gives each suite email domain
  a home region and gives an org mailbox addresses only on its own region's domains (T change, on
  top of #277). The domain's MX records point at its region's MX hosts, and that cell's ledger is
  the authority for every address on it. Routing is DNS alone, and uniqueness is one cell's
  ledger, as today.
  - The alternative is a global address directory consulted at every `RCPT`. It puts a
    cross-region lookup and a new global store in the receive path, and an outage of it would
    refuse mail everywhere. Rejected.
  - The cost: a customer sees which domain belongs to which region, and an org's addresses cannot
    follow it to another region.
- **The masked relay domain stays one domain** (owner decision 7), served by MX hosts in every
  region. Its addresses carry their region:
  - M D3's relay local part is `adjective.animal.` plus 6 random base32 characters. In multi
    mode a seventh character, placed right after `adjective.animal.` and before the 6, is the cell's code from a fixed table (base32, so at most
    32 regions). The 6 random characters stay, so the guessing space is unchanged. K PR 7
    builds D3 with this from the first hosted masked address, so no address lacks a region.
  - A cell allocates only under its own code, so two cells never allocate one address, and each
    cell's tombstones cover only its own code.
  - The MX helper reads the code and forwards `bind` and `accept` to that cell's receiving
    servers over RX R3's pinned TLS. Each MX host is enrolled as a gateway in every cell it
    forwards to (RX R4 per cell). A code that names no cell goes to the MX host's own cell, which
    answers it as unknown and counts it against the source's budget. (MH10)
  - **One message, recipients in two cells.** Each cell holds its own delivery for its own
    recipients. The helper sends `accept` to every cell that bound a recipient and answers `250`
    only after all have committed. If one fails, the sender gets `451` and retries, and the cell
    that committed stores a second copy: at least once, as RX's lost-answer case already is.
  - The probe budget is per cell. A prober who spreads guesses over R cells gets R times the
    budget. That is a residual risk while R is small.
  - A relay subdomain per region (`eu.relay.…`) would need no forwarding, but it is several relay
    domains, which owner decision 7 rules out.
- **Outbound** uses each cell's own relay profile, so a cell's mail leaves from its region.

### Latency budgets

| Path | Budget | Why |
|---|---|---|
| Replica to its cell's PostgreSQL | Under 2 ms round trip at p99 (provisional) | Every fence, CAS and commit. Hosts and the database share one region. The capacity measurement sets the real figure. |
| Synchronous standby (RX R5) | Under 2 ms added per commit (provisional) | Another availability zone of the same region, never another region |
| Fence probe | 1 s interval, 1 s timeout, 2 s bound (unchanged) | Inside one region |
| MX host to a far cell (relay domain only) | 150 ms round trip (provisional) | Two commands per recipient fit easily in RX's 30 s budget (`budget_ms`). |
| A cell to KyIdentity | The orgs poll's cadence and K's 15-minute stale bound | Not on a request path, except sign-in, which already calls KyIdentity |

No request path crosses a region except relay-domain mail and sign-in.

### Failover and split brain between regions

- **Inside a region:** RX R5's synchronous standby in another zone. Promotion and its fencing
  belong to the database service (RX "Past one host" step 3). MH's lock-loss rules apply.
- **Between regions, three options:**
  1. **No cross-region copy:** capsules only. RPO is the backup interval. Cheapest.
  2. **An asynchronous standby in another region of the same residency zone, promoted by an
     operator.** RPO is the replication lag, so mail that got `250` in that window can be lost.
     That breaks R5's promise, so a promotion is a declared disaster with a stated loss.
  3. **A synchronous standby in another region.** RPO 0, but every commit pays a cross-region
     round trip, which breaks the per-commit budget above.

  **Recommended: 2**, with 1 as the floor until it is built. A region's outage then stops its orgs
  until an operator moves the cell. Senders queue and retry inbound mail meanwhile.
- **Split brain between regions.** In normal running there is none: no two cells write the same
  row, and the only flows between cells are stateless (MX forwarding, KyIdentity). The risk is in
  promotion option 2, if the old primary is still alive. Advisory locks are not replicated, so
  nothing in KyPost can detect two primaries. The runbook (MH9 docs) therefore fences first:
  1. stop or isolate the old cell's replicas, receiving servers and primary;
  2. promote the standby;
  3. move the cell's hostname and its domains' MX records;
  4. start the cell's replicas in the new region with the same keys, region ID and pins.

  The promoted database keeps the cell's `meta` record, so a replica pointed at the wrong
  database still refuses on the region and key digest.

### Data residency

An org's mail, documents, accounts, ledger, sessions, logs and capsules stay in its home cell and
its residency zone. What leaves:
- **KyIdentity** is one global issuer (owner decision 2). Sign-in, the directory, the mailbox
  address and the org's region live there. Where KyIdentity runs is T's design.
- **Relay-domain mail** may arrive at another region's MX host and cross to the home cell over
  pinned TLS. The MX host keeps nothing (RX R2): no queue, no store.
- **Staff** reach every cell from wherever they are. That is access, not storage, and the staff
  audit records it per cell.
- **Peer apps** (KyCalendar, KyDrive, KyMessages-Native, KyPulse's replacement) must each have a
  cell in the same region, configured as that KyPost cell's peers. Their region designs are
  theirs.

## Migration

- **Single-tenant installs (SQLite, or R's PostgreSQL replicas on one host): nothing changes
  unless the operator migrates.** Every MH change is selected at open by **table mode**: multi
  mode, or a single install whose `state.db` `meta` holds `volume_stores=postgres`. An install
  without that row keeps its files, its `flock`s, `ErrHost`, the volume IDs and lazy key
  creation. It never creates `volumes.db` or `tenancy.db` (K PR 1).
- **Multi installs: none exist.** Startup refuses `multi` (K PR 1 as built, "K2's other
  prerequisites") and keeps refusing it until K PR 7. The amendment below moves that to after MH7
  and MH9. A multi install starts empty (K2), on PostgreSQL, with `documents` and `accounts` from
  its first start. There is nothing to convert. Test installs built before MH7 are recreated.

### Self-hosted multi-host (opt-in, owner decision 2026-11-22)

The owner will offer multi-host to self-hosted single-tenant installs (open question 1,
overturned). It stays single-tenant: one org, its own administrators, local accounts if it has
them, and its own domain. Only where the volume files live changes, and only on request.

**Where the tables live: a new store, `volumes.db`.** MH1 put `documents` in `tenancy.db`. A
migrated single install must not gain a `tenancy.db`, because code reads that store's presence as
multi mode: `sameOrgCopies` counts one copy per mailbox only when it is absent
(`app/receiving.go:154-156`), a capsule's `tenancy.db` is read as a multi install held by its
ledger (`backup/native.go:73-74`), and `tenancy.Open` says only a multi install's first start
creates it (`tenancy/tenancy.go:159-160`). So `documents` and `accounts` move into `volumes.db`
(catalog name `kypost_volumes`), which:
- a multi install creates at its first start, beside `tenancy.db`;
- `migrate-volumes-to-postgres` creates on a single install, and nothing else does;
- keeps both tables in one store, so the owner guard (a D write locks its `accounts` row in the same
  transaction) is still one database rule;
- joins the backup set and the restore import exactly as `tenancy.db` does (K PR 6), with its
  SQLite DDL only for the capsule copy. Inclusion goes by file name whenever the store exists
  (`snapshotDatabase`, `backup/collect.go:36-38`), never by mode, so a migrated single install's
  capsules always carry it. A run refuses to seal a capsule whose `state.db` has the record but
  whose `volumes.db` is missing or lacks the completion mark;
- is classified in `TestEveryStoreIsClassified` (`documents` D, `accounts` O).

Nothing writes `documents` in production yet, so the move has no data to carry. It must land
before MH2's first store writes a document. The purge's `documents` check
(`tenancy.DocumentRows`) reads `volumes.db` instead.

**`migrate-volumes-to-postgres`** (MH8), an offline command modelled on `migrate-to-postgres`
(`app/migrate_postgres.go:39`, which holds the instance lock at `:53` before it touches
anything):
1. Refuses unless the install is on PostgreSQL (run `migrate-to-postgres` first), on native
   addressing v3 (run the native migrations first), not multi, with a KyRecovery pin (table mode
   refuses local capsule copies, open question 11), and no instance is live
   (`LiveInstances`). It runs on one host with the three shared volumes mounted. It also refuses
   unless table mode's transport rules already hold, so the migrated install can start: the
   database URL uses `sslmode=verify-full` or a loopback host, and the operator passes the
   replicas' TLS certificate and key (`TLS_CERT_FILE`), which it loads and checks. Today's
   documented R setup has neither (`docs/RUNNING.md:257-258,313`), so the guide reconfigures
   both first, while the install still runs on its files.
2. Creates every missing key of the `SECRET_DIR` inventory once, on this host, as the lazy path
   would have, plus `documents.key`, which no file-mode path creates, and the install's
   empty-`org_id` `doc_keys` row (Per-org encryption at rest). It then records MH5's key-set
   digest. From here on every host loads keys and never
   creates them.
3. Handles an existing `volumes.db` that has no `volume_stores` row. One with no completion mark
   (step 5) is a dead earlier run, so it is dropped and recreated. One **with** the mark holds a
   finished migration whose record is missing (deleted by hand, or a crash between steps 5 and
   6), and may hold every write made since. The command refuses it and names
   `migrate-volumes-to-files`, which writes that data back.
4. Copies each classified file into `documents` or `accounts`, one transaction per store, and
   compares each row's SHA-256 with the file's. A D row's body is sealed, so its digest is taken
   after opening it.
5. Writes a completion mark inside `volumes.db`.
6. Writes `volume_stores=postgres` in `meta` last. A run that dies before this leaves the install
   on its files, unchanged.
7. Leaves the files in place. They are stale from the first write after migration and are never
   read in table mode (the volume-free assertion refuses a file-backed store). The operator may
   delete them once the install runs well. Until then they sit on the migrating host only.

**`migrate-volumes-to-files`** (MH8) is the reverse, also offline, on one host with the shared
volumes, refusing while an instance is live. It opens each sealed D body, writes every document and account back to its
file (atomic writes at today's modes), removes a file the tables no longer hold, compares the
digests of the opened bodies with the files, deletes the `volume_stores` row, and then drops `volumes.db`. It also accepts an install
whose `volumes.db` carries the completion mark but whose record is missing, so it can finish a
reverse that died between those last two steps. The install is then an ordinary single-host R
install again, with no `volumes.db`, and must run on one host. Deleting the row by hand is
**not** a rollback: it would lose every write made since the migration.

**An orphaned migrated store refuses the start.** A single install with no `volume_stores` row
whose `volumes.db` carries the completion mark refuses to start and names
`migrate-volumes-to-files`. Serving the stale files instead would lose every write since the
migration. So the one invariant is: an install without the record has no completed `volumes.db`.

**What else a migrated install changes, all keyed on table mode:**
- MH7's host checks are lifted and the volume-free assertion runs, as in multi mode.
- MH5's provisioned keys and key-set digest apply.
- The encryption rules apply ("Encryption in transit"): `sslmode=verify-full` off loopback, and
  TLS from the proxy to every replica.
- **Inventory rows that are "absent" in multi mode exist here,** so they need a target:
  `admin.env` and `first-run-password.txt` (P, `documents`), per-user `imap-config.json` (D,
  `documents`; R installs receive through external IMAP, `docs/RUNNING.md:260-263`), and
  `sender-blocks.json` and `sender-evidence.json` (`documents`; only the single-container Maddy
  and Cloudflare paths use them, which R refuses). `receiving.conf` and the Cloudflare
  credentials stay absent, because R refuses those profiles. The volume-free assertion fails a
  start that writes any of them.
- **Receiving** stays external IMAP, as on R today. The hosted receiving tier stays multi-only
  (RX open question 1); see open question 10 below.
- **Local backup copies** follow open question 3's answer for multi mode: refused once migrated,
  so step 1 also refuses an install with no KyRecovery pin. See open question 11 below.
- **A restore** makes `volumes.db` match the capsule, as it does every other store: a capsule
  taken before the migration has none, so the restore drops the install's `volumes.db`, and the
  restored install is on its files again, on one host. A capsule taken after it restores into
  table mode. A restore **refuses** a capsule whose `meta` holds `volume_stores=postgres` but whose
  `volumes.db` is missing or lacks the completion mark, because neither its tables nor its files
  would hold the data written since the migration. (Dropping a store the capsule lacks, and this
  refusal, are new restore behaviour, built in MH8.)

**Proof that an un-migrated install is untouched** (each in MH8 unless marked):
- `TestSingleTenantUpgradeIsByteForByte` (every MH PR, below) also asserts no `volumes.db`, no
  `volume_stores` row and no key created early, for a SQLite and an R install.
- `TestTopologyGuard`'s single cases stay as they are: an un-migrated R install still refuses
  another boot ID (`ErrHost`) and a missing volume ID.
- `TestUnmigratedInstallOpensOnlyFiles`: with no record, the seam resolves every store to the file
  implementation, the converse of the volume-free assertion, from the same list.
- `TestMigrateVolumesRefuses`: SQLite, v2, multi, a live instance, no KyRecovery pin,
  `sslmode=disable` on a non-loopback host, and a missing or unreadable TLS certificate.
- `TestMigrateVolumesDiesAtEachStore`: a run killed after each store leaves no record, the files
  unchanged, and a second run that succeeds.
- `TestOrphanedVolumesStoreRefuses`: with the record deleted by hand after writes in table mode,
  both the start and a forward run refuse and keep `volumes.db`. The reverse then writes those
  writes back to the files.
- `TestRestoreOfAPreMigrationCapsule`: a migrated install restored from an older capsule has no
  `volumes.db`, and starts on the capsule's files.
- `TestCapsuleRecordNeedsItsVolumesStore`: a backup run with the record but no completed
  `volumes.db` refuses to seal, and a restore of such a capsule (built by hand) refuses.
- `TestMigrateVolumesRoundTrip`: forward then reverse with no change in between leaves every
  volume file's bytes and mode as they were, no record and no `volumes.db`. Between the two, every
  D row's body is sealed (no test name or address in a dump) and opens to the file's bytes.
- `TestMigrateVolumesReverseCarriesWrites`: a contact added, a rule removed and a settings change
  made in table mode are in the files after the reverse.
- The volume-free test and an in-process two-host test run in migrated single mode as well as in
  multi mode.

### Proof that single-tenant stays byte for byte

- **`TestSingleTenantUpgradeIsByteForByte`** (`app/tenancy_upgrade_test.go:213`) compares every file,
  every SQLite schema and row, and on the PostgreSQL job every catalogued schema, the fingerprint
  record included, across a restart with `KYPOST_TENANCY` unset and `single` (K "PR 1 as built").
  **Every MH PR extends its fixture** to exercise the stores that PR moves: a contact, a rule, a
  photo, a settings change, a DKIM rotation, a WKD domain and a pickup link. It then asserts that
  the volume bytes are unchanged and that no `tenancy.db` or `volumes.db` exists.
- **`TestTopologyGuard`** (`replica/replica_test.go:229`) keeps every refusal in single mode,
  including `ErrHost` for another boot ID and `ErrVolumes` for a missing volume ID. MH7 adds the
  multi-mode cases beside them without changing the single-mode cases.
- **The seam test.** Each moved store runs its existing tests against both implementations, as
  RX PR 3's `eachHome` does (`ingress/senders_test.go:29` in RX's "PR 3 as built"). The file
  implementation must pass unchanged.
- **The volume-free test** (MH7), the converse proof for multi mode: a multi fixture runs sign-in,
  provisioning, mail, contacts, settings, masked addresses, DKIM rotation, a scheduled send, a
  pickup link, an import and a backup. SECRET_DIR is mounted read-only and holds only provisioned
  keys. Every step succeeds. The test then diffs the three roots against their state before the
  run: the only paths allowed to change are the inventory's "per host" rows (`imports/upload-*`,
  `mail-bulk.json`, `recovery.pub`), each matched by an explicit pattern list kept beside
  `TestEveryStoreIsClassified`. Any other changed path fails, and so does a per-host pattern that
  nothing in the inventory names. `backups/` is not on the list: open question 3 refuses local
  copies. The test runs in multi mode and in a migrated single install.

## Phased build plan

Each PR ships on its own. Single-tenant behaviour is unchanged in every PR, and each extends the
upgrade test (above). Startup keeps refusing `multi` throughout (K PR 1), so multi paths run only
in tests, as K's do (`makeMultiTenant`).

**Changed by the 2026-11-22 answers.** Every PR from MH2 on selects its tables by **table mode**
(multi, or a single install with `volume_stores=postgres`), not by multi mode alone, and runs its
tests in both. Nothing can set the row before MH8, so MH2–MH7 change no single install in
practice. MH8 (self-hosted migration), MH9 (cells) and MH10 (cross-region relay routing) are new.

1. **MH1: the seam and fences.** The `vol` interface (file and table implementations), the
   `documents` table, and the fence type: `Hold` with R's probe, a shared mode, and the fence
   names. Nothing uses them in production yet.
   Tests:
   - CAS: a stale revision is refused, two writers race on an update and on a first create and in
     each case one wins with the other retrying, a writer that read before a remove and
     re-create is refused (ABA) whatever the delay, and
     the retry bound gives `503`;
   - boundary: an absolute path, `..`, NUL, an empty segment, oversize paths and bodies, all refused
     before SQL;
   - fences: exclusive against shared, a writer not starved by a stream of readers (the
     advisory-lock queueing claim above), a probe killed mid-section cancelling the holder, and
     a second replica winning only after the loss.
   **MH1 as built** (merged as #701; references re-checked at `fb0d6aab`).
   - **The seam** is `backend/internal/vol`: `Vol` (`vol.go:16-24`), `CheckPath` (`vol.go:50-60`),
     `File` (`file.go`) and `Table` (`table.go`), plus `Update`, a read-modify-write that retries
     a stale write at most 5 times and then returns `ErrBusy` (`vol.go:78-93`). Callers answer
     `503` with it. No handler serves documents yet, so the `503` itself is unproven until MH2.
   - **`File` checks no revision.** It is `fsutil.AtomicWriteFile` at mode 0600 and plain `os` reads,
     and a present file reads as revision 1. A store's own flock still orders its writes on one
     volume, as today.
   - **The global sequence is the `revision` column's identity**
     (`GENERATED BY DEFAULT AS IDENTITY`, `tenancy/tenancy.go:141-146`). A create takes its
     default, and an update sets `revision = nextval(pg_get_serial_sequence('documents',
     'revision'))`. Using the identity means a restore needs no new code: `ImportSQLite` already
     restarts every identity above the copied maximum (`dbbackend/copy.go:710-720`).
     `TestTenancySurvivesARestore` (`backup/tenancy_test.go`) now carries a document at
     revision 41 and checks that the next revision is above it.
   - **`Table` is PostgreSQL only** (`NewTable` refuses SQLite). The SQLite DDL
     (`tenancy/tenancy.go:86-91`) exists only for the capsule copy. Both DDLs carry the path `CHECK`.
     `CheckPath` also refuses `.` segments, backslashes and invalid UTF-8.
   - **Owners.** A `users/<id>/…` path gives the owner, and any other path is a P row. A `mailboxes/`
     or `pickup/` path is refused (`table.go:41-57`) until MH2 adds the ledger and record owners,
     so no D row is ever stored without one.
   - **Classification.** `documents` is D in `TestEveryStoreIsClassified`. The purge's final check
     gained a `documents` part (`tenancy.DocumentRows`, `api/tenancy_purge.go`), so a document of
     a purged owner keeps the purge from finishing (`TestPurgeFinalCheckFindsADocument`). Deleting
     those rows is still MH2's, together with the first store that writes them.
   - **Fences** are `state.WithFences` (`state/fence.go`). The fence names are the `Fence*`
     constants, and a per-ID kind's name is its prefix plus the ID. The fixed order is
     `fenceOrder`. Settings → native-domains → sso-lifecycle → quota is today's flock order
     (`api/masked_addresses.go:166-168`, `sso/native_mailbox.go:138-144`,
     `app/receiving.go:54-57`). The places of org, mailbox, sso and backup-operation are
     unverified, and MH4 confirms them.
   - **Changed from the design: fences have their own connection pool.** Each store's
     PostgreSQL pool holds at most 4 connections (`dbbackend/dbbackend.go:314`). A held fence keeps
     its connection, so fences taken from that pool would let five fenced requests starve every
     other `state.db` query, their own work's included. That wait would be unbounded. MH1's
     first writer-preference test ran into exactly this. So fences take their sessions from
     `fencePool`: one pool per schema, shared by every `state.Store` in the process, capped at
     `maxFenceSessions` (32, provisional until the capacity measurement). Only a successful
     open is kept, and a pool keeps no idle connection. A set that cannot get a session within
     the wait is `ErrFenceWait` on every Store of the process
     (`TestFenceSessionsDoNotStarveTheStore`). Fences are on the global `state.db` only, so open
     question 5's connection budget must add up to 32 connections per process from MH4 on, when
     fences are first used. `docs/RUNNING.md` is
     unchanged until then.
   - **The bounded wait** covers the whole set: 10 s (`fenceWait`), enforced by the context and by
     a session `lock_timeout`. A lock timeout (`55P03`) and a deadlock that PostgreSQL detects
     (`40P01`) are reported as `ErrFenceWait` too. The error names only the kind.
   - **Tests** (PostgreSQL unless noted):
     - `vol`: `TestStaleRevisionIsRefused`, `TestUpdateRaceOneWinsTheOtherRetries`,
       `TestFirstCreateRace`, `TestABAAfterRemoveAndRecreate`, `TestRetryBoundGivesBusy`,
       `TestDocumentsCheckRefusesBadPaths`, `TestOwnerComesFromThePath`,
       `TestListStaysUnderItsPrefix`, plus `TestBoundaryRefusesBeforeSQL` and
       `TestFileWritesWhatTheStoresWrite` on any backend;
     - `state`: `TestFenceExclusiveAgainstShared`, `TestFenceWriterNotStarvedByReaders`,
       `TestFenceLossCancelsTheHolderAndOnlyThenAdmitsAnother`, `TestFenceCollisions` (forced
       collisions in one set and across two), `TestFenceCycleEndsInABoundedWait`,
       `TestFenceSessionsDoNotStarveTheStore`, `TestFenceSetsRefuseBadNames`, and
       `TestFencesNeedPostgres` on SQLite;
     - `TestSingleTenantUpgradeIsByteForByte` now also asserts that no `documents` table exists.
       It runs on both backends.

2. **MH2: platform and per-user documents.** First moves `documents` from `tenancy.db` into the new
   `volumes.db` store, with its backup and restore inclusion and its classification
   ("Self-hosted multi-host"); `tenancy.DocumentRows` reads it there. Then every `documents` row
   in the inventory, through the seam, selected at open by table mode. Adds `contact-photos` to
   `TestEveryStoreIsClassified`. Needs K PR 6's `tenancy.db` in the backup set, whose path
   `volumes.db` follows.
   Tests:
   - each store's existing tests on both implementations;
   - a restore of a capsule that carries documents;
   - the root `config.yaml` change seen by a second replica within 5 s;
   - K PR 6's purge removing every document of the org by `owner`, pickup links included,
     checked against the classification;
   - the purge's final check failing while a document of a purged owner remains;
   - expired-pickup cleanup;
   - a cross-org case in `TestCrossOrgMatrix` per new document kind;
   - the upgrade test asserting no `volumes.db` on a single install;
   - sealing (Per-org encryption at rest): every D row's `body` in the database holds no
     plaintext (a test address and name are absent from a dump); a body moved to another path,
     owner or org fails to open; two replicas creating one org's data key at once agree on one
     key; a missing `documents.key` refuses the start; an unopenable body fails the read; the
     purge removes the org's `doc_keys` row; a writer paused before creating the org's key and
     resumed after the purge creates none (MH3, with the owner guard); the purge's final check
     failing while the org's `doc_keys` row remains; P rows stay readable.
3. **MH3: accounts.** The `accounts` table, in `volumes.db`, per-row invariants, the `REPEATABLE READ` read-only
   snapshot, and `SERIALIZABLE` with a bounded `40001` retry for every write that checks a
   whole-set invariant (the last-administrator rule and the demotions checked against it), plus
   the per-org fence between provisioning and the purge snapshot, and the purge's final re-check
   (The `accounts` table).
   Tests:
   - every users-store test on both implementations;
   - two concurrent demotions of the last two administrators on two replicas: exactly one commits;
   - two unbound accounts coexisting;
   - provisioning racing a purge snapshot: the new account is refused or purged, and never
     survives (including with the fence forced lost);
   - the owner guard on D documents: a writer paused across the purge, then resumed and
     retried, writes nothing, because the owner's account row is gone;
   - PR 2's write-once and K7 tests in multi mode;
   - a uniqueness race between two replicas creating one subject, where one wins and the other
     gets `ErrSSOSubTaken`;
   - a snapshot taken during a write.
4. **MH4: cross-store fences and job fences.** The domain, directory, settings, quota, mailbox,
   SSO and backup-operation fences, with `outbox-idle` dropped in table mode. Also the
   catalog-tombstone purge fence, the list of elected jobs with their fences, and, in table mode,
   the refusal of local capsule copies (open question 3).
   Tests:
   - each fence's ordering;
   - a fence lost mid-section, which fails its CAS and writes nothing stale;
   - an import worker paused past the purge, whose write fails on the tombstone;
   - a backup operation on replica A refusing one on replica B.
5. **MH5: provisioned keys.** In table mode, no key is created at runtime. A missing key refuses
   the start. The key-set digest (HMAC-SHA256 of each key file's SHA-256 under R's
   `kypost:replica-config` subkey, `replica/AGENTS.md`, local contracts) is stored in `meta` and
   compared at start, naming the key, never a value. A key rotation is offline, like
   `accept-replica-config`.
   Tests:
   - a missing key;
   - a different key on one host;
   - a re-pin on replica A, then a backup run on host B, whose stale `recovery.pub` is replaced
     from the sealed record and the deposit made to the new key; a record that does not open
     still refuses;
   - every `LoadOrCreateKey` and `CreateExclusive` call site reached in multi mode;
   - single mode still creating keys lazily.
6. **MH6: shared meters and leases.** The meters marked "shared window" above move to R's
   `TakeAttempt` (`state/coordination.go:34`), keyed through `sharedKey`
   (`api/shared_state.go:56-61`). This PR also adds the per-org import and export lease and the
   two-replica quota test.
   Tests: each meter giving the same total across two servers, and each falling back to `503`,
   never to memory, when the database fails (`api/shared_state.go:21-22`).
7. **MH7: lift the host checks in table mode.** In table mode the boot-ID and volume-ID checks are
   skipped and the volume-free assertion runs. This PR also adds the multi-host Compose profile
   and docs: separate volumes per host, a read-only `SECRET_DIR`, PostgreSQL with `sslmode=verify-full`
   and the keepalive settings, and session pooling. It also makes every hop of "Encryption in
   transit" encrypted: TLS from the proxy to each replica, a plain listener refused off loopback,
   `verify-full` on PgBouncer and replication connections, and HTTPS required for KyIdentity, the
   peers, KyRecovery, the push relays, Ollama and the SMTP relay unless on loopback. The same PR
   updates `docs/RUNNING.md` and `replica/AGENTS.md`.
   Tests:
   - `TestTopologyGuard` multi and migrated cases: another boot ID admitted, a file-backed store
     refused;
   - a plain listener on a non-loopback address refused, and each `http://` URL above refused, in
     table mode only; single mode unchanged;
   - the volume-free test;
   - **an in-process two-host test.** Startup still refuses `multi`, so this runs as K's tests do:
     two servers built with `makeMultiTenant`-style wiring on one PostgreSQL. Each has its own
     temporary roots and calls `replica.Start` with its own `BootID` (the field is injectable,
     `replica/topology.go:45`). It runs `TestTwoReplicasSeeOneSuspension`, a
     sign-in on A followed by a request on B, and a fence and role handover when A is killed.
   The **container** two-host harness needs a startable multi mode, so it is K PR 7's: two
   containers with separate volumes and different kernels' boot IDs.
8. **MH8: self-hosted multi-host.** `migrate-volumes-to-postgres` and `migrate-volumes-to-files`
   ("Self-hosted multi-host"), the inventory targets for the rows multi mode lacks, the refusal
   without a KyRecovery pin, the completion mark and the orphaned-store refusal at start, a
   restore dropping a `volumes.db` its capsule lacks, and the operator guide in `docs/RUNNING.md` (a new "Replicas on
   several hosts (self-hosted)" section) and `replica/AGENTS.md`. Needs MH1–MH7. It is not a gate
   for K PR 7.
   Tests: the list under "Proof that an un-migrated install is untouched", plus a container run of
   a migrated install as two containers with separate volumes on one database. A single install
   can start, so this harness does not wait for K PR 7.
9. **MH9: cells.** `KYPOST_REGION` recorded in `meta` with K1's record and refused in single
   mode; the `orgs` poller caching only the cell's own orgs; sync events for another region's org
   refused without a write; the region code table that names each cell's masked relay character;
   and the disaster-recovery runbook. MH9 does not allocate masked addresses: K PR 7 builds D3
   and uses this table, so MH9 merges before K PR 7 with no circular dependency. Needs T's `region` field on orgs and on suite
   email domains.
   Tests:
   - a replica with another region, or none, refused; single mode refusing `KYPOST_REGION`;
   - an org of another region refused at sign-in, device sign-on, a sync event and `bind`, as an
     uncached org is, with nothing written;
   - a sync event for a not-yet-cached org refused as temporary, then accepted after the poll;
   - the code table: one code per region, each a single base32 character, no two regions sharing
     one (masked allocation under the cell's code is K PR 7's test);
   - the upgrade test unchanged.
10. **MH10: cross-region relay routing.** The MX helper's endpoint list per region code, gateway
    enrolment per cell, and an unknown code sent to the MX host's own cell. Needed before a second
    region opens, not before K PR 7.
    Tests: an address of cell B received at cell A's MX host lands in B; one message to
    recipients in A and B lands in both before `250`, and a failure in B answers `451`; an
    unknown code is refused and counted like an unknown address; a cell that does not pin the MX
    host refuses it.

**Against K and RX.**
- MH1 has merged (#701). MH2 needs K PR 6's backup inclusion of `tenancy.db`, and starts by moving
  `documents` to `volumes.db`.
- RX PR 4 is built (#694). Its purge fence is the quota lock and a re-read of the org under it,
  as this note assumed. That is safe on one host, and multi mode cannot start yet. MH4 adds the
  catalog tombstone and moves RX's lease to database time. Both must land before multi starts
  (K PR 7's gate).
- RX PR 5's hosted profile and MH7's multi-host profile are one Compose profile. Whichever lands
  second extends the first.
- **K PR 7 accepts `multi` only after MH1–MH7, MH9 and RX PRs 1–5,** and per-org staff step-up,
  which has merged (#703; K "PR 4 as built", owner answer 5). K's "Phased build plan" is the
  full list. MH9 must come first because
  the region record and the masked region character must exist from the first multi start. It also adds MH7's prerequisites
  to its K2 checks (PostgreSQL, `sslmode=verify-full` on a non-loopback host, a provisioned
  `SECRET_DIR`).
- RX's "Past one host" (RX steps 1 and 4) is replaced by this plan. MH10 changes RX's MX helper
  (endpoints per region code) and RX R4's enrolment (one gateway row per cell). RX step 3 (PostgreSQL as its
  own replicated service with a synchronous standby, RX R5) stays a launch prerequisite outside it.

## Threat model

| Attacker or fault | Goal or effect | Defence |
|---|---|---|
| Network attacker between a replica host and PostgreSQL | Read or change every org's data in transit | Today's Compose URL uses `sslmode=disable` on one Docker network (`docs/RUNNING.md:313`), and the code sets no TLS mode of its own (unverified beyond a search of `dbbackend` and `config`). Multi mode refuses a database URL whose `sslmode` is not `verify-full` unless the host is loopback (MH7, and K PR 7's K2 check). |
| A rogue or misconfigured host joining | Serve with other keys, settings or mode | Joining needs the database credentials. Then K1's tenancy record, R's settings fingerprint and MH5's key-set digest each refuse a mismatch before anything listens. |
| Anyone holding a database copy, a dump or a capsule's `volumes.db`, without `SECRET_DIR` | Read members' contacts and other per-user documents | Every D row's body is sealed under its org's data key, and the data keys are wrapped under `documents.key` (Per-org encryption at rest). Bookkeeping columns stay readable: paths, owners and revisions. |
| A compromised replica host | Read every org | Unchanged in kind: any replica holds every key and the database, today and in multi mode (K residual risks). More hosts mean more such machines. Keys are mounted read-only from the secret store, so a host compromise cannot plant a new key for the others to accept. |
| Member of org A | Reach B's documents by path | Document paths are built by the same helpers from the caller's own admitted IDs (`config.SafeUserPathComponent`). The table validator refuses traversal. `TestCrossOrgMatrix` and `TestOrgAdmitCallSites` (K PR 3) keep running against the table implementation. |
| Oversize input | Fill the database through one document | The per-document cap at the write, plus each store's own caps. Per-org storage quotas cover mail, not documents (K PR 5 owner answer 5). The capacity measurement sizes documents per user. |
| A holder that lost its fence | A lost update | CAS on every document (above). DB constraints cover the ledger (`addresses.address` primary key) and the accounts (`UNIQUE`). |
| A paused import worker after a purge | Recreate a deleted org's mail | The catalog tombstone (MH4) |
| Stale reads on another host | Serve a revoked account or old settings | No cross-replica cache without a revision check. `currentUser` re-reads per request, and the root config's 5 s re-read becomes a revision read. |
| Network attacker between the proxy and a replica on another host | Steal session cookies or change responses | Today that hop is plain HTTP (`docs/RUNNING.md:257-258`). Table mode serves TLS and refuses a plain listener off loopback (MH7). |
| A self-hosted operator, or a script, deleting the `volume_stores` row by hand | Roll back to stale files | The completed `volumes.db` remains, so the start and a forward run both refuse and name `migrate-volumes-to-files` (MH8). Nothing is served from the stale files. |
| An org of region A presented at cell B | Reach another cell's data, or be provisioned twice | Cell B caches only its own orgs and refuses an uncached org at sign-in, device sign-on, sync and `bind`, writing nothing (MH9). KyIdentity sends events only to the home cell. |
| A prober spreading relay-domain guesses over regions | Find live masked addresses | The budget is per cell, so R cells allow R times the budget. The 6 random characters are unchanged. |
| A compromised MX host | Send mail into any cell | It is a gateway in each cell it forwards to, under its own ID only (RX R4), and each cell can retire it alone. |

## Failure modes

| Failure | Effect | Data |
|---|---|---|
| Host lost mid-document-write | The transaction rolls back. The request fails, and the client retries. | Nothing partial: one document is one transaction. |
| Host lost between the ordered steps of a cross-store write (allocation, provisioning) | The same as a crash today. The existing repair runs on the next event or sign-in (K "PR 2 as built", "Write order and repair"). | No change from one host |
| Host lost while holding a fence or a role | PostgreSQL drops its locks once it notices the dead connection (the keepalive setting). Another replica takes over after `Grace` for roles, or at once for fences. | Protected by CAS and the job fences |
| Partition: the holder is alive, but its connection to PostgreSQL is dead | Its probe cancels its work within 2 s. Its writes fail, because they also need PostgreSQL. | Nothing written |
| Partial partition: only the lock connection is dead | The holder writes for up to 2 s while another replica may hold the fence. | Its CAS fails, so there is no lost update. A non-document side effect in that window (an outbound SMTP call) can happen twice. Today's outbox claim token already fences sends. |
| PostgreSQL failover | Every lock is lost, and every role re-elects. | Nothing committed is lost with a synchronous standby (RX R5). With an asynchronous one, the last commits can be lost, which R5 forbids. |
| Different keys on one host | That host refuses to start (MH5). | — |
| A host with a stale image | Mixed versions are not supported (`docs/RUNNING.md:348`). The upgrade stops every replica on every host, which is a full outage across hosts. | — |
| `migrate-volumes-to-postgres` dies mid-run | The install stays on its files; the next run recreates `volumes.db` | Nothing lost: the record is written last |
| A whole region lost | Its orgs are down until an operator moves the cell (Regions, failover option 2). Other cells are unaffected, except relay-domain mail for the lost cell, which their MX hosts answer `451`. | Option 2: the replication lag. Option 1: back to the last capsule. |
| KyIdentity unreachable from a cell | Sign-in fails. After K's 15-minute stale bound, sending fails closed too; receiving keeps the last status. | — |
| A cell's MX hosts cannot reach another cell | Relay-domain mail for that cell gets `451` and is retried by senders | Nothing lost |

## Residual risks

- **One PostgreSQL.** Every host depends on one database service. Multi-host removes the host as a
  single point of failure, not the database. RX step 3 (a synchronous standby) is still a launch
  prerequisite.
- **A full outage on every upgrade**, across every host (R). A rolling upgrade needs mixed-version
  support, which is out of scope.
- **Duplicate side effects in a partial partition.** CAS stops lost updates, but not a push, a
  verification mail or an SMTP call made twice. The outbox claim fences sends. The others are at
  least once, as today.
- **Per-replica meters** that stay per replica allow N times their limit (above). N is now larger.
- **Advisory-key collisions** are negligible with 64-bit fence keys but still possible. The bounded
  wait turns one into a `503`, not a hang.
- **More machines hold every key.** The operator and staff are trusted, as in K.
- **Document scale is unmeasured.** Per-user documents are rewritten whole, as the files are. A
  very large `contacts.json` costs one large row write per change. The capacity measurement (K
  open question 8, RX "Capacity measurement") must include documents per user and write rates.
- **K's residual risk "One install, one database, one host"** (K "Residual risks") now reads
  "one host until MH7 ships".
- **Stale files on a migrated host.** `migrate-volumes-to-postgres` leaves the old files on the
  migrating host until the operator deletes them. They are never served: without the record, a
  completed `volumes.db` refuses the start. Dropping `volumes.db` by hand as well would serve
  them. Only the reverse command is a rollback.
- **A cell is one database.** A region's outage stops its orgs. Cross-region recovery is manual
  and loses the replication lag (Regions).
- **KyIdentity is a cross-region dependency.** Every cell signs in through the one issuer.
- **Regions are visible.** A masked relay address's region character (the first after
  `adjective.animal.`) and a primary domain each show an org's region.
- **No org moves between regions** in this plan (open question 12).

## Open questions: owner decisions (2026-11-22)

1. **Multi-host for self-hosted single-tenant R installs?** *Overturned (2026-11-22): yes.* The
   recommendation was no in v1. The owner will offer multi-host to self-hosted installs, so it is
   an opt-in migration: an install upgrades byte for byte and stays on its files until an operator
   runs `migrate-volumes-to-postgres`, which `migrate-volumes-to-files` reverses ("Self-hosted
   multi-host"). Changed PRs: MH2 (the `volumes.db` move, table-mode selection), MH3, MH5 and MH7
   (table mode), MH8 (new). Proof: "Proof that an un-migrated install is untouched".
2. **Object storage for photos and large documents?** *Approved as recommended (2026-11-22):* no.
   PostgreSQL `BYTEA` with a per-document cap set from MH2's measured bounds (16 MiB is a
   placeholder).
3. **Local backup copies in multi mode?** *Approved as recommended (2026-11-22):* refused, and a
   KyRecovery deposit is required, so no capsule sits on whichever host ran the job (MH4). This
   note applies the same rule to a migrated single install (open question 11).
4. **Database TLS.** *Approved as recommended (2026-11-22):* `sslmode=verify-full` for any
   non-loopback host. The owner wants full encryption in transit, so every hop is covered
   ("Encryption in transit", MH7).
5. **Connection pooling.** *Approved as recommended (2026-11-22):* direct connections or PgBouncer
   in session mode, never transaction pooling (`state/coordination.go:166-167`), with
   `max_connections` sized by R's budget formula (`docs/RUNNING.md:450-459`) times the hosts of
   one cell, plus MH1's 32 fence sessions per process.
6. **Which per-replica meters to share.** *Approved as recommended (2026-11-22):* the five "shared
   window" rows (nine meters), MH6.
7. **One table per store instead of `documents`?** *Approved as recommended (2026-11-22):* one
   `documents` table, except `users.json` (MH3) and M's masked records (K PR 7).
8. **Region.** *Overturned (2026-11-22): multi-region is required.* The recommendation was one
   region. Redesigned as cells, one home region per org ("Regions"): every fence and CAS stays in
   one region, and only routing crosses regions. New PRs MH9 and MH10.
9. **`outbox-idle` in multi mode.** *Approved as recommended (2026-11-22):* dropped (MH4).

### New questions from the redesign (each with the default this note takes)

10. **The hosted receiving tier for migrated self-hosted installs?** *Default:* no. They keep
    receiving through external IMAP, as R installs do today, and RX stays multi-only (RX open
    question 1). Revisit if a self-hosted operator needs their own MX hosts.
11. **Local backup copies after a self-hosted migration?** *Default:* refused, as in multi mode,
    and the migration refuses an install with no KyRecovery pin. The alternative keeps local
    copies on the runner's host and shows that host in the backup status. It is friendlier to a
    self-hoster without KyRecovery, but a capsule would sit on one host only.
12. **Moving an org between regions.** *Default:* not in this plan. A move would need an export
    and import between cells, and new addresses on the new region's domain. Staff can do it today
    only as delete and re-create.
13. **Suite email domains homed per region.** *Default:* yes ("Mail per region"), which needs a
    KyIdentity change on top of #277. The alternative, a global address directory read at every
    `RCPT`, is rejected above.
14. **Cross-region recovery.** *Default:* option 2, an asynchronous standby in another region of
    the same residency zone, promoted by an operator after fencing the old cell. Option 1 (capsules
    only) is the floor until it is built.
15. **Residency zones.** Which regions may hold a standby or a KyRecovery deposit for which other
    region is a legal and product decision. *Default:* only regions in the same jurisdiction.
16. **Seal per-user files on un-migrated single-tenant installs too?** *Default:* no. That would
    change a self-hosted install's files on upgrade, which the byte-for-byte rule forbids.
    Contacts on those installs stay plaintext, as `SECURITY.md` documents. The alternative is an
    opt-in sealed file mode.
