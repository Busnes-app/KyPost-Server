# Scripts

## Purpose

Container initialization, process orchestration, Ollama model management, host-side image updates, and controlled operator mail setup.

## Ownership

All files under `scripts/`.

## Local Contracts

- `ensure-network.sh [NETWORK [SUBNET]]` owns network-only bootstrap for `kypost-net`, using KyPost's fixed Compose ownership labels. Matching reruns inspect only; foreign ownership/subnet/driver is preserved and refused. No server, volume or credential is created.

### Startup Sequence

`entrypoint.sh` (runs `bootstrap.sh` synchronously, then chowns, then runs `kypost-server migrate-native` as `kypost`, then execs) → `supervisord` → programs in the priority order below; receiving programs start only when enabled.

- `entrypoint.sh`: creates required directories, takes root ownership of config/private/state roots using `CAP_CHOWN` before setting them to `0700` (no `CAP_FOWNER` required), runs `bootstrap.sh` inline (as root, before the privilege-dropping `chown`), chowns only config/private/logs/state data directories to `kypost`, runs the native storage migration as `kypost` via `setpriv` (so it never creates root-owned files), then execs `supervisord`. A failed migration prints a remediation and boot continues: native mail refuses half-migrated state on its own and IMAP must stay up, so never turn it into an exit. Running `bootstrap.sh` as a blocking step here — rather than as its own supervisord program — makes "credentials exist before any service starts" a hard guarantee instead of one relying on supervisord's priority-based ordering.
- `bootstrap.sh`: execs `kypost-server --mode bootstrap-admin` (implemented in `backend/internal/app/bootstrap.go`). On a fresh install — neither `users.json` nor `admin.env` present — it writes `ADMIN_PASS_HASH` (Argon2id of the browser-compatible derived auth secret) plus `ADMIN_LOGIN_SALT` and `ADMIN_LOGIN_ITERATIONS` to `admin.env`, which the backend imports as a derived account into `users.json` on first start; a no-op when either file exists, so it is safe across restarts. A generated password is written to `first-run-password.txt` (mode `600`) in `CONFIG_DIR`, never to stdout; a password supplied via `BOOTSTRAP_ADMIN_PASS` writes no file. The hashing lives in the Go binary rather than inline `node -e`, which is what let the runtime image drop from `node:slim` to `debian:stable-slim` — no runtime script here may reintroduce a `node` dependency.
- `crash-exit.sh`: supervisord event listener on `PROCESS_STATE_FATAL`. Core program failures send `SIGTERM` to PID 1 so the container exits and `restart: unless-stopped` restarts it. Only the optional `receiving-server` accelerator stays FATAL without container shutdown: commands fall back in process, and the listener acknowledges the event and keeps watching core programs. It speaks the supervisor event protocol on stdin/stdout, so it must print nothing to stdout but `READY` and `RESULT`.
- `start-receiving.sh`: opt-in Linux x86_64 receiver launcher; requires all three native flags, hashes and executes the same open qualified read-only regular engine file, and regenerates config through existing storage/TLS/domain admission before each start. Failure retains old complete config. No download or spool initialization; trusted host same-inode writes remain operator authority. See `docs/RECEIVING_SETUP.md`.
- `start-ollama.sh`: launches Ollama daemon on port 11434
- `pull-ollama-model.sh`: pulls the model named by `OLLAMA_MODEL` (docker-compose default: `nemotron-3-nano:4b`); requires Ollama daemon to be running first. It owns retries for an unwritable models dir, an unanswered Ollama API and a failed pull (15–60s backoff, then every 900s), exiting 0 once installed. Supervisor restarts unexpected exits only (`autorestart=unexpected`, `exitcodes=0`, `startsecs=0`). Keep the retry loop in-script so an ordinary pull failure never restarts the container. The daemon's classifier warm-up pulls only when `/api/show` says the model is missing, because a pre-populated cache the container cannot write is supported. Classification being down is reported as `classifierFailing` in `GET /api/health`, not as an unhealthy container.
- `update-host.sh`: runs only on the Docker host. It resolves the official image to a digest, verifies its GitHub attestation, locks, waits for health, and recreates the prior digest if the new container fails. It recreates with the compose files recorded on the running container (`com.docker.compose.project.config_files`, minus its own deleted `.kypost-update.*.yml` override; else `COMPOSE_FILE`, else the base file) so overlays survive, and re-points the local tag at the deployed digest after an update, a rollback or an already-current run so a plain `docker compose up -d` cannot downgrade. It never enters the image or receives the Docker socket.
- `test-relays.sh`: the canonical Cloudflare Worker test command, run by CI and by `npm test` in both push Worker packages. It discovers `*.test.mts` and `*.test.mjs` under `push-relay-shared/`, `worker/src/`, `worker-apns/src/` and `receiving-worker/`; it prunes `node_modules` and fails when it finds nothing. Install `worker/` dependencies first: the FCM key-floor fixture executes its locked native workerd with an explicit isolated provider service.
- `install-auto-update.sh`: explicitly enables lingering, then installs a daily per-user systemd timer that calls `update-host.sh --auto`; it is opt-in and runs as the user already authorized to operate Docker, never as root from a mutable checkout.

### Supervisord Programs (from `supervisord.conf` at repo root)

| Program | Type | Priority |
|---------|------|----------|
| api | daemon | 10 |
| daemon | daemon | 10 |
| ollama | daemon | 10 |
| ollama-model | one-shot | 11 |
| receiving-server | optional accelerator; FATAL leaves the container up | 19 |
| receiver | optional daemon; disabled by default | 20 |
| crashexit | event listener (`PROCESS_STATE_FATAL`) | n/a |

`startretries` is 20 on the four long-running programs (including the optional receiver). Not 3 (supervisord's default, which gives up silently) and not a very large number: supervisord does **not** back off between restart attempts, so a program that exits immediately restarts as fast as the machine allows, forever, on a box that is also running an LLM. 20 covers the transient case — a dependency not ready yet — and `crashexit` handles what comes after.

## Work Guidance

- `setup-mail.sh` is host-only and interactive on Linux x86_64. Keep its wizard-template library unchanged. Validate public dotenv values and owned regular private env files before writes; never source dotenv or collect account/provider credentials. Confirm base rebuild (which pauses reception), new-spool initialization and profile launch. Reuse existing UI and production receiving admission; leave external TLS/DNS/delivery observations explicit. Final `setup-status` reports local configuration blockers; its successful exit is not deployment acceptance.

- `bootstrap.sh` must keep running before the scoped recursive data-directory ownership handoff in `entrypoint.sh` — it writes as root, and that chown is what hands the resulting files (`admin.env` and `first-run-password.txt` included) to `kypost`
- No runtime script may depend on `node`. The runtime image is `debian:stable-slim` and has no JavaScript interpreter; the frontend is static files built in an earlier stage
- Never echo a generated credential to stdout from these scripts — that is the container log stream
- `pull-ollama-model.sh` is idempotent; safe to re-run
- `update-host.sh` must remain host-only. Do not mount `/var/run/docker.sock` into KyPost or turn the web UI into an update trigger; that would give a mail-facing process host control.
- Do not raise `startretries` to "fix" a crash loop, and keep `crashexit` active for core programs. Only `receiving-server` may degrade to in-process checks; the actual receiver must still take the container down after persistent startup failure.

## Verification

- `bash scripts/ensure-network.test.sh` uses two disposable Docker networks to prove network-only bootstrap, unchanged matching reruns and foreign-network preservation.

- `python3 scripts/check-publish-main.py` executes the workflow gate against disposable local Git repositories: current tested main publishes, stale reruns and mismatched tested commits skip, and failed CI or remote lookup refuses publication.

- `python3 scripts/check-rspamd.py` checks resolved Compose isolation and the actual pinned Rspamd effective modules/loopback-only socket, direct/Cloudflare clean/GTUBE verdicts, hosted disabled peer-authentication groups and log privacy using disposable network-disabled state. The image pin lives only in `docker-compose.rspamd.yml`; the script and the backend proof read it from there, so a Dependabot bump of that file alone stays green. `rspamd/rspamd.conf` enables the `settings` module so Cloudflare's request-level disabled peer-authentication groups take effect, and is the fixed selected-module policy consumed by the optional overlay; keep its upstream remote services/maps and controller/proxy disabled. Its SPF and DKIM modules feed automatic sender blocks (`R_SPF_ALLOW`, `R_DKIM_ALLOW`); keep both enabled, and rerun the backend `TestReceivingRspamdAuthenticationProof` (`RSPAMD_PROOF=true`) after any policy or image change. Actual Maddy integration is owned by the backend receiving test.

- `bash scripts/setup-mail.test.sh` loads definitions only and checks public-input rejection, private dotenv creation/upserts, unsafe-file refusal and actual base/overlay Compose resolution against conflicting inherited settings. It never opens a browser or starts a container. Verify interactive stages statically; do not run them in CI.

- `python3 scripts/check-receiver-launch.py` verifies incomplete flags and wrong-hash, symlinked or writable engines refuse before configuration creation. The optional actual-image supervised Maddy check in `backend/internal/app/receiving_maddy_test.go` proves automatic crash restart, idle shutdown and incomplete-DATA shutdown; see receiver setup.

- `python3 scripts/check-private-roots.py <built-image>` checks image root permissions and boots the real entrypoint over disposable permissive runtime-owned Docker data volumes with all capabilities dropped except CHOWN, DAC_OVERRIDE, SETUID and SETGID. It verifies unprivileged ownership, private bootstrap metadata, protected executable assets, preservation of existing bytes/descendant modes and that a seeded version-1 mail domain is migrated with every config file owned by the runtime user, with networking disabled.

- `python3 scripts/check-supervisor-shutdown.py --contract-only` verifies the Compose grace covers cumulative Supervisor stop waits plus teardown, and that the api's `stopwaitsecs` covers its own drain (`readinessDrainDelay` + `shutdownTimeout` + `depositBudget`, read from the Go source). `--runtime-only` runs two disposable draining workers against the image's installed Supervisor and proves separate same-priority groups stop serially; CI runs it unprivileged with networking disabled.

- `python3 scripts/check-receiving-gateway.py /path/to/maddy` qualifies the pinned Maddy 0.9.5 x86_64 Linux binary using a disposable loopback receiver/spool: refusal, MIME/envelope fidelity, process-crash replay and acknowledgment cleanup. See `docs/RECEIVING_GATEWAY_ASSESSMENT.md` for the pin and remaining adoption gates; this is test-only, not runtime setup.
- `go test ./internal/app/ -run BootstrapAdmin` covers the seeding contract: a usable admin account, `600` on both secret files, idempotency across restarts, and the `users.json`-already-exists upgrade path
- `sh -n scripts/crash-exit.sh` parses; `docker compose config -q` fails without `KYPOST_BIND` and succeeds with it
- `bash -n scripts/update-host.sh scripts/install-auto-update.sh` parses and `bash scripts/update-host.test.sh` covers no-op (bare-hex Compose IDs), overlay-preserving update with tag move, and health-failure rollback without Docker; do not run the installer in CI because it enables user lingering via `loginctl` and installs live units under `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user`
- `shellcheck scripts/*.sh` is clean (CI); silence a finding only with a targeted `# shellcheck disable=` naming the reason. `#!/bin/sh` scripts must also pass `sh -n` under dash
- `python3 scripts/check-replicas-compose.py` (ci-scripts) renders `docker-compose.yml` + `docker-compose.replicas.yml` with synthetic inputs, starting nothing: each required variable is named when missing; `KYPOST_REPLICAS` applies; no fixed name or app port; the three shared data volumes plus a per-container logs volume; PostgreSQL 17 digest-pinned, C/UTF8, internal network, no host port, operator password; HAProxy probes `/readyz` each second (2 fails, 2 passes) and its `set-timeout` rules equal docs/RUNNING.md's proxy table; `KYPOST_BIND` gates the proxy publish; no database URL in any argv; and the receiving, rspamd and lan-dns overlays refuse to render with it (their `x-single-container` guard on `KYPOST_PROXY_IP`).
- `bash scripts/check-replica-cutover.sh <image>` (ci-docker; shellcheck-clean) drives the documented replica cutover with a built image: SQLite seed (sign-in, first-run password change, `testdata/cutover-recovery-public.b64` pinned; its private half was discarded), a clean stop with no `-wal`, `migrate-to-postgres --check` then the copy with the source digest unchanged and a repeat refused, two replicas behind HAProxy on a fixed loopback port, one sign-in proven on both replicas, one replica stopped with exit 0, Back up now and `backup-drill`, and a container with a fresh config volume refused. Its own project, network and volumes are removed on exit; no relay, AI or outside host. `scripts/replica-cutover-client.py` is its stdlib HTTP client (derives the auth secret as the browser does; prints no secret).
- `scripts/entrypoint.sh` creates `/run/kypost` for supervisord's per-container socket and pid, leaves a data root alone when it is already `kypost:kypost` `0700` (replicas share the volumes), and starts no bundled Ollama when `OLLAMA_BASE_URL` is not loopback.
- `python3 scripts/check-strong-crypto-compose.py` renders five Compose chains with and without `docker-compose.strong-crypto.yml` and fails unless the only difference is `GODEBUG=fips140=on` on `kypost-server`. It starts nothing.
- `scripts/check-env-example.sh` fails when a `VAR=` or `#VAR=` line in `.env.example` is not passed by `docker-compose.yml`; add the passthrough, or allowlist the variable in the script with where it is consumed instead
- `scripts/check-toolchains.sh` fails when `backend/go.mod`'s `toolchain`, `frontend/.nvmrc` or `frontend/package.json` `engines.node` differ from the Dockerfile `golang:`/`node:` tags, or `tools/go.mod` needs a newer Go; each message names the file to update.
- `bash scripts/supervisord.test.sh` checks distinct log files and the real crash listener's event protocol with the PID 1 signal stubbed: accelerator FATAL keeps listening, core FATAL stops the container, and similar program names get no exemption.
- `bash scripts/test-relays.sh` runs every relay test; it must report a nonzero file count, since finding nothing is a failure, not a pass
- On a fresh install `bootstrap.sh` must produce a valid `admin.env` with a non-empty Argon2id hash of the derived auth secret (`$argon2id$` prefix) plus `ADMIN_LOGIN_SALT` and `ADMIN_LOGIN_ITERATIONS`, owned by `kypost`; on an install with `users.json` or `admin.env` present it must leave both untouched

## Child DOX Index

No child AGENTS.md files.
