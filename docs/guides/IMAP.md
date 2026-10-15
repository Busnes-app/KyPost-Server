# KyPost with an existing IMAP/SMTP account

This guide takes you from a fresh checkout to a working KyPost that reads, labels and
sends mail through a mail account you already have. It is for operators and for the
users they create.

## 1. What this mode is

KyPost connects to your existing provider over IMAP to read mail and over SMTP to send
it. Your mail stays at the provider. KyPost adds a web client, automatic labels from a
local Ollama model, PGP, contacts, notifications and the mobile apps on top.

This is a permanent, supported mail source, not a legacy path.

Choose it when:

- you want to keep your current provider and address;
- you do not want to run mail reception, DNS proof or a relay yourself.

Choose [native mail](./NATIVE_MAIL.md) instead when you want KyPost to own the
mailboxes on your own domain. An existing IMAP account cannot be converted into a
native mailbox in place; see [Moving to native mail later](#9-moving-to-native-mail-later).

## 2. Install

You need Docker and Docker Compose.

1. Clone the repository and enter it.
2. Copy the defaults:

   ```bash
   cp .env.example .env
   ```

3. Set `KYPOST_BIND` in `.env`. It has no default; compose refuses to start without it.
   - `127.0.0.1`: a reverse proxy or tunnel on this same host.
   - Your LAN IP: a proxy on another machine.
   - `0.0.0.0`: every interface, deliberately.
4. Decide how TLS reaches the browser. KyPost serves plain HTTP unless you set
   `TLS_CERT_FILE` and `TLS_KEY_FILE` (both or neither).
   - A non-loopback `KYPOST_BIND` without TLS refuses to start unless you set
     `ALLOW_INSECURE_HTTP=true`. That is for a deliberately trusted network, not a TLS
     substitute.
   - The default login CAPTCHA (`CAPTCHA_PROVIDER=pow`) only works over HTTPS or on
     `localhost`. On a plain `http://` LAN address nobody can sign in.
5. Set `SERVER_BASE_URL` to your public `https://` URL if you will use mobile pairing,
   desktop pairing or single sign-on. Those refuse without it.
6. Create the model cache directory once:

   ```bash
   mkdir -p share/ollama/models
   ```

7. Build and start:

   ```bash
   docker compose up --build -d
   ```

   The first start pulls the Ollama model (`OLLAMA_MODEL`), which can take a while.
8. Read the generated admin password, then delete the file. It is written to the config
   volume (mode `600`), never to the logs:

   ```bash
   docker compose exec kypost-server cat /kypost/config/first-run-password.txt
   docker compose exec kypost-server rm /kypost/config/first-run-password.txt
   ```

9. Open the web UI (http://localhost:5866 on the host itself) and sign in as `admin`
   with that password.
10. Change the password when prompted. Until you do, the account can reach only the
    password-change screen.
11. Turn on MFA: **Settings → Security → Sign-in → Authenticator app**. Save the
    recovery codes it shows; they are not shown again.

### Exposing it

- Cloudflare Tunnel: [CLOUDFLARED.md](./CLOUDFLARED.md).
- Your own reverse proxy, `kypost-net`, `TRUSTED_PROXY_CIDRS` and
  `TRUSTED_PROXY_HEADER`: [Reverse proxy networking](../Reverse_Proxy_Networking.md).
- The full TLS discussion: [README Quick Start](../../README.md#quick-start).

Verify after exposing: sign in and open `GET /api/status`. `clientIp` must be your own
public address. Behind a proxy, `proxyHeadersTrusted` must be `true`.

## 3. Administration

Admin pages are under **Settings → Admin → Server** and **Settings → Admin → Diagnostics**.

### Users and roles

There are two roles, `admin` and `user`. Each user connects their own mailbox, owns
their own labels and tuning, pairs their own devices and sets their own notifications.

To create a user:

1. Open **Server → Manage Users**.
2. Enter your own password under **Confirm each change** (or confirm with KySignOn if
   you signed in that way).
3. Under **Create User**, fill in **Username**, **Temporary Password** and **Role**, then
   **Create User**.
4. Give the user their temporary password. They must change it at first sign-in.

Each user row also offers **Reset Password**, **Send Quota**, **Mailbox**, **Clear MFA**
(when enrolled) and **Deactivate**/**Reactivate**. Deactivation is a soft delete; data
stays on disk. KyPost will not deactivate or demote the last active admin.

### Single sign-on (optional)

**Server → Single Sign-On (SSO)** connects an OpenID Connect provider (KySignOn preset,
Authentik, Keycloak). It requires `SERVER_BASE_URL`. SSO only signs people in; each user
still connects their own mailbox.

### Default Mail Server

**Server → Default Mail Server** sets **IMAP Host**, **IMAP Port**, **SMTP Host** and
**SMTP Port**. These prefill **Email Settings** for users who have not connected a
mailbox yet. No credentials are stored here; users still enter their own username and
password. Leave it blank to prefill nothing.

### Setting up a user's mailbox for them

**Server → Manage Users → Mailbox** writes a user's IMAP/SMTP (and CardDAV client)
settings directly. Tick **Lock mail settings (user cannot change them)** to stop the user
editing them; they then see the host and username read-only with a "managed by your
administrator" notice. Send-As addresses and Mailbox Rules stay with the user.

### Other admin settings that matter for IMAP mode

- **Server → Application**: **Scan Interval (seconds)** (default 90), **Rate Limit Per
  Minute** / **Per Hour** for model calls (defaults 10 and 20), and the sending quota
  (60 messages an hour and 1000 recipients a day per user by default).
- **Server → Default Labels**: the label list new accounts start from (see section 5).
- **Diagnostics → System Health** and **Diagnostics → Logs**.

## 4. Connecting a mailbox

Each user does this once, under **Settings → Mail → Email Settings**.

| Field | What to enter |
|---|---|
| **Host** | Your provider's IMAP server. |
| **Port** | IMAP port. Default `993`. |
| **Username** | Your mail login, usually the full address. |
| **Password or App Password** | Your password, or an app password if the provider requires one. Required every time you save. |
| **Mailbox** | The folder KyPost labels. Default `INBOX`. |
| **SMTP Host (optional)** | Leave blank to use the IMAP host with `imap.` changed to `smtp.`. |
| **SMTP Port (optional)** | Default `587`. |

1. Fill in the fields and press **Save Email Settings**.
2. Press **Test Email Settings**. A pass reads `IMAP test passed (host:port mailbox).`

Know before you connect:

- **IMAP is implicit TLS only.** Use the provider's TLS port (normally 993). A plain or
  STARTTLS-only port such as 143 fails the TLS handshake. The server's certificate must
  verify; self-signed certificates are refused.
- **SMTP: port 465 uses implicit TLS; any other port requires STARTTLS.** KyPost refuses
  to send if the server does not offer STARTTLS. `ALLOW_INSECURE_SMTP=true` lifts this
  for the whole deployment, for a plaintext relay on a trusted LAN only.
- **SMTP uses the same username and password as IMAP.** There are no separate SMTP
  credentials.
- **The test checks IMAP only.** SMTP is first exercised when you send.
- **There is no OAuth sign-in.** Providers with two-step sign-in (for example Gmail and
  Fastmail) generally need an app password, created in the provider's account settings.
- **Mail servers on your LAN or tailnet are refused by default.** Add the server's
  hostname to `SANDBOX_PRIVATE_HOSTS`. A bare IP address cannot be allowed; give the
  server a hostname.
- **Delete Stored Email Settings** removes the saved credentials.

### Special folders

KyPost finds Trash, Junk, Archive, Sent and Drafts from the server's special-use
markings. If the server publishes none, it looks for the usual names (for example
`Trash`, `Deleted Items`, `Junk`, `Spam`, `Sent`, `Sent Items`, `Drafts`) and only
creates a folder when none exists.

### How credentials are stored

- Saved credentials are encrypted at rest with AES-GCM in
  `/kypost/config/users/<userID>/imap-config.json`.
- The key is `/kypost/private/imap-config.key`, generated on first use.
- The server holds that key so it can poll while nobody is signed in. Anyone with both
  the config and private volumes, or a backup of them, can read every stored password.
  Protect backups accordingly.

## 5. Labels

KyPost polls each user's configured mailbox for **unread** mail every scan interval,
classifies each message and adds the result as an **IMAP keyword**. It never moves,
deletes or marks mail read when labelling. Other IMAP clients that show keywords see the
same labels.

How a message gets its label:

1. Your **Mailbox Rules** run first.
2. Mail from a contact gets `Primary`; mailing-list and automated mail can never be
   `Primary`.
3. The on-device sorter labels mail it is confident about.
4. Anything else goes to the Ollama model, checked against your allowed labels.

Labels are a sorting hint a sender can influence, not a security boundary. Never build a
rule that trusts a label. See [Architecture](../../README.md#architecture).

### The model

- Set `OLLAMA_MODEL` in `.env`. Default `nemotron-3-nano:4b`; `gemma4:e4b` resists
  prompt injection better but needs about three times the memory. See
  [Choosing a classifier model](../../README.md#choosing-a-classifier-model).
- `CLASSIFIER_ENGINE=llm` turns off the on-device sorter. `EMBED_MIN_CONFIDENCE` sets
  how sure the sorter must be.
- There is no model picker in the UI. **Email Labels → Prompt Tuning** shows the
  installed Ollama version.

### Your labels

- **Settings → Email Labels → Your Labels**: the **Allowlist** (one label per line),
  **Keyword Mappings**, and optional **Label Descriptions**. **Merge IMAP Labels** adds
  keywords already on your server. Press **Save Labels**.
- Change a message's label in the reader (or in another IMAP client) and similar mail
  follows.
- The shipped list is `Primary`, `Promotions`, `Social`, `Updates`. Admins change the
  list new accounts start from under **Server → Default Labels**. That never changes
  existing accounts.

### Prompt tuning

- **Email Labels → Prompt Tuning** edits your own `TUNING.md`. Keep the
  `## Allowed Labels` list and the `[Insert Email Content Here]` placeholder; the loader
  needs both. The shipped default is the repository's [TUNING.md](../../TUNING.md).
- **Automatically apply keyword labels** off skips the model and tags every new message
  with your default label (`Primary` if configured, otherwise your first label).
- **Email Labels → Decisions** records what was applied and why.

## 6. Sending

- **Compose** sends through your SMTP server. **Save Draft** stores the draft in your
  IMAP Drafts folder.
- **Schedule…** sends later (in an hour, tomorrow morning, Monday morning or a chosen
  time). The **Scheduled** page lists waiting mail and cancels it back to Drafts. If
  account security, keys or the From address change first, the message returns to
  Drafts unsent with the reason.
- **Settings → Mail → Send-As Addresses** adds extra From addresses. Each is verified by
  a code mailed through your own SMTP server.
- Sending quotas apply to immediate and scheduled mail. Over the limit, sending fails
  with `sending quota reached; try again in about N minutes`.
- PGP: read [Where your PGP private key lives](../../README.md#where-your-pgp-private-key-lives)
  before relying on it; the design is in [E2E_PGP.md](../E2E_PGP.md). Subjects are not
  encrypted by ordinary PGP/MIME, so your provider still sees them.

## 7. Notifications and mobile apps

- **Settings → Notifications**: browser push for all mail or only for chosen IMAP
  keywords, plus delivery mode and notification content. Mobile notifications are
  generic by default.
- **Settings → Security → Devices** pairs the mobile apps by QR code. It needs
  `SERVER_BASE_URL`. **App Pull** delivery fetches notifications straight from your
  server, bypassing Cloudflare and Firebase.
- Details: [Mobile App Pairing](../../README.md#mobile-app-pairing-native) and
  [Push Relays](../../README.md#push-relays-cloudflare-workers).

## 8. Backups

Your mail lives at the provider and is not in KyPost's backups. What KyPost backs up is
accounts, encrypted credentials, keys, labels, tuning, decisions, devices and mailbox
checkpoints.

- **Sealed backups** (recommended): **Server → Backup**, to KyRecovery or a local
  directory. See [Sealed backup and restore](../RESTORE.md).
- **Manual volume backup**: stop the container and archive all four volumes in one pass.
  See [Backup and Restore](../../README.md#backup-and-restore).
- Keep `kypost_private` with `kypost_config`. Without `imap-config.key` the stored
  credentials cannot be decrypted, and users must re-enter their passwords.

## 9. Moving to native mail later

- Native mail is a separate, opt-in mode for new KyIdentity-provisioned accounts. See
  [NATIVE_MAIL.md](./NATIVE_MAIL.md).
- An existing IMAP-linked account is not converted or adopted. Enabling native mail
  leaves existing IMAP accounts working as they are.
- To bring old mail across, use **Settings → Security → Import Mail** in the new native
  account and import from another mail account. The IMAP password is used for that
  import only and never stored; only public servers over verified TLS are accepted. See
  [Import from another mail account](../NATIVE_PROVISIONING.md#import-from-another-mail-account).

## 10. Troubleshooting

Logs are in `/kypost/logs` (also **Diagnostics → Logs**): `api.err.log`,
`daemon.err.log`, `ollama.log`, `ollama-model.log`. Follow live output with
`docker compose logs -f kypost-server`.

| Symptom | Fix |
|---|---|
| `docker compose up` stops with `set KYPOST_BIND in .env` | Set `KYPOST_BIND` (section 2). |
| Container exits: `refusing non-loopback cleartext HTTP; configure TLS_CERT_FILE/TLS_KEY_FILE or set ALLOW_INSECURE_HTTP=true` | Add TLS, bind to `127.0.0.1` behind a proxy, or accept the risk with `ALLOW_INSECURE_HTTP=true`. |
| Login says the security check needs a secure (HTTPS) connection | Serve over HTTPS (proxy, tunnel or `TLS_CERT_FILE`). Plain HTTP works only on `localhost`. |
| Every user shares a lockout, or `clientIp` in `/api/status` is a gateway address | Fix the proxy path and `TRUSTED_PROXY_CIDRS`: [Reverse proxy networking](../Reverse_Proxy_Networking.md). |
| Save fails: `host, username, and password are required` | Re-enter the password; it is required on every save. |
| Save or test fails: `invalid mailbox name` | Use a real folder name such as `INBOX`. |
| Save fails: `mail settings are managed by your administrator` | An admin locked the settings; ask them to change them. |
| Test fails with a TLS or handshake error | Use the implicit-TLS port (normally 993) and a host whose certificate verifies. |
| Test fails with an authentication error | Check username and password; create an app password if the provider uses two-step sign-in. |
| `imap select folder "...": ...` | The **Mailbox** field names a folder the server does not have. |
| `destination resolves to a private or reserved address` | The mail server is on a private network. Add its hostname to `SANDBOX_PRIVATE_HOSTS`. |
| `missing IMAP credentials` in `daemon.err.log` | The user has not saved Email Settings yet. |
| `... is not encrypted (written before encryption-at-rest, or corrupt); re-save your IMAP/SMTP settings ...` | Re-save **Email Settings** once. |
| `decrypt imap config: ...` | `imap-config.key` does not match the stored file, often after a partial restore. Restore `kypost_private` from the same backup, or re-enter credentials. |
| Send fails: `smtp submission refused: server did not offer STARTTLS and ALLOW_INSECURE_SMTP is not set` | Use port 465 or a STARTTLS port. Only for a plaintext relay on a trusted LAN, set `ALLOW_INSECURE_SMTP=true`. |
| Send fails: `smtp host is not configured` | Set **SMTP Host**, or `SMTP_HOST` for the whole instance. |
| Send fails with an authentication error | SMTP uses the IMAP username and password. Check the SMTP host and port and that the provider accepts that login (or app password) for SMTP. Details are logged as `mail send failed` in `api.err.log`. |
| `smtp: acceptance uncertain; ...` | The relay may already have accepted the message. Check the recipient and the provider before retrying, to avoid duplicates. |
| `sending quota reached; try again in about N minutes` | Wait, or an admin raises the quota under **Server → Application** or **Manage Users → Send Quota**. |
| No labels appear | Check the labels exist in **Your Labels** and `TUNING.md`, that the inbox has unread mail, that **Automatically apply keyword labels** is on, and **Email Labels → Decisions**. |
| `imap set keyword "..." on uid N: ...` | The server refused the keyword. Some servers do not allow custom keywords on a folder. |
| Labels slow or missing after first start | Check `ollama-model.log` for the model pull, and that `OLLAMA_MODEL` fits in memory. `docker compose restart` if needed. |
| Pairing says `pairing is not configured on the server; set SERVER_BASE_URL` | Set `SERVER_BASE_URL` to your public `https://` URL and recreate the container. |
| SSO says `cannot determine this server's external URL; set SERVER_BASE_URL` | Same fix. |

## 11. Environment variables

[CONFIGURATION.md](../CONFIGURATION.md) is the canonical reference. The ones that matter
most in this mode:

| Variable | Purpose |
|---|---|
| `KYPOST_BIND` | Interface port 5866 is published on. Required. |
| `ALLOW_INSECURE_HTTP` | Allow non-loopback cleartext HTTP. |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | Serve HTTPS directly. Set both or neither. |
| `TRUSTED_PROXY_CIDRS`, `TRUSTED_PROXY_HEADER` | Which proxy's forwarded headers to believe, and which header names the client. |
| `SERVER_BASE_URL` | Public `https://` URL. Required for pairing and SSO. |
| `CAPTCHA_PROVIDER` | Login CAPTCHA; `pow` by default, needs HTTPS. |
| `SMTP_HOST`, `SMTP_PORT` | Instance-wide fallbacks when an account names no SMTP host or port. |
| `ALLOW_INSECURE_SMTP` | Allow SMTP without STARTTLS. Deployment-wide. |
| `SANDBOX_PRIVATE_HOSTS` | Hostnames allowed to resolve to private addresses (LAN mail servers). |
| `OLLAMA_MODEL`, `OLLAMA_BASE_URL` | Classifier model and Ollama endpoint. |
| `CLASSIFIER_ENGINE`, `EMBED_MIN_CONFIDENCE` | On-device sorter on/off and confidence. |
| `CLASSIFY_CONCURRENCY`, `CLASSIFY_PACE_MS` | Classifier admission control. |
| `TUNING_FILE` | Default tuning for new users. |
| `PUSH_RELAY_URL`, `APNS_RELAY_URL` | Native push relays for Android and iOS. |
| `KYPOST_NATIVE_MAIL` | Leave `false` for IMAP-only deployments. |
