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

## Mailbox quotas

Every native mailbox, primary and extra, has the same limits (owner decision
2026-10-07): a 25 MiB message (both receiving profiles), a storage quota of
`KYPOST_MAILBOX_QUOTA_BYTES` (default 5 GiB, 5 × 2^30 = 5368709120 bytes;
256 MiB to 1 TiB, anything else refuses startup and `migrate-native`) and
1,000,000 retained records (tombstones included). The quota counts live raw
bytes plus queued outgoing mail. It is one deployment-wide setting: every
process reads the same environment, and a change applies at the next start.

Limits are persisted in four places that must agree: the ledger
(`native-provisioning.json`, each mailbox's `limits`; extra mailboxes carry the
owner's), each mailbox's `native-mailbox.json`, its `mailbox.db` `identity` row,
and the comparisons that admission, preparation and restore validation make
between them. After the format migration, `migrate-native` applies the
configured limits under the same domain → directory → users locks:

1. the ledger, in one atomic write, when any mailbox differs;
2. each published mailbox (`mailbox.ConvergeLimits`, under its preparation
   lock): the `mailbox.db` identity row first, `native-mailbox.json` last, so a
   preparation file at the target proves both are.

The ledger is the source of truth and every step is idempotent, so a crash
anywhere, or a quota changed between two starts, completes on the next start;
until then the half-converged mailbox is refused like any mismatch, never
silently adopted. A second run writes nothing. A failed mailbox is reported and
the others still converge. A restored backup with older limits is migrated the
same way at the next start (the restore hold stays). New mailboxes are prepared
with the configured limits.

A quota lowered below what a mailbox holds does not lock it: it opens, reads,
moves and deletes as before and backup validation accepts it; only new mail
(delivery, import, outgoing queue) is refused until its usage is back under the
quota. Receiving answers a full mailbox temporarily: `452 4.2.2 Mailbox full` at
RCPT and at DATA (helper exit 9), so the sender's queue retries and nothing is
lost; hosted pickup leaves that message in R2 while others' mail flows. Mail
accepted before the mailbox filled stays pending in the receiving buffer and
imports once space is freed.

The drive reserve is separate from quotas: new mail is refused before the
filesystem holding `STATE_DIR` fills, keeping 10% of it or 5 GiB free, whichever
is larger (`fsutil.CheckDriveReserve`, one helper shared with the backup
scratch check). Receiving refuses growth inside it with `452 4.3.1 Insufficient
system storage` (exit 10) at RCPT and DATA; hosted pickup stops fetching and
leaves mail in R2; imports refuse the upload (507) and stop before the message
that would cross it. Exact retries of already accepted mail still answer.

`GET /api/mailboxes` gives each of the caller's mailboxes `usedBytes` (absent
when its database cannot be read) and `quotaBytes`; Settings → Mail → Storage
shows them, warning at 80% and more strongly at 95%. `GET
/api/admin/mail-addresses` (and `/api/admin/mailboxes`) adds the same per
mailbox and a `storage` summary: `quotaBytes` (sum), `usedBytes` (sum),
`freeBytes`, `totalBytes`, `reserveBytes` and `overcommitted`, which is true
when quota not yet used exceeds 80% of the free space beyond the reserve.
Server → Mail addresses shows it as a warning, never a refusal: quotas are
promises the drive may not keep, and the reserve is what actually protects it.

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
deactivation intact. Mailboxes get the [native limits](#mailbox-quotas); a
reservation keeps its ledger limits, which `migrate-native` keeps current.

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

### Mail export

Users export their own native mailbox, or one folder of it, from Settings →
Mail → Export Mail; [mail import](#mail-import) is its mirror.

- `GET /api/export/folders` lists every folder of the selected mailbox
  (`X-KyPost-Mailbox`).
- `POST /api/export` with `{mailbox, folder, format, password|authSecret}`:
  `mailbox` is an ID from `GET /api/mailboxes` (empty or the user ID is the
  primary), `folder` empty for all folders (exact name; only `INBOX` is
  case-insensitive), `format` `mbox` or `eml-zip`. Browser session only (a
  paired-device credential has none and gets 403), CSRF, and `confirmActor`
  behind `withActionDigest`, so the mailbox selection is part of the confirmed
  request. Answers `{url:"/api/export/<64 hex>", expiresInSeconds:300,
  messages}`; `messages` is the live count when the grant is made, shown so the
  user can compare it with the file. One unspent grant per user; a new one
  replaces it. A foreign, unknown or disabled mailbox is 404, a missing folder
  404, an external IMAP account 409, mbox over HTTP/1.0 409. Export follows mail
  admission: an administrator identity holding no mailbox gets 409, a promoted
  KyIdentity administrator 403, and a `legacyMixedUse` administrator exports
  their own mailboxes until migrated. Nobody can export another user's mail.
- `GET /api/export/{token}` spends the grant once and streams
  `Content-Disposition: attachment`; HEAD is 405 and spends nothing. The grant
  is held in memory and bound to user, session, mailbox, folder and format.
  The browser navigated here, so every refusal is `303 See Other` to
  `/settings/mail?tab=export&export=<code>`, which explains it: `expired`
  (another session or user, expired, spent or unknown), `busy` (the slot is
  taken; the grant is kept and `retry=<token>` lets the page retry it),
  `proxy` (mbox over HTTP/1.0; grant kept) or `unavailable` (the mailbox or
  folder is no longer admitted; grant spent).
- One export streams per user and two server-wide (`maxExports`). A reader
  gets 30 seconds plus the message size at 64 KiB/s to take each message
  (a 25 MiB message: 430 seconds), replacing the server-wide 10-minute write
  timeout; a stalled reader times out and frees its slot. Admission is
  rechecked when the download starts and on every 200-message page, so a
  mailbox disabled mid-export stops it.
- A failed stream aborts the connection. Over HTTP/1.1 the body is chunked, so
  the client sees an unterminated body rather than a clean end. HTTP/1.0 has no
  chunking and the response no Content-Length, so a cut-off mbox would end like
  a whole one: mbox therefore requires HTTP/1.1 (behind nginx, set
  `proxy_http_version 1.1;`; its default upstream protocol is 1.0). An EML zip
  is still served over HTTP/1.0, since a truncated zip lacks its central
  directory.
- Formats hold the exact stored bytes, so PGP-encrypted messages stay
  encrypted and the server decrypts nothing. Flags and labels are not exported.
  mbox is mboxrd: a `From <envelope sender> <asctime UTC>` separator
  (`MAILER-DAEMON` when no delivery receipt holds a sender, or it contains
  whitespace or control characters), one `>` added to every `^>*From ` line,
  stored line endings kept (normally CRLF) and an LF blank line after each
  message. The zip holds `<folder>/<message id>.eml` (Deflate); each folder
  segment keeps letters, digits, space, `-`, `_` and `.`, replaces anything
  else with `_`, is cut to 64 bytes, loses leading and trailing dots and
  spaces, and gains a `_` prefix when it names a Windows device (`CON`, `PRN`,
  `AUX`, `NUL`, `COM1`-`COM9`, `LPT1`-`LPT9`, any case, with or without an
  extension), so no entry is absolute, climbs out or opens a device. Messages
  are read in ID order, one in memory at a time; one moved or deleted
  mid-export is skipped.
- Each export is audited as `mail export` with actor, target (the mailbox),
  folder, format, `messages` and `bytes` as integers, a random per-grant
  `correlation_id` (never the token) and result (`authorized`, `started`,
  `finished`, `failed`, `refused`); never subjects or addresses.

### Mail import

Users import mbox and EML files, or the folders of another mail account over
IMAP, into their own native mailbox from Settings → Mail → Import Mail ("From a
file" or "From another mail account"). The file rules come first; [import from
another account](#import-from-another-mail-account) adds its own below.

- `POST /api/import` with `{mailbox, folder, password|authSecret}`: `mailbox`
  as for export, `folder` an existing folder or a new one under an existing
  parent (empty is `Imported`; only `INBOX` is case-insensitive). Browser
  session only (a paired device gets 403), CSRF, and `confirmActor` behind
  `withActionDigest`, because a stolen session must not plant mail (a fake
  bank message) in the user's history. Answers `{url:"/api/import/<64 hex>",
  expiresInSeconds:300, folder, maxBytes}`. The folder name and its parent are
  checked here; a new folder is created only when the upload arrives. One
  unspent link per user, held in memory and bound to user, session, mailbox
  and folder. A foreign, unknown or disabled mailbox is 404, a missing parent
  404, an invalid name 400, an external IMAP account 409, a busy user or server
  409. Admission matches export: an administrator identity holding no mailbox
  gets 409, a promoted KyIdentity administrator 403, and a `legacyMixedUse`
  administrator imports into their own mailboxes until migrated. Nobody can
  import into another user's mailbox.
- `POST /api/import/{token}` spends the link and takes the raw file as the body
  (`application/octet-stream`). It streams to a `0600` temp file in
  `$STATE_DIR/imports/`, never to memory, refusing more than `maxBytes` (413)
  and an empty body (400); the cap is the mailbox's whole storage quota
  (5 GiB by default), since nothing larger can fit. An upload that would cut
  into the [drive reserve](#mailbox-quotas) is refused up front (507). An
  upload idle for a minute, or still sending after two hours in total (at
  least 0.7 MiB/s for 5 GiB), is cut off (408), so a trickling upload frees
  its slot. It answers `202`
  with the job status and the job runs in the background, past the end of
  the request. A busy refusal (409) keeps the link. An upload that finishes
  after shutdown began starts no job (503).
- `GET /api/import` is the caller's latest job: `{state, mailbox, folder,
  imported, duplicates, skipped, bytes, error?, maxBytes, maxMessageBytes}`
  with `state` `idle`, `uploading`, `running`, `finished`, `failed` or
  `cancelled`. `POST /api/import/cancel` (CSRF) stops a running job before
  its next message; 409 when none runs.
- One import uploads or runs per user and two server-wide (`maxImports`).
  Jobs live in memory: a restart or shutdown cancels them and the startup
  removes `$STATE_DIR/imports/`; every other path removes the temp file when
  the job or upload ends. The user re-imports, and duplicates make that safe.
- Formats are detected by content: a zip (`PK`), an mbox (starts `From `),
  otherwise one EML. mbox splits at a `From ` line at the start of the file or
  after a blank line, drops that blank line, removes one `>` from every
  `^>+From ` line (mboxrd; for mboxo it undoes the `>From ` quoting) and keeps
  CRLF or LF line endings as they are, so a KyPost export round-trips exactly.
  Messages are stored as parsed from the file, which for mboxo is not always
  the original: mboxo never quoted a genuine `>From ` line, so it loses one
  `>`, and an unquoted body line starting `From ` after a blank line splits the
  message in two.
  A zip passes every `*.eml` entry (any case) into the one target folder, in
  directory order, read into memory one at a time; entry names are never used
  as paths and folders inside the zip are not recreated. More than 10,000
  entries, or more inflated bytes than `maxBytes`, refuses the archive; an
  entry inflating past 100 times its compressed size (at least 1 MiB) is a
  bomb and skipped, as are entries not named `*.eml` and unreadable or
  corrupt ones.
- Each message is stored as its parsed bytes, seen, with the `Date` header as
  its date (now when absent or invalid) and no flags or labels. A message that
  is empty, has no RFC 5322 header, or exceeds the smaller of the 25 MiB
  inbound cap and the mailbox's message limit (also 25 MiB) is skipped and
  counted, before any lock or admission is taken; one bad message never stops
  the import. A job handles at most 2,000,000 messages (twice the mailbox's
  1,000,000 records), counting duplicates and skipped ones, and fails beyond that
  with a request to split the file, so a flood of empty separators or
  duplicates is bounded work. Each stored message takes the settings lock and
  mailbox admission on its own: holding them across a batch would block the
  user's settings writes, or keep storing into a mailbox already disabled, for
  the whole batch. A folder already holding a
  live copy of the same bytes (SHA-256, indexed per folder) counts a
  duplicate instead; a deleted copy no longer blocks re-import. A full
  mailbox, the drive reserve reached, a mailbox no longer admitted or a zip
  refused as a whole stops the job as `failed` with the reason.
- Imported mail is not received mail: no delivery receipt (receiving dedupe
  and quarantine are untouched). It is stored seen and recorded in the
  mailbox's `imported` table, which the poller's unread-INBOX query excludes,
  so even marked unread or moved to INBOX it never gets rules,
  classification, sorter learning, notifications or push, and it is not
  scanned for spam. The screen says so. (Advancing the poller's checkpoint
  instead would mean the API writing daemon-owned state.)
- Incoming encryption sweeps only unread INBOX mail, so imported mail would
  stay plaintext: import is refused (409) while the user has incoming
  encryption on or a replacement pending, at the link, at upload and before
  every message, which is stored under the owner's settings file lock (the
  lock enabling encryption takes) with the setting re-read. Turning
  encryption on mid-import fails the job before its next message.
- Each import is audited as `mail import` with actor, target (the mailbox),
  folder, `messages` (imported), `duplicates`, `skipped` and `bytes` as
  integers, `reason` for a failure, a random per-link `correlation_id` (never
  the token) and result (`authorized`, `started`, `finished`, `failed`,
  `cancel_requested`, `cancelled`); never subjects or addresses.
- `archive/zip` reads the whole central directory before the entry cap
  applies: about 4.5 times the upload live and 6 times allocated at worst.
  That is bounded by the quota cap; raising the per-mailbox quota raises it
  too.

#### Import from another mail account

The server signs in to the user's other provider and copies the folders they
choose. Admission, the per-user and server-wide job limits, status and cancel,
the per-message cap, the job's message cap, dedupe per folder, the `imported`
table, the incoming-encryption refusal (at the grant, at start and before every
message, under the settings lock) and shutdown are the file import's.

- `POST /api/import/imap` with `{mailbox, host, port, security, username,
  password|authSecret}` (the KyPost step-up; never the provider password):
  browser session, CSRF and `confirmActor` behind `withActionDigest`. `security`
  is `tls` on port 993 or `starttls` on 143; nothing else, because no other port
  is an IMAP server worth reaching and a free port would make the server a
  scanner of other services. Plaintext and unverified certificates are not
  offered. `host` is a DNS name or IP literal. Answers `{token: <64 hex>,
  expiresInSeconds: 600, target: "Imported/<host with '.' as '-'>"}` (folder
  names cannot contain `.`). The grant is in memory, one per user, bound to user,
  session and mailbox like the upload link, and holds host, port, security and
  username. Minting refuses (409) while the user's import runs or the server's
  two import slots are taken, and (503) once shutdown began; it cancels the
  user's previous grant, including a listing still in flight.
- `POST /api/import/imap/{token}/folders` with `{password}` (the provider's)
  connects, signs in and answers `{folders: [{name, path, attributes}], target}`.
  It needs no step-up of its own: the grant is the proof. Sign-ins are bounded
  per user, not per grant, so a fresh step-up does not reset them: six listings
  that do not sign in (wrong password, unreachable or refused server) lock the
  user out of listing for an hour (429 with `Retry-After`, before any dial); a
  successful sign-in refunds only its own attempt. At most four listings dial
  out at once server-wide (503 after waiting up to 3 s for a slot, spending no
  attempt), one per user (a new grant cancels the old listing), and
  none while the user's import runs (409) or after shutdown began (503). A
  listing reads at most 4 MiB, which also bounds what the grant holds. The
  provider password is kept out of the step-up request because the
  KySignOn grant is bound to a SHA-256 of that request's body; an unsalted
  digest over otherwise guessable fields would be an offline guessing target
  wherever it was kept, and replaying the body after the KySignOn popup would
  resend it. After a successful sign-in the grant keeps the password as a byte
  slice; it is wiped when the job signs in, or when the grant is replaced or
  expires (a timer at 10 minutes). The wipe is best effort: the JSON decoder's
  string, the TLS buffers and the copies Go made along the way are only dropped
  and left to the collector.
- `POST /api/import/imap/{token}/start` with `{folders: [name], target}` spends
  the grant and answers `202` with the job status. Folders must come from the
  listing; `target` (default above) and each mapped folder must be valid folder
  names, else 400 naming the folder. Missing folders are created as the job
  reaches them.
- SSRF: the host is resolved once (`netguard.IsPrivateOrReserved`, the guard the
  CardDAV and UnifiedPush clients share). Any loopback, private, link-local,
  CGNAT, multicast, unspecified or reserved answer, IPv4-mapped forms included,
  refuses the name (400), so it cannot pair a public address with an internal
  one. The connection goes to that validated IP and the job reuses it, while TLS
  verifies the certificate for the host name (system roots, TLS 1.2 or newer).
  Unlike the CardDAV guard, `SANDBOX_PRIVATE_HOSTS` does not apply.
- The client is `imap.ImportSource`, not go-imap: that library dials the host
  name itself, again on every automatic reconnect, keeps certificate checks in a
  process-wide variable, has no STARTTLS and buffers whole responses. Its
  vocabulary is `STARTTLS`, `LOGIN`, `LIST "" "*"`, `EXAMINE`, `FETCH lo:hi (UID
  RFC822.SIZE FLAGS INTERNALDATE)`, `UID FETCH <uid> (BODY.PEEK[])` and
  `LOGOUT`: read-only by construction, and EXAMINE plus BODY.PEEK leave `\Seen`
  alone. A greeting other than `* OK` (PREAUTH would skip TLS) or bytes after the
  STARTTLS answer are refused. Responses are bounded at 64 KiB outside literals
  and a literal at the per-message cap (64 KiB outside a body fetch). Timeouts:
  10 s to connect, 30 s per command, 2 minutes per message, 45 s for a listing (so a server that never answers holds a slot that long),
  4 hours for a job.
- Folders: `\Noselect` and `\NonExistent` are skipped, as are names over 255
  bytes or with a control character, backslash or double quote (they cannot be
  quoted back safely). Names are decoded from modified UTF-7 and split at the server's
  delimiter; each level maps under the target with `/` and `.` replaced by `_`.
  Two remote folders that map to the same local name (say `a.b` and `a_b`) are
  merged into one folder. The screen leaves `\All` and `\Flagged` (Gmail's All
  Mail and Starred) unchecked, since they hold every message again and dedupe is
  per folder, and shows names with control, bidi and zero-width characters
  escaped as code points.
- Message counts and sequence numbers from the server are at most 2^31-1 and
  sizes at most 2^32-1; a wider value is a protocol error, never wrapped. A
  response line over its 64 KiB budget ends the session at once. Messages are paged 200 at a time by sequence number; a message deleted on the
  server meanwhile is skipped. Every sequence number a folder's `EXISTS`
  announces counts toward the job's message cap before any page is fetched, so
  a huge `EXISTS` fails the job at once. One announcing more than the
  per-message cap (`RFC822.SIZE`) is skipped without being downloaded; each
  other is fetched with its announced size as the limit, so sending more than
  announced stops the job. Each message is stored as its exact
  bytes with `INTERNALDATE` as its date (the `Date` header when absent), `\Seen`
  as read and `\Flagged` as starred; other flags are ignored. Unread imported
  mail stays out of the poller through the `imported` table. The job's session
  reads at most twice the mailbox's storage, counting every byte from the
  server (duplicates, metadata and unsolicited responses included), then
  stops.
- Not resumable: a failed, cancelled or restarted job is imported again, and
  duplicates are skipped. The status adds `host`, `current` (the local folder in
  progress), `foldersDone` and `foldersTotal`; never the username.
- Audit: the `mail import` record adds `source` (`file` or `imap`) and `host`,
  never the username or password, with results `authorized`, `listed`,
  `list_failed` (with the reason shown to the user), `started`, `finished`,
  `failed`, `cancel_requested` and `cancelled`. The provider's own error text is
  never shown or logged.

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
a [blocked sender](#sender-blocks) returns exit 6 (550 5.7.1, before DNS proof
or any binding), a sender shape the Cloudflare Worker would also refuse exit 7
(550 5.1.7) and an unreadable block list exit 8 (451); storage/proof failures return exit 1 (map to 451). After every accepted recipient,
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

The buffer limits are 25 MiB per message, 512 MiB live payload (a burst of
held mail) and 10,000 records; each recipient's own message limit and remaining
quota are checked at RCPT and again before acceptance. SMTP headers added by the
receiver count toward this limit. Every opener raises a buffer created with
lower limits (the earlier 4 MiB / 64 MiB) to these in one SQLite transaction
and keeps its held mail; lower configured limits are refused, never applied. The daemon revisits pending
deliveries every five seconds. Partial mailbox failure retains holding bytes;
after lease expiry, exact receipts prevent duplicate local delivery. The
acknowledgment that follows every owner's commit archives the delivery in the
same transaction: a compact tombstone (sender, digest, recipients) replaces the
row and its bindings, answers exact replays and stops the receiver ID from being
delivered again. The 10,000-record limit counts only staged, pending and
quarantined deliveries; tombstones are never pruned and count only toward the
physical budget. A typical tombstone is about 190-210 bytes (millions fit the
budget below); an attacker flooding many aliases with 320-byte senders and
100 recipients fits several hundred thousand. Sealed backups without
`KYPOST_BULK_BACKUP_REPOSITORY` refuse `ingress.db` above 64 MiB, at about
360,000 typical tombstones or 64 MiB of held mail; the bulk backup has no such cap. Already
accepted mail imports without fresh DNS, refreshing authorized route TTLs
before claiming. Missing storage, restore holds, directory lag, lock timeouts,
missing sign-on settings and local deactivation retain pending mail. Local
reactivation without a directory state change permits delivery to that same
owner. When admission refuses an import and the ledger durably records a frozen
address as moved, inactive or at a newer generation (an administrator disabled
the mailbox or released the address, or KyIdentity offboarded the owner or
promoted it to administrator), the delivery is quarantined instead: it could
never import again and would hold the 10,000-record and 512 MiB budgets for
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
- **Release to the current owner** applies only to an *unresolved* delivery:
  one the hosted Cloudflare profile captured under a routing table this server
  never published, typically mail that waited at Cloudflare through a restore
  and takeover. Its original owner cannot be proven (after a restore the
  original and the restored instance can each bump an address's generation from
  the same base, so equal generations prove nothing), so it is stored with an
  empty owner. Listing marks it `unresolved` and names the recipient address's
  owner today (`currentMailbox`, `currentUser`), which is not proven to be the
  original. An ordinary release refuses it with that explanation; the explicit
  release binds it to that owner at the address's current generation, under
  the same fences as import and only while the address is active and its
  owner's mailbox is still the one the administrator reviewed in the list (the
  request names it), then releases as above. If the address moved to another
  mailbox between review and confirmation, it refuses: today's owner changed
  since you reviewed it; reload. It is refused while the address is inactive (discard
  remains), and for any delivery that already has an owner. It is audited as
  its own action. A crash between binding and release leaves the delivery bound
  to that owner; finish it with an ordinary release.
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
  `{deliveries:[{sequence,gateway,id,sender,receivedAt,size,unresolved,recipients:[{address,mailbox,user,generation,currentMailbox?,currentUser?}]}]}`;
  page with the last `sequence`. An unresolved recipient has an empty `mailbox`
  and `user` and, while its address is active, today's owner.
- `POST /api/admin/receiving/quarantine/{gateway}/{id}/release` and
  `.../discard` return `{gateway,id,result}` (`released`, `discarded` or
  `partially_released`); 404 unknown or malformed, 409 not quarantined,
  release in progress, release refused or restore hold, 503 storage or mailbox
  capacity (the holding copy is kept). For an unresolved delivery, release
  takes `{"toCurrentOwner": true, "currentMailbox": "<currentMailbox as listed>"}`
  beside the credential (the step-up binds both; either alone is 400)
  and is audited as `release_quarantine_to_current_owner`; discard refuses the
  flag with 400.

CLI, as the runtime user that owns `STATE_DIR` (it refuses any other):

```sh
docker compose exec --user kypost kypost-server kypost-server receiving quarantine list [<after-sequence>]
docker compose exec --user kypost kypost-server kypost-server receiving quarantine release <gateway> <id> --confirm <id>
docker compose exec --user kypost kypost-server kypost-server receiving quarantine release-to-current-owner <gateway> <id> <currentMailbox-as-listed> --confirm <id>
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
`[U+XXXX]` (a run of one code point as `[U+XXXX ×N]`). An empty user reads "mailbox gone or owner changed; release will
be refused". Release confirms first that the mail goes only to the frozen
mailboxes, then names them; Discard confirms first that the deletion is
permanent and may be recorded as partially released. A cancelled KySignOn or
local credential failure changes nothing and leaves the screen usable. Both use
the account credential or KySignOn step-up. A 409 reason is shown as returned
and the list re-read; an unanswered or mismatched answer locks until reload.
With native mail off (404) the tab says so. A delivery whose gateway or ID is
a URL dot segment (`.` or `..`) is CLI-only.

### Sender blocks

Administrators block an envelope sender address or domain manually, through
the admin API or the CLI; with the Rspamd sidecar, the Maddy profile also
blocks authenticated abusive senders automatically ([below](#automatic-sender-blocks)).
Both receiving profiles enforce the same list before storing anything.
Admin → Server → Sender blocks lists the blocks in force with the evidence
status below, adds manual blocks and removes either source; see also
[abusive senders](CLOUDFLARE_CONTINUOUS_RECEIVING.md#abusive-senders).

- **Store.** `STATE_DIR/receiving/sender-blocks.json` (0600), written under its
  own lock file and published by rename; it exists only after `receiving init`
  (writes refuse before, and never create the receiving directory). Sealed
  backups collect it. Each entry has `id` (first 16 hex of SHA-256 of
  `kind:value`), `kind` (`address` or `domain`), `value`, `until` (Unix
  milliseconds, or null: manual blocks never expire unless one is set),
  `source` (`manual`, level 0; or `automatic`, actor `automatic`, reason
  `abuse`, with its escalation `level` 1-3), `createdAt`, `actor` and `reason` (a code: `spam`,
  `phishing`, `abuse` or `other`, the default; no free text). Expired entries
  are ignored and dropped at the next write. At most 5000 blocks (the Worker's
  limit) and 512 KiB of their exact signed-table encoding (measured with the
  table's own encoder, where `&`, `<` and `>` take six bytes), half the
  Worker's 1 MiB table; a list over either limit is refused when added, read
  or backed up. Blocks never stop route publication: if routes grow into the
  blocks' share, the publisher signs routes with as many blocks as fit,
  keeping manual before automatic and newest first, and reports how many it
  left out as `error` in Cloudflare status.
- **Senders both profiles accept, and so can block.** One rule
  (`cfreceiving.ValidSender`, the Worker's `senderOk`, fixture
  `receiving-worker/senders.json`): the null sender, or at most 320 UTF-8
  bytes with a dot-atom local part of ASCII atext or any non-ASCII character
  and an ASCII dot-atom domain. Quoted local parts, domain literals and
  internationalized (U-label) sender domains are refused at reception in both
  profiles (Maddy: `550 5.1.7` at RCPT), because no block could match them the
  same way on both sides. Every accepted sender can be blocked by address, and
  its domain by domain (underscores and other atext included; internationalized
  domains as A-labels).
- **Values and matching.** Both matchers lowercase A-Z only and compare
  non-ASCII exactly (`receiving-worker/blocks.json` is the fixture Go and the
  Worker test against): Unicode case mapping differs between Go and
  JavaScript (U+0130 `İ`) and folds some non-ASCII into ASCII (the Kelvin sign
  to `k`), so `Ü@x.example` and `ü@x.example` are different blocks. An address
  block matches the whole sender address; a domain block matches the sender's
  domain exactly, not its subdomains (block `sub.example` separately). The
  null sender (bounces and other notifications) has no address or domain and
  is never blocked, so delivery reports for mail your users sent still arrive.
- **Your own domains.** The deployment's configured mail domains, and any
  address on them, cannot be blocked (409): a typo there would bounce your own
  users' mail. Adding or re-adding a mail domain is refused (409, "unblock it
  first") while a block matches it or an address on it. Blocking the envelope
  domain of a relay provider your users send through also refuses your own
  users' mail that comes back through it (forwards, list copies); block the
  abusive address instead.
- **IDs.** A block's `id` is an unsalted, truncated SHA-256 of `kind:value`:
  it keeps addresses out of URLs and logs, but anyone holding an ID can confirm
  a guessed address. Treat IDs as sensitive as the list.
- **Enforcement.** Maddy: `receiving bind` exits 6 and the generated
  configuration answers `550 5.7.1 Sender blocked` at RCPT, before DNS proof
  or any binding. An unreadable list makes `receiving bind` exit 8, answered
  `451 4.3.0 Sender blocks unreadable` for every RCPT until it is repaired or
  restored from backup: senders retry, nothing is lost, and no unchecked mail
  is bound. This differs from Cloudflare on purpose: a temporary refusal there
  would not reach the sender (the Worker already accepted the mail), while a
  table that stops publishing ages out and refuses everything permanently, so
  Cloudflare keeps the last published blocks instead. Maddy has no published
  copy to fall back to, and the list is written only by rename, so an
  unreadable one means damaged storage worth stopping for; the refusal is in
  the receiver log (`receiver.log`), since there is no Maddy status screen. Cloudflare: the publisher signs the blocks in force into the
  routing table's `blockedSenders`; a change to the list republishes within
  one loop tick (5 seconds), and the Worker rejects with "Sender blocked". An
  unreadable list never stops route publishing (the Worker refuses all mail
  once its table is 14 days old): the table is still published, hourly and on
  every route change, with the blocks of the last installed revision
  (`cloudflare.db` records each revision's blocks), or none if nothing was
  published before. Status shows `error` with that reason and the daemon logs
  it until the list is repaired or restored. Mail already accepted or
  waiting in R2 is unaffected: blocks never delete mail retroactively.

API (admin only; POST and DELETE need CSRF and the account credential, or
KySignOn step-up, in the JSON body as for quarantine):

- `GET /api/admin/receiving/blocks` returns `{blocks:[{id,kind,value,until,source,level,createdAt,actor,reason}],evidence:{damaged,resetAt,domainBlocksFrom,goodFull,automaticFull}}`
  for blocks in force (empty before `receiving init`).
- `POST /api/admin/receiving/blocks` with `{kind, value, until?, reason?}`
  returns `{block}`; adding an existing kind/value replaces it, and automatic
  blocks give way when the list is full. 400 invalid value, kind, reason or
  past `until`; 409 own domain, list full of manual blocks or receiving not
  initialized; 503 storage.
- `DELETE /api/admin/receiving/blocks/{id}` with the listed 16-hex `id`
  returns `{id,result:"unblocked"}`; 400 malformed ID, 404 when no such block
  is in force. The URL carries the ID, never the address, so reverse-proxy
  access logs record no blocked senders. Removing a block (either source)
  also suppresses automatic re-blocking of that exact address or domain for
  30 days; if that cannot be recorded the block is still removed and the
  answer carries a `warning`.

CLI, as the runtime user that owns `STATE_DIR` (it refuses any other), with the
value typed again after `--confirm`:

```sh
docker compose exec --user kypost kypost-server kypost-server receiving blocks list
docker compose exec --user kypost kypost-server kypost-server receiving blocks add address|domain <value> [--until <RFC3339>] [--reason spam|phishing|abuse|other] --confirm <value>
docker compose exec --user kypost kypost-server kypost-server receiving blocks remove address|domain <value> --confirm <value>
```

Both audit `block_sender`/`unblock_sender` (the CLI's `list` as `list_sender_blocks`) with actor, kind, result and the
block `id` whenever the value is valid (refusals included; empty otherwise),
never the address or domain (API to `api.err.log`, CLI to the terminal). The value is attacker-chosen: render it as plain text.

#### Automatic sender blocks

Maddy profile with `KYPOST_RECEIVING_RSPAMD=true` only. The Cloudflare
profiles never feed evidence (the Worker has no SMTP peer, so there is no SPF),
but automatic blocks in the shared list are published like manual ones.

- **Evidence.** A reject verdict counts against an address only when all
  hold: the envelope sender equals the single address of the message's single
  `From` header (A-Z lowercased on both sides, non-ASCII exact); the scanner's
  `R_SPF_ALLOW` is present (SPF pass for the envelope domain, evaluated from
  the actual Maddy peer IP); and an `R_DKIM_ALLOW` option `d:s=selector` has
  `d` exactly equal to that address's domain (DMARC strict alignment: a parent
  or subdomain signature does not count). Nothing else is trusted:
  `Authentication-Results`, `DKIM_TRACE`, other headers and symbols are
  ignored. Tagged, deferred and accepted mail never counts; the null sender,
  the deployment's own domains (and addresses on them) never count.
- **Address blocks.** 5 counted verdicts within 1 hour block the address
  (`source: automatic`) for 1 hour, then 24 hours, then 7 days on each repeat
  (level 1-3, capped). The level climbs only when a block was actually made.
  30 days with no counted verdict resets it. Blocks expire at `until`.
- **Domain blocks.** When 5 distinct addresses on one domain have been
  automatically blocked within 24 hours (counting only blocks since that
  domain's last automatic block), the domain is blocked with the same
  escalation, but only if no *authenticated* mail (same identity rule) was
  accepted from it. Accepted authenticated domains are recorded and never
  evicted, so a flood of authenticated throwaway domains cannot unprotect a
  real one: at most 50,000 in all and 50 per crude parent (the last two
  labels, so all of `co.uk` shares one parent: past 50 there, further domains
  stay unprotected). A full record stops recording (`goodFull` in status);
  domain blocks continue for unrecorded domains only. Domain
  blocks start only 30 days after the first authenticated acceptance was
  recorded, so a new or freshly restored-from-nothing deployment cannot
  mistake its short history for "never". A restore keeps that start time.
  Automatic domain blocks are listed with `source: automatic`, `kind: domain`.
- **Administrators win.** An automatic block never replaces or extends a
  manual block that covers the address or its domain. Automatic blocks may
  use at most half the list (2500 entries and half its wire budget). A manual
  block that needs room evicts the soonest-expiring automatic block
  (unaudited; the list shows what is in force), so it never fails because of
  automatic ones. An automatic block never evicts anything: past the share it
  is refused, without escalation, logged (`receiving sender evidence`, result
  `automatic-full`) and reported as `automaticFull` in status, so an attacker
  cannot earn blocks on throwaway identities to free an abuser early. Add a
  manual domain block to cover a flood. Removing any block suppresses
  automatic blocks of that exact address or domain for 30 days; by then its
  escalation has reset. The removal never depends on that record: if it
  cannot be written, the block is still removed, the API answers with a
  `warning` and the CLI prints one.
- **State.** `STATE_DIR/receiving/sender-evidence.json` (0600, own lock,
  rename publish, never creates the receiving directory). Every key is a block
  ID and every value a count or time: per address or domain the counted verdict
  times inside the hour, level and last evidence/block times; accepted domains
  with last-seen time and the warm-up start; unblock suppressions. No addresses
  and no content. At most 4096 records of each kind (one-off identities are
  evicted before any with a level, so a flood cannot reset a known abuser),
  which keeps the file under 6 MiB at worst (8 MiB bound). A malformed or
  oversized file is renamed to `sender-evidence.damaged.json` on the next write
  and counting restarts (logged; `resetAt` in status). Backups seal a valid
  file and skip a bad or damaged one with a warning; it is heuristic state.
- **Status.** `GET /api/admin/receiving/blocks` and `receiving blocks list`
  include `evidence: {damaged, resetAt, domainBlocksFrom, goodFull,
  automaticFull}` (`domainBlocksFrom` is null until an authenticated
  acceptance has been recorded).
- **SMTP path.** Evidence work runs after the verdict or commit with its own
  2-second deadline; on lock contention it is dropped and logged, never
  holding up or changing the SMTP reply.
- **Audit.** Each automatic block logs `receiving sender block change`, actor
  `automatic`, action `block_sender`, kind, result `blocked`, block `id`,
  `block_level` and `until_ms`; dropped or failed evidence logs `receiving
  sender evidence` with the delivery ID. Never addresses or content.
- **Residual risks.** The identity rule trusts the sending provider:
  - A provider that lets one account send with another account's envelope
    and `From` under its own DKIM and SPF can get that other address blocked
    here (for up to 7 days, until an administrator unblocks it).
  - DKIM replay: anyone holding a message DKIM-signed by a victim's domain
    can resend it. If they also send from an IP the domain's SPF authorizes
    (a shared ESP or provider relay that does not bind the envelope to the
    account) with the victim as envelope sender, and the replayed message
    still scores a reject, it counts against the victim.
  - Either way enough addresses on one domain could block a provider your
    users have not yet received authenticated mail from, after the warm-up.
    Unblock it; the 30-day suppression then stands.
  - Saturation is chosen over eviction. Every bound refuses new entries
    rather than evicting existing ones, because eviction would let attacker
    entries free abusers or unprotect real domains. An attacker who earns
    2500 automatic blocks (at least 12,500 reject-scored authenticated
    messages) stops further automatic blocks, and one who records 50,000
    authenticated domains across at least 1000 parents stops further
    protection. Both show in status (`automaticFull`, `goodFull`); the
    remedy is a manual block.
  The [Rspamd sidecar](RECEIVING_SETUP.md#optional-rspamd-sidecar) is
  required; without it there are only manual blocks.

New route writes, RCPT bindings and MIME acceptance also check physical storage
inside the immediate SQLite writer transaction. The admission budget is derived
from the durable limits: `max(32 MiB, 4 × payload bytes + 32 KiB × records)`
(about 2.3 GiB for this profile), counting `ingress.db`, WAL and shared-memory
file lengths. Reserve 32 actual SQLite pages plus twice the incoming payload
for the next write, and keep at least 16 MiB plus that allowance available to
the process on the filesystem, and that allowance on top of the
[drive reserve](#mailbox-quotas). Failure to measure storage refuses new growth;
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
GOTOOLCHAIN=go1.26.6 go test -race ./internal/config ./internal/mailbox ./internal/ingress ./internal/sso ./internal/app ./internal/api -run 'TestMailboxQuotaBytes|TestConvergeLimits|TestQuotaRefuses|TestNativeOutboxSnapshotAcceptsUsageAboveLoweredQuota|TestHoldingLimitsRaiseOnly|TestDriveReserve|TestMigrateNativeLimits|TestMigrateNativeCommandAppliesMailboxQuota|QuotaAndDriveReserve|^TestNativeMailboxesAdminAPIAndSelection$|^TestMailImport$' -count=1 -timeout=10m
```

The quota checks raise a primary and an extra mailbox with a crash after the
ledger write, a crash between mailboxes and a crash between `mailbox.db` and
`native-mailbox.json`, re-target after a crash, a no-op second run, a restored
copy with old limits, the command reading `KYPOST_MAILBOX_QUOTA_BYTES`, and a
lowered quota that keeps mail readable and deletable while refusing new mail;
the buffer raise and lowering refusal; RCPT/DATA refusals (exit 9 and 10),
hosted pickup leaving mail in R2; the drive reserve with an injected `statfs`
in both receiving paths and import; and the usage API and storage summary.

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
