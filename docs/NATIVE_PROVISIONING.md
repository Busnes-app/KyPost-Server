# Native domain proof and account allocation

Production directory webhooks retain verified desired state and enforce account
access. Admin mail-domain setup is available under Server → Mail domain and through the API. Native allocation
and reconciliation are enabled only with `KYPOST_NATIVE_MAIL=true`. The default
remains external IMAP. This opt-in selects prepared native mailboxes in both API
and daemon. Direct receiving additionally requires `KYPOST_NATIVE_RECEIVING=true`;
primary-address sending uses the operator-owned [domain relay](DOMAIN_RELAY.md).

## Operator domain setup

Pair/configure KyIdentity first. As an admin, read `GET /api/admin/mail-domain`,
then `PUT /api/admin/mail-domain` with `{domain,password}` or
`{domain,authSecret}`. Mutations require session CSRF and the current account
credential; KySignOn sessions use the existing request-bound step-up round trip.
The response includes the exact `recordName` and `recordValue` to publish:
`_kypost-mail.<domain>` TXT `kypost-mail-verify=<random challenge>`.
Then `POST /api/admin/mail-domain/verify` with the same credential fields.
Responses are `Cache-Control: no-store` and always `receivingEnabled:false`.
The Server → Mail domain screen presents these same protected operations, with
exact TXT fields and explicit confirmation before replacing a challenge. It
does not install a public receiving gateway or change MX.

The first profile binds one lowercase ASCII DNS domain and the configured issuer.
Changing either is refused. Configure the issuer without a trailing slash before
claiming a mail domain, matching the provider verifier's effective spelling. Reconfiguring the same pair rotates the challenge,
expires initial verification after 24 hours and clears prior establishment/verification.
An exact TXT match establishes the profile. Thereafter retain the same record
and automatically recheck it; operators need no daily DNS rotation. The
`established` field only remembers that initial setup completed; it never grants
authority without a fresh exact DNS match. A match gives at most five minutes
of evidence; the first match is also capped by initial challenge expiry. Every allocation performs a fresh lookup; stored positive status alone
never authorizes it. Lookup timeout is five seconds; contention has a 30-second
context. DNS failure, expiry and in-flight challenge rotation fail closed.
Queries use an absolute DNS name; resolver search suffixes cannot stand in for
the configured domain. Domains whose challenge name exceeds DNS name limits
are refused at configuration. System DNS does not prove DNSSEC authenticity or instantaneous revocation;
resolver caching remains a trust dependency. WKD challenges have a separate
purpose and never authorize mail-domain ownership.

Unauthenticated requests return 401, non-admin/CSRF refusal 403, malformed or
oversized bodies 400, missing KyIdentity setup/unreadable GET state 503, and
refused configuration/verification 409. KySignOn step-up returns the existing
403 challenge response. A failed proof does not enable transport: correct the
exact TXT record/DNS availability or rotate an expired challenge and retry.
The owner-only `$CONFIG_DIR/native-domain.json` holds a public challenge, not a
secret. Do not publish production MX or assume outgoing delivery readiness.

## Account allocation

`LifecycleStore.AllocateNativeAccount` is an internal new-account publication
flow. It proves the domain before acquiring locks, then holds locks in order:
domain → directory → users → account. It requires a live retained signed SCIM
resource with matching issuer/subject and active state. Roles come from that
resource, never a token email/name or classifier label. Exactly one explicit
primary bare ASCII dot-atom email in the proven domain is required; comparisons
are case-insensitive. Quoted/SMTPUTF8 addresses and implicit aliases are refused.

Reuse an existing durable reservation's local ID or reserve a new UUID. Resolve
username collisions under the users lock with a stable `native-<localID>` name.
Prepare the mailbox, prebound state and manifest and durably acknowledge the
source before exposing the user in `users.json`. An ordinary lazy state opener
therefore cannot win first and create IMAP state. Existing linked IMAP accounts
are refused before preparation; no migration or adoption is attempted.

Private `nativeMailboxIssuer` and `nativeMailboxSource` fields bind the user
record to prepared ownership. Retries require the same issuer/subject/local ID
and exact source, without changing roles, activity, credentials or revocation.
Issuer-aware login and signed-directory lookup refuse foreign issuers. Directory
handling uses the captured verified issuer, never a later settings read. The
existing issuerless link method refuses all native relinks; unlink retains
ownership and revokes the credential. Supported native reauthorization is an
activation gate. Legacy link/unlink behavior is preserved, and the shared users
writer now refuses duplicate nonempty SSO subjects across creation and linking.

Allocation context is at most 30 seconds and never outlives the domain proof.
Cancellable flock/mutex waits leave no abandoned waiter that acquires later.
Legacy writers preserve blocking flock behavior. Open/fsync and read-only SQLite
validation are not cancellable: healthy-volume qualification/watchdog and prompt
revocation under stalled storage remain activation gates. No network lookup runs
under directory/users locks. Instance-wide preparation serializes directory
updates; whole-file reservation rewrites/address scans require scale measurement.

## Reservations, failure and restore

`$CONFIG_DIR/native-provisioning.json` retains immutable issuer/subject/local ID,
primary address, absolute state root and limits; revision/digest/activity;
pending/applied/failed status, failure code and acknowledged source. Reserve
pending before touching files. Keep reservations through failure/offboarding.
Preparation uses no-replace publication; lost acknowledgement reuses the exact
published namespace. Acknowledged sources validate existing files read-only,
refusing missing, legacy or damaged storage rather than recreating it.
Conflicting primary updates retain current failed status and the old address.
Offboarding revokes access without deleting mailbox data or freeing an address.
Status is historical preparation evidence, never a live access/receiver grant.

The lifecycle initialization fence refuses a missing ledger or a ledger paired
with an unfenced restored lifecycle. A kill between first fence and ledger write
requires explicit recovery, sacrificing availability to preserve reservations.
Backup exports/drills and offline restore validate historical domain claim,
users/issuer/source, directory lifecycle, reservations, mailbox/state and receiver
bindings together. Restored roots are authoritative for file lookup, never saved
absolute paths. Consistent older paired files cannot prove current authority.
Offline native restores persist `state/native-restore-hold.json`; allocation
refuses any present/unreadable hold and there is no release path. Follow
[restore gates](RESTORE.md#offline-restore-and-native-quarantine), including fresh
access repair and stale-message-ID fencing, before runtime activation.

Rollback preserves all these files and mailbox bytes. Disable workers before
changing binaries; older writers can discard new fields/fences. Use a compatible
binary and fresh verified directory revision before resuming. Never erase a
ledger or switch sources to make failed preparation succeed.

## State admission before runtime integration

API ordinary/maintenance state access and daemon state access check native
issuer/subject/local ID/source against the acknowledged reservation and prepared
mailbox/state before every cache lookup. A present or unreadable restore hold
refuses state access. Missing acknowledged storage is not recreated.
`state.OpenNative` opens an existing database with SQLite mode=rw and checks its
source before schema migration; it does not initialize a new database or import
legacy JSON. SQLite file URIs escape the configured path, including `#`, `?`
and `%`, so path punctuation cannot discard the existing-only open mode.
Losing the file after validation therefore fails at the open boundary.
Refusal does not close a handle already borrowed by a request.

This checks immutable storage ownership, not current activity/roles or live mail
permission. Inactive state remains available for administrative revocation when
not held. Native classification assumes intact private user markers; arbitrary
mixed-root/older-writer recovery is unsupported, and qualified restore rejects
missing markers. Live mail admission is separate from this storage-only check, as described below.

## Opt-in native runtime

The [internal outbox](NATIVE_OUTBOX.md) now preserves encrypted intent/claims/Sent through qualified storage and sealed snapshots. Native primary sending now uses fresh domain/settings/directory/users/device admission and a joined recovery worker; historical storage evidence alone grants no sending authority.

Protected admin domain-relay settings are available separately; [the relay contract](DOMAIN_RELAY.md) describes credential confirmation, fresh DNS/issuer fencing, encrypted storage and backup dependencies. Explicit native mode enables primary-address compose and client PGP through current admission and the durable outbox; saving credentials alone proves no provider readiness.

Set `KYPOST_NATIVE_MAIL=true` for both API and daemon, after configuring the
issuer and mail-domain proof. Empty or `false` preserves external IMAP mode;
other values refuse startup. Existing linked IMAP accounts stay external.
New native accounts require retained signed directory resources; token-only
JIT creation is refused while provisioning is pending. Webhooks retain desired
state before attempting allocation outside the directory lock. A failed attempt
returns the normal accepted-directory status with `mailboxStatus:pending`;
the API worker retries retained subjects at startup and every minute. Signed newer
revisions apply activity/roles before retention; storage retries leave local
deactivation intact. Initial limits are 5 MiB per message, 32 MiB live payload,
and 10,000 retained records per mailbox; existing reservations keep their limits.

Every mailbox operation and mail-authenticated cache read checks the configured
active issuer, current user and retained directory activity/role, primary
address, reservation revision and immutable storage source under the directory
fence. Restore holds deny admission. Cached clients confer no continuing
permission. An operation already admitted may finish after revocation; network
and database work do not hold the directory lock. Missing databases are opened
existing-only before initialization. Runtime clients retain their store during
active operations; dropping a cache reference does not close a borrowed handle.

`GET /api/imap/config` reports native accounts as configured, managed and native,
with their primary username, INBOX and `smtpConfigured:false`. User IMAP changes,
IMAP connection tests and admin IMAP assignment refuse native accounts. Drafts,
folders, labels, reads and incoming encryption use the existing client contract;
native inbox refresh uses full snapshots. PGP bootstrap suggests the verified
primary address; incoming encryption uses native INBOX rather than a leftover
IMAP file. Existing key custody and WKD publication proofs are unchanged.

Native primary compose/client-PGP sends use the configured relay and durable
outbox. Native pickup creation, aliases and system/own-address SMTP probes remain
refused or skipped pending their authority/dependency integration. A leftover IMAP
credential file cannot enable any native legacy SMTP path. Do not publish MX for this runtime alone. Roll back by
disabling both native flags in both processes and keeping all
native storage/ownership files intact; native mail becomes unavailable without
being converted to IMAP. Use a compatible binary, not an older metadata writer.

## Direct receiving runtime (qualification profile)

`KYPOST_NATIVE_RECEIVING=true` requires `KYPOST_NATIVE_MAIL=true` and is disabled
by default. It adds trusted-local `kypost-server receiving init|bind|accept`
commands and a daemon importer; it does not install or start a public receiver.
Use Linux with mounted procfs, existing owner-only configuration/state roots,
the established issuer-bound domain, prepared accounts and no restore hold.
Run `receiving init` explicitly once after domain setup; it refuses an existing
receiving directory. Normal operation opens existing users and ingress storage
without bootstrap, migrations or lost-spool recreation.

The gateway identity is fixed to `maddy-local`. Trusted gateway configuration
passes its own transaction ID, SMTP envelope sender and recipient to
`receiving bind <receiver-id> <sender> <recipient>` at RCPT. An empty reverse
path is valid. Definite unknown addresses return exit 3 (map to SMTP 550);
storage/proof failures return exit 1 (map to 451). After every accepted recipient,
`receiving accept <receiver-id> <sender>` reads exact raw MIME from stdin and
commits it before exit success. Map DATA failures to 451. Run binding as the
last RCPT authority check: a later recipient rejection would leave a phantom
binding. No header supplies ownership, gateway identity or transaction identity.
The process writes operation/result/correlation logs to stderr and no stdout.

SMTP stdin must be a named pipe or regular file. Pipe reads have a 30-second
deadline; inherited pipes are reopened through `/proc/self/fd` for Go's poller.
Regular files and filesystem operations retain the stalled-volume ceiling.
New reception performs fresh DNS proof outside locks. Domain → directory →
users locks then fence every frozen owner through acceptance. Import validates
all prepared owners/sources before commits and holds the same authority fences
through mailbox receipts and ingress acknowledgment; no network or stdin work
runs inside these fences. Local deactivation waits for admitted commits.

The buffer limits are 4 MiB per message, 64 MiB live payload and 10,000 records;
each recipient's own message limit is checked before acceptance. SMTP headers
added by the receiver count toward this limit. The daemon revisits pending
deliveries every five seconds. Partial mailbox failure retains holding bytes;
after lease expiry, exact receipts prevent duplicate local delivery. Already
accepted mail imports without fresh DNS, refreshing authorized route TTLs
before claiming. Missing storage, restore holds or disabled authority retain
pending mail. Same-revision local reactivation permits delivery to that same
owner. A proven owner/signed-generation conflict quarantines pending mail with
its bytes and frozen bindings intact; an active competing claim cannot be
invalidated. New signed revisions conservatively fence older bindings even
when their owner is unchanged. Quarantine requires operator reconciliation;
there is no reassignment or automatic release.

New route writes, RCPT bindings and MIME acceptance also check physical storage
inside the immediate SQLite writer transaction. The admission budget is derived
from the durable limits: `max(32 MiB, 4 × payload bytes + 32 KiB × records)`
(about 568.5 MiB for this profile), counting `ingress.db`, WAL and shared-memory
file lengths. Reserve 32 actual SQLite pages plus twice the incoming payload
for the next write, and keep at least 16 MiB plus that allowance available to
the process on the filesystem. Failure to measure storage refuses new growth;
no mail is evicted. Exact binding/payload retries remain idempotent. Near the
budget, reserve the whole WAL length plus 16 MiB before attempting a bounded
truncation checkpoint: copying uncheckpointed pages can grow the main database
before WAL blocks are released. Insufficient or unreadable headroom skips that
checkpoint and refuses new growth, while exact retries remain available. Recheck
actual bytes and headroom under the writer transaction. A pinned backup
reader can prevent reclamation; admission retries after it releases the snapshot.

These are conservative admission estimates, not hard SQLite or filesystem quotas.
Other writers can consume shared free space; repeated recovery writes and pinned
WAL can exceed the admission budget. The reserve does not guarantee space for
all recipient mailbox commits. Import retains the source after any failure.
The importer may renew only an existing unchanged route (owner, signed generation
and activation), then claim/import/acknowledge accepted mail even when new
admission is closed. It cannot use that path to create or reactivate a route.
Free space, finish blocking readers/imports and retry; preserve receipts if
reconciliation or an operator volume quota is needed. Never remove WAL or
shared-memory files from an open database to make space.

This profile is for controlled qualification. Before public MX, qualify bounded
receiver concurrency/rates, safe abandoned-RCPT and archived-receipt cleanup,
hard database/WAL/volume quotas and representative free-space reserves, TLS/spam policy, receiver provenance
and licensing, and power-loss/restore behavior on the intended volumes. Logical
payload limits do not bound physical disk growth. Successful RCPT followed by
disconnect consumes retained records; those records do not expire automatically.
The gateway process shares trusted local storage authority, so this profile is
not isolation from a compromised gateway. Disable acceptance before rollback;
preserve and reconcile all pending/quarantined bytes and receipts.

Qualification uses the pinned Maddy binary from
[receiver assessment](RECEIVING_GATEWAY_ASSESSMENT.md):

```sh
cd backend
MADDY_PROOF_BINARY=/absolute/path/to/pinned/maddy GOTOOLCHAIN=go1.26.6 go test -race ./internal/app ./internal/ingress -run '^TestNativeReceiving|^TestMaddyHoldingBoundary|^TestMailboxImporter' -count=1 -timeout=5m
```

The actual runtime check runs Maddy against the production receiving command
and daemon importer, with test-only loopback DNS. It checks native two-recipient
delivery, hidden envelope recipients, raw MIME, unknown-recipient refusal and
shutdown. Other checks cover local revocation, DNS outages, restore holds,
missing spool, pipe deadline, signed-generation quarantine and partial quota
failure/retry. Tests do not prove public deployment readiness.

## Verification and next gates

```sh
cd backend
GOTOOLCHAIN=go1.26.6 go test -race ./internal/fsutil ./internal/users ./internal/sso ./internal/mailbox ./internal/api -run '^TestNativeAllocation|^TestNativeDomain|^TestNativeMailDomain|^TestNativeAccountIssuer|^TestNativePublication|^TestLockFileContext|^TestPrepareAccount|^TestDirectory' -count=1 -timeout=20m
GOTOOLCHAIN=go1.26.6 go test -race ./internal/api ./internal/processor ./internal/mailbox ./internal/config -run '^TestNativeRuntime|^TestRuntimeClient|^TestExistingMailbox|^TestNativeMailRequiresExplicitBoolean' -count=1 -timeout=5m
GOTOOLCHAIN=go1.26.6 go test -race ./internal/state ./internal/sso ./internal/api ./internal/processor -run '^TestOpenNative|^TestNativeState|^TestNativeUserStorage|^TestNativePollerState' -count=1 -timeout=5m
```

Checks use real SQLite/filesystem and authenticated admin routes for DNS purpose,
issuer/profile immutability, expiry/rotation, transient DNS failures, cancellation,
legacy refusal, stable reservations, username collision, retained raw mail,
publication ordering and native login/directory/relink ownership. Existing
preparation/reconciliation SIGKILL checks remain. Process crashes are not
hardware power-loss evidence. A real allocator kill between acknowledgement and
user publication remains a runtime activation gate.

Next qualify whole-stack backup/restore and allocator publication crashes, then
qualify the opt-in runtime and its periodic repair at representative scale. Qualify storage waits, orphan cleanup, scale,
representative receiver revocation/import load and runtime shutdown behavior
before public transport activation; the local receiving/selector checks above
are implemented. Durable scoped client deltas, aliases and relay readiness remain
separate work. This change adds system DNS TXT lookups, no dependency or secret,
and no Android/Linux/iOS client wire change.
