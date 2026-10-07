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
The Server → Mail domain screen lists every domain of the set (see
[Several domains](#several-domains)) with its status (established, awaiting
verification with the exact TXT fields and expiry, lapsed, retired), marks the
founding domain and offers Add, Verify, Replace challenge, Retire and Re-add
through the same protected operations. Replacing a challenge and retiring ask
for confirmation first; the retire prompt names the in-use refusals, that address
records are kept and that re-adding needs fresh DNS proof. Against a server
without the domain-set API it shows the single founding domain. It does not
install a public receiving gateway or change MX.

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
The owner-only `$CONFIG_DIR/native-domains.json` (domain set, version 1) holds
the issuer, the founding domain and each domain's public challenge, not a
secret, plus the retired domains. Its `native-domains.json.lock` is the domain
fence in every lock order. Configuring a domain also creates an empty
version-2 ledger with the initialization fence set, then replaces the version-1
`native-domain.json` with the tombstone `{"migratedTo": "native-domains.json"}`.
Do not publish production MX or assume outgoing delivery readiness.

### Several domains

The single-domain routes above serve the founding domain (the first configured
one still in service) and refuse any other, so existing clients are unchanged.
The domain-set routes take the same CSRF, credential confirmation and KySignOn
step-up, answer `no-store` and use the same status codes:

- `GET /api/admin/mail-domains` returns `{issuer, founding, domains[]}`; each
  entry has `domain`, `configured`, `retired`, `recordName`, `recordValue`,
  `established`, `expiresAt` and `verifiedUntil`. Retired entries have no record.
- `POST /api/admin/mail-domains` with `{domain}` adds a domain with its own
  challenge, under the set's one issuer, or rotates the challenge of one already
  configured. Re-adding a retired domain configures it afresh (new challenge, not
  established): nothing routes, sends or allocates on it until it verifies again,
  then its kept addresses resume for their recorded mailboxes. Re-adding never
  changes the founding domain unless no domain is in service.
- `POST /api/admin/mail-domains/{domain}/verify` proves that domain only.
- `DELETE /api/admin/mail-domains/{domain}` retires it: refused (409)
  while any address on it is `active`, a `queued`/`retryable` outbox job sends
  from it, accepted (pending) incoming mail is bound to it, or the relay still
  sends for it (remove it from the relay first); a restore hold also refuses.
  Retirement keeps its address records, so generations are never reused and old
  bindings and snapshots still validate. It is never automatic. Until the domain
  is re-added and verified, a subject whose KyIdentity primary sits on it cannot
  be provisioned or admitted. Retiring the
  founding domain hands the single-domain routes to the smallest remaining one.

Every fence compares only the proofs of the domains an operation touches:
allocation the requested primary's domain, receiving each recipient's domain,
sending the `From` domain. Re-verifying one domain never fences work on
another; a lapsed proof suspends only its own domain, and a delivery to several
domains is all-or-nothing. A version-1 binary refuses a set with more than one
domain or any retired one.

A primary whose domain is missing or not a configured, non-retired member is
recorded once per revision as `failed` / `primary_domain_unavailable`, without
DNS. Sending ends a job (stale, quarantined) whose `From` domain is unconfigured
or retired or no longer in the relay set; a lapsed proof on a configured domain
leaves it for retry. Ownership and directory staleness are checked before the
proof is required, so a deactivated owner's job ends even while DNS is down. The
repair worker records deactivations with a disable-only reconcile that refuses
if the subject is active again.

## Storage format migration

Container startup runs `kypost-server migrate-native` as the runtime user after
the data-volume ownership handoff and before supervisord starts any service. It
converts version-1 native storage in place, under domain → directory → users
locks, following [the addressing spec](NATIVE_ADDRESSING_V2.md#migration-v1--v2):

1. copy `native-domain.json`, `native-provisioning.json` and `native-relay.json`
   to `*.v1-migrated`, re-copying each while it still holds version-1 data;
2. write `native-domains.json` from the domain copy;
3. write the version-2 ledger from the ledger copy (or an empty one when a domain
   has no ledger), seeding each primary address generation from the subject's
   directory revision and setting `legacyMixedUse` for administrator subjects
   ([administrator separation](#administrator-separation));
4. reseal the relay as version 2 with the same key and `generation`;
5. replace `native-domain.json` with the tombstone.

Every output derives from the copies, so a crash or a version-1 binary run in
between converges on re-run. Every copy is validated before the first output is
written; a version-1 ledger behind an unset initialization fence (which version 1
refused) is refused, not migrated. A tombstone means already migrated; fresh installs
and deployments without native files are untouched. Migration ignores and keeps a
restore hold and grants no authority. On failure it logs the cause with a
remediation and the container keeps booting: every native path then refuses
(`ErrNativeMigration`) because `native-domain.json` still holds version-1 data,
the ledger is not version 2, or the tombstone lacks `native-domains.json` or the
ledger, so domain setup cannot adopt a leftover version-1 ledger; external IMAP
accounts keep working. Keep the config volume and its `*.v1-migrated` copies, fix
the reported cause and restart. Runtime readers accept only ledger and relay
version 2.

Rollback to a version-1 binary fails closed: it rejects the tombstone (empty
token), ledger version 2 and relay version 2, so domain setup, allocation,
receiving, admission and sending all refuse. To roll back, restore the backup
taken before the upgrade; see [restore](RESTORE.md#storage-format-migration).

## Account allocation

`LifecycleStore.AllocateNativeAccount` is an internal new-account publication
flow. It proves the domain before acquiring locks, then holds locks in order:
domain → directory → users → account. It requires a live retained signed SCIM
resource with matching issuer/subject and active state. Roles come from that
resource, never a token email/name or classifier label. Exactly one explicit
primary bare ASCII dot-atom email is required; its domain must be a configured,
non-retired member of the set and is the one freshly proven; comparisons are
case-insensitive. Quoted/SMTPUTF8 addresses and implicit aliases are refused.

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

## Administrator separation

A subject whose retained directory resource carries `kypost.admin` is an
administrator identity and owns no native mailbox, per the suite rule and
[the addressing spec](NATIVE_ADDRESSING_V2.md#behaviour):

- In native mode the webhook creates it as an ordinary non-native, mailbox-less
  account inside the directory apply, so administrators can be provisioned while
  native mail is held. The repair worker does the same for a retained
  administrator resource with no account. Neither reserves an address, prepares
  storage or sets `nativeMailboxIssuer`/`nativeMailboxSource`; allocation refuses
  a new reservation for an administrator resource. A reservation left
  unpublished when its subject was promoted stays orphaned: retained, never
  published or delivered to.
- Demotion leaves that account non-native: the worker never converts an existing
  non-native account. An everyday identity is a separate KyIdentity subject.
- Native admission (API mail, sending, receiving bind/import, daemon polling)
  refuses an administrator-role subject unless its ledger account has
  `legacyMixedUse`. Promotion of an everyday native subject is enforced by
  admission from the next request; mail, reservation and storage are retained and
  demotion restores access. The directory apply that grants the role also
  disables all of the subject's addresses with a generation bump (see
  [addresses](#addresses-aliases-and-generations)). The role change does bump `nativeSendEpoch`, so outbox jobs queued before
  promotion are quarantined and never resume after demotion.
- The refusal is `sso.ErrNativeAdministrator` (it wraps `ErrNativeProvisioning`,
  so receiving and outbox keep their refusal handling). HTTP mail routes, device
  credentials, decision history, IMAP settings and native sending answer 403
  `{"error":"administrator identities have no mailbox; use your everyday
  identity","administratorIdentity":true}`, never 401, and log actor, action,
  target and result. Administrator routes stay available.
- In native mode an administrator also cannot save or test personal IMAP
  settings (`POST /api/imap/config`, `POST /api/imap/test`), and an
  administrator cannot assign IMAP to an administrator account
  (`PUT /api/users/{id}/imap-config`); same 403. IMAP-only deployments are
  unchanged. IMAP configuration stored before native mode is neither deleted nor
  blocked: removing it is part of the separately specified mixed-use migration
  and out of scope here.
- `legacyMixedUse` is set only by migration, for subjects holding a mailbox and an
  administrator role. It is cleared, under the directory lock, when an active
  resource lacks the role (in the directory apply and in reconcile); deactivation
  alone does not clear it. A demotion that cannot read or write the ledger fails
  the webhook, so KyIdentity retries it and the revision is never recorded over a
  surviving flag. While an initialized ledger is missing or unreadable, every
  applied revision therefore fails and is retried (the address state rule must
  run); a deactivation's account revocation inside the apply still takes effect,
  only its record waits. Deployments that never initialized a native ledger, or whose
  version-1 domain data still awaits storage migration, are unaffected:
  migration recomputes the flag from the directory recorded meanwhile. Nothing sets the flag again; restoring an older backup brings
  back flag and directory together, and the replayed demotion clears it. Snapshot
  validation refuses a flag on a demoted subject; the recovery authority digest
  includes it.
  Exception owner: the deployment owner. Expiry: the mixed-use migration, which
  moves the content to an everyday identity and removes the flag.

## Addresses, aliases and generations

Every ledger address is `active` (routes, may be `From`), `disabled` or
`reserved` (a released alias held until an administrator reassigns it).
Addresses are unique across domains, lowercase dot-atom, and never deleted.
Primary addresses come from KyIdentity and cannot be released or reassigned; a
KyIdentity primary that equals any existing alias or reserved address is
recorded as `address_conflict`.

- **Generations change only on reassign, disable, re-enable and release**, each
  to the previous value plus one, so ordinary directory edits never fence mail.
  Reassignment appends `(mailbox, generation)` to `history`.
- **Desired-state rule**, level-triggered on every applied directory revision
  inside `ApplyDirectory` (directory lock, before the lifecycle record), on
  every reconcile save and in the API worker's minute pass
  (`ReconcileNativeAddresses`, which writes only on divergence, such as a
  re-added domain): `active` iff the owner is active, has no
  `kypost.admin` role (or is `legacyMixedUse`) and the address domain is
  configured (not retired); otherwise `disabled`; `reserved` stays `reserved`.
  Each change bumps the generation. The same write then writes the ingress route
  of every non-`active` address inactive at its generation
  (`ingress.DeactivateRoutes`; no receiving store is a no-op). A failed ledger or
  route write fails the webhook; KyIdentity retries and the rule converges. The
  ledger is written first, so a route write that fails after it leaves the change
  durable (`ErrNativeRoutesPending`); every later commit and the worker pass
  retry the routes, and import already quarantines on the ledger generation.
- **Administrator actions** (`withAdmin`, CSRF, `withActionDigest` +
  `confirmActor` step-up, audited with actor, action, mailbox and result; never
  the address), under domain → directory locks and refused by a restore hold:
  - `GET /api/admin/mail-addresses[?user=<id>]` lists mailboxes with `address`,
    `kind`, `state`, `generation`.
  - `POST /api/admin/mail-addresses` `{mailbox, address}` adds an alias on a
    configured, non-retired domain to an everyday identity's mailbox. An address
    that any subject's KyIdentity resource names as its primary
    (case-insensitive, any domain, administrators and not-yet-provisioned
    subjects included) is refused, for add and reassign alike.
  - `DELETE /api/admin/mail-addresses/{address}` releases an alias (`reserved`,
    generation + 1, route inactive).
  - `POST /api/admin/mail-addresses/{address}/reassign` `{mailbox}` hands a
    reserved alias to a mailbox, the original included, at generation + 1.
  - 400 malformed address, 404 unknown mailbox/address, 409 taken, reserved,
    primary, a KyIdentity primary, wrong state, administrator or
    `legacyMixedUse` owner, unconfigured or retired domain, or a restore hold. A
    committed change whose route write failed answers 200 with the committed
    record and a `warning`, audited `committed_routes_pending`; repeating it gets
    the ordinary answer for the new state (a second release is 409).
- **Admin screen.** Server → Mail addresses lists each mailbox (labelled by its
  primary address and owner) with every address's kind, state and generation,
  filterable by user. Aliases offer Release (confirmation: the address becomes
  reserved, delivery and sending stop immediately, delivered mail stays,
  reassignment is explicit) and, once reserved, Reassign to a chosen mailbox
  (confirmation names the target); primaries offer nothing. Add alias takes a
  mailbox and an address and confirms that the address is held permanently. The list does not say which owners are
  administrators, so their refusal is the server's 409 text. A `warning` answer
  is shown as a committed change with pending routes, not an error.
- **Switch-over safety.** Ledgers written before this change (no
  `addressGenerations` marker) carried the directory revision in routes and
  bindings; loading raises each address generation to the owner's current
  directory revision, and the first write records the marker and freezes the
  result. Generations therefore never fall below any route or binding already
  written, and the ingress store's monotonic rule holds without a route rewrite.
  An older binary refuses a ledger holding aliases (native mail stops, IMAP is
  unaffected); without aliases it resumes raising generations, which stays
  monotonic. Roll back by restoring the pre-upgrade backup.
- The recovery authority digest includes every address record.

## Extra mailboxes

Administrators give an everyday identity further mailboxes beside the
KyIdentity primary ([spec](NATIVE_ADDRESSING_V2.md#mailbox-storage)).

- **Ledger.** An extra mailbox is a `mailboxes` record of kind `extra`, owned by
  an existing account's subject, with a random ID `mbx-<uuid>` (user IDs are bare
  UUIDs, so the two never collide), its own `stateRoot`/`limits` copied from the
  owner's primary, `state` `active` or `disabled` (the administrator's choice)
  and `source` once prepared. Its primary address is an address record of kind
  `primary` in that mailbox. Primary mailbox IDs stay equal to user IDs.
- **Storage.** `$STATE/mailboxes/<mailboxID>/` with `native-mailbox.json`,
  `mailbox/mailbox.db` and a mail-only `state.db` (processed, decisions,
  checkpoint, deferrals; never devices, pairing, subscribers or
  notifications). `mailbox.PrepareMailboxContext`/`ValidatePreparedMailbox`
  take the parent directory. Device, notification and sorter state stays in the
  owner's primary `state.db`.
- **Desired-state rule.** An extra mailbox's addresses are `active` only while
  the mailbox is prepared and not administrator-disabled, besides the owner
  terms above, so subject deactivation or promotion disables every mailbox of
  the subject and disable/re-enable bump generations through the same rule.
- **Admin API** (same gates, audit and error mapping as the address routes;
  audit names the mailbox, never an address):
  - `GET /api/admin/mailboxes[?user=<id>]` lists mailboxes with `kind` and
    `state` (the address listing carries `state` too).
  - `POST /api/admin/mailboxes` `{user, address}` creates an extra mailbox for a
    published native everyday account. The address must be on a configured,
    established, non-retired domain, globally unique and not any subject's
    KyIdentity primary; administrator and `legacyMixedUse` owners are 409, an
    unknown or non-native user 404. The ledger records the mailbox before its
    storage and the source after; repeating the request with the same user and
    address resumes an interrupted creation.
  - `POST /api/admin/mailboxes/{id}/disable` and `/enable` change an extra
    mailbox's state (409 when already in it, 404 for a primary or unknown ID).
    Mail is retained while disabled; outbox jobs still queued or retryable in
    it are quarantined on their next recovery attempt and never resume, even
    after re-enabling (accepted Sent obligations still finish).
  - Listings carry `prepared`: false while a creation has reserved a mailbox
    but not published its storage. Poller, outbox discovery and
    `GET /api/mailboxes` skip unprepared mailboxes; repeat the creation to
    finish it.
  - Creating or enabling one is also 409 while the owner has incoming
    encryption on or a replacement pending (see below).
- **Admin screen.** Server → Mail addresses captions each mailbox with its
  kind and state; an unfinished extra mailbox shows "not in service" and how to
  finish it, with no actions. New mailbox takes a user and an address and
  confirms that the address is held permanently and the mailbox cannot be
  deleted, that the user selects it in their client, and that incoming
  encryption is unavailable while it is active (disabling restores the option;
  it cannot be re-enabled while their encryption is on). Prepared extra
  mailboxes offer Disable (confirmation: delivery and sending stop at once,
  mail is kept, the user cannot open it until it is enabled, queued outgoing
  mail is quarantined for good) or Enable; primaries offer nothing. Errors,
  warnings and locking follow the address actions.
- **Authority.** `AdmitNativeMailbox` and `WithNativeMailAccess` take mailbox
  IDs: the owner is admitted as today, then an extra mailbox must belong to the
  same subject, be `active` and prepared, and pass storage validation. Unknown,
  foreign and disabled IDs refuse alike (`ErrNativeMailboxUnknown`).

Allocation context is at most 30 seconds and never outlives the domain proof.
Cancellable flock/mutex waits leave no abandoned waiter that acquires later.
Legacy writers preserve blocking flock behavior. Open/fsync and read-only SQLite
validation are not cancellable: healthy-volume qualification/watchdog and prompt
revocation under stalled storage remain activation gates. No network lookup runs
under directory/users locks. Instance-wide preparation serializes directory
updates; whole-file reservation rewrites/address scans require scale measurement.

## Reservations, failure and restore

`$CONFIG_DIR/native-provisioning.json` (version 2: `accounts`, `mailboxes`,
`addresses`) retains immutable issuer/subject/local ID (the primary mailbox ID),
primary address, absolute state root and limits; revision/digest/activity;
pending/applied/failed status, failure code and acknowledged source. Every
address keeps its state, a `generation` that never decreases and its `history`
(see [addresses](#addresses-aliases-and-generations)); this profile has only
primary mailboxes. Reserve
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
refuses state access. Native HTTP/notification references carry a separate mailbox
reference generation; stale/foreign/bare IDs fail before reads/actions. API and
daemon bind the rebuildable cache to that prefix before use, dropping old windows
while retaining body-omission policy. Internal owner/source/journal IDs stay intact.
Missing acknowledged storage is not recreated.
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
address on a configured, non-retired domain, reservation revision and immutable
storage source under the directory fence. Restore holds deny admission. Cached clients confer no continuing
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

Native compose/client-PGP sends use the configured relay and durable outbox,
from the primary or any `active` alias of the sending mailbox (an unowned or
inactive `From` is 403).

Mail endpoints accept an optional `X-KyPost-Mailbox` header selecting one of
the caller's mailboxes (absent, or the caller's user ID, is the primary). It is
resolved through the ledger on every request after authentication; an unknown,
foreign or disabled mailbox answers `404 {"error":"mailbox not found"}` before
any storage is opened (an administrator subject gets the usual 403). Per-user
endpoints (devices, pairing, contacts, CardDAV, PGP keys, settings, rule
definitions) ignore it; `/api/labels` reports the primary mailbox's labels. The
manual `POST /api/rules/run` runs the user's rules in the selected mailbox. `GET /api/mailboxes` lists the caller's
accessible mailboxes and their active addresses. Every mail cache is keyed by
mailbox ID. The poller polls each active mailbox with the owner's settings,
rules and labels, skipping disabled and unprepared mailboxes; sorter learning
covers the primary mailbox only. Incoming encryption keeps one journal per user
for the primary mailbox, so it and extra mailboxes exclude each other: creating
an extra mailbox is 409 while the owner has incoming encryption on or a
replacement pending, so is re-enabling one, enabling incoming encryption is 409
while the user has an active, prepared extra mailbox (disabling every extra
mailbox lifts it; an unfinished one finishes only through create), and the
poller refuses to poll an extra mailbox whose owner has it on. Both sides decide
under the owner's settings file lock, so concurrent requests cannot leave both
on; the lock order is settings, then domains, then directory. Native pickup creation and system/own-address SMTP
probes remain refused or skipped pending their authority/dependency integration. A leftover IMAP
credential file cannot enable any native legacy SMTP path. Do not publish MX for this runtime alone. Roll back by
disabling both native flags in both processes and keeping all
native storage/ownership files intact; native mail becomes unavailable without
being converted to IMAP. Use a compatible binary, not an older metadata writer.

## Direct receiving runtime (qualification profile)

`KYPOST_NATIVE_RECEIVING=true` requires `KYPOST_NATIVE_MAIL=true` and is disabled
by default. It adds trusted-local `kypost-server receiving init|bind|accept`
commands and a daemon importer; `receiving config` generates the bounded TLS-only
Maddy qualification profile described in [controlled setup](RECEIVING_SETUP.md).
It does not install or start a public receiver.
Use Linux with mounted procfs, existing owner-only configuration/state roots,
at least one established issuer-bound domain, prepared accounts and no restore hold.
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
New reception performs fresh DNS proof of each recipient domain outside locks;
an address on a domain outside the set is unknown (exit 3). Domain → directory →
users locks then fence every frozen owner through acceptance. Import validates
all prepared owners/sources before commits and holds the same authority fences
through mailbox receipts and ingress acknowledgment; no network or stdin work
runs inside these fences. Local deactivation waits for admitted commits.

The buffer limits are 4 MiB per message, 64 MiB live payload and 10,000 records;
each recipient's own message limit is checked before acceptance. SMTP headers
added by the receiver count toward this limit. The daemon revisits pending
deliveries every five seconds. Partial mailbox failure retains holding bytes;
after lease expiry, exact receipts prevent duplicate local delivery. The
acknowledgment that follows every owner's commit archives the delivery in the
same transaction: a compact tombstone (sender, digest, recipients) replaces the
row and its bindings, answers exact replays and stops the receiver ID from being
delivered again. The 10,000-record limit counts only staged, pending and
quarantined deliveries; tombstones are never pruned and count only toward the
physical budget. A typical tombstone is about 190-210 bytes (about 2 million in
the budget below); an attacker flooding many aliases with 320-byte senders and
100 recipients fits about 170,000. Backups refuse `ingress.db` above 64 MiB, at
about 360,000 typical tombstones. Already
accepted mail imports without fresh DNS, refreshing authorized route TTLs
before claiming. Missing storage, restore holds, directory lag, lock timeouts,
missing sign-on settings and local deactivation retain pending mail. Local
reactivation without a directory state change permits delivery to that same
owner. When admission refuses an import and the ledger durably records a frozen
address as moved, inactive or at a newer generation (an administrator disabled
the mailbox or released the address, or KyIdentity offboarded the owner or
promoted it to administrator), the delivery is quarantined instead: it could
never import again and would hold the 10,000-record and 64 MiB budgets for
good. A restore hold suppresses this. After re-enabling the mailbox an
administrator can release it, since release goes to the frozen mailbox. Routes and bindings carry the address generation;
a binding whose address is no longer `active`, owned by the bound mailbox and at
the bound generation quarantines with its bytes and frozen bindings intact; an
active competing claim cannot be invalidated. Ordinary directory edits change no
generation, so they fence nothing. Quarantine is never reassigned or released
automatically; an administrator releases or discards it (below).

### Quarantine release

Administrators list quarantined deliveries and release or discard each one,
through the admin API or the CLI. Both show envelope metadata only: gateway,
delivery ID, received time, envelope sender, size and, per recipient, the
address, frozen mailbox ID, owning user ID (empty once the mailbox is gone) and
frozen generation. Bodies, subjects and headers are never shown, and the reason
a delivery was quarantined is not stored.

- **Release** delivers to the mailboxes the delivery was frozen to, never to a
  newly chosen target or an address's current owner. Each frozen mailbox must
  still exist, be active, belong to the same issuer/subject, and its owner and
  storage must pass the same admission import uses, checked under the
  directory and users fences import holds. The address generation is not
  checked: the mail was addressed to that mailbox at that time, and a
  reassignment is the usual reason it was quarantined. Admission is all
  owners or none, but a capacity failure or crash during the commits can
  leave some owners with the mail; a retry completes it, and mailbox receipts
  prevent duplicates. A refusal answers 409 with one of two reasons: the
  mailbox was deleted or disabled, or its owner was offboarded, promoted or
  changed (durable; discard remains available), or the mailbox is not
  currently admitted (directory sync, storage or sign-on configuration;
  resync and retry). Release ends in the same tombstone as import, with
  disposition `released`. Repeating a completed release succeeds.
- **Discard** removes the bytes and bindings and leaves a tombstone, so an
  exact receiver replay or re-pickup is answered without delivering. Its
  disposition is `discarded`, or `partially_released` when a release had
  started: that release may already have reached some frozen mailboxes, and
  those copies stay. Discard remains available after an interrupted release
  because one owner may stay disabled for good. SQLite free pages, WAL and
  earlier backups may hold the bytes until reused or rotated.
- A release in progress holds a five-minute lease; discard refuses until it
  ends. Both refuse under a restore hold.

API (admin only; POSTs need CSRF and the account credential, or KySignOn
step-up, as for `/api/admin/mailboxes`):

- `GET /api/admin/receiving/quarantine[?after=<sequence>]` returns up to 100
  `{deliveries:[{sequence,gateway,id,sender,receivedAt,size,recipients:[{address,mailbox,user,generation}]}]}`;
  page with the last `sequence`.
- `POST /api/admin/receiving/quarantine/{gateway}/{id}/release` and
  `.../discard` return `{gateway,id,result}` (`released`, `discarded` or
  `partially_released`); 404 unknown or malformed, 409 not quarantined,
  release in progress, release refused or restore hold, 503 storage or mailbox
  capacity (the holding copy is kept).

CLI, as the runtime user that owns `STATE_DIR` (it refuses any other):

```sh
docker compose exec --user kypost kypost-server kypost-server receiving quarantine list [<after-sequence>]
docker compose exec --user kypost kypost-server kypost-server receiving quarantine release <gateway> <id> --confirm <id>
docker compose exec --user kypost kypost-server kypost-server receiving quarantine discard <gateway> <id> --confirm <id>
```

Shell access as that user already reaches every key the API's step-up
protects, so the CLI asks for deliberate intent instead: the delivery ID typed
again after `--confirm`. Discard prints its disposition. Every action is
audited with actor, action, gateway/ID and result, never correspondence. API
actions go to the API log (`api.err.log`) with the administrator's user ID.
CLI actions are logged as actor `cli:<uid>` to the invoking terminal only:
KyPost opens no log files of its own (see `LOGGING.md`). For the CLI the
durable record is the tombstone disposition and, for a release, the mailbox
receipt.

The sender is attacker-controlled: any interface must render it, and every
other listed field, as plain text.

Admin UI: Server → Quarantine lists the same envelope fields 100 at a time
(Load more pages with `after`), as plain text with control, bidi,
zero-width and blank-letter characters and stacked combining marks shown as
`[U+XXXX]`. An empty user reads "mailbox gone or owner changed; release will
be refused". Release confirms first that the mail goes only to the frozen
mailboxes, then names them; Discard confirms first that the deletion is
permanent and may be recorded as partially released. A cancelled KySignOn or
local credential failure changes nothing and leaves the screen usable. Both use
the account credential or KySignOn step-up. A 409 reason is shown as returned
and the list re-read; an unanswered or mismatched answer locks until reload.
With native mail off (404) the tab says so. A delivery whose gateway or ID is
a URL dot segment (`.` or `..`) is CLI-only.

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
The importer may renew only an existing unchanged route (owner, address generation
and activation), then claim/import/acknowledge accepted mail even when new
admission is closed. It cannot use that path to create or reactivate a route.
Free space, finish blocking readers/imports and retry; preserve receipts if
reconciliation or an operator volume quota is needed. Never remove WAL or
shared-memory files from an open database to make space.

This profile is for controlled qualification. Before public MX, qualify bounded
receiver concurrency/rates, safe abandoned-RCPT cleanup, tombstone pruning and
capacity recovery (durable limits cannot be raised, so a full budget stops
reception for good),
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
missing spool, pipe deadline, address-generation quarantine and partial quota
failure/retry. Tests do not prove public deployment readiness.

## Verification and next gates

```sh
cd backend
GOTOOLCHAIN=go1.26.6 go test -race ./internal/fsutil ./internal/users ./internal/sso ./internal/mailbox ./internal/api -run '^TestNativeAllocation|^TestNativeDomain|^TestNativeMailDomain|^TestNativeAccountIssuer|^TestNativePublication|^TestLockFileContext|^TestPrepareAccount|^TestDirectory' -count=1 -timeout=20m
GOTOOLCHAIN=go1.26.6 go test -race ./internal/api ./internal/processor ./internal/mailbox ./internal/config -run '^TestNativeRuntime|^TestRuntimeClient|^TestExistingMailbox|^TestNativeMailRequiresExplicitBoolean' -count=1 -timeout=5m
GOTOOLCHAIN=go1.26.6 go test -race ./internal/state ./internal/sso ./internal/api ./internal/processor -run '^TestOpenNative|^TestNativeState|^TestNativeUserStorage|^TestNativePollerState' -count=1 -timeout=5m
GOTOOLCHAIN=go1.26.6 go test -race ./internal/sso ./internal/app ./internal/backup -run 'TestNativeMigration|TestMigrateNative|TestNativeBackupAcceptsV1AndV2Snapshots' -count=1 -timeout=5m
```

The migration checks start from real version-1 files (allocated accounts, an
administrator subject, a relay, receiving bindings at an older and the current
directory revision and a queued outbox job) and check unchanged assignments,
relay generation, quarantine/import outcomes and claims; byte-identical results
after a crash at every step; convergence after a version-1 rewrite; refusal of
unmigrated and half-migrated state; version-1 rejection of migrated files; the
unchanged recovery digest and hold; and v1/v2 snapshot validation.
`scripts/check-private-roots.py` seeds a version-1 domain and checks the real
entrypoint migrates it as the runtime user.

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
are implemented. Durable scoped client deltas and relay readiness remain
separate work. This change adds system DNS TXT lookups, no dependency or secret,
and no Android/Linux/iOS client wire change.
