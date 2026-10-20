# Configuration

Environment variables the server reads. `docker-compose.yml` passes the ones `.env.example` offers; the rest are image defaults, overlay inputs or advanced overrides.

**A bad value stops startup; it is never replaced by the default.** Unset or empty means the default. A value that is set but malformed makes the api and daemon both exit with an error naming the variable or `config.yaml` key, logged on every attempt to `api.err.log` / `daemon.err.log`. Supervisor retries, then stops the container.

- Numbers must be whole numbers in range: `WEB_PORT` and `SMTP_PORT` 1–65535; `POW_MAX_NUMBER`, `UNHEALTHY_RESTART_SECONDS` and `CLASSIFY_CONCURRENCY` (up to 1024) at least 1; `CLASSIFY_PACE_MS` 0–3600000.
- On/off switches (`ALLOW_INSECURE_SMTP`, `MFA_CLEAR_ALL`, `KYPOST_NATIVE_*`, `KYPOST_RECEIVING_RSPAMD`) take exactly `true` or `false`.
- `config.yaml` refuses unknown keys. A misspelled `redacton:` stops startup rather than leaving the redaction defaults in force. Keys older versions wrote (`llama`, `classifier`, and `mode`, `keywords` or `folder` under `notifications`) are still accepted and are dropped the next time the file is saved. Per-user `users/<id>/config.yaml` files are written by the server itself and stay lenient. So does the backup step that reads `config.yaml` from a sealed capsule.

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
- `KYPOST_DISABLE_AI_SORTING` (default `false`): `true` turns off automatic labels for every account, overriding each user's auto-apply setting. Filter rules still run; everything else gets the fallback label (Primary). Neither the embedding sorter nor Ollama runs, the bundled Ollama and its model pull are not started, and the Ollama version check and update email stop. Other values refuse startup.
- `KYPOST_KYCALENDAR_URL` and `KYPOST_KYCALENDAR_SECRET_FILE` (both unset by default: KyCalendar pairing off). The URL is KyCalendar's `https://host` with no path; the file holds the shared secret, at least 32 bytes after trimming whitespace. Setting one without the other, a non-https or loopback URL, or a missing or short secret refuses startup. `KYPOST_KYCALENDAR_ALLOW_PRIVATE` (default `false`): `true` lets KyPost reach KyCalendar at a private LAN address; loopback, link-local and multicast stay refused. See [KYCALENDAR.md](KYCALENDAR.md).
- `INSTALL_OLLAMA` (Docker build arg, default `true`): `false` builds the image without Ollama, about 1.7 GB smaller. Such an image never starts Ollama; use it with `KYPOST_DISABLE_AI_SORTING=true` or an external `OLLAMA_BASE_URL`.
- `OLLAMA_BASE_URL` (default `http://127.0.0.1:11434`)
- `OLLAMA_MODEL` (default `nemotron-3-nano:4b`; see [Choosing a classifier model](../README.md#choosing-a-classifier-model))
- `CLASSIFIER_ENGINE` (default `hybrid`: the embedding sorter answers when sure, the LLM otherwise; `llm` sends everything to the LLM. Any other value refuses to start.)
- `EMBED_MIN_CONFIDENCE` (default `0.6`; how sure the embedding sorter must be, 0–1, to label without the LLM)
- `EMBED_MODEL_DIR` (default `/opt/kypost/models/potion-base-8M`, installed by the Dockerfile. A missing or unreadable model is logged as an error and the daemon runs LLM-only.)
- `TUNING_FILE` (default `/kypost/config/TUNING.md`)
- `OLLAMA_MODELS_HOST_DIR` (default `./share/ollama/models`)
- `KYPOST_MEM_LIMIT` (default `8g`) and `KYPOST_CPUS` (default `4.0`): the container's `mem_limit` and `cpus` in `docker-compose.yml`, not read by the server. The bundled Ollama and its model count against this limit.
- `IMAP_CONFIG_FILE` (default `$SECRET_DIR/imap-config.json`)
- `IMAP_CONFIG_KEY_FILE` (default `$SECRET_DIR/imap-config.key`)
- `TOTP_SECRET_KEY_FILE` (default `$SECRET_DIR/totp-secret.key`)
- `SERVER_BASE_URL` (**required for mobile pairing, desktop pairing and Single Sign-On**; recommended always. KyPost embeds this public URL as `srv` in the QR code and uses it to build `reg`, and it is the OIDC `redirect_uri`. Every one of those URLs is where a credential gets sent, so it is never derived from the request's `Host` header — leave it unset and those three features refuse with an error naming it. Pickup links and PGP QR key-exchange URLs fall back to `http://localhost:5866`, which works only on the server itself.)
- `PAIRING_SECRET` (optional. HMAC secret for pickup links, PGP QR key exchange and mobile pairing tokens. Generated automatically on first start and persisted at `PAIRING_SECRET_FILE` — set it only if several replicas must share one secret, and use `openssl rand -base64 32` if you do. A value shorter than 32 bytes is refused and those three features stay disabled, with the reason logged. Bytes, not characters, because the value is used as the HMAC key verbatim; for the ASCII `openssl rand -base64 32` produces they are the same number.)
- `PAIRING_SECRET_FILE` (default `$SECRET_DIR/pairing.key`)
- `REVIEW_PAIRING_USERNAME` (optional, default empty. Enables fast pairing for a disposable Google Play review account at `POST /api/notifications/review-pairing`, which takes the username and derived `authSecret` with the same CAPTCHA/PoW, lockouts and KDF budget as login. Matched exactly (case-insensitive); there is no wildcard. Leave unset on normal servers; MFA and forced-password-change accounts are refused.)
- `PUSH_RELAY_URL` (optional. Base URL of the central push relay Worker that delivers Android native push to FCM. Must be `https://` — the relay key travels on every request — except for loopback.)
- `PUSH_RELAY_KEY` (optional override. Setting `PUSH_RELAY_URL` alone is enough: the server self-registers with the relay on first start and persists the key it is issued at `$SECRET_DIR/push_relay_key`. Set this only to pin a key the operator issued you, or when several servers share one public IP — the relay keeps one active key per address, so the newest registration displaces the previous one.)
- `APNS_RELAY_URL` (optional. Base URL of the central APNs relay Worker that delivers iOS native push. Must be `https://` — the relay key travels on every request — except for loopback.)
- `APNS_RELAY_KEY` (optional override, exactly as `PUSH_RELAY_KEY` above, persisted at `$SECRET_DIR/apns_relay_key`.)
- `SANDBOX_PRIVATE_HOSTS` (optional, default empty. Comma-separated hostnames allowed to resolve to private, loopback, link-local, CGNAT/Tailscale or reserved addresses. Every user-supplied destination is refused there otherwise: CardDAV and WKD URLs, and the IMAP/SMTP servers of external mail accounts, checked at connect time against the address actually dialled. List a mail server on your LAN or tailnet here; an account configured with a bare IP address needs a hostname first, because IP literals are ignored. Exact match only, no wildcards. The admin-configured domain relay and the OIDC issuer are not subject to this guard.)
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
  the daemon shuts down gracefully, as on SIGTERM, and exits non-zero so
  supervisord restarts it.)
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

## Redaction patterns

`redaction.patterns` in `config.yaml` (or admin-only `PUT /api/config`) is
applied in order to each message's sender, subject and body before the
on-device sorter or the classifier sees them. The defaults
(`config.Default()` in `backend/internal/config/config.go`) replace:

- email addresses, including Unicode and IDN (`jürgen@münchen.de`, `xn--` domains);
- IBANs, unbroken or spaced in fours, any case;
- phone numbers: `+` or `00` followed by 7–15 digits; US `555-123-4567` /
  `(555) 123-4567`, also with a bare country code (`15551234567`,
  `1-800-555-0199`, `442079460958`), as in 0.4; and national numbers with a
  leading `0` and at least 9 digits (`030 12345678`, `020 7946 0958`);
- card numbers (13–19 digits);
- SSNs: `123-45-6789`, `123 45 6789`, and nine bare digits only after "SSN" or
  "Social Security", because a bare nine-digit number is usually an order number.

Dates, times, prices, IP addresses, version strings and short order numbers stay
intact. Any run of 10–13 digits (including a UUID's last group), and any number
of 9 or more digits starting with `0`, is read as a phone number: everything the
0.4 defaults removed is still removed. Not caught: national numbers written with dots
(`030.12345678`, which looks like a `dd.mm.yyyy` date) or split across lines, and
a number joined to an email address by a bare hyphen (`555-123-4567-jo@x.de`),
where the address takes the number's last digits and leaves the rest.

The list is saved to `config.yaml` on first start and a saved list replaces the
defaults whole. A list still exactly equal to an earlier default list (0.3–0.4, or the
pre-2026-07-17 list whose doubled backslashes matched nothing) is replaced
with the current defaults at startup and saved back; any edited list, even one
changed character, is left alone. To adopt the current defaults over an edited
list, delete the `redaction:` block from `config.yaml` and restart. Patterns are Go RE2 regular expressions; a `$` in a replacement
must be written `$$`.
