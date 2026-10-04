# Native domain proof and account allocation

Production directory webhooks retain verified desired state and enforce account
access. Admin mail-domain setup is available through the API. Native allocation
and reconciliation remain internal: no background worker, native source
selector or receiver is enabled. Existing deployments still use external IMAP.

## Operator domain setup

Pair/configure KyIdentity first. As an admin, read `GET /api/admin/mail-domain`,
then `PUT /api/admin/mail-domain` with `{domain,password}` or
`{domain,authSecret}`. Mutations require session CSRF and the current account
credential; KySignOn sessions use the existing request-bound step-up round trip.
The response includes the exact `recordName` and `recordValue` to publish:
`_kypost-mail.<domain>` TXT `kypost-mail-verify=<random challenge>`.
Then `POST /api/admin/mail-domain/verify` with the same credential fields.
Responses are `Cache-Control: no-store` and always `receivingEnabled:false`.
No frontend wizard is present yet.

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

## Disabled allocation flow

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
missing markers. Live admission needs coherent directory/users fencing, cached
client checks and all sending/pickup paths before native selection is enabled.
The provisioning worker and native mail selectors remain disabled.

## Verification and next gates

```sh
cd backend
GOTOOLCHAIN=go1.26.6 go test -race ./internal/fsutil ./internal/users ./internal/sso ./internal/mailbox ./internal/api -run '^TestNativeAllocation|^TestNativeDomain|^TestNativeMailDomain|^TestNativeAccountIssuer|^TestNativePublication|^TestLockFileContext|^TestPrepareAccount|^TestDirectory' -count=1 -timeout=20m
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
integrate explicitly enabled provisioning/periodic repair and diagnostics ahead
of every ordinary state opener. Qualify storage waits, orphan cleanup, scale,
receiver revocation/import ordering and runtime selectors before transport
activation. Durable scoped client deltas, aliases and relay readiness remain
separate work. This change adds system DNS TXT lookups, no dependency or secret,
and no Android/Linux/iOS client wire change.
