# Configuration

Environment variables the server reads. `docker-compose.yml` passes the ones `.env.example` offers; the rest are image defaults, overlay inputs or advanced overrides.

- `KY_LOG_LEVEL`: `info` by default (`debug`, `warn`, `error` supported).
- `KYPOST_BACKUP_DEPOSIT_INTERVAL`: `24h`; `0` disables, otherwise 15 minutes–366 days.
- `KYPOST_BACKUP_DIR`: empty disables local copies; `/kypost/state/backups` or outside the data roots.
- `KYPOST_BACKUP_KEEP`: `7` local copies.
- `KYPOST_BACKUP_ALLOW_PRIVATE_RECOVERY`: `false`; explicit private-network opt-in.
- `KYPOST_BULK_BACKUP_REPOSITORY`: empty keeps mail databases in the capsule; `rest:https://…` (an append-only rest-server, recommended) or an absolute path outside the data roots and backup directory sends them to a restic repository. A local repository is writable by KyPost, so a compromised container could delete its snapshots.
- `KYPOST_RESTIC_BINARY`: `restic`; the restic executable (0.16.0 or newer).
- `KYPOST_BACKUP_SCRATCH_DIR`: empty uses `STATE_DIR/backup-scratch`; an absolute path outside the data roots for backup scratch when STATE_DIR lacks space for a copy of the mail plus a 10%/5 GiB reserve.

Common variables:

- The host-side setup wizard accepts `ENV_FILE` (default repository `.env`) for an owned regular mode-`0600` file. It saves public setup inputs only; provider/account credentials stay in the protected admin UI. This is a wizard input, not a backend setting.

- `KYPOST_NATIVE_MAIL` (default `false`): opt-in KyIdentity native provisioning and local mailbox API/polling. Requires domain proof; preserves existing IMAP accounts. Direct receiving qualification additionally requires `KYPOST_NATIVE_RECEIVING=true`; compose/client-PGP sending from the primary or an administrator-assigned alias requires the configured operator relay; pickup/system sending remains pending. See [native runtime setup and rollback](NATIVE_PROVISIONING.md#opt-in-native-runtime).
- `KYPOST_MAILBOX_QUOTA_BYTES` (default 5 GiB, `5368709120`): every native mailbox's storage quota in bytes, 256 MiB to 1 TiB; other values refuse startup when `KYPOST_NATIVE_MAIL=true` (IMAP-only deployments ignore it). Applied to existing mailboxes by `migrate-native` at the next start; lowering it below a mailbox's usage refuses only that mailbox's new mail. Pair a real quota with `KYPOST_BULK_BACKUP_REPOSITORY`. See [mailbox quotas](NATIVE_PROVISIONING.md#mailbox-quotas).
- `KYPOST_NATIVE_RECEIVING` (default `false`): controlled receiving commands and daemon import; requires native mail, domain proof, prepared users and explicit spool initialization. `receiving config` generates a bounded TLS-only receiver profile; STARTTLS-only reception refuses plaintext senders. `receiving cloudflare route|pickup` supports the separate [one-message hosted pilot](CLOUDFLARE_RECEIVING.md). `receiving quarantine list|release|release-to-current-owner|discard` lists quarantined deliveries (envelope only) and releases one to its frozen mailbox (or, if unresolved, to its address's current owner) or discards it, with the delivery ID repeated after `--confirm`; run it as the state owner (`docker compose exec --user kypost kypost-server kypost-server receiving quarantine list`). Mail waiting for a disabled mailbox or an offboarded or promoted owner is quarantined rather than retried forever ([quarantine release](NATIVE_PROVISIONING.md#quarantine-release)). `receiving blocks list|add|remove` manages manual sender blocks (address or exact domain, optional `--until`, the value repeated after `--confirm`), enforced by both receiving profiles before storage ([sender blocks](NATIVE_PROVISIONING.md#sender-blocks)). No bundled receiver starts. Read [controlled setup and gates](RECEIVING_SETUP.md).
- `KYPOST_RECEIVING_RSPAMD` (default `false`): strict opt-in local spam scanning before new native SMTP acceptance; scanner failure returns a temporary refusal. The Rspamd overlay enables it and Cloudflare pickup requires it; the hosted path uses content/DKIM checks without unavailable peer authentication and retains provider copies on refusal. Existing accepted obligations remain recoverable without rescanning.
- `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN` (default empty, off): the continuous Cloudflare profile's Worker origin, `https://<worker>.<account>.workers.dev`. Requires `KYPOST_NATIVE_RECEIVING=true` and `KYPOST_RECEIVING_RSPAMD=true`, and refuses startup beside the bundled receiver (`KYPOST_NATIVE_RECEIVER=true`). `receiving cloudflare init` creates the credentials and prints the two Worker secrets; `receiving cloudflare rotate`, `takeover --confirm move-receiving-here` and `status` manage them. See [continuous Cloudflare profile](RECEIVING_SETUP.md#continuous-cloudflare-profile).
- `KYPOST_NATIVE_RESTORE_RELEASE` (default `false`): enables `POST /api/admin/native-recovery/release`, which releases a qualified, repaired native restore hold; other values refuse startup. Off, the route answers 404. Enable it only for a deliberate release until the pending qualifications are done ([releasing the hold](RESTORE.md#releasing-the-hold)).
- `KYPOST_NATIVE_RECEIVER` (image default `false`): optional Supervisor receiver startup; requires both native flags above, prepared storage, current proof and operator-owned engine/TLS files. The receiving overlay enables it explicitly.
- Receiver launcher inputs: `KYPOST_RECEIVER_BINARY` (default `/opt/kypost/receiving/maddy`), `KYPOST_RECEIVING_LISTEN` (`0.0.0.0:2525`), required `KYPOST_RECEIVING_HOSTNAME`, `KYPOST_RECEIVING_CERT` (`/opt/kypost/receiving/tls/fullchain.pem`) and `KYPOST_RECEIVING_KEY` (`/opt/kypost/receiving/tls/privkey.pem`). Compose overlay inputs `KYPOST_MADDY_BINARY`, `KYPOST_RECEIVING_TLS_DIR`, `KYPOST_SMTP_BIND` and `KYPOST_SMTP_PORT` are required with no defaults. Follow [setup order and rollback](RECEIVING_SETUP.md#optional-supervised-container-profile).
- `TRUSTED_PROXY_CIDRS` (default empty, trusts nothing): comma-separated CIDRs or IPs of the reverse proxy whose forwarded headers are believed. A malformed entry refuses startup. See [Reverse proxy networking](Reverse_Proxy_Networking.md#trusting-the-proxy).
- `TRUSTED_PROXY_HEADER` (default `x-forwarded-for`): the one header a trusted proxy names the client in, `x-forwarded-for` (nginx, Caddy; right-most element) or `cf-connecting-ip` (cloudflared). The other is never read. Any other value refuses startup.
- `WEB_PORT` (default `5866`)
- `TZ` (default `America/New_York`)
- `SECRET_DIR` (default `/kypost/private`. Every `*_KEY_FILE` / `*_SECRET_FILE` default below is derived from this, so moving it moves all of them together.)
- Domain relay configuration uses `$CONFIG_DIR/native-relay.json` and `$SECRET_DIR/native-relay.key`, with no individual path overrides or environment credentials. Its retained key also derives owner/job-bound outbox encryption keys. Configure through the protected admin API; primary native compose/client-PGP sending uses fresh authority and the durable outbox.
- `OLLAMA_BASE_URL` (default `http://127.0.0.1:11434`)
- `OLLAMA_MODEL` (default `nemotron-3-nano:4b`; see [Choosing a classifier model](../README.md#choosing-a-classifier-model))
- `CLASSIFIER_ENGINE` (default `hybrid`: the embedding sorter answers when sure, the LLM otherwise; `llm` sends everything to the LLM. Any other value refuses to start.)
- `EMBED_MIN_CONFIDENCE` (default `0.6`; how sure the embedding sorter must be, 0–1, to label without the LLM)
- `EMBED_MODEL_DIR` (default `/opt/kypost/models/potion-base-8M`, installed by the Dockerfile. A missing or unreadable model is logged as an error and the daemon runs LLM-only.)
- `TUNING_FILE` (default `/kypost/config/TUNING.md`)
- `OLLAMA_MODELS_HOST_DIR` (default `./share/ollama/models`)
- `IMAP_CONFIG_FILE` (default `$SECRET_DIR/imap-config.json`)
- `IMAP_CONFIG_KEY_FILE` (default `$SECRET_DIR/imap-config.key`)
- `TOTP_SECRET_KEY_FILE` (default `$SECRET_DIR/totp-secret.key`)
- `SERVER_BASE_URL` (**required for mobile pairing, desktop pairing and Single Sign-On**; recommended always. KyPost embeds this public URL as `srv` in the QR code and uses it to build `reg`, and it is the OIDC `redirect_uri`. Every one of those URLs is where a credential gets sent, so it is never derived from the request's `Host` header — leave it unset and those three features refuse with an error naming it. Pickup links and PGP QR key-exchange URLs fall back to `http://localhost:5866`, which works only on the server itself.)
- `PAIRING_SECRET` (optional. HMAC secret for pickup links, PGP QR key exchange and mobile pairing tokens. Generated automatically on first start and persisted at `PAIRING_SECRET_FILE` — set it only if several replicas must share one secret, and use `openssl rand -base64 32` if you do. A value shorter than 32 bytes is refused and those three features stay disabled, with the reason logged. Bytes, not characters, because the value is used as the HMAC key verbatim; for the ASCII `openssl rand -base64 32` produces they are the same number.)
- `PAIRING_SECRET_FILE` (default `$SECRET_DIR/pairing.key`)
- `REVIEW_PAIRING_USERNAME` (optional, default empty. Enables password-based fast pairing for a disposable Google Play review account at `POST /api/notifications/review-pairing`. A trailing `*`, such as `your-demo-username*`, admits usernames with that prefix; bare `*` is refused. Leave unset on normal servers; MFA and forced-password-change accounts are refused.)
- `PUSH_RELAY_URL` (optional. Base URL of the central push relay Worker that delivers Android native push to FCM. Must be `https://` — the relay key travels on every request — except for loopback.)
- `PUSH_RELAY_KEY` (optional override. Setting `PUSH_RELAY_URL` alone is enough: the server self-registers with the relay on first start and persists the key it is issued at `$SECRET_DIR/push_relay_key`. Set this only to pin a key the operator issued you, or when several servers share one public IP — the relay keeps one active key per address, so the newest registration displaces the previous one.)
- `APNS_RELAY_URL` (optional. Base URL of the central APNs relay Worker that delivers iOS native push. Must be `https://` — the relay key travels on every request — except for loopback.)
- `APNS_RELAY_KEY` (optional override, exactly as `PUSH_RELAY_KEY` above, persisted at `$SECRET_DIR/apns_relay_key`.)
- `ALLOW_INSECURE_SMTP` (optional, default off. KyPost **requires** STARTTLS on SMTP submission and refuses to send if the server does not offer it. Opportunistic STARTTLS is not enough: the capability is advertised by the server, so an on-path attacker strips it from the EHLO response and the session continues in cleartext with the full message and the password. Set this to `true` only for a genuinely plaintext relay on a trusted LAN. There is deliberately no per-request or per-account way to set it — a downgrade has to be a deployment decision, not something a caller or a remote server can trigger.)
- `CAPTCHA_PROVIDER` (optional. Set `pow`, `turnstile`, or `friendly` to require a CAPTCHA solution on login. It works together with the built-in lockout of 3 strikes and 15 minutes.)
  - `pow` is self-hosted proof-of-work: the only provider that makes no third-party network call and adds no third-party origin to the CSP. It requires no account with anyone and no keys to obtain — the signing key is generated on first use at `POW_SECRET_FILE`. **It requires HTTPS**: the browser half uses `crypto.subtle`, which browsers expose only in a secure context, so on a plain-`http://` deployment (anything but `localhost`) the check cannot run and nobody can sign in — put TLS in front of the server, as the note in [Quick Start](../README.md#quick-start) says to anyway, or pick another provider. Its difficulty adapts per client IP: an honest first login solves the cheap base challenge in a blink, and each recent failed login from the same address multiplies the next challenge's difficulty, up to a ceiling, decaying after 15 minutes or on a successful login. Each challenge is bound to the address that requested it — the address is signed into the challenge and re-checked when the solution arrives — so the escalation cannot be sidestepped by fetching cheap challenges from a clean address and spending them from an escalated one. (If your own address changes mid-check, say a phone moving from wifi to cellular, the sign-in page tells you to try again and costs you no lockout strike.) Escalation is still counted per address, so an attacker spraying from many addresses gets the base difficulty at each: it prices repetition, not a distributed attacker. An attacker running native code is also one to two orders of magnitude faster than a browser, so this deters casual scripted spraying, not a determined campaign. It does **not** replace the three-strikes lockout, which remains the real brute-force defence. Multi-replica deployments must set `POW_SECRET` so every replica agrees on one signing key — generate it with `openssl rand -base64 32`; anything shorter than 16 characters is refused and the login CAPTCHA then rejects every attempt.
  - `turnstile` and `friendly` verify a token against a third-party siteverify endpoint and need a site key + secret key.
- `CAPTCHA_SITE_KEY` and `CAPTCHA_SECRET_KEY` (required together with `CAPTCHA_PROVIDER=turnstile` or `friendly`; not used by `pow`. The site key is public. The server verifies solutions with the secret key.)
- `POW_MAX_NUMBER`, `POW_SECRET_FILE`, `POW_SECRET` (optional, `CAPTCHA_PROVIDER=pow` only — see `.env.example` for tuning notes)

Less common variables:

- `SMTP_HOST`, `SMTP_PORT` (instance-wide fallbacks used only when an account's
  own IMAP config names no SMTP host or port. Resolution order is: the account's
  saved SMTP settings, then these, then the IMAP host with a leading `imap.` (or
  an embedded `.imap.`) rewritten to `smtp.` — and the IMAP host unchanged if it
  matches neither. The port falls back to `587`.)
- `CLASSIFIER_BASE_URL`, `OLLAMA_API_KEY`, `OLLAMA_GENERATE_PATH` (point the
  classifier at something other than the bundled Ollama — a shared instance, or
  any endpoint that speaks the same generate API. `OLLAMA_BASE_URL` wins if both
  it and `CLASSIFIER_BASE_URL` are set; `OLLAMA_GENERATE_PATH` defaults to
  `/api/generate`; `OLLAMA_API_KEY` is sent as a bearer token when set.
  **A remote classifier receives your mail.** Every message body goes to it after
  redaction, so a non-loopback plaintext endpoint puts that mail — and the API
  key — on the wire in the clear. The server logs an error once per boot when the
  endpoint fails that transport policy, but classification continues, because
  refusing to start a mail server over a variable the operator set deliberately
  is the worse outcome. Use `https://` for anything off-host.)
- `UNHEALTHY_RESTART_SECONDS` (default `300`. How long health may stay red before
  the process exits and lets supervisord and the Docker restart policy bring it
  back.)
- `PGP_PRIVATE_KEY_FILE` (default `$SECRET_DIR/pgp-private-key.key`) and
  `PICKUP_STORE_KEY_FILE` (default `$SECRET_DIR/pickup-store.key`) — the two
  remaining `SECRET_DIR`-derived secret paths.
- `FRONTEND_DIR` (default `/opt/kypost/frontend`. Where the API server reads the
  built SPA from; there is no reason to change it inside the shipped image.)
- `MFA_CLEAR_ALL` (recovery only. Clears two-factor enrollment for **every**
  account on the next boot, then writes a marker so it never runs twice — see
  `.env.example` before using it.)

Notes:

- The sending quota is not an environment variable. Admins set it under Admin → Application (`sendQuota` in `config.yaml`, 60 messages an hour and 1000 recipients a day by default; both must be at least 1) and per account under Admin → Users. A zero in a hand-edited `config.yaml` falls back to the default, never to no limit.

- The classifier model defaults to `nemotron-3-nano:4b` everywhere — `Dockerfile`,
  `docker-compose.yml`, `.env.example`, and the backend's own fallback.
- The image sets `OLLAMA_MODELS=/kypost/ollama-models`.
