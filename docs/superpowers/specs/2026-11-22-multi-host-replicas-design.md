# KyPost multi-host replicas for multi-tenant mode, design

Date: 2026-11-22. Status: proposed design. Nothing is built.

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

Line references are to KyPost `origin/main` at `3e467f2b`. Paths are relative to
`backend/internal/` unless they start with `docs/` or a top-level folder. The references were
checked by reading the code at that commit. "Unverified" marks a claim this note did not check in
code or by running it. **Nothing here was run.** Every statement about behaviour comes from reading
code.

## Summary

- **What binds replicas to one host is small and countable.** Everything already in PostgreSQL
  (every `.db` store, sessions, windows, replay guards, elections) or in process memory works with
  any number of hosts. Only four things depend on the host:
  1. the files on the three shared volumes;
  2. the `flock`s that fence them;
  3. the boot-ID and volume-ID checks that refuse a replica on another host;
  4. the keys in `SECRET_DIR`, which are created lazily, on whichever replica gets there first.
- **In multi mode, every volume file that a replica writes or must share moves into PostgreSQL.**
  Nothing is left for a second host to see or lock. Two new multi-only tables go in `tenancy.db`.
  `documents` holds the JSON and YAML stores keyed by their root-relative path, so each store
  keeps its own format and rules. `accounts` holds `users.json` as one row per account. No object
  storage and no new dependency are added.
- **Locks give liveness; revisions give safety.** Every document write is a compare-and-swap on
  the document's revision inside one transaction, so a lost lock can cause a retry but never a lost
  update. The cross-store fences (the domain fence, the directory fence, the settings lock and the
  per-mailbox quota lock) become PostgreSQL advisory locks with R's ownership probe.
- **Keys are provisioned, not created.** In multi mode every host mounts the same read-only
  `SECRET_DIR` from the orchestrator's secret store. A missing key refuses the start; it is never
  minted. A keyed digest of the key set in `meta` refuses a host with other keys.
- **The host checks change in multi mode only.** The boot-ID check (`replica.ErrHost`) and the
  volume-ID files give way to one assertion: no store a replica writes is file-backed. Single-tenant
  installs, SQLite and R's single-host replicas, keep every check, file and lock byte for byte.
- **K PR 7 must not accept `multi` until multi-host works.** This note's PRs MH1–MH7 come before
  it. The amendment is in K's "Phased build plan" in the same PR as this note.

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
`TestEveryStoreIsClassified`'s list (`tenancy/inventory_test.go:71-105`) extended with a target
column (below). A store with a file target, other than one marked "per host" in this note, refuses
the start. This is the check that makes removing `ErrHost` safe: a host can join only when nothing
it writes lives on its own disk.

### Every host-local lock

All volume locks go through `fsutil` (`fsutil/lock.go:29` `WithFileLock`, `:42` `LockFile`, `:48`
`LockFileContext`, `:54` `LockFileShared`, `:62` `LockFileGated`), except four direct `flock`s. A
`flock` holds on one kernel and one inode only (`docs/RUNNING.md:240-245`).

| Lock (file it fences) | Call sites | What it orders | Replacement in multi mode |
|---|---|---|---|
| `users.json` | `users/users.go:839` (shared reads), `:871` (gated writes), `:1396`; `users/native_recovery.go:35,120` | Every account write against every snapshot reader | The `accounts` table's own transactions (MH3). Writers lock the rows they change (`SELECT … FOR UPDATE`). A whole-set snapshot reads in one `REPEATABLE READ` transaction, so no writer waits for a reader. |
| **The domain fence** (`native-domains.json`) | `sso/native_domain.go:363,448,511`; `sso/native_mailbox.go:144`; `sso/native_address.go:318`; `sso/native_allocate.go:65`; `sso/hr_mailbox.go:28`; `sso/native_outbound.go:94` (shared); `api/native_relay_handlers.go:124,282`; `api/native_recovery_handlers.go:312`; `app/receiving.go:502` (shared); `app/receiving_config.go:231`; `app/migrate_native.go:75` | Domain proof, address and mailbox allocation, outbound admission and the receiving `config` fetch against one another. It spans the domains document, the ledger (`native-authority.db`) and `users.json`. | A **fence** (below): a session advisory lock named `fence:native-domains`, with shared and exclusive modes and R's probe. |
| **The directory fence** (`sso-lifecycle.json`) | `sso/directory.go:284` (gated), `:291` (shared) | Every lifecycle write in every process (`sso/directory.go:277-278`) | A fence named `fence:sso-lifecycle`. |
| The settings lock (per-user `config.yaml`) | `config/config.go:519`; `api/masked_addresses.go:176`; `api/native_mailbox_handlers.go:286` | Settings and masked records, before the domain fence and then the directory fence (`api/masked_addresses.go:166-168`, order settings → domains → directory) | A fence named `fence:settings:<user ID>`, taken in the same order. |
| `quota-<mailbox>` | `app/receiving.go:61` | The mailbox quota check against its write. RX PR 4 also uses it as the fence between an import write and the purge (RX "Import leased per mailbox"). | A fence named `fence:quota:<mailbox>`. The purge's safety must not depend on it alone: see "The purge and a lost fence". |
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
`state/coordination.go:281-296`), with four changes, all made only in multi mode:

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
   exclusive one. Whether it does so for advisory locks is unverified. MH1's tests must show it.
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
- **`documents`:** a row in the new multi-only `tenancy.db` table (MH2);
- **`accounts`:** the new multi-only `tenancy.db` table (MH3);
- **already PG:** the store is already on PostgreSQL in every R install;
- **absent:** not present in multi mode;
- **per host:** stays on the host's own disk, with the reason.

The file names are the ones `TestEveryStoreIsClassified` lists (`tenancy/inventory_test.go:71-105`).

| File | Defined | Class | Target | Why |
|---|---|---|---|---|
| `users.json` | `app/bootstrap.go:47`, `users/users.go:701,812` | O | `accounts` | One file holds every customer's account and is rewritten whole under one lock (K "Replicas", "Scale before launch"). One row per account lets writers lock one account. |
| `admin.env`, `first-run-password.txt` | `app/bootstrap.go:83` | – | absent | K6 |
| Root `config.yaml` | `config/config.go` (root config) | P | `documents` | Edited at runtime from the admin settings and re-read by every replica within 5 seconds (`docs/RUNNING.md:285`). The re-read becomes a revision check. |
| `mail-defaults.json` | `api/server_mail_defaults.go:26` | P | `documents` | Admin-edited at runtime |
| `sso.json` | `sso/sso.go:94` | P | `documents` | Admin-edited at runtime |
| `sso-lifecycle.json`, `native-provisioning.json` | `sso/lifecycle.go:91`, `sso/native_provision.go:73` | P | `documents` (stubs) | v3 leaves them as stubs (K inventory). The stubs are still read, and the directory fence names the lifecycle file. Whether a v3 multi install reads the stubs' contents is unverified, so MH2 moves them rather than dropping them. |
| `native-domains.json` | `sso/native_domain.go:27` | P | `documents` | Platform domains (K9). It is also the domain fence's name. |
| `native-relay.json` (relay credentials and **every DKIM private key**, sealed under `native-relay.key`) | `api/native_relay_handlers.go:47` | P | `documents` | The sealed bytes move unchanged. The sealing key stays in `SECRET_DIR` (MH5). |
| `wkd-domains.json` | `wkdpublish/store.go:74` | P | `documents` | |
| `receiving.conf` | `api/native_recovery_handlers.go:379` | P | per host (MX host) | In multi mode an MX host renders its own profile from the replica's `config` answer (RX PR 2 as built, "`config`"). Whether a multi replica still writes `receiving.conf` is unverified. MH7's volume-free assertion fails if it does, and the fix is then to make it absent. |
| `quota-<mailbox>` | `app/receiving.go:61` | D | fence (lock only, no data) | See the lock table above. |
| `sender-blocks.json`, `sender-evidence.json`, `sender-evidence.damaged.json` | `ingress/blocks.go`, `ingress/autoblocks.go` | – | absent (already PG tables) | RX PR 3 |
| Per-user `config.yaml` (settings, signatures, masked records) | `config/config.go:414-416` | D | `documents` | M D8 moves the masked records into their own table in hosted mode (K PR 7). The rest of the settings become one document per user. |
| Per-user `imap-config.json` | `config/config.go:407` | – | absent | K6: there is no external-IMAP source in multi mode. |
| Per-user `tuning.md` | `config/config.go:411` | D | `documents` | |
| Per-user `carddav-auth.json`, `carddav-client.json` | `api/server_userscope.go:246-255` | D | `documents` | `carddav-auth.json` is re-read on every use (`docs/RUNNING.md:283`). That becomes one indexed row read. |
| Per-user `contacts.json`, `groups.json`, `rules.json`, `send_as_aliases.json` | `contacts/store.go:64`, `groups/store.go:44`, `rules/store.go:43`, `sendas/store.go:62` | D | `documents` | |
| Per-user `pgp-discovery.json`, `pgp-discovery-suppressions.json` | `pgpdiscovery/settings.go:23`, `pgpdiscovery/suppress.go:28` | D | `documents` | |
| Per-user `kycalendar-invitations.json`, `kycalendar-flags.json` | `kycalendar/invitations.go:32`, `kycalendar/flags.go:35` | D | `documents` | |
| Per-user `incoming-encryption.json` | `processor/incoming_encryption.go:58` | D | `documents` | |
| **Per-user `contact-photos/`** (one file per photo, named by content hash) | `api/server_userscope.go:417-424`, locked at `api/contacts_photo.go:173` | D | `documents`, one row per photo | **Not in `TestEveryStoreIsClassified`**, because the name is a directory with no extension (`tenancy/inventory_test.go:119-120`). MH2 adds it to `extras` and classifies it as D. K PR 6's purge must cover it whichever PR lands first. |
| Per-mailbox `mailcache.json` | `mailcache/store.go:105` | D | `documents` | |
| Per-mailbox `native-mailbox.json` | `mailbox/prepare.go:26` | D | `documents` | |
| Per-mailbox `outbox-idle` | `mailbox/outbox_idle.go:22` | D | absent | Dropped in multi mode (lock table) |
| `$STATE/pickup/<id>.json` | `api/server.go:507` | D | `documents` | Public pickup links can be opened on any replica. |
| `$STATE/imports/upload-*` | `api/mail_import.go:38` | D | per host (scratch) | One streamed request writes it, and the job runs on the replica that received it (`api/mail_import.go:305-313`). Grants and job status are already in PostgreSQL. The upload is lost if that host dies, and the user starts again, as today when a replica stops. |
| Native restore hold, qualification and release files, `mfa-clear-all.*` | `sso/native_restore.go`, `sso/native_restore_qualification.go`, `sso/native_release.go`, `app/app.go` | P | `documents` | Whole-install recovery state that every replica must see. Restore runs offline. These files are read at start and by the release routes, so they must be shared. |
| `backups/` (local capsule copies) | `backup/backup.go:90-99` | P | per host (the runner's host only), or off: open question 3 | A local copy is useful only on the host that holds it. KyRecovery deposits are already off-host. |
| `mail-bulk.json` | `backup/bulk.go:38-43` | P | per host (scratch for one run) | It is written into the capsule during a run (`bulkManifestPath`). Whether it outlives the run on disk is unverified. MH7's assertion decides. |
| `backup-operation.lock` | `backup/backup.go:524` | P | fence | Lock table |
| `replica-volume.id` | `replica/topology.go:19` | P | absent in multi mode | MH7 |
| `server-instance.lock` | `state/coordination.go:319` | P | absent (SQLite only) | |
| `tenancy.db` and every other `.db` | `dbbackend` | O/D/P | already PG | R and K PR 1 |
| `recovery.pub` | `backup/replica.go:240,303` (`recoveryclient.RecoveryKeyPath`) | P | per host, rewritten from the sealed anchor | R already rewrites a missing `recovery.pub` from the sealed record (`app/replica.go:52-58`, `backup/replica.go:28-30`). Every host gets it at start. |
| TLS certificate files (`TLS_CERT_FILE`, `KYPOST_TLS_CERT_FILE`) | `api/tls.go:42`, `api/pairing_pin.go:180` | P | per host (read-only, provisioned) | HAProxy or the edge proxy ends TLS (`docs/RUNNING.md:257-258`). The pin source must be the same on every host. The shared-settings fingerprint already covers the path (`replica/config.go:48`). |
| Logs, supervisord socket and pid, Ollama models, backup scratch | `docs/RUNNING.md:289-290`; `replica/config.go:60-69` (`PerHost`) | P | per host | Already per container |

**Keys (`SECRET_DIR`).** All keys in `TestEveryStoreIsClassified` (`tenancy/inventory_test.go:96-101`)
are **per host, provisioned identically and read-only** (MH5). They are not moved into PostgreSQL,
because then a database dump would hold both the sealed records and the keys that open them. Today
they are created lazily by `cryptutil.LoadOrCreateKey` (`cryptutil/cryptutil.go:47-57`). The
callers include `app/replica.go:46` (`totp-secret.key`), `api/scheduled_send.go:162`,
`api/import_rows.go:331`, `api/relay_identity.go:41`, `api/pow_handlers.go:70,95`,
`api/server_imap_config.go:245`, `api/pickup_handlers.go:396`, `pgpmail/pickup.go:231`,
`mailmsg/domain_relay.go:362`, `mfa/mfa.go:475` and `backup/bulk.go:206`. Others use
`fsutil.CreateExclusive` (`config/config.go:862`, the VAPID key; `app/receiving_tls.go:174`,
`receiving-tls.key`). On one shared volume, an exclusive create makes concurrent first starts agree
(`docs/RUNNING.md:286`). On separate disks, each host would mint its own key, and every host would
then refuse the others' sealed records, sessions and links. In multi mode every one of these calls
**loads**, and a missing file refuses the start. This includes K PR 6's `address-tombstone.key`.

**Not files, unchanged by host count.** Sessions, CSRF, revocations, lockouts, challenges, replay
guards, import grants and job status, the orgs cache, the gateway pins and the elections are
already in PostgreSQL (`docs/RUNNING.md:278-281,284`; `api/shared_state.go:17-55`). In-process caches
over PostgreSQL rows, such as the per-user store handles and the device and subscriber indexes
(`api/server.go:361-362`), behave the same with N replicas on one host or on N hosts. R already
runs them with N > 1.

### The `documents` table

```
documents(root TEXT, path TEXT, revision BIGINT, body BYTEA, updated_at BIGINT,
          PRIMARY KEY (root, path))      -- multi-only, in tenancy.db (SQLite DDL too, K "PR 1 as built")
```

- **The key is the root and the path.** `root` is `config` or `state`. `path` is the same relative
  path the file store would use, built by the same helpers (`config.UserDir`,
  `config.SafeUserPathComponent`, `config/config.go:392-403`). A `CHECK` and a boundary validator
  refuse an absolute path, `..`, an empty segment, a NUL and anything over 512 bytes. A store
  therefore keeps its own code and format and changes only its I/O.
- **One seam.** A `vol` interface (Read, Write with an expected revision, Remove, List under a
  prefix) has a file implementation, which is today's `fsutil` calls unchanged, and a table
  implementation. The choice is made once, at open, by `KYPOST_TENANCY=multi`, as RX PR 3 picks the
  sender tables (`app/receiving.go:455-461`). No store branches on the mode
  anywhere else.
- **CAS.** A write gives the revision it read. `UPDATE … SET body=?, revision=revision+1 WHERE
  root=? AND path=? AND revision=?`, or, for revision 0 (the document was absent),
  `INSERT … ON CONFLICT (root, path) DO NOTHING`, so two replicas creating one document never
  see a key violation. If either statement touches no row, the write returns `ErrStale`, and the store retries its read-modify-write. The retry is bounded. A store
  that exhausts the bound fails the request with `503`, never with a partial write.
- **Size.** Each document has a cap of 16 MiB, refused at the write. That is about twice the
  largest store's own bound. MH2 measures the bounds per store, and the stores keep their own,
  smaller caps.
- **Class.** `documents` is D or P per row, by its `path` prefix (`users/<id>/…` or
  `mailboxes/<id>/…` is D; anything else is P). `TestEveryStoreIsClassified` gains a per-prefix
  rule. K PR 6's purge deletes an org's rows by the snapshot's user and mailbox IDs.
- **Backups.** `tenancy.db` joins the backup set in K PR 6 (K "PR 1 as built"). A multi install's
  documents and accounts are then in every capsule's database export. A restore loads them with the
  rest of `tenancy.db`, so it writes no files. That PR is therefore a hard dependency of MH2.

### The `accounts` table

```
accounts(id TEXT PRIMARY KEY, username TEXT UNIQUE, issuer TEXT, subject TEXT,
         revision BIGINT, record BYTEA, UNIQUE (issuer, subject))   -- multi-only, tenancy.db
```

- `record` holds the same JSON object one `users.json` entry holds (`users/users.go:49-165`),
  including `OrgID`. The users store's invariants (K "PR 2 as built": `orgInvariants`,
  `writeFileUnlocked` comparing old and new, `users/users.go:570,995`) become
  per-row comparisons in the same transaction. The `UNIQUE` constraints enforce `ErrSSOSubTaken`
  and username uniqueness in the database rather than in a whole-file scan.
- `WithCurrentUsers` (`users/users.go:830-865`), which reads every account under the shared lock,
  becomes one `REPEATABLE READ` scan. Its callers are the snapshot readers: recovery, the
  last-administrator rule and the purge snapshot. The stat-based read cache (`users/users.go:644-682`)
  becomes a per-account read that is revision-checked, so no replica serves an account it has not
  re-read this request (`currentUser` already re-reads, K "What KyPost has today").
- `user_orgs` stays as it is (K3). PR 2's three-record check (`checkAccountOrg`) is unchanged,
  because `accounts.record.OrgID` is the `users.json` `OrgID`.

## Per-replica meters

R keeps these in process memory, so with N replicas each one allows up to N times its limit
(`docs/RUNNING.md:282`, O4). They are built at `api/server.go:525-555`, and only the lockouts, the
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

The roles are `daemon`, `mail-runtime` and `api-sweepers` (`app/app.go:374-375,467`), each
elected by `worker.Run` (`worker/worker.go:44-60`) over `WithLock`. A new owner waits a `Grace`
longer than the probe bound before its first job (`app/replica.go:68-75`). The election promises
single ownership, not exactly-once delivery: "jobs keep their own fences (claims, leases, CAS
moves)" (`worker/worker.go:44-46`).

Across hosts, three things can happen that one host did not see often:

1. **A partition between a holder and PostgreSQL.** The holder's probe fails within 2 s and
   cancels its jobs. PostgreSQL releases the lock only when it notices the dead connection. That
   takes TCP keepalive time, which defaults to hours on Linux (unverified for the Compose
   PostgreSQL image). Until then no other replica can win the role. That is a stall, not a split
   brain. **Requirement (MH7):** the multi profile sets PostgreSQL's `tcp_keepalives_idle`,
   `tcp_keepalives_interval` and `tcp_keepalives_count` so that a dead holder is dropped within
   about 30 s. (`client_connection_check_interval` does not help here: it checks only during a
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
     `docs/RUNNING.md:277`), the orgs poller (`ApplyOrg`'s revision fence, K PR 5) and the purge
     (`org_purges` rows, K PR 6).
3. **A PostgreSQL failover.** Advisory locks are not replicated, so every holder loses every lock
   and every fence at once. Every probe fails, every role re-elects after `Grace`, and every fenced
   section in flight fails its CAS or its commit. With RX R5 (synchronous standby) nothing that
   committed is lost.

**Every lease or window compares database time** (`state/coordination.go:26-28` `nowMillis`).
Leases added by RX PR 4 and MH6 must too, so clock skew between hosts changes no answer.

## The purge and a lost fence

RX's import fence relies on `quota-<mailbox>`. The purge holds it while it removes a mailbox's
storage, and the import write re-reads the org's status after taking it (RX "Fence against the
purge"). With a fence that can be lost, a paused import worker could write after the purge removed
the schema. If the store's open recreates a missing schema, that write would bring the deleted
org's mail back.

**Requirement (MH4, before RX PR 4 ships in multi mode):** a purged mailbox's catalog entry
(`kypost_catalog.stores`, `dbbackend/dbbackend.go:205-207`) is tombstoned in the
same step that drops its schema, and opening a tombstoned store fails. The purge is then safe in
the database whatever happens to the lock. The fence only reduces wasted work. K PR 6's final check
(no mailbox, receipt or schema left) also asserts that the tombstone exists.

## Migration

- **Single-tenant installs (SQLite, or R's PostgreSQL replicas on one host): no migration.**
  Every MH change is selected by `KYPOST_TENANCY=multi` at open. A single install keeps its files,
  its `flock`s, `ErrHost`, the volume IDs and lazy key creation. It never creates `documents` or
  `accounts`, because it never creates `tenancy.db` (K PR 1).
- **Multi installs: none exist.** Startup refuses `multi` (K PR 1 as built, "K2's other
  prerequisites") and keeps refusing it until K PR 7. The amendment below moves that to after MH7.
  A multi install starts empty (K2), on PostgreSQL, with `documents` and `accounts` from its first
  start. There is nothing to convert. Test installs built before MH7 are recreated.
- **A self-hosted R install that wants several hosts** is out of scope for v1 (open question 1).
  If the owner wants it later, it is one offline command, `migrate-volumes-to-postgres`, modelled on
  `migrate-to-postgres` (`app/migrate_postgres.go:53`). It refuses while any instance is live
  (`LiveInstances`). It copies each classified file into `documents` or `accounts` in one
  transaction per store, checks every digest, and records `volume_stores=postgres` in `meta` last,
  so a run that dies leaves the install on its files. Rolling back means deleting that one row.
  Because it would give single-tenant installs a `tenancy.db`-like store, it needs its own decision
  and is not in this plan.

### Proof that single-tenant stays byte for byte

- **`TestSingleTenantUpgradeIsByteForByte`** (`app/tenancy_upgrade_test.go:213`) compares every file,
  every SQLite schema and row, and on the PostgreSQL job every catalogued schema, the fingerprint
  record included, across a restart with `KYPOST_TENANCY` unset and `single` (K "PR 1 as built").
  **Every MH PR extends its fixture** to exercise the stores that PR moves: a contact, a rule, a
  photo, a settings change, a DKIM rotation, a WKD domain and a pickup link. It then asserts that
  the volume bytes are unchanged and that no `tenancy.db` exists.
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
  `mail-bulk.json`, `backups/` while open question 3 stands, `recovery.pub`), each matched by an
  explicit pattern list kept beside `TestEveryStoreIsClassified`. Any other changed path fails,
  and so does a per-host pattern that nothing in the inventory names. If open question 3 is
  answered "refuse local copies", `backups/` leaves the list.

## Phased build plan

Each PR ships on its own. Single-tenant behaviour is unchanged in every PR, and each extends the
upgrade test (above). Startup keeps refusing `multi` throughout (K PR 1), so multi paths run only
in tests, as K's do (`makeMultiTenant`).

1. **MH1: the seam and fences.** The `vol` interface (file and table implementations), the
   `documents` table, and the fence type: `Hold` with R's probe, a shared mode, and the fence
   names. Nothing uses them in production yet.
   Tests:
   - CAS: a stale revision is refused, two writers race on an update and on a first create and in
     each case one wins with the other retrying, and
     the retry bound gives `503`;
   - boundary: an absolute path, `..`, NUL, an empty segment, oversize paths and bodies, all refused
     before SQL;
   - fences: exclusive against shared, a writer not starved by a stream of readers (the
     advisory-lock queueing claim above), a probe killed mid-section cancelling the holder, and
     a second replica winning only after the loss.
2. **MH2: platform and per-user documents.** Every `documents` row in the inventory, through the
   seam, selected at open. Adds `contact-photos` to `TestEveryStoreIsClassified`. Needs K PR 6's
   `tenancy.db` in the backup set.
   Tests:
   - each store's existing tests on both implementations;
   - a restore of a capsule that carries documents;
   - the root `config.yaml` change seen by a second replica within 5 s;
   - K PR 6's purge removing every document of the org, checked against the classification;
   - a cross-org case in `TestCrossOrgMatrix` per new document kind.
3. **MH3: accounts.** The `accounts` table, per-row invariants and the `REPEATABLE READ` snapshot.
   Tests:
   - every users-store test on both implementations;
   - PR 2's write-once and K7 tests in multi mode;
   - a uniqueness race between two replicas creating one subject, where one wins and the other
     gets `ErrSSOSubTaken`;
   - a snapshot taken during a write.
4. **MH4: cross-store fences and job fences.** The domain, directory, settings, quota, mailbox,
   SSO and backup-operation fences, with `outbox-idle` dropped in multi mode. Also the
   catalog-tombstone purge fence, and the list of elected jobs with their fences.
   Tests:
   - each fence's ordering;
   - a fence lost mid-section, which fails its CAS and writes nothing stale;
   - an import worker paused past the purge, whose write fails on the tombstone;
   - a backup operation on replica A refusing one on replica B.
5. **MH5: provisioned keys.** In multi mode, no key is created at runtime. A missing key refuses
   the start. The key-set digest (HMAC-SHA256 of each key file's SHA-256 under R's
   `kypost:replica-config` subkey, `replica/AGENTS.md`, local contracts) is stored in `meta` and
   compared at start, naming the key, never a value. A key rotation is offline, like
   `accept-replica-config`.
   Tests:
   - a missing key;
   - a different key on one host;
   - every `LoadOrCreateKey` and `CreateExclusive` call site reached in multi mode;
   - single mode still creating keys lazily.
6. **MH6: shared meters and leases.** The meters marked "shared window" above move to R's
   `TakeAttempt` (`state/coordination.go:34`), keyed through `sharedKey`
   (`api/shared_state.go:56-61`). This PR also adds the per-org import and export lease and the
   two-replica quota test.
   Tests: each meter giving the same total across two servers, and each falling back to `503`,
   never to memory, when the database fails (`api/shared_state.go:21-22`).
7. **MH7: lift the host checks in multi mode.** In multi mode the boot-ID and volume-ID checks are
   skipped and the volume-free assertion runs. This PR also adds the multi-host Compose profile
   and docs: separate volumes per host, a read-only `SECRET_DIR`, PostgreSQL with `sslmode=verify-full`
   and the keepalive settings, and session pooling. The same PR updates `docs/RUNNING.md` and
   `replica/AGENTS.md`.
   Tests:
   - `TestTopologyGuard` multi cases: another boot ID admitted, a file-backed store refused;
   - the volume-free test;
   - a two-host harness, two containers with separate volumes and different boot IDs on one
     database, running `TestTwoReplicasSeeOneSuspension` and a sign-in on A then a request on B.

**Against K and RX.**
- MH1 can start now. MH2 needs K PR 6's backup inclusion of `tenancy.db`. MH4's catalog tombstone
  must land before RX PR 4's import fence is relied on in multi mode, so it lands with or before
  RX PR 4's multi path. If RX PR 4 merges first, it is still safe, because multi cannot start.
- RX PR 5's hosted profile and MH7's multi-host profile are one Compose profile. Whichever lands
  second extends the first.
- **K PR 7 accepts `multi` only after MH1–MH7 and RX PRs 1–5.** It also adds MH7's prerequisites
  to its K2 checks (PostgreSQL, `sslmode=verify-full` on a non-loopback host, a provisioned
  `SECRET_DIR`).
- RX's "Past one host" (RX steps 1 and 4) is replaced by this plan. RX step 3 (PostgreSQL as its
  own replicated service with a synchronous standby, RX R5) stays a launch prerequisite outside it.

## Threat model

| Attacker or fault | Goal or effect | Defence |
|---|---|---|
| Network attacker between a replica host and PostgreSQL | Read or change every org's data in transit | Today's Compose URL uses `sslmode=disable` on one Docker network (`docs/RUNNING.md:310`), and the code sets no TLS mode of its own (unverified beyond a search of `dbbackend` and `config`). Multi mode refuses a database URL whose `sslmode` is not `verify-full` unless the host is loopback (MH7, and K PR 7's K2 check). |
| A rogue or misconfigured host joining | Serve with other keys, settings or mode | Joining needs the database credentials. Then K1's tenancy record, R's settings fingerprint and MH5's key-set digest each refuse a mismatch before anything listens. |
| A compromised replica host | Read every org | Unchanged in kind: any replica holds every key and the database, today and in multi mode (K residual risks). More hosts mean more such machines. Keys are mounted read-only from the secret store, so a host compromise cannot plant a new key for the others to accept. |
| Member of org A | Reach B's documents by path | Document paths are built by the same helpers from the caller's own admitted IDs (`config.SafeUserPathComponent`). The table validator refuses traversal. `TestCrossOrgMatrix` and `TestOrgAdmitCallSites` (K PR 3) keep running against the table implementation. |
| Oversize input | Fill the database through one document | The per-document cap at the write, plus each store's own caps. Per-org storage quotas cover mail, not documents (K PR 5 owner answer 5). The capacity measurement sizes documents per user. |
| A holder that lost its fence | A lost update | CAS on every document (above). DB constraints cover the ledger (`addresses.address` primary key) and the accounts (`UNIQUE`). |
| A paused import worker after a purge | Recreate a deleted org's mail | The catalog tombstone (MH4) |
| Stale reads on another host | Serve a revoked account or old settings | No cross-replica cache without a revision check. `currentUser` re-reads per request, and the root config's 5 s re-read becomes a revision read. |

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
| A host with a stale image | Mixed versions are not supported (`docs/RUNNING.md:344-345`). The upgrade stops every replica on every host, which is a full outage across hosts. | — |

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
- **K's residual risk "One install, one database, one host"** (K "Residual risks") is outside the
  sections this PR may edit. Once MH7 ships it reads "one install, one database".

## Open questions, with recommendations

1. **Multi-host for self-hosted single-tenant R installs?** *Recommend* no in v1. Every MH change is
   gated on multi mode, which keeps single-tenant byte for byte. The `migrate-volumes-to-postgres`
   sketch (Migration) is the path if the owner wants it later. This matches RX open question 1.
2. **Object storage for photos and large documents?** *Recommend* no: use PostgreSQL `BYTEA` with a
   16 MiB cap per document. Object storage adds a dependency, a second consistency domain and a
   second backup path. Revisit if the capacity measurement shows documents dominate the database.
3. **Local backup copies in multi mode?** *Recommend* refusing local copies in multi mode and
   requiring KyRecovery deposit, so no capsule sits on whichever host ran the job. The alternative
   is to keep them on the runner's host and show which host in the status.
4. **Database TLS.** *Recommend* `sslmode=verify-full` required for any non-loopback host. The owner
   may decide that a private overlay network (WireGuard) is enough. The recommendation is still
   TLS, because the database carries every org.
5. **Connection pooling.** Fences and elections hold session advisory locks on dedicated
   connections, and R requires "a direct connection or session pooling, never transaction
   pooling" (`state/coordination.go:166-167`). *Recommend* direct connections or PgBouncer in session
   mode, with `max_connections` sized by R's budget formula (`docs/RUNNING.md:447-456`) times the
   number of hosts. Fences add one dedicated connection per request holding a fence set, so that
   peak is part of the capacity measurement.
6. **Which per-replica meters to share.** *Recommend* the five "shared window" rows above (nine meters), which
   send something to a third party or face unauthenticated callers, and leaving the per-account
   load-shedding meters per replica.
7. **One table per store instead of `documents`?** *Recommend* `documents` for every store except
   `users.json` (MH3) and M's masked records (K PR 7). Per-store tables would rewrite each store's
   logic for no isolation gain, because a document is already keyed by its owner's path. Split a
   store out only if the capacity measurement shows it needs indexed queries.
8. **Region.** *Recommend* hosts in one region with low single-digit millisecond latency to
   PostgreSQL. Every fence and CAS is a database round trip, and cross-region latency would
   multiply request time. Cross-region is out of scope.
9. **`outbox-idle` in multi mode.** *Recommend* dropping it (MH4) and building RX's index-driven
   outbox recovery with the capacity work, rather than putting a per-mailbox flag in PostgreSQL.
