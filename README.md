<img src="./ky50p.png" alt="KyPost" />

# KyPost

KyPost is a self-hosted IMAP web client. It applies keyword labels to your mail automatically with a local Ollama model.

KyPost polls unread mail, classifies each message, and applies IMAP keywords. It also gives you a web UI to read mail, change the configuration, manage notifications, view logs, and compose mail. Compose supports send and draft save.

## Features

- IMAP web client: inbox reader, folders, compose with SMTP send, scheduled send and draft save, filter rules (GUI and Sieve).
- Automatic keyword labels from an on-device embedding sorter backed by a local Ollama model; each account keeps its own labels. Labels are a hint, not a security boundary — see [Architecture](#architecture).
- PGP encryption and signing, recipient key discovery, Web Key Directory publishing and opt-in incoming encryption. Read [Where your PGP private key lives](#where-your-pgp-private-key-lives) before relying on it.
- Contacts with CardDAV server and client, groups, and CSV/vCard import and export.
- Multi-user with admin and user roles; TOTP, recovery codes and push-approval MFA; OpenID Connect single sign-on; self-hosted proof-of-work login CAPTCHA by default.
- Browser push, and native push for the mobile apps through the [push relays](#push-relays-cloudflare-workers).
- Opt-in native mailboxes: KyIdentity provisioning across several domains, quotas, receiving through Cloudflare Email Routing or an operator-supplied receiver (both still in qualification), operator-owned relay sending, sender blocks, quarantine, and mail import and export.
- Sealed configuration and state backups to KyRecovery or a local directory, with restic for mail databases.
- Single Docker container managed by supervisord, with shared JSON logs.

The full list, with the trade-offs each feature carries, is in [docs/FEATURES.md](docs/FEATURES.md).

## Requirements

- Docker
- Docker Compose

Optional for local development (outside Docker):

- Go 1.26+
- Node.js 20+
- npm

## Quick Start

1. Clone the repository.
2. Copy the environment defaults and set `KYPOST_BIND`.

   ```bash
   cp .env.example .env
   ```

   `.env.example` ships with `KYPOST_BIND=127.0.0.1`, which is right when your
   reverse proxy runs on this same host. It is **required** — compose refuses to
   start without it — because the alternative is a silent default, and this port
   serves plain HTTP unless `TLS_CERT_FILE` is set. See step 4.

3. Build and start the container.

   ```bash
   docker compose up --build -d
   ```

4. Open the web UI at http://localhost:5866.

   > **Before exposing this to a network, get TLS in front of it.** By default
   > KyPost serves plain HTTP. The session cookie is marked `Secure` only when the
   > request demonstrably arrived over TLS, so on a bare `http://` deployment the
   > cookie is sent in the clear on every request. `http://` on localhost, for one
   > machine, is fine. Compose refuses a non-loopback cleartext bind unless you
   > explicitly set `ALLOW_INSECURE_HTTP=true`; that escape hatch is for a
   > deliberately trusted network, not a TLS substitute.
   >
   > `KYPOST_BIND` decides which interface port 5866 is published on, and it has
   > no default — compose will not start until you set it. An unproxied 5866 is
   > plain HTTP, and with `TRUSTED_PROXY_CIDRS` set, anything that reaches it
   > directly can forge `X-Forwarded-For` and bypass the lockouts. Use
   > `127.0.0.1` for a proxy on this host, your LAN IP for a proxy elsewhere, or
   > `0.0.0.0` to publish everywhere deliberately. **Check where your proxy
   > actually reaches this container from before choosing**: loopback publishing
   > severs a proxy that arrives by the host's LAN address, which is the usual
   > shape for cloudflared or an nginx on another machine.
   > Better still, run the proxy as a container on `kypost-net` — the network the
   > compose file defines for exactly this — and point it at
   > `http://KyPost-Server:5866`. That network has DNS, so the name keeps working
   > across rebuilds, and the path ignores published ports entirely so nothing
   > needs publishing. The snippet for joining it from another compose project,
   > and how to recover from the two Docker errors this setup produces, are in
   > [docs/Reverse_Proxy_Networking.md](docs/Reverse_Proxy_Networking.md).
   >
   > **This is what makes the client IP correct, not a nicety.** A proxy on a
   > *separate* Docker network — or one that reaches this container through the
   > published port, even from `kypost-net` — is source-NATed on the way in, so the
   > server sees the same `172.x.0.1` gateway for every caller. That address is the
   > lockout key, so every user shares one bucket, and the MFA sign-in push names
   > the gateway instead of whoever is signing in.
   >
   > Three ways to get TLS, and they are not equivalent:
   >
   > **1. Terminate TLS in KyPost.** Set `TLS_CERT_FILE` and `TLS_KEY_FILE` to
   > mounted certificate paths (see `.env.example` and the commented volume in
   > `docker-compose.yml`). This is the only option where "did this arrive over
   > TLS?" is answered by the connection itself rather than by a header, so
   > `TRUSTED_PROXY_CIDRS` does not apply at all. Renewals are picked up without a
   > restart, which matters because a restart logs everyone out. Setting only one
   > of the two paths is a startup error, not a fallback to cleartext.
   > Certificates are deliberately never baked into the image.
   >
   > **2. Cloudflare Tunnel.** cloudflared gives the browser a real HTTPS origin,
   > which is all the login proof-of-work needs, with no TLS configuration of your
   > own.
   >
   > **3. A reverse proxy you run** (nginx, Caddy).
   > nginx speaks HTTP/1.0 to the upstream by default; set
   > `proxy_http_version 1.1;`. Mail export refuses mbox over HTTP/1.0, where a
   > cut-off download is indistinguishable from a complete one.
   >
   > Options 2 and 3 need `TRUSTED_PROXY_CIDRS` set to the proxy's address — e.g.
   > `127.0.0.1/32` for a proxy on the same host, or the address you pinned for a
   > proxy container on `kypost-net` (e.g. `10.89.0.10/32`). Putting the proxy on
   > that network is what makes such an address meaningful, but it is not a
   > substitute for setting this: with it empty, forwarded headers are discarded
   > and every caller is keyed as the proxy. Only with it set does the server believe
   > `X-Forwarded-Proto`/`-Host`/`-For`, which is what marks the cookie `Secure`
   > and keys the login and CardDAV lockouts off the real caller rather than the
   > proxy. **Name the proxy's address specifically, not a wide range:** any peer
   > inside the range you name can forge its own client IP and bypass every rate
   > limit and lockout keyed on it. This replaces the old
   > `TRUST_PROXY_HEADERS=true`, which is no longer read — it trusted forwarded
   > headers on *every* connection from any peer, so it was only ever safe when
   > nothing but the proxy could reach the port.
   >
   > Behind Cloudflare specifically, the client address is read from
   > `CF-Connecting-IP` in preference to `X-Forwarded-For`: the edge appends the
   > visitor IP to XFF, but cloudflared can append its own hop after it, which
   > would make every visitor look like `127.0.0.1` and collapse the per-IP lockout
   > into one shared bucket.
   >
   > If the proxy runs on a **different host**, combine option 1 with 2 or 3 — that
   > hop carries session cookies across a real network. A self-signed certificate
   > is enough there; tell the proxy to skip verification (cloudflared
   > `noTLSVerify: true`, nginx `proxy_ssl_verify off`).
   >
   > **Verify rather than assume.** Sign in and fetch `GET /api/status`: `clientIp`
   > must be your own public address and `proxyHeadersTrusted` must be `true`
   > (option 1 reports `false`, correctly — it trusts no headers). If `clientIp` is
   > a loopback or bridge address, every user is sharing one lockout key and the
   > session cookie is not being marked `Secure`.
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
6. Change the password when KyPost prompts you. Until you change it, the
   account can reach only the password-change screen.
7. In Config, save the IMAP and SMTP settings. Then run IMAP Test.
8. In Tuning, change the labels and the prompt. Then save.

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

Point 1 was not true before. Earlier versions POSTed the plaintext password to
`/api/auth/login` on every sign-in, so the server was handed the wrapping key's
source material repeatedly and merely chose not to keep it — a few lines in the
login handler would have opened every client-protected key on the instance. That
made the claim in this section, and in the code, false. Existing accounts convert
automatically on their next sign-in; nothing needs to be re-imported.

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
  Label rules are a Server tab, since the allowlist is instance-wide.
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

## Mobile App Pairing (Native)

The backend handles mobile pairing directly. It does not require Novu.

- Nothing to configure: the pairing secret is generated on first start and kept at `/kypost/private/pairing.key`. Set `PAIRING_SECRET` only if you run multiple replicas that must share one.
- Required: set `SERVER_BASE_URL` so that QR code payloads point to the correct public backend URL. Use an `https://` URL: pairing tokens, pickup links and QR key-exchange URLs are all built from it, and each carries a bearer credential in the query string. Unset, the pairing panel reports "set SERVER_BASE_URL" and mints no token — the address a credential is sent to is not something a request's `Host` header may choose, and there is no safe guess. It is also what the pairing QR's certificate pin is read from. See the TLS note in Quick Start and [Certificate pinning](#certificate-pinning-in-the-pairing-qr) below.
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
4. **Watch one poll tick** in Configuration > Application (or
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
docker exec -it kypost-server ps aux
docker exec -it kypost-server ls -la /kypost/config /kypost/state
docker volume ls | grep kypost
```

Persistence behavior:

- `docker compose up --build` keeps the named volumes.
- `docker compose down -v` removes the named volumes and the stored app data.

## Updating KyPost

KyPost checks GitHub releases hourly. When a newer KyPost release is found, it
emails the primary admin once and shows the update in Configuration >
Application. The container reports availability but never controls Docker on
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
release instead, set `KYPOST_VERSION=0.3.0` in `.env` before running it.

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

- Verify the host, port, username, password, and mailbox in Config.
- Run IMAP Test in Config.
- Check `daemon.err.log` and `api.err.log` for authentication, TLS, and keyword failures.

### SMTP send issues

- Verify the SMTP host and port in Config.
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
- `docs/`: the contracts the client repos implement against — [PLATFORM_BASELINE.md](docs/PLATFORM_BASELINE.md) (what a client must implement to call itself a KyPost client), [E2E_PGP.md](docs/E2E_PGP.md), [WKD_Publishing.md](docs/WKD_Publishing.md), [WEBMAIL_HANDOFF.md](docs/WEBMAIL_HANDOFF.md), [INBOX_PAYLOAD_HANDOFF.md](docs/INBOX_PAYLOAD_HANDOFF.md), [DEVICE_ENROLLMENT.md](docs/DEVICE_ENROLLMENT.md) — plus the operator guide [Reverse_Proxy_Networking.md](docs/Reverse_Proxy_Networking.md) and the README overflow: [FEATURES.md](docs/FEATURES.md), [CONFIGURATION.md](docs/CONFIGURATION.md), [API.md](docs/API.md)
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

The GitHub organisation was renamed on 2026-09-16 and the image now lives at `ghcr.io/busnes-app/kypost-server`. The project no longer controls `ghcr.io/busness-app`; GHCR does not redirect it, and anything served under that name must be treated as untrusted. If `KYPOST_IMAGE` in `.env` still names the old namespace, re-pinning is required, not optional: first inspect `git remote -v` and replace a retired-owner remote with `https://github.com/Busnes-app/KyPost-Server.git` (prefer a fresh clone plus a known commit). A first `scripts/update-host.sh` run on a container pulled from the retired namespace is expected to refuse because its rollback digest has the old repository prefix; follow [Moving a locally built install onto published images](#moving-a-locally-built-install-onto-published-images), but verify the newly selected digest with `gh attestation verify` before `docker compose up -d`. Once the running image has the official digest, run [`scripts/update-host.sh`](scripts/update-host.sh), which resolves `stable` to a digest and verifies its GitHub build attestation before updating the service. See [Updating KyPost](#updating-kypost) for the procedure.
