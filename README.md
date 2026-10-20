# KyPost

**Start here:** [Standalone installation](INSTALL.md) → [existing IMAP/SMTP account](docs/guides/IMAP.md).
KyIdentity, KyRecovery and the native clients are optional. Real mail access needs a working mailbox and sending service; a healthy web UI alone does not verify delivery.

KyPost is self-hosted email. Use it as a web client for the IMAP account you already have, or let it host your mailboxes as well. It encrypts with OpenPGP, so you can exchange end-to-end encrypted mail with anyone who uses PGP; interoperability depends on the correspondent’s PGP implementation and needs a real send/receive check for your configuration. A local Ollama model sorts your unencrypted mail into labels without it leaving your server, and encrypted mail is never sent to the model. The iOS, Android and Linux phone apps get native push notifications.

Hosting your own mailboxes is opt-in, and receiving mail for them is still in qualification. To rely on KyPost today, connect it to an existing IMAP server.

KyPost polls unread mail, classifies each message, and applies IMAP keywords. It also gives you a web UI to read mail, change the configuration, manage notifications, view logs, and compose mail. Compose supports send and draft save.

## Features

- IMAP web client: inbox reader, folders, compose with SMTP send, scheduled send and draft save, filter rules (GUI and Sieve).
- Sending quotas across immediate and scheduled mail: 60 messages an hour and 1000 recipients a day by default, adjustable by administrators per installation or account.
- Automatic keyword labels from an on-device embedding sorter backed by a local Ollama model; each account keeps its own labels. Labels are a hint, not a security boundary — see [Architecture](#architecture).
- PGP encryption and signing, recipient key discovery, Web Key Directory publishing and opt-in incoming encryption. Read [Where your PGP private key lives](#where-your-pgp-private-key-lives) before relying on it.
- Contacts with CardDAV server and client, groups, and CSV/vCard import and export.
- Multi-user with admin and user roles; TOTP, recovery codes and push-approval MFA; OpenID Connect single sign-on; self-hosted proof-of-work login CAPTCHA by default.
- Browser push, and native push for the mobile apps through the [push relays](#push-relays-cloudflare-workers).
- Opt-in native mailboxes: KyIdentity provisioning across several domains, quotas, receiving through Cloudflare Email Routing or an operator-supplied receiver (both still in qualification), operator-owned relay sending, sender blocks, quarantine, and mail import and export.
- Optional KyCalendar pairing: KyCalendar sends invitations, cancellations and replies from a user's own mailbox, and receives the DKIM-verified invitations that arrive there; each user opts in. See [docs/KYCALENDAR.md](docs/KYCALENDAR.md).
- Sealed configuration and state backups to KyRecovery or a local directory, with restic for mail databases.
- Single Docker container managed by supervisord, with shared JSON logs.

The full list, with the trade-offs each feature carries, is in [docs/FEATURES.md](docs/FEATURES.md).

## Requirements

- Docker
- Docker Compose

Optional for local development (outside Docker):

- Go 1.27.2 (the toolchain pinned in `backend/go.mod`)
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
   serves plain HTTP unless `TLS_CERT_FILE` is set. See [docs/TLS.md](docs/TLS.md).

3. Pull the published image and start the container.

   ```bash
   docker compose pull && docker compose up -d
   ```

   This runs `ghcr.io/busnes-app/kypost-server:stable`. To pin a release, set
   `KYPOST_VERSION=0.4.2` (no `v`) in `.env` first. Verify the image before
   relying on it: [Verifying an image by hand](#verifying-an-image-by-hand).
   Building from source (`docker compose up --build -d`) still works, but
   `scripts/update-host.sh` refuses a locally built image; see
   [Moving a locally built install onto published images](#moving-a-locally-built-install-onto-published-images).

   With AI sorting on, the container also starts Ollama and pulls
   `OLLAMA_MODEL` (the default is about 2.9 GB resident; see
   [Choosing a classifier model](#choosing-a-classifier-model)). Compose caps
   the container at `KYPOST_MEM_LIMIT` (default `8g`). To skip Ollama, see
   [Running without AI sorting](#running-without-ai-sorting).

   The container joins the `kypost-net` network, which other Ky services share.
   If you installed KyIdentity or KyNotes first, read
   [When the network already exists](docs/Reverse_Proxy_Networking.md#when-the-network-already-exists).

4. Open the web UI at http://localhost:5866.

   Before exposing it to a network, put TLS in front of it: read
   [docs/TLS.md](docs/TLS.md). By default KyPost serves plain HTTP.

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

   Start-to-finish guides: [external IMAP](docs/guides/IMAP.md),
   [native mail](docs/guides/NATIVE_MAIL.md) and
   [Cloudflare Tunnel](docs/guides/CLOUDFLARED.md).

## Architecture

The container runs these processes:

- API server: `kypost-server --mode server`
- Polling daemon: `kypost-server --mode daemon`
- Ollama service: `ollama serve`
- One-shot startup pull: `ollama pull <configured model>`

Classification flow:

1. Fetch unread messages from IMAP (`INBOX` by default), plus their list/automation headers and any header a filter rule tests.
2. Run filter rules.
3. Pre-sort: a contact's mail gets `Primary` and skips steps 5-8 (and the rate limit); list or automated mail has `Primary` removed from the labels the model may choose. Only accounts whose label set includes `Primary` are affected.
4. Redact sensitive patterns, then embed the redacted sender, subject and body with the on-device sorter. If it is at least `EMBED_MIN_CONFIDENCE` sure, apply its label and skip steps 5-7 (and the rate limit).
5. Build the prompt from sender, subject, body, and tuning context.
6. Call Ollama `/api/generate`.
7. Match the output against the allowed labels.
8. Apply the IMAP keywords, and remember what was applied (as a vector and a hash) so a later relabel by the user can be learned.
9. Save the checkpoint and the decision history.

> **Labels are a hint, not a security boundary.** The classifier reads
> attacker-supplied text, so a sender can write instructions into their message
> and influence which keyword it gets. No small local model resists this
> reliably. Running `backend/cmd/modeleval` against the injection bucket of its
> corpus puts the shipped default at roughly 50–87% resistance depending on
> prompt config, and every model measured let some through. Treat it as a known
> property of the feature rather than a bug with a fix pending.
>
> The embedding sorter follows no instructions, so "file this under Primary"
> is just more words to it (it resisted all 8 of the corpus's injection probes),
> but a sender still chooses the words it reads and can make a message look
> like whatever label they prefer. Its training data is only your own relabels,
> never the LLM's answers or anything a sender controls on its own.
>
> What that buys an attacker is small and bounded: they can steer the label on
> **their own message** — typically into `Primary` instead of `Promotions`. The
> keyword allowlist is enforced in Go after the model answers (step 7), so
> output that is not an allowed label is discarded; a message cannot be labelled
> as something you never configured, cannot be moved, deleted, or marked read,
> and cannot affect any other message. The worst case is a promotional email
> that sorts itself into your main tab — the same thing a sender achieves by
> writing a more convincing subject line.
>
> Do not build a security control on top of these keywords: no filter rule that
> grants trust based on a label, and no "auto-archive anything labelled X."
> Every decision is recorded, so you can audit what the model actually did on
> the Decisions page.

## Where your PGP private key lives

The location of your private key decides what PGP protects here. This question
gets its own section for that reason, not a bullet.

KyPost has two protection modes for your PGP private key.

**Client-protected (end-to-end).** Your browser generates or imports the key.
The browser then wraps the key under a key derived from your account password.
It uses PBKDF2-HMAC-SHA256 with 600,000 iterations and AES-256-GCM. The browser
uploads only the wrapped blob and the public half.

The server stores that blob and cannot open it, and two things have to be true
for that to hold:

1. **Your password never reaches the server.** The browser stretches it with a
   per-account login salt (fetched from `GET /api/auth/login-params`) and splits
   the result: an authentication half, which is what gets sent, and a
   key-wrapping half, which never leaves the page.
2. **The two halves are derived under different salts.** The wrapping key uses a
   random salt stored inside the envelope; authentication uses the account's
   login salt. Neither value can be computed from the other without the password.

The server accepts only the derived authentication half, never the password. An
account still holding a hash from before derived authentication cannot sign in
until an admin resets its password; such accounts are flagged "needs password
reset" in Admin → Users.

Your browser decrypts and signs. A person who takes the disk, a backup, or the
memory of this process gets ciphertext.

**What this does not defend against.** This server ships the JavaScript that does
the derivation. A server that wants your password can serve a modified bundle
that sends it, and no amount of client-side cryptography prevents that. What you
get is protection against a server that keeps too much, against your password
reaching logs, heap dumps and backups, and against someone who obtains the data
at rest. That is the same trust boundary as every other end-to-end product
delivered through a browser.

The costs are real. Know them before you choose this mode:

- **An admin password reset can strand the password-wrapped key.** Before
  you need a reset, use Security → Encryption to create a recovery copy and
  keep its separately displayed secret. The browser tests the encrypted file
  in memory, requests its download and, after you confirm saving the secret, stores a sealed copy on the server.
  After signing in with the reset password, **Use server recovery copy** can
  restore the current identity even if you lost the download. The server never
  receives the recovery secret or plaintext key. Keep an offline file too:
  deleting the identity removes the server copy, and a server loss removes it
  as well. **Run recovery drill** tests your copy and secret without changing
  the key; its date is recorded only in this browser, for the copy tested.
- **You unlock the key once for each browser session.** The browser holds the
  unwrapped key in page memory only, never in localStorage or sessionStorage.
  After a reload you must enter your password again.
- **KyPost does not add verified send-as addresses to your key
  automatically.** That edit re-signs the key and needs the private half. The
  browser makes the edit, not the background poller.

**Server-protected.** The server seals the key with a master key on the same
volume and unwraps it whenever it needs to. You get the convenience: mail
decrypts without you unlocking anything, a password reset never costs you the
key, send-as addresses get signed in for you, and the background poller can do
its work while no browser is open. Nothing about the key is your problem after
setup.

You pay for that with the trust boundary. This mode is **not** end-to-end
encryption, and earlier versions of this README described it as if it were. The
server, and any person who can read that volume — whoever holds root, whoever
holds a backup, whoever seizes the disk — can decrypt everything you have ever
received. If you run this server yourself on hardware you control, that may be a
trade you are happy to make. If someone else runs it, you are trusting them with
your mail in the clear.

Choose this mode deliberately, not by accident. If you decide the trade is not
worth it, the Security page offers a one-time migration: it hands the key to your
browser, rewraps it under your password, and deletes the server-readable copy.

Some facts apply to both modes, and they are worth a plain statement:

- **Ordinary PGP/MIME does not encrypt subject lines.** Your mail provider sees
  them in both modes. KyPost protects the subject inside the encrypted part when
  it can, but the outer header remains.
- **Mobile push notifications are generic by default** for this reason. See
  Settings → Notifications.
- **A recipient without a key can get a one-time pickup link.** KyPost stores
  that message on this server and encrypts it with the server's own key. The
  message stays until the recipient reads it or it expires. It is not end-to-end
  encrypted. Nothing sent to a person with no key can be. This fallback is
  opt-in, not automatic. An encrypted send to a keyless recipient fails with an
  error unless the request asks for the pickup-link fallback. Plaintext
  therefore never reaches the server as a side effect of an encrypted send.

## Session Behavior

- Login sessions expire after 24 hours without activity.
- Session expiry slides forward. Each authenticated request extends the TTL by
  24 hours.
- A session also has a hard cap. It dies 7 days after KyPost issued it, whatever
  the activity. A thief cannot keep a stolen cookie alive with their own traffic.
- KyPost sweeps expired sessions every hour. It does not wait for the cookie to
  arrive again.
- Logout invalidates the server-side session and clears the cookie.
- A deactivation or a role change takes effect on the user's next request, not
  at the next login.

## Users and Roles

Accounts live in `/kypost/config/users.json`. The roles are `admin` and `user`.

- Admins manage users from Settings, under Admin. The Server panel creates
  users, changes roles, resets passwords, and deactivates or reactivates
  accounts, alongside runtime settings, updates, verified mail domains and native addresses.
  Diagnostics holds the full health view, the system logs and health repair.
  Default Labels is a Server tab: the label list new accounts start from.
  Email Labels is entirely per-user — each account's own prompt tuning and
  classification decisions — so it is not admin-only.
- Users connect their own IMAP and SMTP account. They read and label their own
  mail, pair their own devices, set their own notification preferences, and tune
  their own prompt.
- Admins can also do the connecting. **Server > Default Mail Server** publishes
  host and port defaults that prefill every new user's Email Settings form; users
  still supply their own username and password. **Server > Manage Users > Mailbox**
  writes a specific user's IMAP/SMTP and CardDAV-client credentials directly, with
  an optional lock. A locked user still sees the host and username on their
  Settings > Mail tabs, rendered read-only with a "managed by your administrator"
  notice, and the server refuses their own writes. Send-As addresses, Mailbox
  Rules, and the built-in CardDAV Access password are never admin-managed. Every
  admin assignment is logged with `user_id`, `admin_id`, and `managed`.
- Deactivation is a soft delete. The user can no longer sign in. KyPost keeps
  their data on disk until you remove it manually.
- KyPost does not let you deactivate or demote the last active admin.

Per-user data layout:

- `/kypost/config/users/<userID>/`: encrypted IMAP credentials, `carddav-client.json` (encrypted CardDAV-client credentials), tuning prompt (`tuning.md`), notification preferences (`config.yaml`)
- `/kypost/state/users/<userID>/`: `state.db` — an SQLite database holding the mailbox checkpoint, the processed set, decision history, push subscriptions, and paired devices. SQLite runs in WAL mode, so `state.db-wal` and `state.db-shm` sit alongside it while the database is open and are part of the state, not scratch files.

Upgrade from a single-admin installation: on the first start, KyPost imports the
legacy `admin.env` account into `users.json`. KyPost also copies the legacy
global mailbox state, IMAP credentials, tuning file, and notification
preferences into that admin's per-user directories. KyPost leaves the legacy
files in place but no longer reads them. There is no automatic rollback. To
reset to a fresh multi-user state, delete `users.json` and the `users/`
directories.

## Ports

- `5866`: web UI and backend API
- `2525`: internal controlled SMTP listener only with `docker-compose.receiving.yml`; the overlay requires an explicit host address and port. See [receiver setup](docs/RECEIVING_SETUP.md).
- `11434`: Ollama API (not exposed by default in `docker-compose.yml`)

Container startup sets config, private-key and state volume roots to owner-only `0700` before bootstrap and services; host inspection or backups require the volume owner or appropriate operator privileges. Existing descendant permissions are retained.

Mail-domain setup adds no environment variables. Its public challenge and issuer are stored owner-only in `$CONFIG_DIR/native-domains.json`; preserve it with `users.json`, `sso-lifecycle.json`, `native-provisioning.json` and mailbox/state data during backup or rollback. Native issuer/source fields remain private in `users.json`. Each container start runs `kypost-server migrate-native` before the services to convert older native storage in place; it is a no-op without native files, and a failure keeps native mail refused while IMAP works. Roll back by restoring the pre-upgrade backup ([native provisioning](docs/NATIVE_PROVISIONING.md#storage-format-migration)).

## Environment Variables

The full reference is [docs/CONFIGURATION.md](docs/CONFIGURATION.md); `.env.example` documents each variable it offers.

### Choosing a classifier model

The default is picked to run on modest hardware. Measured on a 60-email
benchmark (`backend/cmd/modeleval`), five repeats each with zero run-to-run
variance:

| Model | Unambiguous mail | Keyword traps | Prompt injection | RAM resident |
|---|---|---|---|---|
| `nemotron-3-nano:4b` (default) | 100% | 75% | 63% | 2.9 GB |
| `gemma4:e4b` | 100% | 75% | 88% | 8.8 GB |

Both label ordinary mail equally well, and both are perfect on unambiguous
messages. `gemma4:e4b` resists two more of the eight prompt-injection probes —
emails written to talk the classifier into filing them somewhere they do not
belong — but wants three times the memory. Set `OLLAMA_MODEL=gemma4:e4b` if the
host has 12 GB or more free.

Classification speed is not tabulated because it depends far more on your CPU
and on what else the host is doing than on the model: the same request measured
between 13 and 19 seconds on one machine purely as background load varied. The
two models were within about 20% of each other under identical conditions, with
`gemma4:e4b` slightly ahead. The poller paces itself at one message every three
seconds regardless, so throughput is bounded by that unless the host is very
slow.

Either way the damage from a successful injection is bounded: the label
allowlist means a hostile email can at most choose which of the four folders it
lands in, and one probe (an email claiming the label set itself had changed)
defeated every model and every prompt variant tested. Do not treat the assigned
label as a security decision.

Create the model cache directory once before the first run:

```bash
mkdir -p share/ollama/models
```

### Running without AI sorting

Set `KYPOST_DISABLE_AI_SORTING=true` in `.env` to turn off automatic labels for
every account. Filter rules still run, and all other new mail gets the default
label (Primary). Ollama is not started, no model is pulled, and the server never
contacts Ollama. Releases up to and including 0.4.2 ignore this variable and
always start Ollama. To drop Ollama from the image as well (about 1.7 GB smaller),
build it yourself:

```bash
docker compose build --build-arg INSTALL_OLLAMA=false
```

An image built that way never starts Ollama. If you leave AI sorting on with it,
point `OLLAMA_BASE_URL` at an Ollama running elsewhere.

## Mobile App Pairing (Native)

The backend handles mobile pairing directly. It does not require Novu.

- Nothing to configure: the pairing secret is generated on first start and kept at `/kypost/private/pairing.key`. Set `PAIRING_SECRET` only if you run multiple replicas that must share one.
- Required: set `SERVER_BASE_URL` so that QR code payloads point to the correct public backend URL. Use an `https://` URL: pairing tokens, pickup links and QR key-exchange URLs are all built from it, and each carries a bearer credential in the query string. Unset, the pairing panel reports "set SERVER_BASE_URL" and mints no token — the address a credential is sent to is not something a request's `Host` header may choose, and there is no safe guess. It is also what the pairing QR's certificate pin is read from. See [docs/TLS.md](docs/TLS.md) and [Certificate pinning](#certificate-pinning-in-the-pairing-qr) below.
- Keep all pairing secrets on the server only.

Desktop pairing behavior:

- Security's Devices tab renders a QR code link with `sub`, `hash`, `srv`, `reg`, `pt`, and — when the serving certificate can be read — `pin`.
- Set `SERVER_BASE_URL` in `.env`. Then `srv` and `reg` always point to the deployment address that the mobile app must use. Nobody enters a server URL by hand.
- `pt` is a signed pairing token. It is valid for 90 seconds.
- The UI shows a 4px countdown bar under the QR code. The bar shrinks over 90 seconds and changes from green to red. It is red for the last 15 seconds.
- The mobile app scans the QR code and registers its push token through `reg`. If `reg` is absent, the app uses `srv` plus `/api/notifications/native/register` instead.

### Certificate pinning in the pairing QR

The pairing request is the one call that carries the pairing token, the push
endpoint and the app's WebPush keys. Without a pin the app sends all of it and
only *then* decides whether to trust the certificate it just used. On a network
with a locally trusted CA — an MDM root, a certificate someone installed, a
captive portal — an interceptor reads the token, registers its own device
against your server first, and hands the app back credentials it controls.

So the QR carries `pin`: the SHA-256 of your serving certificate's public key.
The app pins the registration handshake to that one key *before* it sends
anything, and refuses a certificate that does not match.

**Set `SERVER_BASE_URL` to your public `https://` URL and this works with no
further configuration.** At the moment it builds each QR, the server makes a
verified TLS connection to that URL and reads the certificate you are actually
serving — whether that is Caddy, Traefik, nginx-proxy-manager, a Cloudflare
tunnel, or this server's own `TLS_CERT_FILE`. Because it is read fresh every
time, certificate renewal needs no action from you.

Two things worth knowing:

- **Behind a proxy, you are pinned to the proxy.** With Cloudflare in front,
  this closes the hostile-network hole between the phone and Cloudflare. It does
  not make the tunnel end to end; Cloudflare still terminates TLS.
- **A private or self-signed CA gets no pin.** The server will not tell the app
  "trust only this key" on the word of a certificate it could not verify. Use
  `TLS_CERT_FILE`/`TLS_KEY_FILE` to terminate TLS here instead, and the pin is
  read from that certificate directly.

A probe failure — an unreachable URL, a router that will not route your public
hostname back to itself — simply omits `pin`, and pairing proceeds on trust on
first use. It never breaks a pairing; it only declines to protect one.
Already-paired devices are unaffected. A missing `SERVER_BASE_URL` is the one
case that is not merely unpinned: there is then no address to put in the QR at
all, so pairing refuses rather than guessing one from the request.

Native registration behavior:

- `POST /api/notifications/native/register` validates the pairing token. It stores the native device metadata and token in the backend state.
- `GET /api/notifications/native/devices` lists the paired native devices and their reported enrollment envelope versions. Older devices default to v2; webmail refuses setup for devices that require a newer format. V3 delivery and conversion remain gated.
- `DELETE /api/notifications/native/devices` removes one paired native device by `deviceId`.
- `POST /api/notifications/native/unpair` revokes all paired native devices for the signed-in user.

Firebase credential guidance:

- The backend never holds Firebase credentials. It never reads `google-services.json`.
- A central **push relay** (a Cloudflare Worker) delivers native push. The relay holds the one Firebase service account that the published mobile app is built against. This lets anyone run their own server with the same app, with no Firebase account and no recompile.
- `google-services.json` belongs in the mobile project, usually at `app/google-services.json` in the Android app module. Never commit it.

## Push Relays (Cloudflare Workers)

Cloudflare Workers deliver native push. The project maintainer runs them.
- **Android/FCM**: [`worker/`](worker/) — Firebase Cloud Messaging relay
- **iOS/APNs**: [`worker-apns/`](worker-apns/) — Apple Push Notification service relay

Self-hosters set one variable and the server does the rest — it registers itself
with the relay on first start, over its public `/register` endpoint, and persists
the key it is issued under `SECRET_DIR`. No operator involvement, no ticket, no
waiting.

- Android: set `PUSH_RELAY_URL` (Firebase relay)
- iOS: set `APNS_RELAY_URL` (APNs relay)

The server looks for a key in three places, in order: the `PUSH_RELAY_KEY` /
`APNS_RELAY_KEY` environment variable, then the key file from a previous
registration, and only then does it register. So restarts reuse the key on disk
rather than minting a new one.

Two things are worth knowing before you rely on it:

- **The relay keeps one active key per public IP address.** Two self-hosted
  servers behind the same address will take the key from each other, newest
  registration wins, and the displaced server's push silently stops working. If
  that is your setup, ask the operator for keys and pin them with
  `PUSH_RELAY_KEY` / `APNS_RELAY_KEY` instead.
- **Registration can be closed.** The operator can turn `/register` off — for
  abuse, or cost. If it is closed, the server logs the refusal and native push
  stays off; `App Pull` delivery works regardless and needs no relay at all.

Self-hosters need no Firebase account and no Apple Developer account. You never recompile the app.

Relay operators deploy both Workers and can still mint keys by hand. See [`worker/README.md`](worker/README.md) and [`worker-apns/README.md`](worker-apns/README.md) for setup, secrets, and key management.

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

For sealed capsules, pairing, scheduling and offline recovery, follow [docs/RESTORE.md](docs/RESTORE.md). The manual volume procedure below produces an unencrypted archive and requires secure storage.

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

Shutdown can take up to 36 minutes when both API and daemon are draining work: Supervisor stops their groups sequentially. Compose supplies that budget; give external orchestrators the same allowance. Idle services exit promptly. See [sealed backup shutdown](docs/RESTORE.md).

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
leaves the browser (see [Where your PGP private key lives](#where-your-pgp-private-key-lives)).

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

## API Highlights

The endpoint reference is [docs/API.md](docs/API.md).

## Build and Dev Checks

These are the same gates CI runs. All of them must pass before a PR merges —
see [CONTRIBUTING.md](CONTRIBUTING.md) for the full contract.

Backend:

```bash
cd backend
go build -buildvcs=false ./...
gofmt -l .            # must print nothing
go vet ./...
go test -race -count=1 -timeout=20m ./...
```

The `-timeout` is not optional. `internal/api` alone exceeds Go's default 600s
under `-race`, which is why CI splits it into its own job.

Frontend:

```bash
cd frontend
npm ci
npx tsc --noEmit
npm test -- --run
npm run build
```

Push relays (only if you touch `worker/`, `worker-apns/` or `push-relay-shared/`):

```bash
(cd worker && npm ci && npm run typecheck)
(cd worker-apns && npm ci && npm run typecheck)
./scripts/test-relays.sh
```

Install and update scripts:

```bash
shellcheck scripts/*.sh
scripts/update-host.test.sh
scripts/check-env-example.sh
```

## Operations

Runtime checks:

```bash
docker compose ps
docker compose logs -f kypost-server
docker compose exec kypost-server ps aux
docker compose exec kypost-server ls -la /kypost/config /kypost/state
docker volume ls | grep kypost
```

Persistence behavior:

- `docker compose up --build` keeps the named volumes.
- `docker compose down -v` removes the named volumes and the stored app data.

## Updating KyPost

KyPost checks GitHub releases hourly. When a newer KyPost release is found, it
emails the primary admin once and shows the update in Settings → Admin →
Server → Application. The container reports availability but never controls Docker on
its host.

See [`CHANGELOG.md`](CHANGELOG.md) for what changed in each release and for the
upgrade/rollback matrix. Take a backup before upgrading — see
[Backup and Restore](#backup-and-restore).

Published releases are available from GitHub Container Registry. From the
checkout, apply the current `stable` image with health-gated rollback:

```bash
./scripts/update-host.sh
```

The script resolves `stable` to an immutable digest, verifies its GitHub build
attestation with `gh attestation verify`, and preserves that exact digest for
rollback. It requires Docker Compose v2, Docker Buildx, and the GitHub CLI
(`gh`), and checks for each up front; it fails closed
when either verification or the health check fails. To stay on a specific
release instead, set `KYPOST_VERSION=0.4.2` in `.env` before running it.

### Verifying an image by hand

Without `update-host.sh` (`gh` logged in), verify the version tag, not `:stable`:
`:stable` moves, so resolve the version it should be and verify that
(`gh release view --repo Busnes-app/KyPost-Server --json tagName --jq .tagName` prints the newest).
The attestation must come from `release-image.yml` at that tag and record the commit the tag
points to, and the image you pull must be that digest:

```bash
v=<release version you intend to run, e.g. 0.4.2>
sha=$(gh api repos/Busnes-app/KyPost-Server/commits/refs/tags/v$v --jq .sha) \
  && d=$(docker buildx imagetools inspect ghcr.io/busnes-app/kypost-server:$v --format '{{.Manifest.Digest}}') \
  && gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/release-image.yml@refs/tags/v$v \
  && [ "$(gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/release-image.yml@refs/tags/v$v \
       --format json --jq '.[0].verificationResult.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit')" = "$sha" ] \
  && docker pull ghcr.io/busnes-app/kypost-server:$v \
  && docker image inspect ghcr.io/busnes-app/kypost-server:$v --format '{{range .RepoDigests}}{{println .}}{{end}}' \
       | grep -qxF "ghcr.io/busnes-app/kypost-server@$d"
```

`:main` is built from main's tip by `publish-main.yml`; name the commit yourself, and the
attestation must record it:

```bash
sha=<full commit sha of main's tip, e.g. $(git rev-parse origin/main)>
d=$(docker buildx imagetools inspect ghcr.io/busnes-app/kypost-server:main --format '{{.Manifest.Digest}}') \
  && gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/publish-main.yml@refs/heads/main \
  && [ "$(gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/publish-main.yml@refs/heads/main \
       --format json --jq '.[0].verificationResult.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit')" = "$sha" ] \
  && docker pull ghcr.io/busnes-app/kypost-server:main \
  && docker image inspect ghcr.io/busnes-app/kypost-server:main --format '{{range .RepoDigests}}{{println .}}{{end}}' \
       | grep -qxF "ghcr.io/busnes-app/kypost-server@$d"
```

Then start that digest, not the tag. Compose builds the image name from `KYPOST_IMAGE` and
`KYPOST_VERSION`, so a tag can resolve to another image than the one you verified, and a
`KYPOST_IMAGE` left on the retired `ghcr.io/busness-app` namespace runs an untrusted image.
Set `KYPOST_IMAGE` to the official name (or remove it) in `.env` first, then, in the same shell
as `$d`:

```bash
grep -q '^KYPOST_IMAGE=' .env && ! grep -qxF 'KYPOST_IMAGE=ghcr.io/busnes-app/kypost-server' .env \
  && echo "fix KYPOST_IMAGE in .env first" >&2 \
  || { o=$(mktemp ./.kypost-manual.XXXXXX.yml) \
       && printf 'services:\n  kypost-server:\n    image: %s\n' "ghcr.io/busnes-app/kypost-server@$d" >"$o" \
       && docker compose -f docker-compose.yml -f "$o" up -d --no-build kypost-server; rm -f "$o"; }
```

`--no-build` stops Compose from building the image from source when the pull did not land.
Then set `KYPOST_VERSION` to that version (or `main`) in `.env`, so later updates follow it.

### Moving a locally built install onto published images

**Every install created before 2026-08-25 needs this once.** There were no
published images before then, so any install older than that is running one it
built itself.

`update-host.sh` refuses a locally built image: it has no published immutable
digest, so there is no rollback target to preserve, and the updater will not
guess at one. Rebuilding from source with `docker compose up --build -d` keeps
you on a local build and hits the same refusal next time. The one-time move onto
published images is:

```bash
git pull --ff-only     # pick up the current compose file
docker compose pull    # fetch the published image over the locally built tag
docker compose up -d   # recreate the container against it
```

Confirm it took — this is the exact property `update-host.sh` tests:

```bash
docker image inspect ghcr.io/busnes-app/kypost-server:stable \
  --format '{{range .RepoDigests}}{{println .}}{{end}}'
```

One `ghcr.io/busnes-app/kypost-server@sha256:...` line means the migration
worked and `./scripts/update-host.sh` will run from now on. No output means the
image is still locally built.

Your data is untouched by this: config, state and private keys live in the four
named volumes, not in the image. Take a backup first anyway — see
[Backup and Restore](#backup-and-restore).

To keep building from source instead, that is still supported — `docker-compose.yml`
retains its `build:` stanza. You update with `git pull --ff-only && docker compose
up --build -d` and simply do not use `update-host.sh`.

Automatic updates are opt-in and require a systemd host. Run this from the
checkout to install a daily timer (03:15 local time plus up to one hour of
jitter). It enables systemd lingering for the Docker-operating user so the
timer continues after logout and reboot; if that needs approval, run the
printed `sudo loginctl enable-linger <user>` command once and rerun it:

```bash
./scripts/install-auto-update.sh
```

Disable it with `systemctl --user disable --now kypost-update.timer`. The timer
runs as the Docker-operating user and uses the same host-side updater, not code
inside the container. On systems without systemd, schedule
`./scripts/update-host.sh --auto` with
`KYPOST_AUTO_UPDATE=true` in the scheduler environment.

## Troubleshooting

### Ollama or model issues

- Check the logs with `docker compose logs -f kypost-server`.
- Confirm that the model pull completed for your `OLLAMA_MODEL`.
- `ollama-model.log` in `/kypost/logs` holds the model installer's own output,
  including its pull retries; `ollama.log` holds the Ollama runtime's.
- If necessary, restart with `docker compose restart`.

### IMAP connection issues

- Verify the host, port, username, password, and mailbox in Settings → Mail →
  Email Settings. IMAP needs the implicit-TLS port (normally 993); STARTTLS on
  143 is not supported.
- Run Test Email Settings there.
- Check `daemon.err.log` and `api.err.log` for authentication, TLS, and keyword failures.

### SMTP send issues

- Verify the SMTP host and port in Settings → Mail → Email Settings. SMTP signs
  in with the IMAP username and password.
- Port 465 requires implicit TLS. KyPost supports it.
- If your provider requires app passwords, use them.
- `smtp submission refused: server did not offer STARTTLS` means exactly that —
  the server advertised no STARTTLS, so KyPost refused rather than sending your
  message and password in the clear. Fix the relay, or, for a plaintext relay on
  a network you trust, set `ALLOW_INSECURE_SMTP=true` and understand what you
  are giving up.
- Check `api.err.log` for `mail send failed` details.
- `smtp: acceptance uncertain` means the final DATA acknowledgment was lost or
  invalid. The relay may already have accepted the message; check its logs and
  the recipient before retrying to avoid duplicate mail. A pickup notification
  with this result keeps its encrypted message until it is opened or expires.

### KyPost does not apply labels

- Confirm that the labels exist in the allowlist and the tuning file.
- Confirm that the unread inbox holds eligible messages.
- Check the Decisions page and the poller logs.

### PWA installation on Firefox

- The install button lives on the **Get the apps** page (sidebar, under Settings), alongside links to the native clients.
- Firefox can omit the install prompt event that Chromium browsers emit.
- KyPost still provides a service worker and a manifest. The installation flow differs by browser.

## Project Structure

- `docker-compose.rspamd.yml`: optional pinned private scanner sidecar; fixed configuration in `scripts/rspamd/rspamd.conf`.
- `docker-compose.receiving.yml`: optional controlled receiver supervision and explicit SMTP publish with operator-supplied read-only engine/TLS mounts.

- `backend/internal/mailmsg/`: shared MIME/SMTP helpers and encrypted domain relay profile. Strict native implicit-TLS transport is internally qualified; native primary sending uses fresh admission and the durable outbox; provider readiness and pickup/system paths remain pending.

- `backend/internal/sso/`: signed directory lifecycle, admin mail-domain DNS proof, native reservations, opt-in prepare-before-publication account allocation and shared native outbound admission/recovery. See [provisioning contract](docs/NATIVE_PROVISIONING.md).

- `backend/internal/cfreceiving/`: KyPost side of the continuous Cloudflare wire contract: pickup credentials, signed routing tables and rotations, the bounded Worker client and local pickup state. See [continuous receiving](docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md#kypost-side).
- `backend/internal/ingress/`: durable receiving-buffer core and receipt bridge, selected by opt-in local receiving commands and daemon import; public receiver packaging remains gated. See [receiving qualification](docs/RECEIVING_GATEWAY_ASSESSMENT.md).
- `backend/internal/mailbox/`: internal permanent per-owner SQLite mail, metadata, receipt and change storage, with a complete internal mail Client and transactional incoming-encryption recovery. The internal importer commits these receipts before releasing receiving-buffer payloads. Source guards refuse switching or reusing references against another native database. Native API qualification uses fresh full snapshots; efficient scoped deltas remain pending; `KYPOST_NATIVE_MAIL=true` selects prepared local mailboxes. The internal directory reconciler retains primary-address/account reservations and preparation status; provisioning retries retained signed directory subjects when native mode is enabled. Internal new-account preparation atomically publishes an empty mailbox plus prebound state and refuses legacy/incomplete directories; matching preparations are validated on retry. See [implementation evidence](docs/TURNKEY_MAIL_PHASE1.md#durable-directory-desired-state-and-native-account-preparation). Encrypted outbox claims and independent Sent receipts back admitted primary compose/client-PGP sending and API/daemon recovery; pickup/alias/system paths remain pending.

- `backend/internal/backup/`: KyRecovery adapter, collection and drill checks for state, native mailbox and receiving SQLite snapshots.
- `docs/RESTORE.md`: operator backup and offline restore procedure.

- `backend/`: Go API, poller, adapters, config, state, health, and the on-device embedding sorter (`internal/sorter`)
- `frontend/`: React and Vite UI
- `scripts/`: container entrypoint, supervisord orchestration, Ollama model management, host-side update helpers and the controlled native-domain setup wizard
- `push-relay-shared/`: shared Cloudflare Worker logic for the push relays — API-key issuance, rate limiting, device-token ownership, and the `RelayCoordinator` Durable Object
- `worker/`, `worker-apns/`: the FCM and APNs deployments of that relay. Each holds only its provider's `handleSend` plus its wrangler config; everything else is imported from `push-relay-shared/`
- `receiving-worker/`: independent Email Workers with private R2 retention and authenticated pickup: the one-message pilot (`docs/CLOUDFLARE_RECEIVING.md`) and the continuous protocol Worker, whose KyPost side is `backend/internal/cfreceiving/` (`docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md`); built-in Node tests
- `docs/`: the contracts the client repos implement against — [PLATFORM_BASELINE.md](docs/PLATFORM_BASELINE.md) (what a client must implement to call itself a KyPost client), [E2E_PGP.md](docs/E2E_PGP.md), [WKD_Publishing.md](docs/WKD_Publishing.md), [WEBMAIL_HANDOFF.md](docs/WEBMAIL_HANDOFF.md), [INBOX_PAYLOAD_HANDOFF.md](docs/INBOX_PAYLOAD_HANDOFF.md), [DEVICE_ENROLLMENT.md](docs/DEVICE_ENROLLMENT.md) — plus the operator guides [Reverse_Proxy_Networking.md](docs/Reverse_Proxy_Networking.md) and [TLS.md](docs/TLS.md), and the README overflow: [FEATURES.md](docs/FEATURES.md), [CONFIGURATION.md](docs/CONFIGURATION.md), [API.md](docs/API.md)
- `share/`: host-side Ollama model blob cache, bind-mounted into the container. Never committed
- `tools/`: separate Go module pinning the CI tools (golangci-lint, govulncheck), run as `go tool -modfile=../tools/go.mod <tool>` from `backend/`; Dependabot bumps them
- `testdata/`, `fonts/`: test fixtures and the bundled webfonts
- `Dockerfile`: single image build (backend, frontend, Ollama runtime, pinned embedding model)
- `docker-compose.yml`: local orchestration
- `supervisord.conf`: in-container process supervision
- `AGENTS.md`: the contribution contract for automated agents. Every subtree with its own rules carries one

## Licence

KyPost is released under the [MIT License](LICENSE.txt).

[![OctoCounts](https://api.octocounts.com/badge/Busnes-app/KyPost-Server/branch/main)](https://octocounts.com/github/Busnes-app/KyPost-Server/tree/main)

## Upgrading after the Busnes-app owner move

The GitHub organisation was renamed on 2026-09-16 and the image now lives at `ghcr.io/busnes-app/kypost-server`. The project no longer controls `ghcr.io/busness-app`; GHCR does not redirect it, and anything served under that name must be treated as untrusted. If `KYPOST_IMAGE` in `.env` still names the old namespace, re-pinning is required, not optional: first inspect `git remote -v` and replace a retired-owner remote with `https://github.com/Busnes-app/KyPost-Server.git` (prefer a fresh clone plus a known commit). A first `scripts/update-host.sh` run on a container pulled from the retired namespace is expected to refuse because its rollback digest has the old repository prefix; follow [Moving a locally built install onto published images](#moving-a-locally-built-install-onto-published-images), but verify the newly selected digest as in [Verifying an image by hand](#verifying-an-image-by-hand) before `docker compose up -d`. Once the running image has the official digest, run [`scripts/update-host.sh`](scripts/update-host.sh), which resolves `stable` to a digest and verifies its GitHub build attestation before updating the service. See [Updating KyPost](#updating-kypost) for the procedure.
