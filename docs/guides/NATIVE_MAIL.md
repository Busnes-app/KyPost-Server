# Native mail: operator guide

Start-to-finish setup of KyPost-hosted mailboxes for your own domains. Every
command runs from the directory holding your `docker-compose.yml` and `.env`.
Deep references are linked per section; [CONFIGURATION.md](../CONFIGURATION.md)
is canonical for settings.

## 1. What native mail is, and choosing a receiving profile

With native mail, KyPost stores each user's mail itself (one private SQLite
mailbox per mailbox) instead of reading an external IMAP account:

- **People come from KyIdentity.** A signed directory push creates the person's
  KyPost account and primary mailbox at the primary address KyIdentity holds.
  KyPost administrators add extra mailboxes and aliases.
- **Sending** goes through an SMTP relay account you own (AWS SES, Cloudflare
  Email Sending or any implicit-TLS SMTP provider with AUTH). Busnes runs no
  relay service.
- **Receiving** uses exactly one profile per deployment:

| | A. Cloudflare Email Routing (primary, recommended) | B. Maddy on your own port 25 |
| --- | --- | --- |
| How mail arrives | Cloudflare accepts mail into your Worker and private R2 bucket; KyPost picks it up over outbound HTTPS | Senders connect to Maddy, which KyPost runs inside the container, on your public port 25 |
| Inbound port | None | Port 25 reachable from the internet |
| Privacy trade-off | Cloudflare and its account administrators can read ordinary mail in transit and at rest in the bucket, and the published routing table shows them every address | Mail stays on your host, but your server's public address is exposed as your MX |
| Spam scanning | Rspamd sidecar **required**; spam is filed to Junk (Cloudflare already accepted it) | Rspamd sidecar optional; with it, high scores are refused before acceptance |
| Automatic sender blocks | No (manual blocks only) | Yes, with Rspamd |
| Needs | Cloudflare account on Workers Paid, the domain's zone on Cloudflare | Linux x86_64, the pinned Maddy 0.9.5 binary, a TLS certificate |

External IMAP accounts remain a permanent, supported option for anyone who
wants neither; see [IMAP.md](./IMAP.md). Existing IMAP accounts are never
converted to native mailboxes.

**What is not production-ready yet**, as the code and docs state it:

- The Cloudflare continuous profile is implemented and tested offline only;
  nothing has been qualified live (for example, what a sender sees when the
  Worker fails). See [continuous receiving](../CLOUDFLARE_CONTINUOUS_RECEIVING.md#qualification).
- The Maddy profile is a controlled qualification profile for a dedicated test
  domain. Public production MX remains gated (tombstone pruning, capacity
  recovery, hard volume quotas, power-loss and packaging/licensing checks); see
  [receiver qualification](../RECEIVING_GATEWAY_ASSESSMENT.md#gates-before-production-selection).
- Restore-hold release is off by default pending qualification.
- There is no tool to reconcile or retry an outgoing message in the
  `uncertain` or `submitting` state.

Use a dedicated test domain first, keep your current MX and DNS recorded for
rollback, and change production MX only under your own approved plan.

## 2. Prerequisites

- KyPost running behind HTTPS with `SERVER_BASE_URL` set to its public
  `https://` origin. See [CLOUDFLARED.md](./CLOUDFLARED.md) or
  [reverse proxy networking](../Reverse_Proxy_Networking.md).
- A KyIdentity (KySignOn) instance you administer.
- A domain whose DNS you control. For profile A, its zone must be on Cloudflare.
- An outgoing relay account that is authorized to send for that domain.
- For profile B: a Linux x86_64 host, inbound TCP 25 reachable from the
  internet, and a certificate for the receiving hostname.
- A separate, local (non-SSO) KyPost administrator account. Keep it: after a
  restore only a local password administrator can release the restore hold.
- For real mail volumes: a restic repository for bulk backup (section 12).

## 3. KyIdentity setup

In KyIdentity:

1. Register an OIDC client for KyPost with redirect URI
   `<SERVER_BASE_URL>/api/auth/oidc/callback`.
2. Restrict it to assigned users, and use the app role `kypost.admin` for
   administrators.
3. Add KyPost as a webhook system: URL `<SERVER_BASE_URL>/api/sync/webhook`,
   sync secret either KyPost's pairing secret or the OIDC client secret. The
   pairing secret is `PAIRING_SECRET` if you set one, otherwise the value KyPost
   generated at first start. Read it privately on the host, never into chat or
   logs:

   ```sh
   docker compose exec --user kypost kypost-server cat /kypost/private/pairing.key
   ```

4. Give every mail user **exactly one** email marked primary, a plain ASCII
   address on a domain you will add to KyPost (no quoted or international
   addresses).
5. Keep administrators and mail users separate. A subject holding
   `kypost.admin` gets no mailbox; give the same person a second, everyday
   identity for mail.

In KyPost, open **Admin → Server → Single Sign-On (SSO)**: tick **Enable Single
Sign-On (SSO)**, set **Issuer URL** (HTTPS, no trailing slash; it cannot change
once a mail domain is bound to it), **OAuth Client ID** and **Client Secret**,
then **Save SSO Settings** and confirm with your password. The **Administrator
group** field is for other OIDC providers; KySignOn uses `kypost.admin`.

Native mailboxes are created only from the signed directory push, never from a
sign-in alone. Installers can apply the same SSO settings and domains with
`apply-setup`; see [installer setup](../INSTALLER_SETUP.md).

## 4. Enable native mail

1. In `.env`:

   ```dotenv
   KYPOST_NATIVE_MAIL=true
   # Optional; empty is 5 GiB per mailbox.
   KYPOST_MAILBOX_QUOTA_BYTES=
   ```

2. Recreate the container. Use the same `-f` files on every command once you
   add receiving overlays (sections 8 and 9):

   ```sh
   docker compose up -d
   ```

3. Check the configuration report:

   ```sh
   docker compose exec -T --user kypost kypost-server kypost-server setup-status
   ```

   It prints JSON `checks` (`public-origin`, `identity`, `webhook-key`,
   `native-mail`, `mail-domains`, `restore-authority`, `relay`, `receiving`,
   `bulk-backup`, `sealed-backup`, `backup-schedule`, `backup-receipt`,
   `backup-result`). Each failed check carries a message telling you what to
   do. `readyForExternalChecks: true` means local configuration is complete,
   not that mail flows; `externalChecks` lists what you must still observe
   yourself. The report refreshes no DNS and sends no mail.

## 5. Domain proof and DNS

### Prove the domain

1. **Admin → Server → Mail domain**, enter your password (or confirm with
   KySignOn) under **Confirm each action**.
2. **Add a domain** → **Add domain**. KyPost shows a TXT name and value.
3. Publish it at your DNS provider, then press **Verify**. The first challenge
   expires after 24 hours; **New challenge** issues another.
4. **Leave the TXT record published for as long as the domain is in use.**
   KyPost rechecks it before allocating, receiving and sending. If it
   disappears, that domain alone stops: mailbox creation is refused, Maddy
   answers senders with a temporary `451`, Cloudflare mail waits in R2 and
   outgoing mail waits for retry.

Repeat for each domain. **Replace…** rotates a challenge (verification clears
until DNS matches again); **Retire…** removes a domain from service but is
refused while any address on it is active, queued mail sends from it,
received mail is pending for it, or the relay still sends for it. Addresses
are kept, so **Re-add** restores them after fresh proof.

### DNS checklist

| Record | Name | Value | Profile | Set by |
| --- | --- | --- | --- | --- |
| TXT (ownership proof) | `_kypost-mail.<domain>` | `kypost-mail-verify=<challenge>` exactly as shown | Both | You, from Server → Mail domain; keep it |
| MX | `<domain>` | Cloudflare's Email Routing MX records | A | Cloudflare, when you enable Email Routing (locked) |
| SPF | `<domain>` | Cloudflare's SPF record (policy `~all`) | A | Cloudflare replaces the apex SPF when Email Routing is enabled. If your relay needs an include on the same name, merge it into that one record. Disabling Email Routing deletes it: restore your own |
| MX | `<domain>` | your receiving hostname (`KYPOST_RECEIVING_HOSTNAME`) | B | You |
| A / AAAA | receiving hostname | the public address whose TCP 25 reaches `KYPOST_SMTP_BIND:KYPOST_SMTP_PORT` | B | You; the certificate must name this host |
| SPF, DKIM, custom MAIL FROM | as your relay provider specifies | as your relay provider specifies | Both | You, from the relay provider's instructions. KyPost does not sign DKIM; the relay does |
| DMARC | `_dmarc.<domain>` | your policy | Both | You. KyPost neither publishes nor checks your own DMARC record |
| MTA-STS, TLS-RPT | — | — | — | Not provided by KyPost. Maddy already refuses senders that cannot use STARTTLS |
| PTR | — | — | — | Not needed: KyPost sends only through your relay, whose servers carry their own reverse DNS |

Leave the `_kypost-mail` TXT record untouched when Cloudflare or the relay
provider rewrites other records. Full domain rules: [native provisioning](../NATIVE_PROVISIONING.md#operator-domain-setup).

## 6. Mailboxes and addresses

- **Primary mailboxes** appear automatically after a KyIdentity user with a
  primary address on a verified domain is assigned to KyPost. Primary addresses
  cannot be released or reassigned in KyPost; change them in KyIdentity.
  Changing an existing user's primary address is refused.
- **Admin → Server → Mail addresses** lists every mailbox with its addresses'
  kind, state and generation (**Show mailboxes of** filters by user).
- **Add alias**: choose a mailbox and an address on a verified domain. Addresses
  are held permanently, never deleted.
- **Release** an alias: it becomes reserved, delivery and sending from it stop
  at once, delivered mail stays. **Reassign** a reserved alias to a chosen
  mailbox.
- **New mailbox**: choose a user and an address to give an everyday identity an
  extra mailbox. Extra mailboxes cannot be deleted, only **Disable**d (mail is
  kept; queued outgoing mail in it is quarantined for good) and **Enable**d.
  An extra mailbox and incoming encryption exclude each other for that user.
  A mailbox shown as "not in service" was interrupted mid-creation: repeat
  **Create mailbox** with the same user and address.
- **Quotas**: every mailbox gets `KYPOST_MAILBOX_QUOTA_BYTES` (default 5 GiB;
  256 MiB to 1 TiB) and at most 1,000,000 stored records. A change applies to all
  mailboxes at the next start. Lowering it below a mailbox's usage only refuses
  new mail to it. Separately, new mail is refused while the state drive has less
  than 10% or 5 GiB free, whichever is larger; senders retry. Server → Mail
  addresses warns when promised quotas exceed 80% of free space; size the volume
  for quota × mailboxes plus that reserve.
- Inbound messages are capped at 25 MiB in both profiles.

Details: [addresses and aliases](../NATIVE_PROVISIONING.md#addresses-aliases-and-generations),
[extra mailboxes](../NATIVE_PROVISIONING.md#extra-mailboxes),
[quotas](../NATIVE_PROVISIONING.md#mailbox-quotas).

## 7. Sending

1. At your provider, verify the sending domain and publish its DNS records.
2. **Admin → Server → Mail domain → 2. Configure outgoing delivery**: enter
   **Relay host**, **Relay port** (implicit TLS, usually 465; STARTTLS and
   plaintext are refused), **Relay username**, **Relay password**, then **Save
   outgoing relay**.
   - AWS SES: the region's SMTP host, port 465, SES SMTP credentials (not AWS
     access keys). SES replaces `Message-ID`, so replies do not thread against
     the Sent copy.
   - Cloudflare Email Sending: `smtp.mx.cloudflare.net`, port 465, username
     `api_token`, password an Email Sending: Edit token for an onboarded domain.
3. Tick the domains under **Relay sending domains** and **Save sending
   domains** (no password needed; a domain must be verified to be added).
4. **Check saved relay**. It tests TLS and login only; no mail is sent. Success
   proves nothing about delivery, SPF/DKIM/DMARC or inbox placement.
5. Sign in as a test user and send to a recipient you control, with and
   without PGP. Check the Sent copy, your provider's logs and the received
   headers.

Users may send from their mailbox's primary address or any active alias of it.
Every relay save replaces the whole profile and invalidates queued mail not yet
claimed under the old one; the screen asks you to confirm.

**Outgoing states.** Each message is a durable job: `queued` → `submitting` →
`accepted` (then filed to Sent). A temporary refusal becomes `retryable` (up to
six attempts, 30 s to 8 min apart); a permanent refusal `failed`; lost or
ambiguous provider answers `uncertain`; jobs whose sender, address, mailbox or
relay changed are `quarantined` and never sent.

**When a send looks stuck:**

- The user gets `ok:false` with an `outboxId`. The status is at
  `GET /api/mail/outbox/{id}` (the user's own session); nobody else can read it.
- The API/daemon log line `native relay attempt requires delivery evidence
  before resubmitting` carries the job's `correlation_id`, a `reason`
  (`smtp_400`–`smtp_599` or `transport_error`) and `result`:

  ```sh
  docker compose exec kypost-server grep 'native relay attempt' /kypost/logs/daemon.err.log /kypost/logs/api.err.log
  ```

- `submitting` (process killed mid-send) and `uncertain` jobs are never retried
  automatically and there is no retry button. Check the provider's logs; resend
  as a new message only if it was not delivered.

Details: [domain relay](../DOMAIN_RELAY.md), [native outbox](../NATIVE_OUTBOX.md#delivery-and-sent-states).

## 8. Receiving, profile A: Cloudflare

Requires Workers Paid (Free's CPU limit cannot handle large messages).

1. **Enable receiving and create the spool once**, after the domain is
   verified and at least one mailbox exists:

   ```sh
   docker compose exec --user kypost -e KYPOST_NATIVE_RECEIVING=true kypost-server \
     kypost-server receiving init
   ```

   Never run it again; it refuses an existing spool.

2. **Create this instance's Worker credentials:**

   ```sh
   docker compose exec --user kypost kypost-server kypost-server receiving cloudflare init
   ```

   It prints `PICKUP_TOKEN_SHA256` and `ROUTING_PUBLIC_KEY`. Neither is secret
   or able to read mail; the bearer and signing key stay in KyPost. It refuses
   if credentials already exist. Use a fresh pair for every Worker.

3. **Prepare the Worker** in a private directory outside the checkout, so its
   live configuration never sits in the repository. Use the repository's pinned
   Wrangler:

   ```sh
   npm ci --prefix worker
   W="$PWD/worker/node_modules/.bin/wrangler"
   install -d -m 700 ~/kypost-receiving-worker
   cp receiving-worker/continuous.mjs receiving-worker/wrangler.toml.example ~/kypost-receiving-worker/
   cd ~/kypost-receiving-worker
   cp wrangler.toml.example wrangler.toml
   ```

   Edit `wrangler.toml`: a dedicated `name`, `main = "continuous.mjs"`, your new
   private bucket as `bucket_name`. Keep `workers_dev = true` (KyPost accepts
   only `https://*.workers.dev` origins) and observability disabled.

4. **Create the bucket, deploy, install the two secrets** (Wrangler prompts for
   each value; paste the line printed by step 2):

   ```sh
   $W login
   $W r2 bucket create <dedicated-private-bucket>
   $W deploy
   $W secret put PICKUP_TOKEN_SHA256
   $W secret put ROUTING_PUBLIC_KEY
   ```

   Keep the bucket private, with no public access and no lifecycle (expiry)
   rules.

5. **Point KyPost at the Worker.** In `.env`:

   ```dotenv
   KYPOST_NATIVE_RECEIVING=true
   KYPOST_CLOUDFLARE_RECEIVING_ORIGIN=https://<worker>.<account>.workers.dev
   ```

   Start with the Rspamd overlay and **without** `docker-compose.receiving.yml`
   (that overlay switches on the Maddy receiver, and KyPost refuses to run both):

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.rspamd.yml up -d
   ```

   Use these two `-f` files for every later `up`; `docker compose exec` works
   without them.

6. **Check that KyPost published its routing table** (within about 30 seconds):

   ```sh
   docker compose exec --user kypost kypost-server kypost-server receiving cloudflare status
   ```

   Expect `"state":"running"` and a nonzero `lastRevision`. The same JSON is at
   `GET /api/admin/receiving/cloudflare`.

7. **Only now enable Email Routing** on the domain's zone in Cloudflare and point
   the **catch-all** rule at this Worker (no per-address rules). Before KyPost's
   first table, the Worker refuses all mail. Note the SPF change in section 5.

8. **Verify**: send mail to a user from an outside account, check it arrives,
   send to a non-existent address and confirm the sender gets a bounce. Watch
   `status`: `waiting` should return to 0; `oldestUnpickedWarning: true` means
   mail has waited in R2 over an hour (check the scanner, domain proof, disk
   space and `fenced` state). `refused` counts objects KyPost cannot hold; they
   stay in R2 for inspection.

Behaviour to know:

- Rspamd's reject verdicts are delivered to the mailbox's **Junk** folder,
  never dropped. Lesser scores are delivered normally.
- Mail survives KyPost downtime: the Worker keeps accepting against its last
  table for up to 14 days, and KyPost deletes each R2 copy only after storing it.
- `kypost-server receiving cloudflare rotate` replaces the bearer and signing
  key at any time; repeating it after a crash is safe.
- **Never clone** a receiving instance's volumes; two copies would both consume
  the queue. Restores are handled in section 12.

Details: [continuous Cloudflare profile](../RECEIVING_SETUP.md#continuous-cloudflare-profile),
[design and trust](../CLOUDFLARE_CONTINUOUS_RECEIVING.md).

## 9. Receiving, profile B: Maddy on port 25

Linux x86_64 only. KyPost does not download or distribute Maddy; you supply the
exact upstream 0.9.5 binary and KyPost refuses any other.

1. **Get and verify the binary** (outside the checkout, as yourself):

   ```sh
   d=$(mktemp -d)
   curl -fL https://github.com/foxcpp/maddy/releases/download/v0.9.5/maddy-0.9.5-x86_64-linux-musl.tar.zst -o "$d/maddy.tar.zst"
   echo "665cb3ffc43dcad0b904bb2792f31cd1025e484b0bacaf4ece06e2bd0091d5e2  $d/maddy.tar.zst" | sha256sum -c -
   tar --zstd -xf "$d/maddy.tar.zst" -C "$d"
   echo "6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db  $d/maddy-0.9.5-x86_64-linux-musl/maddy" | sha256sum -c -
   sudo install -d -m 755 /opt/kypost-receiving
   sudo install -m 755 "$d/maddy-0.9.5-x86_64-linux-musl/maddy" /opt/kypost-receiving/maddy
   ```

   The hash pins content; the upstream signature is not verified, and Maddy's
   GPLv3 obligations are yours to review. Keep the file and its parent
   directories unwritable by anyone untrusted.

2. **TLS certificate.** Obtain a certificate for the receiving hostname with
   your ACME client. Place it in a dedicated directory as `fullchain.pem` and
   `privkey.pem`; the key must be owned by UID 1000 (the container's `kypost`
   user) with mode `0600`, and the directory must be traversable. Paths must
   exist before you start (Compose will not create them).
   On renewal, copy the new files in atomically (write a temporary file, then
   `mv` it over), then restart the receiver and recheck with a TLS client:

   ```sh
   docker compose exec kypost-server supervisorctl -c /etc/supervisord.conf restart receiver
   ```

3. **Guided setup (optional).** `bash scripts/setup-mail.sh https://your-kypost-origin`
   walks the same steps for this profile: base rebuild in native mode, domain
   and relay checkpoints in the UI, the five receiver inputs, spool
   initialization and launch. It asks before every change, never collects
   credentials, edits no DNS and downloads nothing. It does not add Rspamd.
   Or continue by hand:

4. **Create the spool once**, after the domain is verified and mailboxes exist:

   ```sh
   docker compose exec --user kypost -e KYPOST_NATIVE_RECEIVING=true kypost-server \
     kypost-server receiving init
   ```

5. **Receiver inputs** in `.env`. The container listens on 2525 internally;
   publish host port 25 to it on a public address:

   ```dotenv
   KYPOST_MADDY_BINARY=/opt/kypost-receiving/maddy
   KYPOST_RECEIVING_TLS_DIR=/absolute/path/to/tls
   KYPOST_RECEIVING_HOSTNAME=mail.example.com
   KYPOST_SMTP_BIND=<public host address>
   KYPOST_SMTP_PORT=25
   ```

   `KYPOST_SMTP_BIND=127.0.0.1` with `KYPOST_SMTP_PORT=2525` is for a local test
   only. Open TCP 25 in your firewall. Put no TCP proxy in front of port 25 if
   you use Rspamd: SPF would see the proxy's address and automatic blocks would
   never trigger.

6. **Start.** Without spam scanning:

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.receiving.yml config -q
   docker compose -f docker-compose.yml -f docker-compose.receiving.yml up -d
   ```

   With the optional Rspamd sidecar (recommended):

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.receiving.yml -f docker-compose.rspamd.yml config -q
   docker compose -f docker-compose.yml -f docker-compose.receiving.yml -f docker-compose.rspamd.yml up -d
   ```

   From a source checkout instead of a published image, add `--build`. Use the
   same `-f` files for every later `up`; recreating with the base file alone
   switches the receiver off.

7. **Verify:**

   ```sh
   docker compose exec kypost-server supervisorctl -c /etc/supervisord.conf status receiver
   ```

   Expect `RUNNING`. Then, from an outside network, check STARTTLS and the
   certificate on port 25, send ordinary and PGP mail with an attachment, and
   confirm a non-existent address is refused. Run `setup-status` (section 4).

Behaviour to know:

- STARTTLS is mandatory (TLS 1.2+); senders without TLS are refused.
- Every start rechecks the binary hash and regenerates Maddy's configuration
  from current domains and certificates. After adding or retiring a domain,
  restart the receiver.
- With Rspamd, scores of 15 and above are refused with a permanent error
  before acceptance and can reject legitimate mail; scanner outages give
  senders a temporary `451`. Lesser scores are delivered unchanged.
- Maddy logs envelope and IP metadata to `/kypost/logs/receiver.log` and
  `receiver.err.log`; protect them.

Details: [receiver setup](../RECEIVING_SETUP.md), [Rspamd sidecar](../RECEIVING_SETUP.md#optional-rspamd-sidecar).

## 10. Quarantine and sender blocks

**Quarantine** holds received mail KyPost will not deliver on its own, for
example mail to an address that was reassigned or disabled after it was
accepted. **Admin → Server → Quarantine** lists envelope data only (never
bodies):

- **Release** delivers only to the mailbox the mail was originally addressed
  to, and is refused unless that mailbox still exists, is active and has the
  same owner.
- Mail marked "original owner unknown" (Cloudflare mail that waited through a
  restore) can only be **Release to current owner…** or discarded.
- **Discard** deletes it permanently.

The same from the command line (the ID is typed again after `--confirm`):

```sh
docker compose exec --user kypost kypost-server kypost-server receiving quarantine list
docker compose exec --user kypost kypost-server kypost-server receiving quarantine release <gateway> <id> --confirm <id>
docker compose exec --user kypost kypost-server kypost-server receiving quarantine discard <gateway> <id> --confirm <id>
```

**Sender blocks**: **Admin → Server → Sender blocks** → **Block sender…** blocks
an envelope sender address, or a domain (exactly that domain, not its
subdomains), optionally until a date; **Remove…** unblocks. Both profiles refuse
blocked senders before storing anything. Your own mail domains and the null
sender (bounces) cannot be blocked. With Maddy and Rspamd, KyPost also blocks
authenticated abusive senders automatically with an escalating cooldown (1 hour,
24 hours, 7 days); removing a block suppresses automatic re-blocking for 30
days. Blocks work only once the receiving spool exists (`receiving init`).

```sh
docker compose exec --user kypost kypost-server kypost-server receiving blocks list
docker compose exec --user kypost kypost-server kypost-server receiving blocks add address spammer@example.net --reason spam --confirm spammer@example.net
docker compose exec --user kypost kypost-server kypost-server receiving blocks remove address spammer@example.net --confirm spammer@example.net
```

Details:
[quarantine release](../NATIVE_PROVISIONING.md#quarantine-release),
[sender blocks](../NATIVE_PROVISIONING.md#sender-blocks).

## 11. What users see

- **Settings → Mail → Storage**: usage per mailbox against its quota, with
  warnings at 80% and 95%.
- **Settings → Security → Import Mail**: **From a file** (mbox, a single EML or
  a zip of EML files) or **From another mail account** (IMAP over TLS on 993 or
  STARTTLS on 143). Imported mail is stored as read and is not sorted, scanned
  or notified. Import is refused while the user has incoming encryption on.
- **Settings → Security → Export Mail**: a mailbox or one folder as one mbox
  file or a zip of `.eml` files. Exact stored bytes; PGP mail stays encrypted.
  Behind nginx, set `proxy_http_version 1.1;` or mbox downloads are refused.
- Import and export work only from a browser session for the user's own
  mailboxes, after re-confirming their password or KySignOn. Administrators
  cannot run them for a user.
- Users with several mailboxes see them in the sidebar; compose sends from the
  selected mailbox's addresses.

**Moving an existing IMAP user** in place is not supported. Create a new
native identity in KyIdentity, then the user imports their old account with
**From another mail account**. To leave native mail, the user exports.

Details: [mail import](../NATIVE_PROVISIONING.md#mail-import),
[import from another mail account](../NATIVE_PROVISIONING.md#import-from-another-mail-account),
[mail export](../NATIVE_PROVISIONING.md#mail-export).

## 12. Backups and restore

Configure sealed backups in **Admin → Server → Backup** (KyRecovery pairing or a
local directory). A capsule carries the domain set, mailbox ledger, encrypted
relay profile and its key, sender blocks, Cloudflare credentials and pickup
state, and, unless bulk backup is set, every `mailbox.db` and the receiving
spool `ingress.db`.

**Bulk backup is required for real mail.** Without it, each file is limited to
64 MiB and the whole capsule to 256 MiB, and backups fail once mail grows past
that. Set `KYPOST_BULK_BACKUP_REPOSITORY` so mailboxes and the spool go to a
restic repository:

- Recommended: an append-only rest-server, `rest:https://user:password@host/path`.
- Or a host directory mounted at `/kypost-bulk` (uncomment the mount in
  `docker-compose.yml`), owned by the container's `kypost` user, on another disk.

Nothing is pruned automatically; see [mail bulk backup](../RESTORE.md#mail-bulk-backup).
Keep your `.env`, compose overrides, TLS files and the Maddy binary backed up
separately; capsules do not contain them.

**Restore** is offline, into new volumes: [RESTORE.md](../RESTORE.md#offline-restore-and-native-quarantine).
A restored native server is **held**: native sign-in, mailboxes, sending and
receiving stay refused, devices must pair again, and queued outgoing mail is
quarantined and never resent. Start a restored server with receiving off (base
compose file only, `KYPOST_NATIVE_RECEIVING=false`, Cloudflare origin unset):
receiving refuses to run while the hold exists.

**Releasing the hold** (off by default):

1. Set `KYPOST_NATIVE_RESTORE_RELEASE=true` and recreate the container.
2. Sign in as a local, non-SSO administrator with a password.
3. Check `docker compose exec --user kypost kypost-server kypost-server restore status`
   (or `GET /api/admin/native-recovery/status`); its `nextSteps` lists what to do.
4. Within about five minutes, back to back: `POST /api/admin/native-recovery/challenge`,
   export the signed evidence from KyIdentity, `POST .../evidence`,
   `POST .../repair`, `POST .../release`. On the Maddy profile, stop the
   original host's receiver first and add `"confirm": "original-host-decommissioned"`.
5. Afterwards: run a KyIdentity resync, set `KYPOST_NATIVE_RESTORE_RELEASE=false`,
   re-enable your receiving profile and recreate the container. For Cloudflare,
   move receiving to this host:

   ```sh
   docker compose exec --user kypost kypost-server kypost-server receiving cloudflare takeover --confirm move-receiving-here
   ```

This guide gives no copy-paste `curl` for step 4: each request needs a browser
session cookie, CSRF protection and an `authSecret` derived in the browser from
your password, and the evidence comes from KyIdentity's protected exporter.
Follow [releasing the hold](../RESTORE.md#releasing-the-hold) and the
[protected procedure](../NATIVE_RESTORE_AUTHORITY.md#protected-consumer-procedure).
Never delete the hold file by hand.

## 13. Disabling and rollback

Nothing here deletes mail. Never delete volumes, the spool, ledgers, keys or
`*.v1-migrated` files to "reset" native mail.

| To turn off | Do this | Effect |
| --- | --- | --- |
| Cloudflare receiving | Unset `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN`, recreate | Mail waits in R2 (up to 14 days of table age); set the origin again to drain it. If you disable Email Routing, restore your own SPF record |
| Rspamd (Maddy profile) | Recreate with `-f docker-compose.yml -f docker-compose.receiving.yml`; set `KYPOST_RECEIVING_RSPAMD=false` if you set it in `.env` | Filtering and automatic blocks stop; manual blocks stay |
| Rspamd (Cloudflare profile) | Turn off Cloudflare receiving first | KyPost refuses the Cloudflare profile without Rspamd |
| Maddy receiver | Restore your previous MX, then recreate with `docker compose up -d` (base file only) | Receiver and SMTP publish stop; native state and accepted mail are kept. Accepted mail keeps importing only while `KYPOST_NATIVE_RECEIVING=true` is in `.env` |
| Native receiving | Also set `KYPOST_NATIVE_RECEIVING=false`, recreate | Pending received mail stays in the spool until re-enabled; drain or release quarantined mail first |
| Sending | Remove domains under Relay sending domains | Refused while queued or retryable mail sends from that domain |
| A domain | Remove it from the relay, then **Retire…** | Refused while any address on it is active or mail is pending; addresses are kept for **Re-add** |
| Native mail | `KYPOST_NATIVE_MAIL=false` (and receiving off), recreate | Native users lose access to their mailboxes; mail is kept but not converted to IMAP. Users should export first if they are leaving |

Rolling back to an older KyPost image after a storage migration or quota change
needs the backup taken before the upgrade; older images refuse the newer
formats. See [storage format migration](../RESTORE.md#storage-format-migration).

## 14. Troubleshooting

Logs: `docker compose logs kypost-server` (startup and `migrate-native`), and in
the container `/kypost/logs/api.err.log`, `daemon.err.log`, `receiver.log`,
`receiver.err.log`. See [LOGGING.md](../../LOGGING.md).

| Symptom | Cause | Fix |
| --- | --- | --- |
| Webhook answers `mailboxStatus: pending`; log `native mailbox provisioning pending` | The mailbox could not be created yet: not exactly one primary email, primary on a domain that is not configured or not verified, address already used by another mailbox or alias, or a DNS lookup failure | Fix the address in KyIdentity or the TXT record. KyPost retries at startup and every minute; a KyIdentity edit or resync sends a newer revision |
| A user's primary address change is not applied | Changing an existing user's primary address is refused | Keep the address, or create a new identity and have the user import |
| `administrator identities have no mailbox; use your everyday identity` | The subject holds `kypost.admin` | Use a separate everyday identity for mail |
| Native mail refused with `native mail storage is not in the current format (migration to native-domains.json pending, failed or incomplete)…`; IMAP still works | `migrate-native` failed at startup | Read its error in `docker compose logs kypost-server`, fix the cause, recreate. Or restore the pre-upgrade backup |
| `these mailboxes could not take the configured limits…` | A quota change could not be applied to the named mailboxes | Fix the reported cause and restart; other mailboxes work |
| Startup refused: `KYPOST_MAILBOX_QUOTA_BYTES must be a whole number of bytes from 268435456 (256 MiB) to 1099511627776 (1 TiB)` | Bad quota value | Correct or empty it |
| Startup refused: `KYPOST_NATIVE_MAIL must be true or false` (same for the other switches) | Any value other than `true`, `false` or empty | Use exactly `true` or `false` |
| Startup refused: `KYPOST_NATIVE_RECEIVING requires KYPOST_NATIVE_MAIL=true` | Receiving on, native mail off | Enable native mail or turn receiving off |
| Daemon refused: `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN requires KYPOST_NATIVE_RECEIVING=true` | Origin set without receiving | Set `KYPOST_NATIVE_RECEIVING=true` |
| Daemon refused: `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN and the bundled receiver are separate profiles; enable only one` | Cloudflare origin set and `docker-compose.receiving.yml` in use | Drop the receiving overlay (Cloudflare) or unset the origin (Maddy) |
| Daemon refused: `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN requires KYPOST_RECEIVING_RSPAMD=true and the qualified local sidecar` | Started without `docker-compose.rspamd.yml` | Add the Rspamd overlay |
| Daemon refused: `pickup requires a verified HTTPS workers.dev origin without credentials, ports, query or redirects` | Origin is not `https://<name>.workers.dev` | Use the Worker's `workers.dev` origin; custom domains are not accepted |
| `receiving requires explicit KYPOST_NATIVE_MAIL=true and KYPOST_NATIVE_RECEIVING=true` from a `receiving` command | Flags missing in that command's environment | Add `-e KYPOST_NATIVE_RECEIVING=true` (init) or set both in `.env` |
| `mail domain proof missing, expired or changed; configure and verify the current DNS challenge` on `receiving init` | No domain currently proves | Publish the TXT record and **Verify** |
| `receiving directory already exists; open or reconcile it instead of initializing` | The spool already exists | Do not initialize again; use the existing spool |
| `run as the owner of …` | Command run as root | Add `--user kypost` |
| `native mail is held after restore; …` | Restore hold present | Section 12; never delete the hold |
| Senders get `451 4.3.0 Receiving storage unavailable` (Maddy) | Domain proof lapsed (TXT removed, changed or DNS failing), spool or storage unavailable, or a restore hold | Restore the TXT record and **Recheck**; check `daemon.err.log` |
| `452 4.2.2 Mailbox full` / `452 4.3.1 Insufficient system storage` | Mailbox quota reached / drive reserve reached | Free space or raise the quota; senders retry |
| `451 4.3.0 Sender blocks unreadable` | Damaged sender block list | Restore it from backup; senders retry |
| Receiver `FATAL`, container restarting | Launcher refused; see `receiver.err.log` for `native receiver refused:` followed by `engine hash differs from the qualified Maddy 0.9.5 binary`, `mount the qualified executable read-only as a regular nonsymlink file`, `only the qualified Linux x86_64 engine is supported`, or `enable all three native receiver flags after domain setup and receiving init`; or a configuration error such as `receiving certificate must currently be valid for the explicit hostname` | Fix the binary, certificate, hostname or spool; a persistent failure stops the container on purpose |
| Cloudflare `status` shows `uninitialized` | No Worker credentials | `receiving cloudflare init` (section 8) |
| Cloudflare `status` shows `fenced` | This is a restored instance, or another instance took over | Leave it, or `receiving cloudflare takeover --confirm move-receiving-here` on the host that should receive |
| `oldestUnpickedWarning: true` | Mail has waited in R2 over an hour | Check Rspamd, domain proof, disk space, `fenced` and `error` detail |
| Cloudflare bounces mail to a real address | Address not active, domain not verified, or no table published yet | Check the address in Server → Mail addresses and `receiving cloudflare status` |
| `instance state unavailable; start the base server before setup-status` | Container not started yet | Start it first |

## 15. Environment variable reference

Canonical list and validation rules: [CONFIGURATION.md](../CONFIGURATION.md).
Switches accept exactly `true` or `false`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `SERVER_BASE_URL` | empty | Public HTTPS origin; required for SSO (OIDC callback) |
| `PAIRING_SECRET` | generated | Overrides the generated pairing secret (webhook sync secret); set only for several replicas |
| `KYPOST_NATIVE_MAIL` | `false` | Native mailboxes provisioned from KyIdentity |
| `KYPOST_MAILBOX_QUOTA_BYTES` | 5 GiB | Per-mailbox quota, 268435456 to 1099511627776 bytes |
| `KYPOST_NATIVE_RECEIVING` | `false` | Receiving commands and import; needs native mail |
| `KYPOST_RECEIVING_RSPAMD` | `false` | Local spam scanning; set by `docker-compose.rspamd.yml`; required by Cloudflare |
| `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN` | empty (off) | Profile A Worker origin, `https://<worker>.<account>.workers.dev` |
| `KYPOST_NATIVE_RECEIVER` | `false` (image) | Starts Maddy; set only by `docker-compose.receiving.yml` |
| `KYPOST_MADDY_BINARY` | none | Profile B: absolute host path of the pinned binary |
| `KYPOST_RECEIVING_TLS_DIR` | none | Profile B: host directory with `fullchain.pem` and `privkey.pem` |
| `KYPOST_RECEIVING_HOSTNAME` | none | Profile B: lowercase hostname on the certificate |
| `KYPOST_SMTP_BIND` | none | Profile B: host address SMTP is published on |
| `KYPOST_SMTP_PORT` | none | Profile B: host port (25 for real mail) |
| `KYPOST_RECEIVER_BINARY`, `KYPOST_RECEIVING_LISTEN`, `KYPOST_RECEIVING_CERT`, `KYPOST_RECEIVING_KEY` | container paths, `0.0.0.0:2525` | Launcher inputs for non-container installs; leave unset with Compose |
| `KYPOST_NATIVE_RESTORE_RELEASE` | `false` | Enables the restore-hold release route; on only for a deliberate release |
| `KYPOST_BULK_BACKUP_REPOSITORY` | empty | Restic repository for mailboxes and the spool; needed for real mail |
| `KYPOST_BACKUP_SCRATCH_DIR` | `STATE_DIR/backup-scratch` | Backup scratch (holds plaintext mail during a run) |
| `KYPOST_RESTIC_BINARY` | `restic` | Restic executable, 0.16.0 or newer |
