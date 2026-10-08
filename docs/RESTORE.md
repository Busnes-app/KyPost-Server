# Sealed backup and restore

Configure backups on **Server → Backup**. Pair with KyRecovery using its one-time
six-digit code, or paste the suite public key and k-of-n from its ceremony page.
Compare the key fingerprint out of band. The pin is write-once; unpair removes the
URL and token but keeps the key, receipts and local copies. A KyRecovery admin must
separately revoke the token. Products cannot download from KyRecovery.

Set `KYPOST_BACKUP_DIR=/kypost/state/backups` for local copies on the existing volume,
or mount a separate writable host directory outside the data roots. Backups on the
same disk do not protect against disk loss. Retention defaults to seven own-prefix
capsules; other applications' files are left alone. The interval defaults to 24h;
the UI overrides it without restart (0 off, otherwise 15 minutes–366 days).

A backup seals once, then attempts local delivery and the paired KyRecovery deposit.
Inspect the result and receipt; a remote failure can leave a successful local copy,
and a local failure does not cancel the remote attempt. Uploads are bounded to 16
minutes. Browser disconnects do not cancel uploads; deployment shutdown allows them
to drain separately after the normal 20-second HTTP grace; unrelated requests do
not get the extended backup deadline. Supervisor stops API and daemon groups
sequentially, with 1,000 seconds allowed for each; Compose therefore allows up to
36 minutes overall. Idle services exit promptly. External orchestrators must
provide the same shutdown budget; a shorter forced stop can interrupt deposits.
Reverse proxies may need a matching response timeout. The process lock rejects
competing operations rather than queueing them behind an upload.

For a LAN destination, explicitly set `KYPOST_BACKUP_ALLOW_PRIVATE_RECOVERY=true`.
HTTPS remains mandatory; redirects, loopback and link-local destinations are refused.
The setting admits private/CGNAT destinations and is recorded on pairing audit rows.
If a private hostname needs a LAN resolver:

```sh
KYPOST_DNS=192.168.1.1 docker compose -f docker-compose.yml -f docker-compose.lan-dns.yml up -d --build
```

This resolver also sees IMAP, SMTP and WKD lookups. Verify container DNS with
`docker inspect KyPost-Server --format '{{.HostConfig.Dns}}'`.

## What a capsule carries

- Encrypted domain relay configuration and its matching dedicated master key. Collection and drills decrypt the collected bytes and check the historical domain/issuer binding. A missing or corrupt key refuses backup; see [relay recovery](DOMAIN_RELAY.md#storage-recovery-and-activation).

- Configuration and accounts, including opaque client-wrapped PGP keys.
- Deployment secrets in SECRET_DIR, including totp-secret.key, which seals the
  KyRecovery token under a distinct derivation label. Never replace this key.
- Install-wide and per-user state.db snapshots including committed WAL rows,
  address books and other persistent state. Native-device secrets are in the
  per-user databases. Capsules retain historical device/subscription evidence, but native restore revokes those registrations before publication. Pending encrypted pickup messages are included.
- Internal mailbox.db and ingress.db snapshots inside the collected roots (or,
  with `KYPOST_BULK_BACKUP_REPOSITORY`, the [bulk manifest](#mail-bulk-backup) naming them),
  including committed WAL rows and exact stored MIME/receipt data. Mailbox snapshots also preserve encrypted outbox intent, claims and Sent receipts; nonempty queues require the matching relay profile/key and additive verification recipe. Native
  reception and provisioning are opt-in qualification paths; public reception remains unavailable.

- Continuous Cloudflare receiving credentials (`cloudflare-receiving.json`) and
  the `cloudflare.db` snapshot (published tables and the pickup ledger). The
  host-local `cloudflare-receiving.host.json` is not collected, so a restored
  instance starts fenced and contacts no Worker until an operator confirms
  `receiving cloudflare takeover`; see
  [continuous Cloudflare profile](RECEIVING_SETUP.md#continuous-cloudflare-profile).
  If compromise prompted recovery, re-bootstrap through the Cloudflare account
  as described there instead of taking over with the restored credentials.
  Take over promptly: mail waiting in R2 under a table newer than the backup
  arrives quarantined as unresolved, and an administrator releases it to the
  address's current owner or discards it
  ([quarantine release](NATIVE_PROVISIONING.md#quarantine-release)). A backup
  older than the last rotation cannot take over; re-bootstrap instead.

IMAP mail, rebuildable mailcache.json, Ollama model blobs, logs and runtime files
are excluded. Each database has a consistent snapshot; separate databases and JSON
files are collected sequentially, not as a transaction across the whole deployment.
For a quiescent recovery point, stop services and use the CLI export.

Native exports and drills now check historical ownership: users, issuer/subject
reservations, directory revisions, mailbox namespace/state, domain set and
receiving routes/frozen bindings must agree. Missing acknowledged storage,
orphan databases and foreign ownership fail closed. Outbox checks decrypt frozen bytes, verify quota/claim/Sent consistency and refuse orphan claims or partial queue schemas; historical evidence grants no replay authority. Validation uses only the
collected/restored roots, never the original absolute ledger path. It does not
prove current external authority or freshness; consistent old backups can pass.
Native validation needs additional scratch space for the collected metadata and
databases. Without `KYPOST_BULK_BACKUP_REPOSITORY` the 64 MiB per-file and 256 MiB total
limits apply to mail databases too; oversized backups fail, naming that variable,
rather than omit mail. Native mailboxes have 5 GiB quotas by default, so set it
before they grow. A restored backup taken with older mailbox limits is raised to
the configured ones by `migrate-native` at the next start
([mailbox quotas](NATIVE_PROVISIONING.md#mailbox-quotas)). Keep
`KYPOST_MAILBOX_QUOTA_BYTES` unchanged while a native recovery repair is in
progress: a quota change between recording its evidence and completing the
repair voids the evidence, and a new challenge is needed. Rolling back to an
image older than mailbox quotas needs the backup taken before the upgrade: the
older image refuses the raised receiving-buffer limits. The limit applies to the consistent snapshot, including committed WAL rows, not just the main database file size. A small main file is therefore insufficient to predict whether a backup will fit. Automated mailbox/receiving capacity checks verify refusal without a local capsule or scratch leftovers and preservation of original committed probe data; they do not qualify domain-sized mail throughput.

Capsules carry the native domain set `native-domains.json` and the
`native-domain.json` tombstone, never the `*.v1-migrated` copies. Validation and
drills accept snapshots taken before the [storage format migration](#storage-format-migration)
(version-1 domain, ledger and relay) as well as after it, and the mixes a migration
crash leaves (a version-1 domain beside a version-2 ledger or relay). A version-2
domain beside a version-1 ledger or relay, or a ledger or relay without a domain,
is refused; a restored version-1
snapshot is migrated by `migrate-native` at the next container start, with the
restore hold left in place. Configuring a mail domain now also initializes an
empty native ledger, so restoring a deployment that configured a domain but has
no native accounts is held like a relay-only restore.

With several domains, validation checks each domain's proof format, that every
ledger address sits on a domain in the set (configured or retired), that the
relay's `domains` and `retiredDomains` are within the same set, and outbox jobs
against the relay's `domains ∪ retiredDomains`. Retired domains keep their
address records, so a snapshot taken after retirement, and historical jobs and
bindings on a retired domain, still validate.

Version-2 receiving routes and bindings are checked against each address's
`history`: a route or binding to mailbox `m` at generation `g` is valid only if
some history entry `i` names `m` with `history[i].generation ≤ g <
history[i+1].generation`, or `i` is the last entry and `history[i].generation ≤ g
≤ generation`. A snapshot taken after an alias was released and reassigned
therefore validates (its old route and bindings fall in the old owner's
interval); one bound outside every interval is refused. Every alias and reserved
address must be canonical and on a known domain. Version-1 snapshots keep the
primary-address check. Ledgers written before per-address generations have their
generations raised to the owner's directory revision when loaded, so their
routes and bindings validate unchanged. The recovery authority digest includes
every address record, so any alias add, release, reassignment or state change
invalidates an outstanding recovery challenge: reissue it afterwards.

Extra mailboxes (`$STATE/mailboxes/<mailboxID>/`) are collected like the rest of
STATE_DIR. Validation requires each to belong to a published native user's
subject, carry an `mbx-` ID that is no user's, share the owner's state root and
limits, match its manifest and keep a mail-only `state.db`: any device,
notification, pull-notification or subscriber row refuses the snapshot. Storage
is checked per mailbox root, so a mailbox database under the wrong root, or an
unknown one under either, is an orphan. A mailbox reserved but not yet prepared
(an interrupted creation) carries no storage and still validates. The recovery
authority digest includes extra mailbox records (owner, state, storage), so
creating, disabling or re-enabling one also needs a fresh recovery challenge.

The version-1 recipe remains compatible. Use this version of KyPost or newer to
check all three database names and the additive relay credential/authority recipe; older drills do not attest the new relay checks. A new integrity
check cannot recover WAL rows omitted from an older raw-copy native backup.

The collector refuses missing keys needed by stored encrypted data, symlinks,
unsupported special files, files over 64 MiB or a total payload over 256 MiB.
Secret-file overrides must use their canonical SECRET_DIR locations; VAPID and
TUNING_FILE overrides must be inside collected roots, even when missing. A missing
optional TUNING_FILE inside CONFIG_DIR, SECRET_DIR or STATE_DIR does not block
backup: the default container falls back to its bundled prompt.
Restore into the original configured
root paths or update embedded paths before starting.

Keep a separate protected copy of the operator's `.env`, compose overrides and
external TLS files. Environment-only credentials (for example PAIRING_SECRET,
relay keys and CAPTCHA credentials), DNS/network settings, and TLS mounts are not
captured. Restore them before startup. Client-protected PGP still requires its
owner's password/recovery material; a capsule does not bypass that protection.

## Mail bulk backup

Set `KYPOST_BULK_BACKUP_REPOSITORY` and every `mailbox.db` and
`receiving/ingress.db` goes to a restic repository (restic 0.16.0 or newer; the
image ships Debian's) instead of the capsule; the rest is sealed as before. Two
forms are accepted:

- `rest:https://user:password@host/path`, an append-only
  [rest-server](https://github.com/restic/rest-server) (`--append-only`, its own
  credentials). **Recommended.** A local repository is writable by KyPost, so a
  compromised container could delete snapshots; KyRecovery, by contrast, only
  accepts deposits. HTTPS is required; a query or fragment is refused. The URL
  reaches restic only in its environment; the Backup screen shows scheme, host
  and path with credentials masked. Restic failures are reported as a fixed
  diagnostic (wrong password, no repository, snapshot missing, unreachable…),
  never restic's own text, which can repeat the address or name mail files; run
  restic by hand for details.
- An absolute container path outside CONFIG_DIR, SECRET_DIR, STATE_DIR,
  `KYPOST_BACKUP_DIR` and `KYPOST_BACKUP_SCRATCH_DIR`, compared by path and by file
  identity, so a symlink or bind-mount alias of a data root is refused. Compose:
  mount a host directory at `/kypost-bulk`, owned by the container's `kypost`
  user, ideally on another disk. A path that is not a mount lives in the
  container's writable layer and disappears with the container; KyPost cannot
  detect that, so check `docker inspect` mounts.

- **Key and identity.** The first backup, with no key and an empty or absent
  repository, creates `private/bulk-backup.key`, runs `restic init` and records
  the repository ID in `private/bulk-backup.repo`. Both are sealed in every
  capsule. The restic password is the hex of the key, given to restic only in a
  minimal environment (PATH, HOME, TMPDIR, LANG). It is a dedicated key so the
  repository never depends on keys with other jobs: rotating or losing
  `totp-secret.key` must not strand mail. Refused, never re-keyed: an existing
  repository without the key; a key beside an empty directory (an unmounted
  mountpoint: a repository existed, mount it); a repository whose ID is not the
  recorded one; a non-empty directory that is not a repository.
- **Backup.** At the start of every run, under the backup lock, scratch left by a
  killed backup, drill or restore is deleted: it holds plaintext mail. Free space
  is checked first: the scratch filesystem must hold the mail databases and their
  WAL files and still keep 10% or 5 GiB free, whichever is larger; the error
  names the shortfall. `ingress.db` is snapshotted first, then each `mailbox.db`,
  streamed to `STATE_DIR/backup-scratch/bulk`, or to
  `KYPOST_BACKUP_SCRATCH_DIR/kypost-backup-scratch` (absolute, outside the data
  roots, the backup directory and the repository) when STATE_DIR is short of
  space. After the payload validates, one `restic backup --host kypost --tag
  kypost-mail` stores them from that fixed path, so runs form one series and
  restic reuses the previous snapshot. The capsule seals `state/mail-bulk.json`
  with the full snapshot ID and each file's path, SHA-256 and size, and its
  recipe declares the bulk snapshot. Any restic failure fails the whole backup.
  Server → Backup shows the snapshot of the last capsule that reached a
  destination; drills and exports do not change it.
- **Time.** A run, drill or export may take 16 minutes plus 10 minutes and a
  minute per 2 GiB of mail databases, at most 4 hours more. Shutdown still drains
  backups for only 16 minutes (the supervisor budget below): a run still going
  then is abandoned, leaves no capsule, and the next run sweeps its scratch. A
  reverse proxy in front of the Backup screen needs a matching response timeout,
  or use the CLI (`kypost-server deposit`, `backup-drill`).
- **Drills** restore the snapshot into drill scratch, re-hash it and run the
  usual database checks, so a drill reads all mail once.
- **Retention.** Nothing is pruned automatically. A capsule's snapshot ID cannot
  be read without opening it, so keep every snapshot newer than your oldest
  retained capsule (KyRecovery and local copies) plus a day. With an append-only
  rest-server, prune from the server side with its unrestricted credentials. For
  a local repository, with services stopped:

  ```sh
  restic -r /srv/kypost-restic \
    --password-command "sh -c 'base64 -d /path/to/private/bulk-backup.key | od -An -v -tx1 | tr -d \"[:space:]\"'" \
    forget --host kypost --tag kypost-mail --keep-within 400d --prune
  ```

  where 400d exceeds that age. `--password-command` reads the key file on each
  run; do not paste the derived password into a shell, history or
  `RESTIC_PASSWORD`. `--keep-last` and similar policies work too;
  automated tests check that `forget --keep-last 1` removes older runs.

Restore needs the same repository: the restore command reads
`KYPOST_BULK_BACKUP_REPOSITORY` (and `KYPOST_RESTIC_BINARY`, default `restic`). A
capsule whose recipe declares a bulk snapshot is refused before shares are read
when the variable is unset. After the capsule opens, restore checks the
repository ID against the sealed one, restores exactly the sealed snapshot with
`restic --no-lock restore --verify` into private scratch, re-hashes every file,
refuses missing, extra or mismatched files, trailing manifest data and a capsule
that already holds a mail database, and only then places them before ownership
validation and quarantine. A capsule naming a snapshot the repository lacks
fails; a capsule is never combined with another snapshot. `--no-lock` lets the
repository be mounted read-only:

```sh
docker run --rm -i --user "$(id -u):$(id -g)" \
  -v "$PWD:/restore" -v /srv/kypost-restic:/kypost-bulk:ro \
  -e KYPOST_BULK_BACKUP_REPOSITORY=/kypost-bulk \
  "${KYPOST_RESTORE_IMAGE:?set the recorded image digest reference}" \
  /usr/local/bin/kypost-server restore /restore/backup.kycap /restore/recovered
```

Do not prune while a restore reads the repository.

## Offline restore and native quarantine

1. Stop the deployment and retain its existing volumes. Take a separate copy before
   replacing anything. Download the capsule using a KyRecovery operator session, or
   use a local `.kycap`. Record its expected capsule ID, digest, time and key ID from
   the receipt; successful decryption alone does not prove freshness. Run
   `sha256sum backup.kycap` and compare it with the receipt digest before restoring.
   The manifest payload hash printed by restore is a different hash.
2. Run the same or a newer compatible KyPost binary against an absent target path:

   ```sh
   kypost-server restore backup.kycap "$PWD/recovered"
   ```

   Enter at least k custodian shares on stdin, one per line, then EOF (Ctrl-D).
   Never put shares in argv, chat or shared notes. A capsule with a
   [mail bulk manifest](#mail-bulk-backup) also needs its restic repository. Docker has no ENTRYPOINT; name
   the executable explicitly, use `-i` for stdin and mount a writable staging root.
   Set `KYPOST_RESTORE_IMAGE` to the recorded compatible image digest reference
   (`ghcr.io/busnes-app/kypost-server@sha256:…`), then run:

   ```sh
   docker run --rm -i --user "$(id -u):$(id -g)" \
     -v "$PWD:/restore" "${KYPOST_RESTORE_IMAGE:?set the recorded image digest reference}" \
     /usr/local/bin/kypost-server restore /restore/backup.kycap /restore/recovered
   ```

3. Compare the authenticated manifest summary with the receipt. Refuse stale,
   foreign-service, wrong-key or corrupted capsules. Extraction occurs in an
   owner-only sibling `.kypost-restore-*` directory. Failed extraction/validation
   retains it for inspection and prints no success summary. A native target must
   be absent; Linux publishes it atomically without replacement after validation,
   persistence of `state/native-restore-hold.json`, and quarantine of restored
   queued/retryable outgoing deliveries. Their encrypted contents and claim history
   remain retained; accepted/Sent and submitting/uncertain evidence is unchanged.
   Each quarantine attempt writes a fresh random UUID-v4 `epoch` in the version-1
   hold, including retries and failed validation. It distinguishes this restore
   from historical copies; it grants no recovery authority. An empty-epoch hold
   is persisted before random generation, so a fatal failure or returned error
   keeps staging held and unpublished. Future recovery
   evidence must bind a valid current epoch; retries invalidate earlier evidence.
   For every native account, restore also atomically removes native device and
   browser push registrations and rotates the subscriber ID. Old device secrets,
   push-MFA approvers, enrollment acknowledgements and outstanding pairing tokens
   do not regain authority. Mail, pull-notification history and opaque wrapped
   keys remain retained. After the account/device fence, remove each native CardDAV app-password hash; failure leaves staging held and unpublished. Legacy IMAP registrations and CardDAV credentials are unchanged.
   After historical ownership and credential cleanup, restore atomically raises each published native subject’s existing directory ID-token cutoff to at least the current server time plus 31 seconds. This includes the accepted 30-second future clock skew; higher cutoffs and legacy directory entries remain unchanged. Earlier browser/device tokens are refused before grants. Wait for the cutoff interval to pass and obtain a new provider token after future recovery qualification. This does not prove fresh primary authentication or revoke local passwords/MFA recovery material. Missing authority or persistence failure keeps staging held and unpublished.
   Restore also rotates a separate mailbox reference generation, in every
   primary and extra mailbox, preserving the immutable encryption namespace. Native HTTP/notification references include
   this generation; old-generation or bare numeric references are refused.
   Corrupt generation metadata refuses publication; older snapshots without the
   table gain a fresh generation. See [reference qualification](NATIVE_RESTORE_REFERENCES.md).
   A failure leaves staging held and unpublished. Legacy restores also
   accept an existing empty target. Occupied targets/files are never overwritten.
4. Native restores remain quarantined with **no supported release path yet**.
   Preserve the hold file when copying volumes. Keep a separately provisioned legacy local recovery administrator: a restored native administrator cannot administer a held server. The authentication guard applies even with native feature flags disabled and either private native marker present. Refused MFA completion preserves TOTP/recovery material; held QR exchange preserves its nonce. Offline restore requires stopped services; admission checks do not cancel operations already admitted. Native password/derived login, pending MFA completion, SSO sign-in/step-up, existing session authority (including admin), QR key exchange, allocation, native runtime, native CardDAV (including cached Basic auth), local receiving and relay updates refuse a present
   or unreadable hold. Relay-only restores also persist this hold. Keep native workers stopped; manually deleting the hold
   does not qualify recovery. With services still stopped, copy `recovered/config/`, `recovered/private/` and
   `recovered/state/` into the corresponding retained/mounted volumes. Preserve
   owner-only permissions and set ownership for the runtime account. Restore `.env`
   and external dependencies. Recreate the container without deleting volumes.
5. Confirm readiness, the same recovery-key fingerprint and a new successful
   backup. Run `backup-drill` and inspect its SQLite, account and credential checks.
   Sessions are memory-only, so users sign in again. Test external mailbox access.
   Native mailbox/device access remains blocked by the restore hold. After future
   recovery qualification, each native device must pair and enroll again, and
   browsers must subscribe again. Native CardDAV clients also need a newly generated app password after qualified recovery. Repeating quarantine rotates subscriber IDs
   again while held; no device authority is reintroduced. Device-held private
   keys are not remotely erased. Push-only MFA may require the existing account
   recovery procedure before fresh pairing; restore does not disable MFA.

Read the [pinned identity authority findings](NATIVE_RESTORE_AUTHORITY.md) before designing hold release: ordinary KyIdentity resync does not establish complete offboarding evidence.

The protected [held repair procedure](NATIVE_RESTORE_AUTHORITY.md#protected-consumer-procedure) reconciles fresh KyIdentity activity/roles and revokes native transport credentials while preserving the hold and retained mail/PGP data. Partial cleanup failure remains incomplete and requires fresh evidence after remediation. Before native recovery can resume writes, remaining activation work must qualify domain proof, receiver generations, restored-credential cutoffs, worker fencing and provider credential/relay evidence. Do not remove the hold manually.
Replaying an already-applied directory revision is insufficient to repair
restored user access. An older database also rewinds message IDs while retaining
its namespace: the separate wire-reference generation rejects stale client
references without changing cryptographic bindings. Outbox recovery preserves interrupted/uncertain claims and quarantines restored
queued/retryable deliveries; native hold release and current admission
remain unavailable. See [outbox qualification](NATIVE_OUTBOX.md).

Private staging establishes the process-crash publication boundary, not whole-tree
power-loss durability: extraction does not fsync every restored file/directory.
Hardware fault qualification remains open. Preserve orphan preparation/restore
stages for inspection; deleting directories by prefix can erase mailbox data.
Rollback keeps services stopped and retains the original volumes and failed
staging; use a compatible binary, not an older writer that discards ownership.

Native restore always revokes its historical device pairings. If compromise prompted
recovery, also revoke affected legacy-account devices through Security, revoke old KyRecovery tokens at KyRecovery and pair again to the
same key. For a file-backed pairing secret, stop services and remove
`/kypost/private/pairing.key` to generate a replacement on the next start; an explicit
PAIRING_SECRET must instead be rotated in `.env`. Rotate externally issued relay
credentials at their provider and update the matching protected configuration.
Never regenerate native-relay.key, imap-config.key or totp-secret.key: doing so strands encrypted data.

## Storage format migration

Each container start runs `kypost-server migrate-native` as the runtime user before
any service, converting version-1 native domain, ledger and relay files to the
version-2 formats; see [native provisioning](NATIVE_PROVISIONING.md#storage-format-migration).
It keeps the version-1 sources as `$CONFIG_DIR/*.v1-migrated` and ends by replacing
`native-domain.json` with a tombstone. Take a backup before upgrading.

If migration fails, the log names the cause; native mail stays refused while
external IMAP works. Fix the cause and restart; re-running is safe. A version-1
binary refuses migrated state instead of reading it, so rollback to one means
restoring the backup taken before the upgrade with the procedure above, not
editing or deleting the tombstone, copies or ledger.

## Verification commands

```sh
kypost-server backup-drill
kypost-server export-capsule fresh.kycap
kypost-server deposit
```

Export refuses to overwrite an existing file. A drill creates a throwaway recovery
key internally, checks the actual opened manifest and wipes its scratch directory.
It tests the payload and recipe, not the real custodian cards. Automated synthetic
restore coverage lives in `backend/internal/backup/backup_test.go` and
`backend/internal/app/backup_test.go`; actual deployment pairing
and a real-card restore remain separate operator proofs.
