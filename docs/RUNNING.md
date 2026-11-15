# Running

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Requirements

- Docker
- Docker Compose

Optional for local development (outside Docker):

- Go 1.27.4 (the toolchain pinned in `backend/go.mod`)
- Node.js 26.13.0 (see `frontend/.nvmrc`)
- The npm version bundled with that Node release; install dependencies with `npm ci`

## Quick Start

1. Clone the repository.
2. Copy the environment defaults and set `KYPOST_BIND`.

   ```bash
   cp .env.example .env
   ```

   `.env.example` ships with `KYPOST_BIND=127.0.0.1`, which is right when your
   reverse proxy runs on this same host. It is **required** — compose refuses to
   start without it — because the alternative is a silent default, and this port
   serves plain HTTP unless `TLS_CERT_FILE` is set. See [docs/TLS.md](TLS.md).

3. Pull the published image and start the container.

   ```bash
   docker compose pull && docker compose up -d
   ```

   This runs `ghcr.io/busnes-app/kypost-server:stable`. To pin a release, set
   `KYPOST_VERSION=0.6.0` (no `v`) in `.env` first. Verify the image before
   relying on it: [Verifying an image by hand](OPERATIONS.md#verifying-an-image-by-hand).
   Building from source (`docker compose up --build -d`) still works, but
   `scripts/update-host.sh` refuses a locally built image; see
   [Moving a locally built install onto published images](OPERATIONS.md#moving-a-locally-built-install-onto-published-images).

   With AI sorting on, the container also starts Ollama and pulls
   `OLLAMA_MODEL` (the default is about 2.9 GB resident; see
   [Choosing a classifier model](AI-SORTING.md#choosing-a-classifier-model)). Compose caps
   the container at `KYPOST_MEM_LIMIT` (default `8g`). To skip Ollama, see
   [Running without AI sorting](AI-SORTING.md#running-without-ai-sorting).

   The container joins the `kypost-net` network, which other Ky services share.
   If you installed KyIdentity or KyNotes first, read
   [When the network already exists](Reverse_Proxy_Networking.md#when-the-network-already-exists).

4. Open the web UI at http://localhost:5866.

   Before exposing it to a network, put TLS in front of it: read
   [docs/TLS.md](TLS.md). By default KyPost serves plain HTTP.

5. Sign in with the bootstrap credentials. The username is `admin`. On the
   first start KyPost writes the generated password to
   `first-run-password.txt` in the config volume, mode `600`. Read it, then
   delete the file:

   ```bash
   docker compose exec kypost-server cat /kypost/config/first-run-password.txt
   docker compose exec kypost-server rm /kypost/config/first-run-password.txt
   ```

   The password is deliberately not printed to the container logs: those are
   unrotated by default, kept for the life of the container, readable by
   anything with access to the Docker socket, and forwarded to whatever log
   aggregator you have configured. To set your own password instead, pass
   `BOOTSTRAP_ADMIN_PASS` on the first run (no file is written in that case,
   since you already have it). You can also pass `BOOTSTRAP_ADMIN_USER`.
   Compose does not forward either from `.env`; add them under the service's
   `environment:` (for example in a `docker-compose.override.yml`) for that run.
6. Change the password when KyPost prompts you. Until you change it, the
   account can reach only the password-change screen.
7. Create a separate everyday account and sign in as that user. In Settings → Mail → Email Settings, enter your IMAP/SMTP account, press
   Save Email Settings, then Test Email Settings (the test checks IMAP only).
8. In Email Labels, adjust Your Labels and Prompt Tuning. Then save.

   Start-to-finish guides: [external IMAP](guides/IMAP.md),
   [native mail](guides/NATIVE_MAIL.md) and
   [Cloudflare Tunnel](guides/CLOUDFLARED.md).

## Ports

- `5866`: web UI and backend API
- `2525`: internal controlled SMTP listener only with `docker-compose.receiving.yml`; the overlay requires an explicit host address and port. See [receiver setup](RECEIVING_SETUP.md).
- `11434`: Ollama API (not exposed by default in `docker-compose.yml`)

Container startup sets config, private-key and state volume roots to owner-only `0700` before bootstrap and services; host inspection or backups require the volume owner or appropriate operator privileges. Existing descendant permissions are retained.

Mail-domain setup adds no environment variables. Its public challenge and issuer are stored owner-only in `$CONFIG_DIR/native-domains.json`; preserve it with `users.json`, `sso-lifecycle.json`, `native-provisioning.json` and mailbox/state data during backup or rollback. Native issuer/source fields remain private in `users.json`. Each container start runs `kypost-server migrate-native` before the services to convert older native storage in place; it is a no-op without native files, and a failure keeps native mail refused while IMAP works. Roll back by restoring the pre-upgrade backup ([native provisioning](NATIVE_PROVISIONING.md#storage-format-migration)).

## Postgres

KyPost keeps its databases in SQLite files under `STATE_DIR` and `CONFIG_DIR` by default.
It can keep them in one Postgres database instead. This release runs one instance (the
API and daemon processes of one container) per database; more than one is a later
release. Backup, the drill, `restore` and capsules work on both backends.

### The database

Set one of these (see [CONFIGURATION.md](CONFIGURATION.md#database)):

| Variable | Contents |
|---|---|
| `KYPOST_DATABASE_URL` | a `postgres://` URL |
| `KYPOST_DATABASE_URL_FILE` | the path of a file (a mounted Secret) holding that URL; preferred |

Setting both is refused. With neither set, KyPost uses the SQLite files exactly as before.
The URL carries the password: KyPost logs only the host and database name, and no error,
`/api/health` or `/api/status` response repeats it. `/api/health` and `/api/status` report
`"databaseBackend": "sqlite"` or `"postgres"`. Off the host, put `sslmode=verify-full` in
the URL; otherwise TLS is whatever its `sslmode` says.

The server must be Postgres 16 or newer, and the database UTF8 with the C locale, so that
labels and addresses compare exactly as on SQLite (case-insensitive for ASCII letters
only). Create it with:

```sql
CREATE DATABASE kypost ENCODING 'UTF8' LOCALE 'C' TEMPLATE template0;
```

Every command checks this at startup and refuses to run, before writing anything, if the
variables conflict, the URL does not parse, the `_FILE` is unreadable or empty, the
server is older than 16 or the locale differs. There is no fallback to SQLite when
Postgres is configured but unusable. KyPost then creates the `citext` extension (trusted,
so the database owner needs no superuser) and its catalog.

Each SQLite file becomes its own schema: `state.db` is `kypost_state`, the receiving
databases `kypost_ingress` and `kypost_cloudflare`, `native-authority.db`
`kypost_native_authority`, and each account's or mailbox's `state.db` and `mailbox.db` a
`kypost_f_<hash>` schema. `kypost_catalog.stores` maps each file's path to its schema.
The directories stay: JSON settings, keys and contacts are still files in `CONFIG_DIR`,
`SECRET_DIR` and `STATE_DIR`, so mount those volumes as before.

Connections require a writable primary (`target_session_attrs=read-write` unless the URL
sets another value), connect within 5 s, wait at most 5 s for a lock (SQLite's busy
timeout) and end a transaction left idle for 30 s. Each open store keeps a small pool (at
most 4 connections, 1 idle for 15 s); allow a `max_connections` of at least 100.

### The single writer

SQLite lets one writer into a file at a time, and KyPost's stores rely on that. On
Postgres every write transaction takes an advisory lock derived from its schema, so writes
to one account's mailbox are serialised exactly as on SQLite while different accounts
never wait for each other. Reads take no lock.

### Backups

A backup exports each schema from one consistent snapshot into a SQLite file with the
original name and path, so a capsule made on Postgres has the same members as one made on
SQLite, and the drill checks them the same way. Scheduled backups, Back up now, the bulk
(restic) mail backup and the drill work unchanged. Either kind of capsule restores onto
either backend.

### Moving an install from SQLite to Postgres

1. Stop KyPost (`docker compose stop kypost-server`). The copy refuses a database with a
   non-empty `-wal` beside it, which a running or uncleanly stopped server leaves; start and
   stop it once if so.
2. Create the database as above. Nothing else may have written to it.
3. Copy the data as the data owner, with the volumes and the URL file mounted (the image
   has no ENTRYPOINT, so name the executable):

   ```bash
   docker compose run --rm --no-deps --user kypost \
     -e KYPOST_DATABASE_URL_FILE=/run/secrets/kypost-db-url \
     kypost-server /usr/local/bin/kypost-server migrate-to-postgres -from /kypost/state
   ```

   `-config` names `CONFIG_DIR` when it is not the default. The command copies every
   database under the state and config directories in one transaction, prints each
   table's row count, then counts again after the commit and fails (removing the copy) if
   any differs. It refuses a database that holds any row, and a file from a newer release
   (one from an older release is brought up to date on a private copy first). It never
   modifies the SQLite files and exits non-zero on any failure; a failure before the
   commit leaves Postgres as it was.
4. Set the variable on the service and start it. Keep the SQLite files until you have
   checked the install; KyPost on Postgres never reads a file whose path its catalog holds.

The copy is one transaction over every account's tables, so it holds a lock per table: a
large install needs `max_locks_per_transaction` raised for the copy (roughly 40 × the
number of accounts ÷ `max_connections`; 1024 covers a few thousand accounts).

There is no command back. To return to SQLite, take a capsule on Postgres (Back up now or
`export-capsule`) and restore it with the variable unset ([RESTORE.md](RESTORE.md)).

### Restoring into Postgres

Stop KyPost, then run `restore` with the variable set. The capsule is checked and staged
exactly as on SQLite: every restore hold, device-credential revocation, scheduled-send
return and agent-token revocation runs on the staged files. The staged databases are then
loaded into Postgres in one transaction and recounted, and only the other files are
published to the target. A database that holds any row is refused unchanged; restore into
a new, empty database.
