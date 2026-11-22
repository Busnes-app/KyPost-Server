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
It can keep them in one Postgres database instead, which also lets several identical
containers run on one Docker host ([Replicas on one host](#replicas-on-one-host)). Backup,
the drill, `restore` and capsules work on both backends.

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

1. Stop KyPost (`docker compose stop kypost-server`). The copy takes the SQLite
   server-instance lock and refuses while KyPost runs on that data, and it refuses a
   database with a non-empty `-wal` beside it, which an uncleanly stopped server leaves;
   start and stop it once if so.
2. Create the database as above. Nothing else may have written to it.
3. Copy the data as the data owner, with the volumes mounted. Put the URL in a file only
   the data owner can read (`chmod 600 kypost-db-url`) and bind-mount it read-only; the
   image has no ENTRYPOINT, so name the executable. `-from` is required:

   ```bash
   docker compose run --rm --no-deps --user kypost \
     -v "$PWD/kypost-db-url:/run/secrets/kypost-db-url:ro" \
     -e KYPOST_DATABASE_URL_FILE=/run/secrets/kypost-db-url \
     kypost-server /usr/local/bin/kypost-server migrate-to-postgres -from /kypost/state
   ```

   Run it first with `--check` before `-from`: it does the whole copy and every check,
   then rolls back and leaves the database empty. Then run it again without `--check`.
   `-config` names `CONFIG_DIR` when it is not the default. The command copies every
   database under the state and config directories in one transaction, prints each
   table's row count, and checks every table's row count and a digest of its content
   (BLOBs byte for byte) against the source before the commit; it counts again after the
   commit and fails (removing the copy) if any differs. It refuses a database that holds
   any row, a file from a newer release (one from an older release is brought up to date
   on a private copy under `STATE_DIR/backup-scratch`, mode 0700, removed afterwards), a
   pinned recovery key whose `recovery.pub` is missing, and a database URL on the command
   line (argv is visible to every process; use the file). It never modifies the SQLite
   databases and exits non-zero on any failure; a failure before the commit leaves
   Postgres as it was.
4. Give the service the same file and variable, and start it. Put this in
   `docker-compose.override.yml` beside `docker-compose.yml` (Compose merges it in; the
   service already has an `environment:` block, so do not paste it into that file):

   ```yaml
   services:
     kypost-server:
       environment:
         KYPOST_DATABASE_URL_FILE: /run/secrets/kypost-db-url
       secrets:
         - kypost-db-url
   secrets:
     kypost-db-url:
       file: ./kypost-db-url
   ```

   (or set `KYPOST_DATABASE_URL_FILE` in `.env`, which the compose file passes through, and
   mount the file yourself). With either variable set, KyPost never reads a SQLite file
   under `STATE_DIR` or `CONFIG_DIR`, even while Postgres is unreachable: it refuses
   instead. Once the install checks out, move the old `.db` files out of the volumes so
   nothing can mistake them for current data.

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

## Replicas on one host

On PostgreSQL, several identical KyPost containers can run on **one Docker host**, so one
can be restarted or killed without an outage; an upgrade stops every replica, a short full
outage (below). They share the three data volumes
(`kypost_config`, `kypost_private`, `kypost_state`) and one PostgreSQL behind HAProxy. This
is restart continuity, **not** PostgreSQL high availability and **not** host
failover: every replica depends on the one database and the one host. No capacity or
throughput is claimed.

### Why one host

Users, contacts, rules, the native authority and domain records and the DKIM keys are files
on the shared volumes, fenced by `flock`. A flock coordinates containers on one kernel and
one inode; it does not coordinate across hosts, and on NFS, CephFS or Kubernetes RWX
volumes it is unproven. So KyPost refuses, at startup and before listening:

- a replica that does not mount the deployment's volumes: the first start writes one ID to
  `replica-volume.id` in each of the three roots and records it in the database, and a
  root without it, or with another, is refused;
- a replica on another host: a live replica with another `/proc/sys/kernel/random/boot_id`
  refuses the start ("KyPost replicas must run on one Docker host");
- `KYPOST_NATIVE_RECEIVER`: receiving through Maddy needs one container on port 25 and
  stays in the single-container profile;
- a proof-of-work key that cannot be kept in `SECRET_DIR` (each replica would refuse the
  others' challenges).

Compose itself refuses `TLS_CERT_FILE` with the overlay: HAProxy publishes cleartext and
speaks plain HTTP to the replicas, so TLS ends in a proxy in front of HAProxy.

**Replicas have no native receiving.** Cloudflare pickup requires `KYPOST_RECEIVING_RSPAMD=true`
and the Rspamd sidecar, and `docker-compose.rspamd.yml` shares one container's network, so
Compose refuses it with the replicas overlay. With replicas, receive through an external IMAP
account; for Maddy or Cloudflare, stay on the single container. The one exception is a
multi-tenant install's hosted receiving profile (`docker-compose.hosted.yml` and
`docker-compose.mx.yml`, [RECEIVING_SETUP.md](RECEIVING_SETUP.md#hosted-receiving-tier-multi-tenant-only)),
which gives each receiving server its own Rspamd. It cannot start until multi mode can.

Separate hosts, separate volumes, NFS/RWX volumes and multi-node Kubernetes are refused or
unsupported. KyPost has no IMAP, submission or LMTP server and no search index, so nothing
else is per container.

### What is shared, and how

| Thing | With replicas |
|---|---|
| HTTP listener (5866) | Per replica, behind HAProxy, which routes on `/readyz` |
| Maddy SMTP receiving, `receiver.sock` | Refused: single-container profile only |
| External IMAP polling, receiving import, poller, push, WKD recheck, retention, backup schedule | Run by the one replica holding the `daemon` role; the others stand by. In multi-tenant mode receiving import runs on every replica instead, one worker per mailbox at a time under a lease (receiving tier spec R8), and the `daemon` role keeps only its backstop sweep |
| Native outbox recovery, DKIM rotation | The `mail-runtime` role; the outbox claim token fences request-path sends |
| Scheduled sends, Send delay, provisioning, pickup and contact sweeps, agent-token expiry, version check, shared-row cleanup, and in multi-tenant mode the `/api/suite/orgs` poller | The `api-sweepers` role; a `sending` row records its replica, and only a dead replica's rows become uncertain; a mail a dead replica was returning to Drafts is returned again, so it can appear there twice, never lost |
| Sessions (token hash only), CSRF, revocations, SSO epoch | PostgreSQL: a sign-out or revocation on one replica holds on every other at the next request |
| MFA challenges, step-ups, SSO states, pairing nonces, pending device approvals, export grants | PostgreSQL, each spent once across replicas |
| Login, DAV, MCP, MFA, device, password-change, IMAP-login and review-pairing lockouts, the login budget, office budgets | PostgreSQL windows on database time: the limits are the same in total, never N times |
| Proof-of-work spent set, KyCalendar and suite replay guards | PostgreSQL |
| Per-account meters (people, KyDrive, KyMessages, MCP calls, WKD, account writes, push polls, TLS pin, MFA push), cooldowns, office exempt count | Per replica: with N replicas each allows up to N times its limit (O4) |
| CardDAV app passwords | File on the shared volume; each replica re-checks it on every use |
| Mail-import links, IMAP grants and job status | PostgreSQL: a link or grant is spent once on any replica (an IMAP password sealed under `SECRET_DIR/imap-import.key`); status and cancel reach the replica running the job, which reads as interrupted if that replica stops. The job itself runs where its upload or start landed |
| `config.yaml`, office networks | Re-read by every replica within 5 seconds of a change |
| Keys in `SECRET_DIR` | Shared volume; created exclusively, so replicas starting together agree |
| JSON stores (users, contacts, rules, aliases...), DKIM keys, TLS certificate files | Shared volume, flock |
| Backups, `recovery.pub` | One run at a time (`backup-run` lock); a pinned key's sealed record rewrites a missing `recovery.pub` |
| supervisord socket and pid, logs | Per container (`/run/kypost`, an anonymous logs volume) |
| Ollama | One shared service |

### Starting

1. Stop KyPost and put the variables of step 2 in `.env` first: Compose needs them to
   render the overlay. Then move to PostgreSQL
   ([Moving an install from SQLite to Postgres](#moving-an-install-from-sqlite-to-postgres))
   using the overlay's own database: set `KYPOST_POSTGRES_PASSWORD` (a long random hex
   string; there is no default) and `KYPOST_DATABASE_URL` (below), start only the
   database, check, then migrate:

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.replicas.yml up -d --wait postgres
   docker compose -f docker-compose.yml -f docker-compose.replicas.yml run --rm --no-deps --user kypost \
     kypost-server /usr/local/bin/kypost-server migrate-to-postgres --check -from /kypost/state
   docker compose -f docker-compose.yml -f docker-compose.replicas.yml run --rm --no-deps --user kypost \
     kypost-server /usr/local/bin/kypost-server migrate-to-postgres -from /kypost/state
   ```

   The service takes `KYPOST_DATABASE_URL` from `.env` (protected, mode 600), never argv:
   `postgres://kypost:<KYPOST_POSTGRES_PASSWORD>@postgres:5432/kypost?sslmode=disable`.
2. Set in `.env`: `COMPOSE_FILE=docker-compose.yml:docker-compose.replicas.yml`,
   `KYPOST_REPLICAS` (default 2), `KYPOST_REPLICA_SUBNET` (a private /24 outside Docker's
   default pools, which hand out 172.17-172.31 and 192.168 ranges, e.g. `10.89.1.0/24` beside
   `KYPOST_SUBNET`; `docker network inspect $(docker network ls -q) --format '{{.Name}} {{range .IPAM.Config}}{{.Subnet}}{{end}}'`
   lists the subnets in use) and
   `KYPOST_PROXY_IP` (HAProxy's address inside it, which the replicas trust for
   `X-Forwarded-For`). Behind a TLS proxy (nginx, Nginx Proxy Manager, Caddy, cloudflared),
   run that proxy on `kypost-net` with a fixed address and set `KYPOST_EDGE_TRUSTED_PROXY`
   to it: HAProxy keeps `X-Forwarded-For`, `CF-Connecting-IP` and `X-Forwarded-Proto` only
   from that address; from everyone else it drops them (and sets `CF-Connecting-IP` to the
   caller's own address), and the replicas take the client as
   the right-most address that is neither proxy. Never the network gateway: a proxy on the
   host reaches HAProxy from the gateway, which every other host-side caller shares, so it
   cannot be told apart and its forwarded addresses are dropped (every user would share one
   lockout bucket). Check it: signed in, `GET /api/status` through your proxy shows your own
   address as `clientIp`; the gateway (`.1` of `KYPOST_SUBNET`) means every user shares one
   lockout. `KYPOST_BIND` now gates HAProxy's publish, with the same rules as before.
   - **A TLS proxy on the host** (an nginx or Caddy package, a systemd cloudflared) or on
     another host: move it into a container on `kypost-net` with a fixed address before the
     cutover, or stay on the single container. With replicas, `TRUSTED_PROXY_CIDRS` from
     `.env` is not used.
3. `docker compose pull`, then `docker compose up -d --wait`: the first start downloads the
   shared Ollama image (several GB, even with AI sorting off), and `--wait` shows no progress
   while it does. Your proxy's target (`http://KyPost-Server:5866` or
   `KYPOST_IP`) now reaches HAProxy unchanged. For AI sorting, the shared Ollama uses the
   same model folder as the single container (`OLLAMA_MODELS_HOST_DIR`, by default
   `./share/ollama/models`), so a migrated install already has the model; otherwise pull it
   once: `docker compose exec ollama ollama pull nemotron-3-nano:4b` (or your
   `OLLAMA_MODEL`).

**If a replica keeps restarting**, its reason is in `docker compose logs kypost-server`
("kypost api did not start", followed by the end of its error log), for example the shared
settings that differ.

**Upgrades** stop every replica, because mixed versions are not supported:
`docker compose pull && docker compose stop kypost-server && docker compose up -d --wait`.
That is a full outage: the command took 14 to 21 seconds in eight runs for two replicas on
the test host with the image already pulled (stop, then start and ready; the outage users see
is shorter), and longer while a backup drains (up to 16 minutes). `./scripts/update-host.sh`
is for the single container: with replicas it refuses and prints these commands, so
automatic updates stop too. Disable the timer
(`systemctl --user disable --now kypost-update.timer`) and upgrade by hand. A release that changes a shared setting's default also needs
`accept-replica-config` (below) after the replicas stop.

**Shared and per-host settings.** Every replica must have the same shared settings: the
server URL, CAPTCHA, proxies and office networks, native mail, integrations, push relays,
backups, TLS pinning, AI sorting and every secret file path. KyPost records an HMAC of each
(keyed by `totp-secret.key`, so the database never holds a value) on the first start and
refuses a replica whose settings differ, naming the settings only. Per-host settings (port,
log level, memory, concurrency, scratch, the database URL) may differ. After a deliberate
change: stop every replica, then run
`docker compose run --rm --no-deps --user kypost kypost-server /usr/local/bin/kypost-server accept-replica-config`
once with the new settings, and start again. The full list is in
[CONFIGURATION.md](CONFIGURATION.md#replicas).

### Restoring a replica deployment

Restore into a new, empty database beside the old one, which stays untouched until the
restore checks out. The cutover check (`scripts/check-replica-cutover.sh`) runs these steps.
`docker compose` below uses the `COMPOSE_FILE` in `.env`. Everything goes in `./restore`, a
folder only the container's `kypost` user (UID 1000) can read; if your own UID is not 1000,
use `sudo` to copy into it or remove it.

1. Stop every replica: `docker compose stop kypost-server`.
2. Create the new database: `docker compose exec postgres createdb -U kypost kypost_restored`,
   and point `KYPOST_DATABASE_URL` in `.env` at it (`.../kypost_restored?sslmode=disable`).
3. Make the folder, then put the capsule in `./restore/backup.kycap`:

   ```bash
   mkdir -p -m 700 restore && sudo chown 1000:1000 restore
   ```

   Copy one you keep elsewhere
   (`sudo install -o 1000 -g 1000 -m 600 /path/to/backup.kycap restore/backup.kycap`), or take
   the newest local copy from the state volume (`KYPOST_BACKUP_DIR=/kypost/state/backups`):

   ```bash
   docker compose run --rm --no-deps --user kypost -v "$PWD/restore:/restore" --entrypoint sh kypost-server \
     -c 'umask 077 && cp "$(ls -t /kypost/state/backups/*.kycap | head -n 1)" /restore/backup.kycap'
   ```

   Then restore, entering the custodian shares on stdin (one per line, then Ctrl-D). This
   loads the databases into `kypost_restored` and writes the files to `./restore/recovered`:

   ```bash
   docker compose run --rm --no-deps -i --user kypost -v "$PWD/restore:/restore" kypost-server \
     /usr/local/bin/kypost-server restore /restore/backup.kycap /restore/recovered
   ```
4. Copy the three volumes into `./restore/keep.tar` (mode 600), then replace their contents
   with `recovered/config`, `recovered/private` and `recovered/state`. One command: if the
   copy fails, or `keep.tar` already exists from an earlier attempt, it stops before
   anything is emptied, and a failed refill stops with an error.

   ```bash
   docker compose run --rm --no-deps --user kypost -v "$PWD/restore:/restore" --entrypoint sh kypost-server \
     -c 'if [ -e /restore/keep.tar ]; then echo "restore/keep.tar already exists: nothing was changed. To try again, roll back first, then follow \"To try again\" below the rollback." >&2; exit 1; fi; umask 077 && tar -cf /restore/keep.tar.part -C /kypost config private state && mv /restore/keep.tar.part /restore/keep.tar && for r in config private state; do find "/kypost/$r" -mindepth 1 -delete && cp -pR "/restore/recovered/$r/." "/kypost/$r/" || exit 1; done'
   ```

   The copy is written as `keep.tar.part` and renamed only once it is complete. A
   `keep.tar.part` left behind is from a copy that died: never restore from it; the next
   run overwrites it.
5. `docker compose up -d --wait`, then sign in again: the restore cleared every session. The
   first replica records new volume IDs and settings. Once the restored deployment checks out,
   run **Back up now** (Server → Backup): the restore emptied the old local backups, which
   are now only in `keep.tar`. Then drop the old database and remove `./restore`
   (`sudo rm -rf restore`): `keep.tar` and `recovered` hold every private key unencrypted.

**If the restore does not check out**, go back: stop every replica, point
`KYPOST_DATABASE_URL` back at `kypost`, put the volumes back from `keep.tar`, and start again.
The command reads the whole archive first and empties nothing unless it is readable:

```bash
docker compose run --rm --no-deps --user kypost -v "$PWD/restore:/restore" --entrypoint sh kypost-server \
  -c 'tar -tf /restore/keep.tar >/dev/null && for r in config private state; do find "/kypost/$r" -mindepth 1 -delete || exit 1; done && tar -xpf /restore/keep.tar -C /kypost'
```

**To try again**, with every replica stopped (`docker compose stop kypost-server`): move
`restore/keep.tar` out of `./restore` and keep it until the new restore checks out, clear what
the last attempt left, drop its database, then start again at step 2:

```bash
sudo mv restore/keep.tar keep.tar.first
sudo rm -rf restore/recovered restore/.kypost-restore-*
docker compose exec postgres dropdb -U kypost kypost_restored
```

### Drain and readiness

`/livez` answers 200 while the process serves and never touches the database. `/readyz`
answers 503 while the replica drains, or when the database does not answer a ping within
one second (also a read-only standby); HAProxy checks it every second and takes a replica
out after two failures. At SIGTERM a replica turns `/readyz` to 503, refuses new backups,
drills and exports, keeps serving for 5 seconds, then gives ordinary requests 20 seconds and
running backups up to 16 minutes. A daemon gives up its role first, so another takes over
while it drains. The Docker healthcheck still uses `/api/health`.

**Connection budget.** Measured with the real binary: three idle api processes on one
database hold 12 connections (4 each, besides the test's own). Each process has a catalog
pool and one pool per open database (at most 4 connections each, 1 idle), plus one
dedicated connection for its liveness lock and one for each role or backup lock it holds.
For N replicas (an api and a daemon each) plan on at least 2 × N × (8 + 4 × the databases
busy at once); the overlay's default `max_connections` of 400 leaves room for the default
two replicas. **Unproven:** the peak during a backup, which opens every account's databases,
was not measured, and no account count is claimed. With many accounts a backup could use up
`max_connections`, and every replica's `/readyz` would then fail; raise
`KYPOST_POSTGRES_MAX_CONNECTIONS` if `too many clients` appears.

**Other proxies (not executed).** nginx and Nginx Proxy Manager have only passive health
checks: they keep sending to a draining replica until requests fail. Use HAProxy (the
overlay) in front of replicas, or set the timeouts below and accept errors during restarts.

**Kubernetes (single node, not executed).** One node only, the three volumes as
`ReadWriteOnce` on that node:

```yaml
livenessProbe:  { httpGet: { path: /livez,  port: 5866 }, periodSeconds: 10, timeoutSeconds: 2 }
readinessProbe: { httpGet: { path: /readyz, port: 5866 }, periodSeconds: 1, timeoutSeconds: 2, failureThreshold: 2 }
terminationGracePeriodSeconds: 2160
```

### Request deadline and proxy timeouts

Every request answers within 14 seconds: past it KyPost answers `503` with `{"code":"request_timeout"}`. A `503` after the deadline does not prove the request had no effect; the work may still finish once the database answers, so check before retrying. The routes below keep their own, longer budgets and are not cut at 14 seconds. A proxy in front of KyPost (the replica profile's HAProxy, nginx, Nginx Proxy Manager) must allow each route at least the proxy timeout given here, which is the budget plus headroom. With one value for the whole site, use the largest: 17 minutes.

| Route (a prefix and the paths below it; `*` is one path segment) | Budget | Proxy timeout |
|---|---|---|
| `/api/admin/backup/` | 16 min | 17 min |
| `/api/export` | 10 min | 11 min |
| `/api/contacts/export` | 10 min | 11 min |
| `/api/mail/` | 10 min | 11 min |
| `/api/inbox` | 10 min | 11 min |
| `/api/rules/run` | 10 min | 11 min |
| `/api/import` | 10 min | 11 min |
| `/api/imap/` | 2 min | 3 min |
| `/api/contacts/carddav-client/` | 2 min | 3 min |
| `/api/contacts/import` | 2 min | 3 min |
| `/api/contacts/*/photo` | 2 min | 3 min |
| `/dav/` | 1 min | 2 min |
| `/api/kydrive/` | 2 min | 3 min |
| `/api/pgp/pickup` | 2 min | 3 min |
| `/api/admin/native-recovery/` | 2 min | 3 min |
| `/api/admin/mail-relay` | 1 min | 2 min |
| `/api/admin/mail-domain` | 1 min | 2 min |
| `/api/admin/mail-domains` | 1 min | 2 min |
| `/api/admin/mail/poll-now` | 2 min | 3 min |
| `/mcp` | 2 min | 3 min |
