# Running

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Requirements

- Docker
- Docker Compose

Optional for local development (outside Docker):

- Go 1.27.3 (the toolchain pinned in `backend/go.mod`)
- Node.js 26.11.1 (see `frontend/.nvmrc`)
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
   `KYPOST_VERSION=0.4.2` (no `v`) in `.env` first. Verify the image before
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
