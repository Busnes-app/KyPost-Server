# Storage and recovery

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Persistence

Named volumes:

- `kypost_config` -> `/kypost/config`
- `kypost_private` -> `/kypost/private`
- `kypost_logs` -> `/kypost/logs`
- `kypost_state` -> `/kypost/state`

Host bind mount:

- `${OLLAMA_MODELS_HOST_DIR:-./share/ollama/models}` -> `/kypost/ollama-models`

Important files:

- `/kypost/config/config.yaml` (global system config)
- `/kypost/config/users.json` (user accounts and roles)
- `/kypost/config/users/<userID>/` (per-user IMAP credentials, CardDAV-client credentials, tuning, notification preferences)
- `/kypost/config/mail-defaults.json` (admin-published instance-wide host and port defaults, no credentials)
- `/kypost/config/sso-lifecycle.json` (accepted SSO logout tokens, directory revision/access fences, verified SCIM desired resources and the native-provisioning initialization fence; temporary replay records expire)
- `/kypost/config/native-provisioning.json` (internal account, mailbox and address ledger, version 2: reservations, preparation status and address generations; keep with lifecycle and mailbox/state data during recovery; provisioning is opt-in via KYPOST_NATIVE_MAIL)
- `/kypost/config/native-domains.json` (verified mail domain set and public challenges); `native-domain.json` is the tombstone of the older single-domain file, and `*.v1-migrated` are the older files kept by the startup migration (not backed up)
- `/kypost/config/TUNING.md` (default tuning for new users)
- `/kypost/config/notifications-vapid-private.pem` (shared web-push signing key)
- `/kypost/private/imap-config.key` (master encryption key for stored IMAP credentials)
- `/kypost/private/totp-secret.key` (master encryption key for stored TOTP secrets)
- `/kypost/state/state.db` (global state: AI-credits flag)
- `/kypost/state/users/<userID>/state.db` (per-user mailbox state, decisions, devices, subscriptions)
- `/kypost/state/mailboxes/<mailboxID>/` (an administrator-created extra native mailbox: `native-mailbox.json`, `mailbox/mailbox.db` and a mail-only `state.db`; devices and subscriptions stay with the owner's primary)
- `/kypost/config/admin.env` (legacy single-admin seed. KyPost imports it once, then stops reading it.)

On Postgres (`KYPOST_DATABASE_URL`, [RUNNING.md](RUNNING.md#postgres)) the `.db` files above are schemas in that database, one per file, listed in `kypost_catalog.stores`; every other file stays in its volume. Back up the volumes and the database together, or use sealed capsules, which carry both. The manual volume procedure below covers SQLite only. With replicas on one host, every replica shares these three volumes; what stays per replica and what moves into the database is listed under [Replicas on one host](RUNNING.md#what-is-shared-and-how). The replica files `replica-volume.id` (in each root) and `state/server-instance.lock` (SQLite) are small markers, not data.

## Backup and Restore

For sealed capsules, pairing, scheduling and offline recovery, follow [docs/RESTORE.md](RESTORE.md). The manual volume procedure below produces an unencrypted archive and requires secure storage.

Back up with the container **stopped**. This is not caution for its own sake:

- `state.db` is SQLite in WAL mode. Copying `state.db` while KyPost is writing
  gives you a file whose committed data is still sitting in `state.db-wal`. It
  will open, and it will be missing whatever was in flight.
- The four volumes are not independent. `kypost_private` holds the keys that
  decrypt what is in `kypost_config`, and `kypost_state` holds mailbox
  checkpoints that only make sense against the accounts in `kypost_config`.
  Archiving them at different moments produces a set that never existed
  together — the failure shows up at restore, as credentials that will not
  decrypt or a checkpoint pointing past mail that was never processed.

Nothing here is Ollama: the model bind mount is a cache and re-downloads.

Shutdown can take up to 36 minutes when both API and daemon are draining work: Supervisor stops their groups sequentially. Compose supplies that budget; give external orchestrators the same allowance. Idle services exit promptly. See [sealed backup shutdown](RESTORE.md).

### Back up

```bash
cd /path/to/kypost-server

# 1. Record what you are backing up, as an immutable digest. A backup you cannot
#    match to a version is a backup you cannot safely restore, and a tag is not
#    a version — `stable` will mean something different by the time you need it.
docker image inspect --format '{{index .RepoDigests 0}}' \
  "$(docker compose images -q kypost-server)" > backup-version.txt
cat backup-version.txt

# 2. Stop. Not `pause`, not `kill` — a clean stop lets SQLite check its WAL back
#    into the database file.
docker compose down

# 3. Archive all four volumes in ONE pass, so they are consistent with each other.
#    The volumes are Compose-managed, so their real Docker names carry the
#    project prefix from `name:` in docker-compose.yml — `kypost_config` in the
#    Compose file is `kypost-server_kypost_config` to `docker volume`. Confirm
#    with `docker volume ls | grep kypost` before running this.
docker run --rm \
  -v kypost-server_kypost_config:/v/config:ro \
  -v kypost-server_kypost_private:/v/private:ro \
  -v kypost-server_kypost_logs:/v/logs:ro \
  -v kypost-server_kypost_state:/v/state:ro \
  -v "$PWD":/backup \
  alpine tar czf /backup/kypost-backup.tar.gz -C /v .

# 4. Start again.
docker compose up -d
```

Check the archive is not empty before trusting it — a mistyped volume name
mounts a new empty volume rather than failing:

```bash
tar tzf kypost-backup.tar.gz | grep -E 'private/|state/users/' | head
```

Store `kypost-backup.tar.gz` and `backup-version.txt` together, and store them
encrypted or somewhere you would be willing to keep your mail. The archive
contains `imap-config.key` and `totp-secret.key`, which unwrap every stored IMAP
credential and TOTP secret on the install. It also contains the retained legacy
PGP master key, `pgp-private-key.key`, when present. For unmigrated accounts,
that key can decrypt the `pgpPrivateKeyEnc` private keys in `users.json`.
Treat the archive as containing those accounts' usable PGP private keys.
Client-protected keys remain wrapped under the user's password-derived key;
the archive alone does not supply that client-side secret. Keep the legacy
master key until every legacy account has migrated (see
[Where your PGP private key lives](PGP-KEYS.md#where-your-pgp-private-key-lives)).

### Restore

Restore into **empty** volumes, running the **same version** the backup was
taken from. Restoring an old state directory under a newer server means the
newer server's migrations run against it — which is a supported path, but it is
an upgrade, and doing it in the same step as a restore means a failure has two
possible causes.

```bash
cd /path/to/kypost-server

docker compose down -v          # removes the named volumes and their contents

# Let Compose create the volumes, so they carry the project labels Compose
# expects to find on them. `create`, not `up`: creating them by hand with
# `docker volume create` makes Compose treat them as foreign, and starting the
# server would run first-run bootstrap — generating an admin account and a
# first-run password file into the volumes you are about to restore over.
docker compose create

docker run --rm \
  -v kypost-server_kypost_config:/v/config \
  -v kypost-server_kypost_private:/v/private \
  -v kypost-server_kypost_logs:/v/logs \
  -v kypost-server_kypost_state:/v/state \
  -v "$PWD":/backup \
  alpine tar xzf /backup/kypost-backup.tar.gz -C /v

# Start the exact image recorded in backup-version.txt. A locally built install
# has no published digest — restore it with `docker compose up --build -d` from
# the commit it was built at instead.
printf 'services:\n  kypost-server:\n    image: %s\n' "$(cat backup-version.txt)" \
  > docker-compose.restore.yml
docker compose -f docker-compose.yml -f docker-compose.restore.yml up -d
```

This copies the volumes back as they were, credentials included: an AI agent
token revoked after the backup works again. Ask users to revoke and recreate
their tokens under Security → AI agents. A sealed-capsule restore
([RESTORE.md](RESTORE.md)) revokes every agent token itself.

### Verify the restore

A backup nobody has restored is a hypothesis. Check all four volumes actually
came back, because each one fails differently and three of the four failures are
silent until someone needs them:

1. **Sign in** as an existing user — proves `kypost_config` (`users.json`) and
   session/password material.
2. **Open an encrypted message** — proves `kypost_private` and the PGP key
   wrapping. This is the check people skip; a wrong `imap-config.key` looks fine
   until mail needs decrypting.
3. **Confirm a paired device is still listed**, and that TOTP still validates —
   proves `totp-secret.key` and per-user `state.db`.
4. **Watch one poll tick** in Settings → Admin → Server → Application (or
   `docker compose logs -f`) and confirm the checkpoint advances rather than
   reprocessing the whole mailbox — proves the per-user `state.db` mailbox state
   survived. A reset checkpoint re-labels and re-notifies everything.

Only once that passes should you upgrade to a newer version, as a separate step.

## Table documents under development

MH2 adds `volumes.db` for table-mode settings, contacts, photos and other document stores. Each derived body is encrypted with an org data key wrapped under provisioned `SECRET_DIR/documents.key`. Capsules carry the database and every required key; restore authenticates both document encryption and any nested mail/contact-sync credential encryption before using them. Whole-org purge removes documents and its wrapped key; removing one member preserves the other members' key.

The owner approved a 64 MiB plaintext cap per table document on 2026-11-23. The later opt-in migration must refuse a larger file explicitly. Existing file-backed single-tenant installs keep their current limits and bytes. The capsule library still imposes its own per-file and total limits; a large `volumes.db` can refuse a capsule rather than silently omit data.

MH3 adds table-mode account rows, fresh account reads, serializable mutations and the same-transaction account guard before a derived body or org key can be created. Account capsules carry those rows in `volumes.db` and omit stale `users.json`; restore checks routing columns and required secret keys without expanding an account file. A purge removes account rows first and rechecks late members before completion.

Hosted startup is still refused. Preparation and remaining cross-store fences (MH4), key provisioning, volume-free startup, regions and object storage remain separate gates. No migration command or SaaS readiness is introduced by this groundwork.

Historical MH2 document-table stores or capsules can still carry account authority
in `users.json`. Startup now refuses that mixed layout before mounting account
rows, identifying the retained file and requiring explicit offline conversion.
Keep the restored files/database and services stopped; MH8 owns that migration
and is not implemented by this startup check. Never remove `users.json` to bypass
the refusal: an empty table would discard the retained identities and administrator
access. A migrated single install also refuses empty, administrator-less or corrupt
table accounts. Ordinary un-migrated single installs continue using their files.

Table-mode org purge retains a directory deletion marker in the PostgreSQL store
catalog while dropping its schemas. New opens and staged publication refuse the
marker; an already-open writer refuses the missing namespace after its writer
lock, so it cannot fall through to `public`. Markers are control metadata rather
than capsule database members, and ordinary single-mode removal is unchanged.
Sealed recovery continues to require stopped/fenced sources and fresh erasure
reconciliation before any release; a runtime worker cannot carry its old handle
across a restore. Hosted startup remains refused.

Table-mode services refuse a nonempty `KYPOST_BACKUP_DIR`: unset it and pair
KyRecovery for backup deposits. Without that destination, a run fails explicitly
and keeps its normal retry interval. Explicit capsule export remains available
without leaving a scheduled capsule copy on a replica. Backup CLI verbs select
the same table/account authority as runtime before opening the backup service;
historical retained accounts still require the offline migration. Ordinary
unmigrated single-tenant installs keep their existing local-copy behavior.
Hosted startup and the remaining multi-host launch gates stay in place.
