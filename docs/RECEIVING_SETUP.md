# Controlled direct receiver setup

This is an opt-in Linux x86_64 qualification profile for a dedicated test
domain. It receives SMTP directly into KyPost's durable holding store, then
the daemon imports into native mailboxes without IMAP. Public production MX,
automatic receiver packaging and complete turnkey installation remain gated
by [receiver qualification](RECEIVING_GATEWAY_ASSESSMENT.md).

## Guided operator setup

On the actual Linux x86_64 Docker host, run this checkout's repeatable wizard
as the unprivileged Docker operator with Bash, Python 3 and Docker Compose:

```sh
bash scripts/setup-mail.sh https://your-kypost-origin.example
```

For a local test, a loopback HTTP origin is also accepted. Arrange the existing
TLS/proxy configuration first; the wizard asks for the HTTP publish address (literal IPv4 or bracketed IPv6 without a scope ID)
explicitly and retains all other `.env` settings. `ENV_FILE` optionally selects
an owned regular mode-`0600` dotenv file (default repository `.env`); missing files
are copied from the example after confirmation. Paths used by this wizard must
contain only letters, digits, slash, dot, underscore or hyphen. Use the manual
profile below for other protected paths.

The eight stages guide base deployment, KyIdentity/domain proof and a new test
user, operator relay, receiver/TLS inputs, backups/rollback, one-time spool
initialization, launch/admission and bidirectional tests. With explicit operator
confirmation it rebuilds the base service from this checkout in native mode,
pauses an existing receiver, saves public settings, optionally initializes a
**new** spool as `kypost`, and starts the receiving overlay. It never deletes
storage or repairs missing accepted mail. Reruns retain saved inputs and existing
state; skip initialization for an existing spool.

Persisted keys are `KYPOST_BIND`, `SERVER_BASE_URL`, both native flags and the five
receiver inputs listed below. Account/provider credentials are entered only in
the existing protected UI. The wizard downloads no engine, requests no provider
credentials, edits no DNS and applies no proxy/firewall rules. Keep operator
engine/TLS paths protected; the receiver has instance mail-storage authority.

Receiver status and fresh configuration admission are checked after launch.
The final stage gives external TLS/port/DNS, ordinary/PGP/Bcc, outbox/Sent/provider
receipt and repeat-backup checks; it does not claim those observations were made.
TLS/AUTH and process startup do not prove delivery. Native restore remains held
without a release path; full public deployment gates still apply. Stop at any
stage with Ctrl-C: validated settings and completed actions remain, so use the
rollback procedure below instead of deleting data.

## Prepare the existing stack

1. Use a new native deployment with `KYPOST_NATIVE_MAIL=true` before assigning
   people through KyIdentity. Pair KyIdentity, configure Server → Mail domain,
   publish the exact `_kypost-mail` TXT challenge and verify it. Keep that TXT
   record in place. Assign test users with explicit primary addresses in this
   domain; verify their native mailboxes were prepared. Existing IMAP accounts
   are never adopted automatically.
2. Provide the operator-owned outgoing relay through Server → Mail domain.
   The saved-relay check tests TLS/AUTH only. Use the
   [AWS/Cloudflare test matrix](TURNKEY_MAIL_STACK_PLAN.md#operator-owned-relay-test-matrix)
   to qualify delivery separately. No provider credentials belong in receiver
   configuration.
3. Run all commands below as the same unprivileged OS user and in the same
   filesystem/environment as KyPost, with absolute `CONFIG_DIR` and `STATE_DIR`.
   Both roots must already exist with owner-only permissions. The container image
   and entrypoint enforce `0700` on config/private/state roots, including mounted
   volumes; a host installation must arrange the same permissions itself. Initially leave
   the daemon receiving flag false. Run
   `KYPOST_NATIVE_RECEIVING=true kypost-server receiving init` explicitly once
   after domain setup, then configure `KYPOST_NATIVE_RECEIVING=true` for the
   daemon and receiver command environment and restart the daemon. Initialization refuses an existing spool;
   never repeat it to repair missing accepted mail. A restore hold refuses use
   and currently requires separate reconciliation work.
4. Supply the separately installed, pinned Maddy executable from
   [the assessment](RECEIVING_GATEWAY_ASSESSMENT.md#pinned-executable-and-runnable-check).
   KyPost does not download or distribute it. Keep executable and parent paths
   trusted and read-only to mail-processing processes. The measured hash pins
   content; it does not independently verify upstream provenance or satisfy
   distribution licensing obligations.
5. Supply readable regular PEM certificate/full-chain and private-key files.
   The key must have owner-only permissions. Paths must be absolute, without
   symlinks, controls, quotes, backslashes, braces or dollar signs. Copy renewed
   files into protected regular-file paths atomically. The explicit lowercase
   hostname must match a currently valid server certificate. Generation checks
   the pair, name, dates and server purpose; it does not establish public CA
   trust, complete-chain validity or successful renewal. Qualify these with a
   real client. A private test CA requires that client to trust it explicitly.

## Generate and run

Set `RECEIVING_LISTEN` to an explicit literal `IP:port`, `RECEIVING_HOSTNAME`
to the public/test hostname, `RECEIVING_CERT` and `RECEIVING_KEY` to the protected
TLS files, and `MADDY_BINARY` to the qualified binary path. These shell variables
are command inputs, not new KyPost environment settings. For an initial local
test use a loopback high port; no privileged user is needed.

```sh
set -eu
umask 077
printf '%s  %s\n' \
  6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db \
  "$MADDY_BINARY" | sha256sum -c -
receiver_config_tmp=$(mktemp "$CONFIG_DIR/receiving-config.XXXXXX")
if kypost-server receiving config "$RECEIVING_LISTEN" "$RECEIVING_HOSTNAME" \
    "$RECEIVING_CERT" "$RECEIVING_KEY" > "$receiver_config_tmp"; then
  mv "$receiver_config_tmp" "$CONFIG_DIR/receiving.conf"
else
  rm -f "$receiver_config_tmp"
  exit 1
fi
( ulimit -n 256; exec "$MADDY_BINARY" --config "$CONFIG_DIR/receiving.conf" run )
```

Generation performs existing-only storage admission, fresh TXT verification of
every configured domain, and a final domain-set/issuer/restore check before
emitting complete configuration. Every configured, non-retired domain is listed
as a Maddy `destination`; at least one must currently prove. A domain whose
proof has lapsed stays listed and its RCPT binds answer 451 until it proves
again, so one lapsed domain never blocks the others. The configuration is not
live: after adding or retiring a domain, regenerate it with the commands above
and restart the receiver (or the supervised container). No hot reload exists.
Validation failure emits no configuration; the temporary-file pattern preserves an older
configuration after failure. Successful generation is historical evidence,
not continuing authority or permission to change MX. RCPT and DATA continue
to verify current owners and fresh domain proof. Changing the challenge can
pause reception until the matching DNS value propagates.

The foreground process inherits the native file-descriptor ceiling, bounding
idle socket descriptors as well as active transactions. For container supervision,
use the optional profile below. A host foreground installation needs its own
service manager and protected log rotation before any remote test. Route a dedicated test
domain to it only after checking TLS, backups, firewall and SMTP port 25
reachability. HTTP reverse proxies do not carry SMTP. Preserve prior DNS and
keep the existing provider route available for rollback.

## Optional supervised container profile

The image leaves `KYPOST_NATIVE_RECEIVER=false`; base Compose publishes no
SMTP port. After steps 1–5 above, use `docker-compose.receiving.yml` to enable
all three native flags, mount the operator's engine/TLS directory read-only and
publish an explicitly chosen SMTP address and port. This profile is for the
controlled test domain; it does not download Maddy, initialize storage or
change DNS.

Prepare the base deployment first with `KYPOST_NATIVE_MAIL=true`. After domain
verification and account preparation, initialize its spool once as `kypost`.
Docker exec otherwise defaults to the image’s root user; root-owned spool files
would prevent the unprivileged receiver and importer from opening them:

```sh
docker compose exec --user kypost -e KYPOST_NATIVE_RECEIVING=true kypost-server \
  kypost-server receiving init
```

Set these Compose inputs in the operator's `.env`:

```dotenv
KYPOST_MADDY_BINARY=/absolute/protected/path/to/maddy
KYPOST_RECEIVING_TLS_DIR=/absolute/protected/path/to/tls
KYPOST_RECEIVING_HOSTNAME=mail.your-test-domain.example
KYPOST_SMTP_BIND=127.0.0.1
KYPOST_SMTP_PORT=2525
```

The TLS directory must contain regular `fullchain.pem` and `privkey.pem` files.
Keep the directory traversable and files readable by the container's `kypost`
user (UID 1000 in this image); the key must be owned by that UID with mode `0600`.
The engine must be executable and its host file and parent directories protected
against untrusted writes. Mount sources must already exist; Compose refuses to
create missing paths. Loopback port 2525 supports an initial local test only.
External SMTP requires an explicit reachable host address and port 25, firewall
qualification and the DNS precautions above.

```sh
docker compose -f docker-compose.yml -f docker-compose.receiving.yml config -q
docker compose -f docker-compose.yml -f docker-compose.receiving.yml up -d
docker compose exec kypost-server supervisorctl -c /etc/supervisord.conf status receiver
```

Before every start, the unprivileged launcher checks the exact qualified engine
hash and regenerates configuration through the production storage, TLS and
current-domain checks. It hashes and executes the same open file; trusted host
writes to that inode remain operator authority. Failed preflight preserves the
previous complete configuration and exits. Supervisor bounds startup retries,
restarts a failed receiver, rotates separate receiver logs and stops reception
before API/daemon drains. A persistent startup failure eventually exits the
container through the existing FATAL listener; it does not silently disable
receiving. Health alone does not prove delivery; inspect receiver status/logs.

After atomically replacing TLS files in the mounted directory, restart the
receiver explicitly to reload them and repeat a client TLS check:

```sh
docker compose exec kypost-server supervisorctl -c /etc/supervisord.conf restart receiver
```

The launcher also supports trusted host inputs `KYPOST_RECEIVER_BINARY`,
`KYPOST_RECEIVING_LISTEN`, `KYPOST_RECEIVING_CERT` and `KYPOST_RECEIVING_KEY`;
the container profile uses the fixed mount paths and internal `0.0.0.0:2525`.
Removing the overlay and recreating the base service disables the receiver and
SMTP publish while retaining native state. Drain or reconcile accepted mail;
never delete storage as rollback.

## Optional Rspamd sidecar

After preparing the receiving profile, use a KyPost image containing this
integration. The command below rebuilds this checkout before enabling the
overlay; an older image can ignore the new scanner setting. Rebuilding pauses
reception, so retain the existing spool and qualify the resulting receiver:

```sh
docker compose -f docker-compose.yml -f docker-compose.receiving.yml \
  -f docker-compose.rspamd.yml config -q
docker compose -f docker-compose.yml -f docker-compose.receiving.yml \
  -f docker-compose.rspamd.yml up -d --build
```

**Opting in can reject legitimate mail:** the fixed initial policy rejects score15
and above before SMTP acceptance. Lesser scores are delivered unchanged; there is
no automatic Junk move, header/subject rewrite or deletion. Scanner outages,
invalid/skipped acceptance verdicts, greylisting and temporary rejection return451
for the sender to retry. Existing committed/imported obligations are not rescanned.
The ordinary IMAP path and client API are unchanged.

The overlay enables strict `KYPOST_RECEIVING_RSPAMD=true` and pins official Rspamd
3.14.3 by digest. The nonroot scanner shares KyPost's network namespace, listens
only on loopback11333, publishes no ports and has no KyPost data/key mounts.
Controller/proxy are absent. It can nevertheless reach other KyPost loopback
services and sees raw mail, envelope and receiver-supplied connection metadata.
Memory/CPU/PIDs and ephemeral state are bounded; learning/Redis, remote
payload services, remote map updates and correspondence logging are disabled.
Local MIME/header checks and SPF/DKIM/DMARC DNS checks are enabled. DNS uses the
host resolver; encrypted PGP bodies cannot be scanned for their plaintext content.
No antivirus or sender-reputation blocklist service is configured.

The existing single DATA helper scans before durable commit, outside authority
locks, then rechecks current ownership/revision/restore fences. Peer IP is the
actual Maddy connection; HELO is only a sender assertion. The HTTP request has an
8-second deadline, no environment proxy/redirects and a256KiB reply cap. Provider
replies never become logs or SMTP error text. A rejected/deferred message can leave
its existing staged RCPT reservation; safe reservation reclamation remains gated.
Maddy still records envelope/IP metadata in its separately protected logs.

Keep all three Compose files when updating the service so the shared network
namespace is recreated consistently. To roll back, stop reception, remove the
Rspamd overlay, set `KYPOST_RECEIVING_RSPAMD=false` in the operator dotenv if it
was set manually, then recreate the receiving stack. This explicitly disables
filtering; retain every data volume and accepted obligation. Scanner health/ping
proves process reachability only, not spam effectiveness or delivery.

Run `python3 scripts/check-rspamd.py` for the actual pinned image's effective
module/socket/config, privacy and clean/GTUBE checks on disposable network-disabled
state. From `backend/`, run the actual receiving proof:

```sh
RSPAMD_PROOF=true MADDY_PROOF_BINARY=/absolute/path/to/pinned/maddy \
  GOTOOLCHAIN=go1.26.6 go test -race ./internal/app \
  -run '^TestNativeReceivingMaddyRuntime/rspamd$|^TestReceivingRspamdProtocol$|^TestNativeReceivingRspamdRetentionAndAuthority$' \
  -count=1 -timeout=3m
```

It uses a disposable loopback11333 scanner (refuses an occupied port), synthetic
mail and test DNS. It checks real SMTP clean/GTUBE/outage responses, preserved
MIME/body/attachment delivery and accepted replay during outage. Unit integration
also checks revocation during scanning and conflicting replay. This is not live
production tuning or physical-client qualification. The scanner helper can be
reused by the separate [one-message Cloudflare pilot](CLOUDFLARE_RECEIVING.md).
Its manual pickup uses content/DKIM scanning without original peer metadata;
provider bytes remain retained after refusal or import. Continuous hosted
reception and provider cleanup are not implemented.

## Fixed qualification policy and limits

- STARTTLS is mandatory before MAIL/RCPT; TLS versions 1.2 and 1.3 only. Senders
  unable to use TLS are refused. This is stricter than ordinary opportunistic
  SMTP and deliberately limits compatibility; there is no plaintext switch.
- Maximum message 4 MiB (including receiver-added headers), headers 128 KiB,
  recipients 100. RAM buffering avoids temporary payload spool files; up to
  four active messages can consume roughly 16 MiB plus parsing/process overhead.
- Four active transactions overall, two per IP; bursts of 30/minute overall
  and 10/minute per IP. Limit waits are bounded by the engine's five-second
  acquisition timeout. Read/write/shutdown timeouts are 30 seconds. Active
  transaction limits do not bound idle sockets without the OS descriptor cap.
- Only the verified mail domain is routed, with no SMTP AUTH, outbound queue
  or external relay target. The last RCPT check freezes ownership; the BODY
  check commits MIME before success. The dummy target runs only after that
  durable check. Removing/reordering these checks can discard accepted mail.
- Debug/wire logging stays disabled. Maddy logs envelope/IP/transaction and
  command metadata to stderr; protect and rotate those logs. Its command module
  discards helper stderr, so individual helper JSON diagnostics are unavailable
  through this bridge. Daemon import errors remain in KyPost's normal logs.
- Without the optional Rspamd profile, rate/size limits are not spam authentication or filtering. Sender-written
  authentication headers are untrusted. Hard volume quotas, safe reservation
  cleanup, provenance/licensing and intended-volume power-loss/restore checks
  remain public release gates. Never erase receipts or accepted mail to free space.

## Verify and roll back

Use the actual pinned-Maddy test from the assessment. It consumes generated
production configuration and proves STARTTLS, plaintext refusal without
bindings, external-recipient refusal, per-IP concurrent and burst refusal, durable
two-owner delivery and import after receiver SIGKILL/restart under closed storage admission. This is not a storage-process restart or power-loss proof. The launcher variant also proves failed preflight preserves old configuration.
With `RECEIVING_PROOF_IMAGE=<locally-built-image>` alongside `MADDY_PROOF_BINARY`,
the optional supervised variant uses the actual image Supervisor to restart a
killed receiver before import and verifies idle container shutdown. Its
`supervised_partial` case stops the container during a flushed, incomplete DATA
transfer: the receiver exits cleanly within its stop budget, no SMTP success or
partial payload is published, and the retained RCPT reservation remains staged.
Previously accepted mail survives another receiver start and imports normally.
This does not cover a stalled storage/helper process or every final-ack race. This fixture
uses host networking only for its loopback test DNS/SMTP and mounts only synthetic
state; it does not start the other image services. It uses test-only
loopback DNS and a private CA; it is not live-provider evidence.

The actual-Maddy fixture additionally starts the production HTTPS API against
the same synthetic native state after daemon import. Both envelope owners fetch
the received message and exact binary attachment, and cross-owner native message
references are refused. This uses pre-registered synthetic device credentials;
it does not test registration or a client UI. The HTTP client trusts only the
fixture certificate and keeps hostname verification. The API uses its production
TLS listener on an ephemeral port; run this optional proof on a dedicated test
host. The supervised modes still start only the receiver inside the image; the
API and importer run in the host test process, so this does not qualify a complete
container deployment. No operator domain, relay or credentials are used.

Local qualification (2026-10-05 UTC): direct/launcher race modes passed
28.478s; supervised/restart and incomplete-DATA shutdown modes passed 70.496s
with the provenance-verified main `2327a91` image
`ghcr.io/busnes-app/kypost-server@sha256:88a4dcc0a56a707690b3f004e283b2d2df9f217a2a13357753d8e8bec1516d9c`.
Independent direct/launcher review rerun passed 28.203s. These are synthetic
local results, not dedicated-domain delivery observations.

For the dedicated domain, send ordinary and PGP mail from a controlled external
account, inspect both KyPost mailboxes and raw attachments, and reply through
the configured operator relay. Check actual recipient receipt independently of
relay acceptance. Stop/restart receiving and importing without deleting storage;
accepted mail must survive. Confirm disabled recipients and DNS/storage failure
refuse new reception. Record the domain, relay, recipient and observed results
without credentials.

Before the live test, run this from `backend/`:

```sh
GOTOOLCHAIN=go1.26.6 go test -race ./internal/api \
  -run '^TestNativeOutboundAPIActualTLSAndPGP$' -count=1 -timeout=2m
```

It also checks one allocated native owner's real pairing/register, private durable
holding import, device body/attachment/keyword reads and ordinary/client-PGP
sends over an actual loopback TLS relay without IMAP. This joined core/API proof
uses test DNS and simulated device HTTP, with app/Maddy admission qualified
separately. It does not replace the external observations above.

Rollback: stop new reception first, restore prior test-domain DNS, retain a
compatible KyPost binary and all native state, and drain or reconcile pending
and quarantined deliveries (`docker compose exec --user kypost kypost-server kypost-server receiving quarantine list`, then
release or discard each; see
[quarantine release](NATIVE_PROVISIONING.md#quarantine-release)). DNS rollback changes future routing only. Disabling
the receiver or native flags does not migrate native mail to an IMAP service.
