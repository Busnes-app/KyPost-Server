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
credential and TOTP secret on the install. It does **not** contain anything that
can decrypt a user's PGP private key — that half of the wrapping key never
leaves the browser (see [Where your PGP private key lives](PGP-KEYS.md#where-your-pgp-private-key-lives)).

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
