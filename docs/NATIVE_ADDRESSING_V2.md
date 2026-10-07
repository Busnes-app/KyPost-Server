# Native addressing model v2: domains, mailboxes, addresses

Status: spec, 2026-10-06. Phase 1a is implemented: the version-2 storage formats,
`kypost-server migrate-native` at startup, the tombstone, fail-closed rollback and
v1/v2 backup validation, still with one domain and one mailbox per user and routing
keyed to the directory revision; until phase 3 each saved primary address generation
is `max(stored, directory revision)`. Phase 1b is implemented: administrator
subjects get mailbox-less accounts and are refused by native admission unless
`legacyMixedUse`, which only demotion clears; promotion is enforced by admission
without a generation bump until phase 3. Phase 2a is implemented: the backend
for several verified domains (domain-set admin API, primary address on any
configured domain, per-domain fences, retirement, relay domain set, Maddy
destinations); the admin UI (phase 2b) and phases 3–4 are not implemented.
Prerequisite of
[continuous Cloudflare receiving](CLOUDFLARE_CONTINUOUS_RECEIVING.md); applies to
both native receiving profiles. External IMAP accounts are unaffected.

## Requirements

- Several verified mail domains per deployment, one KyIdentity issuer.
- Several native mailboxes per KyIdentity user; each mailbox owns its addresses,
  which may sit on any verified domain.
- KyIdentity remains the source of who a person is, whether they are active, and
  their primary mailbox and its primary address. **KyPost administrators** add
  further mailboxes, addresses and aliases (decided 2026-10-06). Users cannot create
  addresses or aliases.
- Mailboxes and aliases belong only to everyday (non-administrator) identities, per
  the suite administrator-separation rule.
- Every address has a per-address generation, bumped only on reassignment, disable,
  re-enable or release, so ordinary directory edits never fence mail waiting at a
  receiving gateway. The outbox stays fenced on the directory revision as today and,
  additionally, on the `From` address's generation (see Sending).
- Existing v1 deployments migrate in place with no data moved; v1 backups stay
  restorable; existing client wire contracts stay valid (all API changes additive).

Out of scope: shared mailboxes, user self-service aliases, per-mailbox PGP keys,
moving a mailbox to a different subject, changing a KyIdentity primary address
(still refused), WKD publication of native addresses, and the migration of existing
mixed-use administrator mailboxes (tracked separately; see Administrator
separation).

## Data model

### Domain set: `$CONFIG/native-domains.json` (version 1)

`{version, issuer, founding, domains: {"<domain>": {token, expiresAt, established,
verifiedUntil}}, retired: ["<domain>"]}`. `founding` is the first configured
domain still in service, which the single-domain admin routes serve (absent in
files that hold one domain); retiring it hands those routes to the smallest
remaining domain. One issuer for the whole set; each domain keeps its own
`_kypost-mail.<domain>` proof and re-verification as today. One lock,
`native-domains.json.lock`, takes the position of `native-domain.json.lock` in every
lock order. Fences compare the proofs of the domains an operation touches, so
re-verifying one domain does not fence work on another; concurrent re-verification
of the same domain still fences, as today. A delivery to recipients on several
domains is all-or-nothing, so one lapsed domain delays the others in that delivery.

Removing a domain **retires** it: refused while any address on it is `active` or
a `queued`/`retryable` outbox job sends from it; otherwise the domain moves to
`retired` (no proof, no routing, no sending) and its address records stay, with
their history, so generations are never reused and old bindings still validate.
A retired domain is never re-added.
Retirement is never automatic: a lapsed proof suspends reception and sending on that
domain; mail stays where it is.

### Mailbox and address ledger: `$CONFIG/native-provisioning.json` (version 2)

```
{version: 2,
 accounts:  {"<issuer>\x00<subject>": {revision, digest, desiredActive, status,
             failure, primaryMailbox, legacyMixedUse}},
 mailboxes: {"<mailboxID>": {owner: {issuer, subject}, kind: "primary"|"extra",
             state: "active"|"disabled", stateRoot, limits, source}},
 addresses: {"<address>": {mailbox, kind: "primary"|"alias", state:
             "active"|"disabled"|"reserved", generation,
             history: [{mailbox, generation}]}}}
```

- Writes stay under the directory (lifecycle) lock; one file keeps mailbox and
  address changes atomic together.
- **Primary mailbox ID equals the owning user's ID** for every primary mailbox,
  legacy or new. Recovery barriers, storage ownership and recovery inputs depend on
  it. Extra mailboxes get random IDs disjoint from user IDs.
- The primary mailbox and its primary address come from KyIdentity as today
  (`nativePrimary`, now accepting any verified domain). Extra mailboxes and aliases
  are created only by administrator actions.
- Address states: `active` delivers and may be used as `From`; `disabled` follows
  the owner's KyIdentity deactivation or an administrator disabling the mailbox;
  `reserved` is a released address held until an administrator deliberately
  reassigns it.
- **Generations are monotonic and never reused.** Reassignment, disable, re-enable
  and release each set `generation` to the previous value plus one. Address records
  are never deleted; `history` keeps every `(mailbox, generation)` the address has
  had. A re-created address therefore cannot match mail frozen against an older
  generation.
- Address uniqueness is global across domains; addresses are lowercased dot-atom.
- Releasing or disabling an address writes its ingress route inactive in the same
  action.

### Mailbox storage

- Primary mailboxes stay at `$STATE/users/<userID>/` (no directory moves). Their
  `state.db` keeps every per-user table: devices, pairing, subscribers,
  notifications, pull notifications, sorter data.
- Extra mailboxes live under a separate root, `$STATE/mailboxes/<mailboxID>/`, with
  their own `native-mailbox.json`, `mailbox/mailbox.db` and a mail-only `state.db`
  (processed, decisions, cache). Their `state.db` holds no device, pairing,
  subscriber or notification rows; the restore validator refuses one that does.
  Code that enumerates `$STATE/users/*` as users is unaffected.
- Device, pairing, subscriber, notification and sorter writes always go to the
  primary `state.db`, opened with the primary mailbox's source.
- Per-user configuration under `config/users/<userID>/` (CardDAV, settings) stays
  keyed by user.

## Behaviour

**Provisioning.** The KyIdentity webhook and worker reconcile the primary mailbox as
today. Deactivation and reactivation of a subject disable or re-enable all of its
mailboxes and addresses with a generation bump **inside the `ApplyDirectory`
transition** (under the directory lock, before the lifecycle write), not in the
asynchronous worker, so a deactivate→reactivate between worker runs still fences
mail accepted while disabled. The rule is level-triggered: on every applied
revision, any ledger address whose state differs from the desired state is set and
bumped, so a replay or a partially failed earlier apply converges. Desired
`active` = subject active ∧ (no administrator role ∨ `legacyMixedUse`) ∧ mailbox not
administrator-disabled ∧ address not `reserved` ∧ domain not retired; otherwise
`disabled` (a `reserved` address stays `reserved`); the worker also
reconciles divergence. A failed ledger or ingress-route write fails the webhook,
and KyIdentity retries it; with no receiving store the route write is a no-op. Administrator actions, each step-up confirmed and
audited:

- add or remove a verified domain;
- create an extra mailbox for a user, with its primary address;
- add an alias to a mailbox; release an alias (→ `reserved`);
- reassign a reserved address to another mailbox;
- disable or re-enable an extra mailbox.

**Administrator separation.** Extra mailboxes and aliases may be owned only by
subjects without an application administrator role (the role carried in the
KyIdentity directory resource, as admission checks today). Administrator subjects
still get a KyPost account, created as a non-native, mailbox-less account through
`provisionDirectoryUser` even in native mode, so they can administer KyPost; no
primary mailbox is provisioned for them and native admission refuses them. Demotion
does not convert that account to native: an everyday identity is a separate
KyIdentity subject. A directory
transition that grants an administrator role disables all of that subject's
mailboxes and addresses (generation bump) in the same transition. Subjects that
already hold a native mailbox and an administrator role at migration are flagged
`legacyMixedUse`: set only by migration, cleared on demotion, and exempt from the
admission refusal. They keep working unchanged until the separately specified
mixed-use migration moves their content to an everyday identity, and cannot gain
extra mailboxes or aliases meanwhile. Exception owner: the deployment owner; expiry:
when the mixed-use migration ships, which removes the flag. In native mode
administrators also cannot save or test personal IMAP settings; IMAP
configuration stored earlier is neither deleted nor blocked, which is left to that
migration.

**Receiving.** Routes are per address with `generation` = the address generation.
Bind resolves the address to its mailbox; authority is checked per mailbox ID
(`WithNativeMailAccess` takes mailbox IDs, cap 100, matching Maddy's 100
recipients). Deliveries to several addresses of one mailbox produce one copy
(existing grouping). Only `active` addresses on verified domains are routable.
Maddy's configuration is generated, not live: adding or removing a domain
regenerates it, listing every configured domain, and restarts the receiver; each
bind still refuses an address whose domain proof has lapsed, so one lapsed domain
never blocks adding another.

**Sending.** `From` may be any `active` address of the sending mailbox on a domain in
the relay's domain set. `native-relay.json` version 2 holds a domain set; every
equality check between relay domain and proof domain becomes set membership:
`backup/native.go` relay check, `mailbox/outbox.go` job `From` domain,
`sso/native_outbound.go` relay/proof check and the `mailmsg` `Deliver` domain check.
Adding a domain to the relay set does not change the relay `Generation`. Removing
one is refused while a `queued` or `retryable` job sends from it (`submitting` and
`uncertain` jobs are never reclaimed and do not block); the domain moves to the
relay's `retiredDomains`, and backup validation checks historical jobs against
`domains ∪ retiredDomains`. Unowned or inactive `From` stays 403. Administrator actions (release, reassign,
mailbox disable) do not change the directory revision, so each `OutboundJob` also
records the `From` address's generation; on queue and on every claim or retry the
address must still be `active`, owned by the sending mailbox and at that generation,
otherwise the job ends `ErrNativeOutboundStale` and is never submitted.

**Client access.** Existing clients keep working unchanged: an account session sees
the user's primary mailbox. Additive API: `GET /api/mailboxes` lists the caller's
mailboxes and addresses; mail endpoints may select a mailbox with an
`X-KyPost-Mailbox` header, defaulting to the primary. Authorization contract:

- On every request, resolve the header through the ledger and require the mailbox
  owner to equal the caller's `(issuer, SSOSub)`, the mailbox `state` to be `active`,
  and admission under the directory fence. A disabled mailbox is not readable,
  searchable or writable; its mail is retained and access returns when it is
  re-enabled. The header value is never used as a path before that lookup.
- Unknown, foreign and disabled mailbox IDs return the same 404, and
  `GET /api/mailboxes` omits disabled mailboxes.
- Every mail cache (`userStores`, `userMailCache`, `userMail`, the poller's clients)
  is keyed by mailbox ID.
- Per-user endpoints (devices, pairing, CardDAV, PGP keys, settings) ignore the
  header.
- Push and pull notifications carry the mailbox ID.

Android adopts selection first, per the platform baseline; other clients keep the
primary-only view until their own pass.

**PGP.** One key per user, as today (`users.User` key material, outbox fences on
`PGPRevision`/`PGPFingerprint`/`MaterialGeneration`). Suggested key User IDs list the
user's active addresses. Adding an address to a published key publicly links that
address to the user's others; the UI says so before adding it.

## Migration (v1 → v2)

Runs from `entrypoint.sh` after the data-volume `chown` and before supervisord starts
the API, daemon or receiver, as the runtime user (`setpriv --reuid=kypost`, so every
lock and ledger file it creates is owned by that user), under domain → directory →
users locks. On failure it logs a remediation and the container keeps booting with
native mail refused; it never exits, so external IMAP users stay unaffected.

1. **Preserve v1 sources first.** Copy `native-domain.json`, `native-provisioning.json`
   and `native-relay.json` (if present) to `*.v1-migrated` before overwriting
   anything. Re-copy each source on every run while it still holds v1 data; once a
   source has been overwritten (or tombstoned), keep its existing copy. Every later step derives only from those copies and overwrites its
   output, so a crash, or a v1 rollback that ran in between, converges on re-run.
2. Write `native-domains.json` from the domain copy.
3. Write `native-provisioning.json` v2 from the ledger copy. Each v1 assignment
   becomes an account, a `primary` mailbox with `mailboxID = Owner.Mailbox` (equal to
   the user ID), and a `primary` address whose generation is **seeded from the
   subject's lifecycle directory revision `d.Revision`**, which is at least every
   route and binding generation v1 wrote. Pending bindings at an older revision keep
   failing the generation match and quarantine exactly as in v1, and the ingress
   store's monotonic-generation rule holds with no route rewrite. `history` starts
   as `[{mailbox, generation: 1}]` so v1 historical bindings validate. Subjects with
   an administrator role get `legacyMixedUse`. A deployment with a configured domain
   but no v1 ledger gets an empty v2 ledger with `NativeProvisioningInitialized` set.
4. Write `native-relay.json` version 2 from the relay copy with a one-domain set,
   keeping `Generation` byte for byte, so queued and retrying outbox jobs stay valid.
5. Last, replace `native-domain.json` with a tombstone `{"migratedTo":
   "native-domains.json"}`.

Fresh installs and deployments that never configured a native domain skip migration
and start in v2 format. Whenever v2 creates `native-domains.json` it also writes the
tombstone and an empty v2 ledger with `NativeProvisioningInitialized` set, so a v1
binary can never configure a domain behind it and native mail can start at once. v2 native code refuses to run
when a v1 domain file holds real data without a tombstone, or when the tombstone is
present but `native-domains.json` or the v2 ledger is missing. Rollback to a v1
binary fails closed: it refuses ledger and relay version 2, and its domain `Read`
rejects the tombstone (empty token), so `Configure`, `Verify`, receiving and
admission all refuse instead of accepting a new domain. Recovery is restoring the
pre-migration backup.

Migration grants no authority and runs **while a restore hold is in place**: it is
an offline format change and keeps the hold. The recovery authority digest hashes
the single-mailbox view of the ledger, which since phase 1b includes
`legacyMixedUse`: an outstanding recovery challenge or receipt stays valid unless
migration flagged an administrator subject, which needs a new challenge. A phase
that gives further fields (generations, extra mailboxes) authority must add them
to that digest.

## Backup and restore

- New and changed files are collected; both native payload detection and the
  `validateNativePayload` allowlist add `native-domains.json`.
- The v2 snapshot validator accepts v1 snapshots (single domain, primary mailbox ID
  equal to user ID), which are migrated after restore by the steps above while the
  restore hold remains.
- Ownership: every mailbox belongs to a native user's subject; every primary
  mailbox ID equals its user's ID; extra mailboxes are under `$STATE/mailboxes/`
  with mail-only `state.db`; orphans, manifests and `mailbox.db` placement are
  checked per mailbox; every ledger address is on a domain in the set or retired.
- Receiving: current routes are validated against the current ledger; historical
  bindings are validated against the address `history`: a binding to mailbox `m`
  at generation `g` is valid only if some history entry `i` has mailbox `m` and
  `history[i].generation ≤ g < history[i+1].generation`, or `i` is the last entry and
  `history[i].generation ≤ g ≤ generation`.
- Restore fencing rotates references per mailbox and revokes devices and CardDAV
  per user.

## Touch points

`sso` (domain store, provisioning ledger, `ApplyDirectory` bump, allocate, admission,
storage ownership, outbound including the `From` generation fence, restore, recovery and repair barriers), `users` (native
fields unchanged), `api` (domain and relay handlers, admin mailbox/address handlers,
`GET /api/mailboxes`, mailbox selection and cache keys in userscope, send `From`,
PGP suggestions, notifications), `app` (receiving bind/import, Maddy configuration
generator, Cloudflare route), `mailbox` (outbox `From` domain and `From` generation in `OutboundJob`; `PrepareAccount` and
`ValidatePreparedAccount` take the storage root instead of hard-coding
`stateRoot/users`), `backup` reference rotation by mailbox path, `mailmsg` (relay
domain set), `processor` (runs per mailbox), `backup` (collection, both allowlists,
validation, fencing), `entrypoint.sh` (migration), `frontend` admin (domains list,
per-user mailboxes and aliases). Docs: `NATIVE_PROVISIONING.md`, `NATIVE_OUTBOX.md`,
`DOMAIN_RELAY.md`, `E2E_PGP.md`, `PLATFORM_BASELINE.md` (additive mailbox selection),
`RESTORE.md`.

## Delivery phases

Each phase is its own PR with its own tests; behaviour stays correct between them.

1. **Ledger and domain-set storage with migration**, still one domain and one
   mailbox: migration, tombstone, fail-closed rollback, v1 backup restore under
   hold, administrator-separation rule for new provisioning with `legacyMixedUse`;
   existing tests keep their meaning.
2. **Several domains:** admin domain-set API and UI, primary address on any verified
   domain, relay domain set and the four equality checks, Maddy regeneration.
3. **Aliases and per-address generation:** admin alias actions, the
   `ApplyDirectory` bump, receiving by address generation, `From` any owned active
   address, history-based restore validation.
4. **Extra mailboxes:** admin mailbox actions, `$STATE/mailboxes/` storage,
   mailbox-keyed authority and caches, processor per mailbox, `GET /api/mailboxes`
   and `X-KyPost-Mailbox` with the authorization contract.

## Qualification

- Migration: v1 fixtures, including staged and pending ingress deliveries (some
  bound at an older revision, which still quarantine) and queued outbox jobs,
  migrate and behave identically (relay generation unchanged); runs as the runtime
  user and leaves no root-owned files; a failure keeps the container up with native
  mail refused; fresh installs need no migration; killed at each step and resumed; v1 rollback mid-migration
  then re-run converges; runs under a restore hold; v1 binary refuses migrated state
  including domain re-`Configure`.
- Restore: v1 and v2 snapshots validate; orphan, foreign-route, wrong-owner and
  device-rows-in-extra-mailbox snapshots refuse; a snapshot taken after an alias
  release and reassignment validates.
- Domains: per-domain proof lapse suspends only that domain; retirement refused
  while an `active` address or a `queued`/`retryable` job uses it.
- Addresses: alias delivery lands in the owning mailbox; release then reassign never
  delivers old-generation mail to the new mailbox; a removed and re-created address
  never matches older frozen mail; deactivate→reactivate between worker runs fences
  mail accepted meanwhile; directory edits that touch no address leave generations
  unchanged.
- Administrator separation: a new administrator subject gets a mailbox-less KyPost
  account and can administer; it gets no mailbox or alias; promotion disables
  existing mailboxes; `legacyMixedUse` subjects keep access.
- Directory replay: a deactivate whose earlier apply failed part-way converges on
  replay; ledger state always matches the latest applied revision.
- Domains: retiring a domain keeps its address records and lets old snapshots and
  bindings validate; historical outbox jobs on a retired relay domain validate.
- Mailboxes: two mailboxes of one user stay isolated (storage, search, Sent, caches);
  old clients see only the primary; a foreign, unknown or disabled
  `X-KyPost-Mailbox` returns 404 without touching storage, and disabled mailboxes are
  omitted from `GET /api/mailboxes`; per-user endpoints ignore the header.
- Outbox: a job queued from an alias that becomes retryable, then has its alias
  released (and, separately, reassigned to another mailbox), ends stale on the next
  worker run and is never delivered.

## Decisions (2026-10-06)

- Deactivating a KyIdentity subject disables all of its mailboxes, including
  administrator-created extra mailboxes.
- A disabled mailbox is inaccessible (same 404 as unknown), with mail retained until
  re-enabled, matching the rule that disabled users lose access immediately.
- A released address has no automatic reservation period; reassignment is always an
  explicit administrator action.
