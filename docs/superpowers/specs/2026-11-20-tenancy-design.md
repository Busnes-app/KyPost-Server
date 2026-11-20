# KyPost tenancy: single-tenant and multi-tenant (SaaS), design

Date: 2026-11-20. Status: proposed design, nothing built. Docs only.

KyIdentity's tenancy spec (`KyIdentity-server` `docs/superpowers/specs/2026-11-19-tenancy-design.md`,
cited as **T**) makes every hosted customer, a person or a household, an *organisation* (org) under
one issuer. Tokens carry an opaque `org_id`. This note is KyPost's half: how one KyPost binary runs
as today's self-hosted install or as one shared install that separates mail by org. It builds on
the hosted masked-address note (`docs/superpowers/specs/2026-11-19-masked-addresses-hosted-design.md`,
cited as **M**, #676) and does not contradict it.

Owner decisions (fixed, not reopened):

1. One binary with both modes. Single-tenant (today's self-hosted) is unchanged, byte for byte,
   on upgrade. Multi-tenant is a mode.
2. Orgs under one sign-in: one KyIdentity issuer for every customer, tokens carry `org_id`,
   downstream apps are shared multi-tenant installs. Per-customer realms were rejected.
3. One org per account, with no org switcher.
4. No administrators inside customer orgs. Only our staff are administrators; customers are
   members. Single-tenant installs keep their own administrators.
5. Businesses do not use the shared service. Each gets its own single-tenant install, set up by
   hand (T decision 7).
6. Stripe lives only in a separate billing service, built later. KyPost has no billing code.
7. Hosted masked addresses come from one shared platform relay domain, unique across customers.
   Per-customer custom domains come later. Self-hosted keeps its own domain (M).

Line references are to KyPost `origin/main` at `2c7ecc09`, to KyIdentity `origin/master` at
`2e304a4` for T, and to PR #677 at `746cdfd` where marked **R**. Unless a path says otherwise,
KyPost paths are relative to `backend/internal/`. "Unverified" marks a claim this note did not
check in code or by running it.

## What KyPost has today

KyPost has no org concept. `tenant` and `org_id` appear nowhere in `backend/internal`. Five facts
drive the design:

- **Per-user data is already partitioned by an internal user ID.** `users.User.ID` is a random
  UUID (`users/users.go:47-48,56`). Each user's files live in `<root>/users/<ID>`
  (`config/config.go:396-398`). An administrator-added extra mailbox lives in
  `$STATE/mailboxes/<ID>` (`api/server_userscope.go:442-447`). On PostgreSQL each of those SQLite
  files becomes its own schema, `kypost_f_<sha256(path)[:16]>` (`dbbackend/dbbackend.go:241-247`).
  So most stores need no `org_id`. They inherit the org of the user or mailbox that owns them.
- **Accounts bind to `(issuer, sub)`.** Sign-in (`api/sso_handlers.go:599-600`), the sync webhook
  (`api/sync_handlers.go:133`) and inbound peer calls all resolve through
  `GetBySSOSubIssuer` (`users/users.go:1170-1180`). A `sub` is unique across the install
  (`ErrSSOSubTaken`, `users/users.go:1260`).
- **The org could only arrive in a token or a signed body.** The ID token's identity comes from
  the verified token object (`sso/sso.go:743-747`). `SSOTokenClaims` has no `org_id` field
  (`sso/sso.go:276`), so one sent today is silently ignored. KyPost accepts no KyIdentity access
  token anywhere. The only bearer it parses is its own agent token for MCP (`api/mcp.go:138-143`).
- **The current user is re-read from the store on every request** (`currentUser`,
  `api/server_auth_session.go:1174-1236`). An org attached to the user record therefore reaches
  every handler without trusting the session or the request.
- **Mail infrastructure is install-wide.** That covers the domains (`sso/native_domain.go:68-74`,
  one issuer at `:372`), the relay profile and DKIM keys (`mailmsg/domain_relay.go:28-42`,
  `mailmsg/dkim.go:34-43`), sender blocks (`ingress/blocks.go:22-23`), the receiving buffer and
  quarantine (`ingress/store.go:107-118`) and the address ledger (`sso/authority_v3.go:49-75`).
  Address keys are global (`addresses.address` primary key, `authority_v3.go:65`). M relies on
  that for cross-customer uniqueness.

## Decisions this note makes

| # | Decision | Why |
|---|---|---|
| K1 | `KYPOST_TENANCY=single` (default) or `multi`. A multi install records `tenancy_mode=multi` in the root `state.db` `meta` at first start. A start whose mode differs from the record refuses to start. Single-tenant writes nothing: no record means single. The variable joins the replica configuration fingerprint (R `replica/config.go:35`), which cannot refuse an upgraded single-tenant replica (Replicas). | Install-time, as T D1. Writing nothing keeps upgrades byte-for-byte unchanged. A replica started in the wrong mode must not serve. |
| K2 | **Multi-tenant requires** PostgreSQL (`KYPOST_DATABASE_URL`, `docs/CONFIGURATION.md:124`), native mail (`KYPOST_NATIVE_MAIL=true`), the v3 authority database, a configured KyIdentity issuer, and an **empty install** at first start. Converting a single-tenant install is out of scope, as in T. | T requires Postgres too (T D2). The external-IMAP profile and local accounts have no place in a hosted mail service (K6). Starting empty avoids a migration with no org to assign. |
| K3 | Multi-tenant schema additions are created only by a multi install's first start. A single-tenant install never gets them, so its databases, `users.json` and every file stay byte-for-byte as they are. | This is the zero-change argument. T adds columns on both modes because its SQL is shared. KyPost's org-owned rows (below) are few, and every one of them sits in a store that only a multi install creates. |
| K4 | The org is recorded on two roots only: `users.User.OrgID` (write-once, `omitempty`) and `org_id` on the authority `directory` and `accounts` rows. Every per-user and per-mailbox store is **derived**: it inherits the org of the ID that owns it. Cross-user aggregates in the root `state.db` (`send_ledger`, `agent_tokens`, `scheduled_sends`) carry `org_id` so per-org counts need no join. | Per-user data is already separated by user ID (above). Adding `org_id` to hundreds of per-user schemas would add a column nobody could set wrongly. The risk sits on the hops *between* users, so that is where the checks go (K5). |
| K5 | `AuthContext.OrgID` is set by `currentUser` from the re-read user record, never from the request. Every **cross-user hop** goes through one function, `orgAdmit(ac, target)`: from the caller to another user, mailbox, address, device, delivery or peer subject. On a mismatch it gives the same answer as a missing target. The hops form a named list (Isolation). | One choke point, as KyIdentity's `OrgStore`. KyPost has no single SQL layer to bind, but it does have a single per-request identity. |
| K6 | Off in multi-tenant v1, answering `404`: local password sign-in, local user creation, password reset, SSO link and unlink, `AutoProvision`, the external-IMAP mail source, administrator-assigned IMAP and CardDAV accounts, and HR mailbox reservation (`/api/sync/hr-mailbox`). The first-run bootstrap administrator (`app/bootstrap.go:41-42`) is not created. | Accounts come only from KyIdentity (T decision 5). Staff must never hold a customer's credentials. T switches HR off in multi mode (T "Per-org features"). |
| K7 | **Staff = `kypost.admin` role *and* a token whose `org_id` equals `KYPOST_STAFF_ORG_ID`.** A customer account is never an administrator, whatever roles a token or sync event carries. | `HasAdminRole` (`sso/directory.go:24-31`) trusts the role alone. T's D8 refuses KyIdentity's *own* admin role outside the staff org, but app-role assignment is "per org, run by staff" (T "Per-org features"). So a staff mistake could give a customer `kypost.admin`. KyPost pins the org independently. |
| K8 | Entitlements are pulled from `GET /api/suite/orgs` (T D6) by one elected worker about every 60 s. They are cached in a platform table, `orgs(id, status, revision, enabled, quotas_json, polled_at)`, in the root `state.db`, so every replica reads the same answer. An org missing from the cache is not served. **The staff org (`KYPOST_STAFF_ORG_ID`) is exempt**: it is never gated by the cache, so staff keep the console during a poller or KyIdentity outage. | T's contract point 6. A shared table keeps replicas consistent (R `worker.Run` elects one runner per role). The staff exemption is safe: KyIdentity still decides whether a staff account signs in at all (T `active_principals`). It is unverified whether `/api/suite/orgs` lists the staff org. |
| K9 | Hosted mail domains, the platform primary domain(s) and the relay domain (M D2) are **platform assets**: staff configure, prove, sign and retire them through today's admin routes. No customer configures a domain in v1. | No customer administrators (decision 4). Per-customer custom domains come later (M "Per-customer custom domains"). |
| K10 | Org deletion **scrubs** rather than deletes in the authority database. Account and mailbox rows stay as tombstones with storage removed and no personal fields. Addresses become keyed hashes, as M open question 4 recommends. Everything else the org owns is deleted. | `authority_v3.go:89-99` forbids deletes, so that "never reuse" holds. M D5 needs the tombstone and the privacy rule needs the content gone. One named purge path with its own migration is the only exception to those triggers (open question 9). |

## Store inventory

Classes, as in T's appendix: **O** (org-owned: carries `org_id`), **D** (derived: inherits the org
of the parent named), **P** (platform: install-wide, staff only), **–** (not present in multi mode,
K6). "Multi-only" means the store or column exists only in a multi install (K3).

### SQL databases

| Store | Defined | Class | Reasoning |
|---|---|---|---|
| Root `state.db` `meta` | `state/schema.go:74-290` (SQLite), `state/pg_schema.go:11-69` | P | Settings, health, update dedup. Adds `tenancy_mode` (multi-only). |
| Root `state.db` `agent_tokens` | `state/schema.go:269` | O (multi-only column) | Keyed by `user_id`. `org_id` lets the purge delete an org's tokens in one statement. |
| Root `state.db` `scheduled_sends` | `state/scheduled_sends.go`, `state/schema.go` | O (multi-only column) | Keyed by `user_id` and `mailbox`. A suspended org's due sends are held (open question 6). |
| Root `state.db` `send_ledger` | `state/send_ledger.go:27-38` | O (multi-only column) | Counts the per-user quota today (`api/send_quota.go:25`). Per-org quotas sum it by `org_id`. |
| Root `state.db` `backup_audit` | `state/schema.go` | P | Whole-install backups. |
| New `orgs` cache | (multi-only) | P | K8. The registry every org check reads. |
| New `org_purges` | (multi-only) | P | Purge progress, one row per deleted org, so a purge that died halfway resumes (Lifecycle). |
| Per-user / per-mailbox `state.db` (`processed`, `decisions`, `notifications`, `native_devices`, `pull_notifications`, `deferrals`, `sorter_*`, `snoozes`, `ooo_*`) | `api/server_userscope.go:86-92`; same schema as root | D (user or mailbox directory) | The directory, or on Postgres the `kypost_f_*` schema, is the owner. |
| `mailbox.db` (messages, folders, labels, receipts, usage, `outbox*`, …) | `mailbox/store.go:118-132`, `mailbox/pg_schema.go:17-32` | D (mailbox) | The `identity` row names `(issuer, subject, mailbox)` (`mailbox/store.go:118`). It gains no `org_id`: the mailbox's account carries it. |
| `native-authority.db` `directory`, `directory_primary_emails` | `sso/authority_v3.go:39-48` | O (multi-only column on `directory`) | The KyIdentity directory mirror, keyed `(issuer, subject)`. `org_id` is written from the signed webhook (Identity binding) and never changes. |
| `native-authority.db` `accounts` | `sso/authority_v3.go:49-55` | O (multi-only column) | The ledger root of a person's mailboxes. |
| `native-authority.db` `mailboxes` | `sso/authority_v3.go:56-63` | D (`(issuer, subject)` FK to `accounts`) | |
| `native-authority.db` `addresses`, `address_history`, `routes_pending` | `sso/authority_v3.go:65-76` | D while live (via `mailbox`), P as tombstones | Address keys are global (M D4). After org deletion they become platform tombstones (K10). |
| `native-authority.db` `directory_events`, `logouts`, `sign_ons`, floors, barriers, `lifecycle_records` | `sso/authority_v3.go:77-88` | P | Replay and recovery ledgers keyed by event or JTI. Restore authority is whole-install. |
| `ingress.db` `routes`, `bindings` | `ingress/store.go:108,110` | D (`mailbox`) | Recipient bindings name the mailbox. |
| `ingress.db` `deliveries`, `archived`, `limits` | `ingress/store.go:107,109,117` | P, with org found through `bindings` | The receiving buffer and quarantine. A delivery can bind several recipients in several orgs, so it has no single org. Staff-only (Admin surface). |
| `cloudflare.db` | `cfreceiving/state.go:30`, `cfreceiving/pg_schema.go:11-15` | P | One Worker and one credential set per install (`app/receiving_cfcontinuous.go:73`). |
| PostgreSQL catalog `kypost_catalog.stores` | `dbbackend/dbbackend.go:205-207` | P | Maps paths to schemas. The purge drops an org's per-user schemas through it. |
| R: `sessions`, `session_revocations`, `auth_challenges`, `import_jobs` | R `state/schema.go:302-325` | D (`user_id`) | Keyed by user ID. Suspension revokes by the org's user IDs (Lifecycle). |
| R: `security_windows`, `spent_challenges`, `replay_ids`, `replica_instances` | R `state/schema.go:296-300,327` | P | Keyed by hashed username, IP, nonce or instance. |

### Files on the volumes

| Store | Defined | Class | Reasoning |
|---|---|---|---|
| `users.json` | `app/bootstrap.go:41`, `users/users.go:49-97,507` | O (`OrgID`, multi-only field) | One file holding every account record (identity, password hash, sealed TOTP, PGP keys, send quota). `OrgID` is `omitempty`, so single-tenant bytes do not change. |
| `admin.env`, `first-run-password.txt` | `app/bootstrap.go:42`, `users/users.go:858,870` | – | Not created in multi mode (K6). |
| Root `config.yaml`, `mail-defaults.json`, `sso.json` | `app/app.go:150`, `api/server_mail_defaults.go:26`, `sso/sso.go:89` | P | Install settings. One issuer for every org (decision 2). |
| `sso-lifecycle.json`, `native-provisioning.json` | `sso/lifecycle.go:91`, `sso/native_provision.go:73` | P | v2 authority formats, left as stubs once v3 exists (`sso/authority_v3.go:28-30`). K2 requires v3. |
| `native-domains.json` | `sso/native_domain.go:25-27,68-74` | P | Platform domains only (K9). |
| `native-relay.json` (relay credentials and every domain's DKIM private key) | `api/native_relay_handlers.go:47`, `mailmsg/domain_relay.go:28-42` | P | One relay profile for the platform domains. |
| `wkd-domains.json` | `wkdpublish/store.go:74` | P | WKD on platform domains, staff-run. |
| `receiving.conf`, `sender-blocks.json`, `sender-evidence.json`, `quota-<id>` locks | `api/native_recovery_handlers.go:364`, `ingress/blocks.go:23`, `ingress/autoblocks.go:26-31`, `app/receiving.go:37-82` | P (quota locks D, per mailbox) | Sender blocks stay platform-wide (Mail). |
| Per-user `config.yaml` (settings, signatures, masked records) | `config/config.go:199-245,410` | D (user) | M D8 moves masked records out in hosted mode. |
| Per-user `imap-config.json`, `tuning.md` | `config/config.go:402,406` | – / D | No external-IMAP source in multi mode (K6). `tuning.md` is derived. |
| Per-user `carddav-auth.json`, `carddav-client.json` | `api/server_userscope.go:247,254` | D (user) | The user's own CardDAV password and outside contacts server. Administrator assignment is off (K6). |
| Per-user `contacts.json`, `groups.json`, `rules.json`, `send_as_aliases.json`, `pgp-discovery*.json`, `kycalendar-*.json`, `incoming-encryption.json` | `contacts/store.go:64`, `groups/store.go:44`, `rules/store.go:43`, `sendas/store.go:62`, `pgpdiscovery/settings.go:23`, `pgpdiscovery/suppress.go:28`, `kycalendar/invitations.go:32`, `kycalendar/flags.go:35`, `processor/incoming_encryption.go:58` | D (user) | |
| Per-mailbox `mailcache.json`, `native-mailbox.json`, `outbox-idle` | `mailcache/store.go:105`, `mailbox/prepare.go:26`, `mailbox/outbox_idle.go:22` | D (mailbox) | |
| `$STATE/pickup/<id>.json` | `api/server.go:476`, `pgpmail/pickup.go:88-92` | D (owner user) | One install directory, one record per owner. |
| `$STATE/imports/upload-*` | `api/mail_import.go:37,315` | D (user, through the grant) | Temporary. Grants and jobs are in memory (`api/server.go:164-172`) or in R's `auth_challenges` and `import_jobs`. |
| Native restore hold, qualification and release files, `mfa-clear-all.*` | `sso/native_restore.go:21`, `sso/native_restore_qualification.go:24`, `sso/native_release.go:95`, `app/app.go:574,586` | P | Whole-install recovery. |
| Backups, `mail-bulk.json` | `backup/backup.go:92`, `backup/bulk.go:38-43` | P | Whole-install capsules, as T. |

### Keys (`SECRET_DIR`, `config/config.go:44,52`)

All keys are **P**: one per install, used for every org. Each is listed with what it seals:
`native-relay.key` (relay settings and DKIM keys), `imap-config.key` (per-user IMAP and CardDAV
client credentials), `totp-secret.key` (TOTP secrets, recovery-code digests and R's fingerprint
HMAC), `pgp-private-key.key` (users' sealed PGP private keys), `pickup-store.key`, `pairing.key`,
`pow.key`, `relay-identity.key`, `scheduled-send.key`, `push_relay_key` and `apns_relay_key`,
`notifications-vapid-private.pem`, `cloudflare-receiving.json`, `bulk-backup.key` and `.repo`, and
R's `imap-import.key` (`api/server.go:384-426`, `api/scheduled_send.go:161`,
`processor/native_sender.go:211-218`, `config/config.go:778`, `cfreceiving/state.go:22`,
`backup/bulk.go:40-41`). Per-org keys are not proposed. Every key already seals per-user records,
and a per-org key would protect nothing from the operator, who holds all of them (Residual risks).
New in multi mode: a **tombstone key** for K10's address hashes, `address-tombstone.key`. It is
created exclusively, like R's keys, and backed up, because a restore needs it (M open question 4).

## Isolation

### How `org_id` reaches every row

- **Sign-in and device sign-on** read `org_id` from the verified ID token (Identity binding). The
  account's `OrgID` is written once, by native provisioning, from the signed directory event. It
  is never written from a token.
- **`currentUser`** puts `user.OrgID` into `AuthContext.OrgID` on every request
  (`api/server_auth_session.go:1235`). So do the other paths that build an `AuthContext`: paired
  devices (`api/server_userscope.go:604-630`), CardDAV Basic (`api/dav_auth.go:112`) and MCP
  agent tokens (`api/mcp.go:138-143`). Each reads the org from the user record. In multi mode an
  `AuthContext` without an org is refused with the same `401` as an expired session.
- **Signed public links** (`/pickup/{id}`, export and import links) resolve to an owning user.
  For a user whose org is not `active`, they answer as an expired link does.
- **Per-user stores** are opened by `AuthContext.UserID` (`api/server_userscope.go:308-603`). A
  handler cannot reach another user's store without naming that user, and naming another user is
  a cross-user hop.
- **Per-mailbox stores** are admitted by `admitMailbox` from `X-KyPost-Mailbox`
  (`api/native_mailbox_handlers.go:19,31-68`). It already checks that the mailbox belongs to the
  caller. Multi mode adds `orgAdmit` there, so a foreign mailbox ID answers like an unknown one.
- **Root-store rows** (`agent_tokens`, `scheduled_sends`, `send_ledger`) take `org_id` from the
  `AuthContext` that writes them.
- **Background workers** (provisioning, sweeps, outbox recovery, receiving import) work from the
  ledger, where every mailbox resolves to an account with an `org_id`. The purge and the
  entitlement poller are the only workers that act on an org as a whole.

### The cross-user hops (`orgAdmit` call sites)

Each hop is listed in code. A test fails the build when a handler reads another user's store, or
the users store by an ID that did not come from the caller's own `AuthContext`, without going
through `orgAdmit`. In the style of `TestEveryRouteDeclaresItsAuthModel`
(`api/route_auth_test.go:45`), this is a go/ast scan of `api/` for the store accessors
(`storeFor`, `mailFor`, `contactsFor`, `s.users.Get`, `GetBySSOSubIssuer`) called with a
non-`AuthContext` ID.

| Hop | Today | Multi mode |
|---|---|---|
| Caller to an extra mailbox | `admitMailbox` owner check | Plus `orgAdmit` |
| Suite people search, KyMessages address resolution | `dir.People(ctx, u.SSOSub, …)` (`api/suite_people.go:80`), `dir.Resolve` (`api/kymessage_handlers.go:115-118`) | KyIdentity scopes both by the `sub`'s org (T "Suite directory"). KyPost also drops any answer whose `org_id` is not the caller's. |
| Outbound peer calls (KyMessages, KyCalendar, KyDrive) | `{sub, issuer}` in the body (`api/kymessage_handlers.go:219`, `api/flag_tasks.go:94`, `api/kydrive_handlers.go:161-175`) | Adds the caller's `org_id`. A `target_sub` must be in the caller's org, resolved through the suite directory. |
| Inbound peer calls (KyCalendar iTIP and task state, KyPulse alerts) | `GetBySSOSubIssuer` on body fields (`api/kycalendar_handlers.go:122`, `api/flag_tasks.go:235`, `api/kypulse_handlers.go:121`) | The signed body must carry `org_id` equal to the account's. A mismatch answers like an unknown `sub`. T leaves "what each pair sends" unverified, so each peer's own spec must add the field. |
| Address resolution for delivery between hosted users | Ledger lookup by address | Not a hop: mail between orgs is ordinary mail. Delivery goes by address (Mail), not by org. |
| Staff on an org-bound admin route | n/a | The staff-selected org (Admin surface) is the only org taken from a request, and only after K7 has proved the caller is staff. |
| Quarantine release to the current owner | Only for an *unresolved* delivery with no known original owner, to the mailbox a staff member reviewed (`sso/native_quarantine.go:122-138`) | Allowed only when every mailbox in the address's history (`address_history`) belongs to one org. Otherwise `409`. Multi mode refuses cross-org reassignment, so this holds unless the ledger is wrong. |
| Address reassign and release | `api/server.go:676-677`, `refuseMasked` (`api/native_address_handlers.go:84-97`) | Refuses a target mailbox in another org (M "Admin routes"). |

### Proving a cross-org ID answers exactly like a missing one

`TestCrossOrgMatrix` runs on PostgreSQL in the `postgres` CI job, with a test-only constructor
that builds a multi install from fixtures:

- **Fixtures.** Orgs A and B, each with two members, an extra mailbox, a masked address, a paired
  device, an agent token, a scheduled send, contacts, rules and a quarantined delivery. A staff org
  has a staff administrator.
- **Route enumeration.** Routes are parsed from `api/server.go`, as `route_auth_test.go:45` does.
  Every route with a path parameter or an ID-bearing body or header (`{id}`, `{address}`,
  `{gateway}/{id}`, `X-KyPost-Mailbox`, message, device and token IDs) needs a fixture generator.
  A route without one fails the build.
- **Three calls per route,** as A's member: with A's own object (the control, which must succeed),
  with B's object, and with an ID that has never existed.
- **Assertions:**
  - The B call's status, body and headers equal the never-existed call's **byte for byte**, after
    removing `Date` and request IDs.
  - A digest of every B store (users records, per-user files, `kypost_f_*` schemas, ledger rows) is
    unchanged.
  - No audit line names B.
  - Timing is not asserted (Residual risks).
- **Staff variant.** As a staff administrator with org A selected and B's IDs: the same `404`, and
  an audit row naming A only.
- **Non-HTTP inputs:**
  - A signed sync event for a known `sub` with a different `org_id` is refused, with no change.
  - Each inbound peer route given B's `sub` under A's `org_id` answers like an unknown `sub`.
  - SMTP `RCPT` to B's address delivers into B and never A, for each receiving profile available
    (Mail).
- **Every admin route as a customer member,** including the account holder (T D9): the
  non-administrator refusal of today (`api/server_users.go:16`), with nothing changed.

## Identity binding (T contract points 1, 3 and 4)

| Input | Multi-mode rule | Where |
|---|---|---|
| ID token at sign-in | `SSOTokenClaims` gains `OrgID string \`json:"org_id"\``. Like `Sub`, it is copied only after verification. A token without `org_id` is refused with the generic sign-in failure. Single mode ignores the field. | `sso/sso.go:276,707-747`, `api/sso_handlers.go` callback |
| Account lookup | Binds to `(issuer, sub, org_id)`. `GetBySSOSubIssuer` finds the user. A user whose `OrgID` differs from the token's `org_id` gets `ErrNativeAccountConflict`, the error a wrong issuer gets today (`users/users.go:1176`). The session is not created, and the event is logged as a security event, with no org names. | `api/sso_handlers.go:599-600`, `users/users.go:1170-1180` |
| Device-assertion sign-on | The same rule. T adds `org_id` to the device-assertion token. | `api/native_signon.go:26-112` |
| Back-channel logout | `Covers` also requires the token's `org_id` to match the account's. One without `org_id` is refused in multi mode. Refusing loses nothing: suspension also revokes locally (Lifecycle). | `api/sso_logout.go:37-107`, `sso/lifecycle.go:42` |
| Signed sync webhook | `sso.DirectoryUser` gains `OrgID`, covered by `syncauth` (T "Outbound sync"). A missing `org_id` is refused. The first event for a `sub` writes `directory.org_id`. A later event with a different `org_id` is refused, audited and changes nothing. | `api/sync_handlers.go:29-133`, `sso/directory.go:60-74,181` |
| Admin role | K7: `kypost.admin` in the token or event, **and** `org_id == KYPOST_STAFF_ORG_ID`. In multi mode `KYPOST_STAFF_ORG_ID` is required, and startup refuses without it. A customer event naming `kypost.admin` is applied as a member and logged. | `sso/sso.go:334-345`, `sso/directory.go:24-31`, `api/sync_handlers.go:128-131` |
| Session | Unchanged, except that `currentUser` also kills a session whose user's org is not `active` in the `orgs` cache (Lifecycle). | `api/server_auth_session.go:1224` |
| URL, form or header | Never a source of org. The one exception is the staff org selection on admin routes (Admin surface). | — |

The org therefore comes only from a verified token, a signed webhook or a suite answer (T point 4).
`users.User.OrgID` is written only by provisioning from the signed directory event, and only once.
A later write with a different value returns an error from the users store.

Hosted members are provisioned the native-mail way. There is no JIT account at sign-in
(`api/sso_handlers.go:629-631`). An account appears when the signed `user.created` event has
been applied and a mailbox address has been allocated (Mail, open question 1).

## Mail

### Mailbox addresses and domains in hosted mode

- **Domains.** One or more platform primary domains (`<platform-mail>`) and the separate relay
  domain for masked addresses (M D2). All are platform assets (K9), proved with today's
  `_kypost-mail` TXT bound to the one issuer (`sso/native_domain.go:52-63,369-372`).
- **Primary address.** Today it is the KyIdentity resource's single primary email, which must sit
  on a proven domain (`sso/native_provision.go:721-728,798-811`). In hosted mode a member's
  KyIdentity email is their sign-in address (T D5: username = email). It is usually on someone
  else's domain, so it cannot be the mailbox. The hosted mailbox address needs its own source.
  That is open question 1. The recommendation is a reservation sent by KyIdentity at member add,
  modelled on today's `ReserveHRAddress` (`sso/hr_mailbox.go:13-104`), carrying `org_id`.
- **Uniqueness.** One ledger for the shared install, with global address keys
  (`authority_v3.go:65`) and the never-delete rule (`:89-99`). That covers primaries and masked
  addresses across every customer (M D4).
- **Extra mailboxes and aliases.** Today an administrator adds them
  (`api/server.go:674-681`). Hosted mode has no customer administrators. In v1, staff may add
  them on request, on org-bound routes, and the plan's `mailboxes` quota bounds them (Lifecycle).
- **Plus addressing.** Unchanged (`sso/native_address.go:84-97`), on primaries and masked
  addresses (M).

### Inbound routing to the right org

Routing stays by address. An address resolves to a mailbox, the mailbox to an account, and the
account to its `org_id`. No receiving path takes an org from the message.

- **Order of checks** in `bind` (`app/receiving.go:404-460`): sender blocks, domain proof, the
  plus base, the assignment, the active address, then the owner's refusals. Multi mode adds one
  check after the assignment: the owner's org status from the `orgs` cache.
  - `active`: today's path.
  - `suspended`: temporary refusal (`451 4.2.1`), so senders retry (M open question 2).
  - `deletion_scheduled` and later: the same `550 5.1.1` as an unknown address (M open question 2).
  - Not in the cache: temporary refusal.
- **Cloudflare continuous.** `buildRoutes` (`app/receiving_cfcontinuous.go:342-441`) omits the
  addresses of orgs that are not `active`. The Worker's table holds at most **5000 routes**
  (`receiving-worker/continuous.mjs:6`, `cfreceiving/wire.go:211`). A hosted install passes that
  quickly.
- **Maddy.** After R, `startReplica` refuses the Maddy receiver on **any** PostgreSQL install, not
  only on replicas (R `app/replica.go:30-31`, `replica/topology.go:62`).
- **Result: no receiving profile is ready for a hosted install today.** Multi-tenant requires
  PostgreSQL (K2), which rules out Maddy after R, and Cloudflare stops at 5000 addresses. M also
  recommends Maddy for the relay domain, because Cloudflare's table discloses every customer's
  masked addresses to one third party (M open question 5). This is open question 3, and it blocks
  the last phase.
- **Size and quota.** The 25 MiB cap is unchanged (`mailmsg/limits.go:38`). The per-mailbox quota
  is reserved at bind (`app/receiving.go:458-479`). Hosted mode takes each mailbox's limit from
  the org's `mailboxBytes` quota, not from the install-wide `KYPOST_MAILBOX_QUOTA_BYTES`
  (`config/native_mail.go:33-52`). A lower quota refuses only new mail, never locks a mailbox
  (today's rule, root `AGENTS.md` "Mailbox limits").

### Outbound relay and DKIM

- **Relay profile.** One per install (`mailmsg/domain_relay.go:28-42`). In multi mode it serves the
  platform domains, configured by staff on platform routes. `nativeFrom`
  (`api/native_outbound.go:155-174`) already limits From to the sending mailbox's own live
  addresses. Hosted mode adds the org check at outbox admission, so a suspended org sends nothing.
- **DKIM.** Per-domain keys sealed in `native-relay.json` (`mailmsg/dkim.go:34-43`). Rotation is
  manual (`mailmsg/dkim_rotation.go:12-19`). The relay domain's and primary domains' keys sign
  every customer's mail, so a compromise covers all of them. M recommends relay mode, where the
  provider signs, over direct mode.
- **Bounces and complaints.** Handled as M "Bounces" and M open question 8. KyPost ingests no
  provider feedback today (unverified that nothing exists).

### Quarantine

The #314 quarantine (root `AGENTS.md`) lives in the install-wide `ingress.db`
(`ingress/store.go:107-118`). Release and discard are step-up confirmed and audited, and the list
shows envelope metadata only (`sso/native_quarantine.go:63`). Multi-mode changes:

- **Who.** Staff only, on a platform route. No customer administrator exists to review it.
- **Release.** Release goes only to the frozen original mailboxes, as today. Releasing an
  unresolved delivery to the current owner (`sso/native_quarantine.go:122-138`) also requires the
  address never to have belonged to another org (the hop table).
- **Org state.** A delivery whose bound org is no longer `active` is not released. It waits, or is
  discarded with the org's purge.
- **Audit.** Each staff release, discard or list writes an audit line naming the orgs of the
  envelopes it showed or touched (T "Audit").

### Masked addresses

Exactly as M, which this note does not change. M's "What changes in KyPost" items 1–7 land in the
hosted-mail phase. The `orgAdmit` hops and K10's tombstones are the "KyPost tenancy work" M D6
waits for.

### Abuse limits per org

| Limit | Today's key | Hosted |
|---|---|---|
| Sending | Per user, 60 messages an hour and 1000 recipients a day (`api/send_quota.go:23-54`, `config/config.go:568`) | Per user as today, plus an org cap from `quotas_json` (`sendPerHour`, `recipientsPerDay`), summed from `send_ledger.org_id` |
| Masked creation | Per user, 100 live and 20 a day (`api/masked_addresses.go:31-37`) | M D7: `maskedPerUser`, `maskedPerDay`, `maskedPerOrg`, plus M's platform-wide circuit breaker |
| Mailbox storage | Per mailbox, install default (`config/native_mail.go:33-52`) | `mailboxBytes` per mailbox, and `storageBytes` across the org, both from `quotas_json` |
| Extra mailboxes | Administrator's choice | `mailboxes` from `quotas_json` |
| Import and export | 2 at a time per server, 1 per user (`api/mail_import.go:33`, `api/mail_export.go:31`) | Unchanged, plus 1 per org at a time, so one household cannot hold both server slots |
| Inbound connections | Maddy per IP and install-wide (`app/receiving_config.go:184-189`) | Unchanged. Recipients are per address, not per org, and M adds a refused-recipient budget per source. |
| Sender blocks | Install-wide, manual and automatic (`ingress/blocks.go:22-23`, `ingress/autoblocks.go:36-57`) | Platform-wide, staff only. No per-org blocks in v1 (no customer administrators). |

The quota names are proposals for KyIdentity's opaque `quotas_json` (T "Entitlements"). The
billing service sets them. When a quota is missing, KyPost uses today's default.

## Admin surface

In multi mode the KyPost admin UI is a **staff console**. A route is admitted only for K7 staff,
with today's step-up (`withAdminStepUp`, `api/server_users.go:30`; `withActionDigest`,
`api/sso_stepup.go:55`). Every admin route declares one class. A route with no class fails the
build, which extends `TestEveryRouteDeclaresItsAuthModel`.

| Class | Routes (`api/server.go`) | Multi-mode behaviour |
|---|---|---|
| **Platform** | Mail domains and verify (`667-673`), relay and DKIM (`689-694`), receiving status, sender blocks and quarantine (`682-688`), backups (`658,696-702`), native recovery (`662-666`), SSO settings (`712-713`), rate-limit networks and encryption policy (`714-717`), masked domains (`735-736`), instance config and mail defaults (`709,711`), logs (`759-760`), server version (`777`), WKD domains (`957-960`), suite peer status (`659-661`) | Staff, unchanged. These act on platform assets. Masked domains accept only platform domains (M). |
| **Org-bound** | Users list (`761`), update (`763`), deactivate (`765`), reactivate (`766`), clear MFA (`767`), key setup (`768`) and send quota (`769`), mailboxes (`678-681`), addresses (`674-677`) | Staff select one org, sent as `X-Ky-Org`, as T "Org selection". The selected org is checked against the `orgs` cache. Every query is limited to it, and a request without the header answers `400 org_required`. A user from another org is `404`. Every read writes `staff.read` to the audit log with the org; writes keep their audit lines and gain the org. |
| **Refused** (`404`, K6) | `POST /api/users` (`762`), reset password (`764`), administrator IMAP and CardDAV assignment (`770-775`), poll-now (`705`, external IMAP only) | Accounts come from KyIdentity only, and staff never hold a customer's credentials. |

What staff can and cannot reach:

- **Can:** directory-like data (accounts, mailboxes, addresses, quotas, devices by count),
  envelope metadata in quarantine, and platform settings.
- **Cannot:** message bodies, headers or subjects. No admin route reads another user's mail today
  (`api/native_mail_runtime.go:26-39` keeps administrators out of native mail), and multi mode
  adds none. Staff cannot export a user's mailbox, matching today's rule that administrators
  cannot run export for a user (root `AGENTS.md` "user mail import and export"). Staff cannot set
  a password, change an email, or sign in as a user.
- **Whole-install capsule** (`POST /api/admin/backup/export-capsule`, `api/server.go:698`). It
  contains every org's mail (unverified in detail). It stays a platform route, and its audit line
  names it as cross-org. It is the operator's backup path, not a support tool.

Single mode: no class changes anything. Today's administrators keep every route.

## Lifecycle (T contract points 5 and 6)

### Org status

The elected poller (K8) reads `/api/suite/orgs?since=<revision>` through ky-primitives `suitedir`
(T PR 3 adds an `Orgs` method). A change is applied in one transaction per org:

| Status | Sign-in and sessions | Sending | Receiving | Data |
|---|---|---|---|---|
| `active` | Normal | Normal | Normal | Kept |
| `suspended` | Sign-in and device sign-on refused. All of the org's sessions are revoked: R `session_revocations` is bumped for each of its users. Sessions are short-lived and members sign in again on resume, as T does. Agent tokens, paired devices and CardDAV passwords are **kept but refused** while the org is not `active`: every auth path checks the org's status (Isolation). They work again on resume with nothing to re-issue. Only the purge deletes them. | Outbox admission refuses. Queued and scheduled sends are held, not sent (open question 6). | `451` (Mail) | Kept, nothing changed |
| `deletion_scheduled` | As suspended | As suspended | `550` like unknown | Kept until `deleted` |
| `deleted` | As suspended | As suspended | `550` like unknown | Purged |
| Staff org (`KYPOST_STAFF_ORG_ID`) | Never gated by the cache (K8). KyIdentity's own refusal still applies at sign-in. | Normal | Normal (staff own no mailboxes, `sso/native_provision.go:686-689`) | Kept |
| Not in the cache | Refused | Refused | Temporary refusal | Kept |
| Known, but the cache is stale | Refused once the last good poll is more than 15 minutes old | Refused when stale | Last known status | Kept |

- **Speed.** Sessions are re-read on every request (`currentUser`), so a suspension takes effect on
  the next request after the poll. That is within about 60 s, as T promises.
- **Staleness.** The 15-minute bound matches the access-token lifetime (T threat model). Open
  question 5 asks whether that is right.

### Purge (`deleted`)

Run by the elected `api-sweepers` worker (R `app/replica.go:96-102`). One `org_purges` row per org
records progress, so the purge is **idempotent and resumable**: each step checks what is already
gone and skips it, and a purge that died halfway continues from its row. Steps:

1. **Block new use.** Bump R's `session_revocations` for each of the org's users, which ends every
   session as suspension does. Delete the org's `agent_tokens` and `scheduled_sends` rows by
   `org_id`, and cancel its import jobs (R `import_jobs`).
2. **Remove storage.** For each of the org's users and mailboxes, drop the per-user and per-mailbox
   PostgreSQL schemas through the catalog (`dbbackend/dbbackend.go:205-247`). Then remove their
   directories under `CONFIG_DIR` and `STATE_DIR` (`users/<id>`, `mailboxes/<id>`), and delete
   their pickup records and import uploads. This step deletes the remaining credentials:
   - paired devices (`native_devices`, in the per-user `state.db`);
   - CardDAV app passwords (`carddav-auth.json`);
   - sealed IMAP and CardDAV client credentials (`imap-config.json`, `carddav-client.json`).
3. **Receiving and quarantine.** Delete `ingress.db` `routes` and `bindings` for the org's
   mailboxes. A delivery whose every binding was in the org is deleted. For a delivery shared with
   another org, only this org's bindings go.
4. **Ledger tombstones (K10).** Scrub account and mailbox rows: state `purged`, `state_root`
   cleared, the subject kept (it is an opaque KyIdentity ID, not personal data). Each address
   becomes an HMAC of the address under the tombstone key, held in a new `address_tombstones`
   table that the conflict check consults first (M open question 4). Then `directory` is scrubbed
   the same way.
5. **Root store.** Delete the org's `send_ledger` rows, then its `users.json` records, which hold
   the password hashes, sealed TOTP secrets, recovery codes and PGP keys.
6. **Record the purge.** Write a platform audit line with the org ID and the row and file counts,
   and no content. Mark the `org_purges` row done.

- **One-way.** Step 4 is the only write that touches rows the authority triggers protect. It needs
  a named trigger exception keyed on a live `org_purges` row (open question 9). It cannot be
  undone, which is the point.
- **Backups.** Whole-install capsules taken before the purge still hold the org's data until they
  age out of retention (Residual risks, open question 7).

### Export

Users export their own mailboxes and folders as mbox or EML zip, self-service, as today (root
`AGENTS.md`). There is no org-wide or staff export (Admin surface). During `deletion_scheduled`,
KyIdentity's export-only session issues no tokens (T "Views"), so a member cannot reach KyPost to
export. Open question 4 asks how mail export works in that window.

### Quotas

These come from `quotas_json` in the cache (Abuse limits). A lower quota refuses new mail and new
creations only, never existing data. That matches today's mailbox-quota rule and T's "over_limit,
nobody is disabled".

## Replicas (PR #677)

R runs several app replicas on one host, with one PostgreSQL and shared volumes. The `.db` stores
are in PostgreSQL. Accounts (`users.json`), per-user JSON and `config.yaml`, DKIM keys and the
secret keys stay on the volumes behind host-local file locks (R `docs/RUNNING.md:242-245,283`).
Tenancy fits on top:

- **Mode and staff org.** `KYPOST_TENANCY` and `KYPOST_STAFF_ORG_ID` join R's shared-configuration
  fingerprint (R `replica/config.go:35`). A replica started with another mode or staff org
  refuses, and `accept-replica-config` (R `app/replica.go:143`) is the deliberate way to change
  them, which K1 refuses for the mode anyway.
  - **Upgrade.** R's `Diff` counts a setting known on only one side as no difference, and merges
    it into the record (R `replica/config.go:138-152`). An upgraded single-tenant replica install
    therefore starts without `accept-replica-config`. The merge adds two entries to R's own
    fingerprint record, as it does for any new shared setting. That is R's bookkeeping, not
    behaviour.
  - **Canonical values.** `KYPOST_TENANCY` canonicalises unset or empty to its default,
    `single`, and lowercases the value, before fingerprinting. So an explicit `single` and an
    unset variable give the same fingerprint, and a correctly configured replica never refuses.
    `KYPOST_STAFF_ORG_ID` has no default (it is required only in multi mode), so unset stays
    `""`.
  - PR 1's upgrade test starts an R-format replica install under the new binary. It must start
    with no refusal, and nothing but the fingerprint record may change. It also runs two
    replicas, one with `KYPOST_TENANCY=single` and one with it unset, which must agree.
- **Shared org state.** The `orgs` cache and `org_purges` live in the root `state.db` on
  PostgreSQL, so every replica answers from the same status. One elected runner polls and purges
  (R `worker/worker.go:49`).
- **Session revocation.** R keeps sessions in PostgreSQL keyed by `user_id`, with a per-user
  revocation generation (R `state/schema.go:302-311`). Suspension bumps the generation for each of
  the org's users in one transaction. A per-org generation would save writes, but it is not needed
  for correctness.
- **Install-wide SSO epoch.** R's `EndSSOSessions` ends every SSO session in the install (R
  `state/sessions.go:160`). A staff change to SSO settings therefore signs out every customer.
  That is acceptable, because it is rare and audited.
- **Lockout keys** hash the username or IP with no namespace (R `api/shared_state.go:41-61`).
  Hosted usernames are unique across the install, so no change is needed.
- **Per-replica meters** allow up to N times their limit with N replicas (R `docs/RUNNING.md:278`,
  "O4"). Per-org quotas must not use those in-memory meters. They read PostgreSQL (`send_ledger`)
  or the ledger, so they hold across replicas.
- **Scale before launch.** `users.json` is one file holding every customer's account, rewritten
  whole under one lock. Per-user `config.yaml` files are read by the masked sweep every minute (M
  D8). R names moving these files into PostgreSQL as its follow-up job. Hosted launch depends on
  that follow-up. The isolation phases do not.
- **Receiving.** R refuses Maddy on PostgreSQL (Mail, open question 3).

## Downstream contract, point by point (T "Tokens and suite contracts")

| # | T requires | KyPost multi mode | Proof |
|---|---|---|---|
| 1 | Refuse a token without `org_id` | ID token, device-assertion token and logout token without `org_id` are refused. KyPost accepts no access token. | Token tests per input (Identity binding) |
| 2 | Key every row by `org_id` | `org_id` on the two roots and on root-store aggregates. Every per-user and per-mailbox store is derived from its owning ID (K4, Store inventory). | `TestEveryStoreIsClassified` (Phased build plan, PR 1) |
| 3 | Bind accounts to `(sub, org_id)` and refuse a known `sub` with a different `org_id` | Write-once `OrgID` and `directory.org_id`. A mismatch at sign-in, device sign-on, logout, webhook or peer call is refused like a wrong issuer. | Mismatch tests per input, `TestCrossOrgMatrix` |
| 4 | Org only from a verified token, signed webhook or suite answer | `OrgID` is written only from the signed directory event. Sessions read it from the user record. Only staff org selection reads a header, and only after K7. | Identity-binding tests, `orgAdmit` scan |
| 5 | Stop serving a non-`active` org; keep data unless `deleted` | Lifecycle table. The purge runs only on `deleted`. | Lifecycle tests (PR 5, PR 6) |
| 6 | Enforce own quotas from `/api/suite/orgs` | Abuse limits table, from the `orgs` cache | Quota tests per limit (PR 5) |

## Threat model

| Attacker | Goal | Defence |
|---|---|---|
| Member of org A | Read or change B's mail, contacts, devices, masked addresses or settings by sending B's IDs | Per-user stores open only by the caller's own ID. Every cross-user hop goes through `orgAdmit`, and a foreign ID answers byte-for-byte like a missing one. Proven by `TestCrossOrgMatrix` and the `orgAdmit` call-site scan. |
| Member of org A | Become an administrator | K7: admin needs the staff org's `org_id` as well as the role, so a mis-assigned `kypost.admin` on a customer is applied as a member and logged. Local accounts and the bootstrap administrator do not exist in multi mode (K6). |
| Malicious or buggy suite peer | Act for B's user while calling for A | Signed peer bodies carry `org_id`, which must match the local account. KyIdentity scopes directory answers by the `sub`'s org (T). |
| Forged or replayed sync event | Move an account into another org | `syncauth` signature (`api/sync_handlers.go:90-106`), revision fence (`sso/directory.go:181`), write-once `org_id`. A mismatch is refused and audited. |
| Internet sender | Deliver into the wrong org, or learn which org owns an address | Routing goes by address to one ledger mailbox. Refusals for a suspended or deleted org use the same codes as for a temporary failure or an unknown address. No SMTP answer names an org. |
| Internet sender | Probe the relay domain for live masked addresses | M's defences (entropy, identical refusals, per-source budget) |
| User of a suspended or deleted org | Keep reading or sending mail | Session kill on the next request, device and CardDAV refusal, outbox admission refusal, 15-minute stale-cache bound |
| Abusive customer | Burn the shared domains' reputation | Per-org send, masked and storage quotas. Staff can suspend through billing or KyIdentity (T). Platform circuit breaker (M). |
| Staff, or a stolen staff account | Read customers' mail | No route reads bodies. Org-bound reads are audited per org. KyIdentity pins staff to passkeys and step-up (T "Staff"). Residual risk: staff see envelope metadata in quarantine, and the operator holds every key. |
| Operator restoring a backup | Roll one org back, or load A's data into B | Whole-install capsules only (R, T). No per-org restore or import in v1. |
| A customer reusing a deleted org's address | Receive mail meant for the old owner | Ledger tombstones survive deletion as HMACs, and the conflict check consults them first (K10, M D5). |

## Phased build plan

Each PR ships on its own, with single-tenant behaviour unchanged. `KYPOST_TENANCY=multi` is
refused at startup until PR 7. Until then multi-mode code runs only in tests. PR 1 can start now.
PR 2 onward needs KyIdentity's T PR 3 (`org_id` in tokens, logout tokens and webhooks, and
`/api/suite/orgs`).

1. **Mode and inventory.** `KYPOST_TENANCY` (refused if `multi`), the multi-only `meta` record,
   the multi-only schema additions (`orgs`, `org_purges`, the `org_id` columns), `users.User.OrgID`
   (`omitempty`), and `KYPOST_STAFF_ORG_ID`. Tests:
   - `TestEveryStoreIsClassified` requires every table in every schema (`state`, `mailbox`,
     `ingress`, `cfreceiving`, `sso/authority_*`, and R's tables once merged) and every registered
     file store to have exactly one class. A new store with no class fails the build.
   - A single-tenant upgrade test: a fixture install started under the new binary keeps
     `users.json`, every schema dump and every file byte-for-byte identical.
   - The full `go test -race ./...` and the `postgres` job.
2. **Identity binding.** `org_id` parsing and refusal for ID tokens, device-assertion tokens,
   logout tokens and sync events. Write-once `OrgID` and `directory.org_id`, and the K7 admin rule.
   Tests: each refusal, each mismatch, and the single-tenant sign-in golden path unchanged.
3. **The org gate.** `AuthContext.OrgID`, `orgAdmit`, the hop list, `org_id` on peer bodies (in step
   with each peer's own spec), and the call-site scan. `TestCrossOrgMatrix` starts here and grows
   with every later PR.
4. **Staff console.** Route classes and the build-failing check, `X-Ky-Org` on org-bound routes,
   the K6 refusals, and staff audit lines. Tests: the matrix's staff variant, and every admin route
   called as a customer member.
5. **Entitlements and lifecycle.** The `suitedir` `Orgs` poller under election, the `orgs` cache,
   status effects on sign-in, sending and receiving, held sends, and quotas. Tests: each status
   against each surface, the stale-cache bound, each quota, two replicas seeing one suspension
   (R's PostgreSQL job), and staff still reaching the console with the poller failing and the
   staff org absent from the cache.
6. **Purge.** `org_purges`, the purge steps, the tombstone key, the `address_tombstones` table, and
   the named trigger exception (after the owner's answer to open question 9). Tests: a purge killed
   at every step resumes and finishes; nothing of the org remains, checked against the
   classification; tombstones refuse reuse, including after a restore with the same key.
7. **Hosted mail.** The primary-address source (open question 1), M's masked changes 1–7, per-org
   abuse limits, and the hosted receiving profile (open question 3). Startup then accepts
   `multi`. Tests: M's hosted verification list, and a native-mail harness run
   (`docs/DEVELOPMENT.md`) against KyIdentity in multi mode.

Launch prerequisites outside this plan: R's follow-up that moves accounts and per-user files into
PostgreSQL, and a measured capacity target (open question 8).

## Residual risks

- **One install, one database, one host.** Every org shares R's single host and single
  PostgreSQL, and a restore rolls every org back (R, T).
- **Backups outlive a purge.** A deleted org's mail stays in older capsules until they expire.
- **The operator and staff are trusted.** The operator holds every key and the database. Staff see
  every org's directory data and quarantine envelopes.
- **Timing.** `TestCrossOrgMatrix` proves equal answers, not equal timing. A foreign ID may take a
  different path before it is refused (unverified; no measurement).
- **Shared reputation and provider visibility.** As M residual risks.
- **Schema count.** Each user and each mailbox is one or two PostgreSQL schemas
  (`dbbackend/dbbackend.go:241-247`). Behaviour at hosted scale is unverified.
- **Offline window.** A suspended org keeps working for up to one poll (about 60 s). Up to 15
  minutes if the poll fails.
- **Staff exemption.** The staff org is never gated by KyPost's cache (K8). Stopping staff
  access depends on KyIdentity alone: disabling the account or the staff org there.

## Open questions, with recommendations

1. **Where a hosted member's mailbox address comes from.** The KyIdentity email is the sign-in
   address, often on another domain (T D5), and native provisioning needs an address on a proven
   domain (`sso/native_provision.go:798-811`).
   - (a) KyIdentity reserves one at member add and sends it with `org_id`, as `hr.mailbox.reserve`
     does today (`api/hr_mailbox.go:21-31`, `sso/hr_mailbox.go:13-104`).
   - (b) The member picks a local part at first sign-in.
   - (c) KyPost generates one.
   - *Recommend (a)*: the person and their primary address stay owned by KyIdentity, as in
     self-hosted (root `AGENTS.md` "Native mail"). It needs a T change, because T turns HR off in
     multi mode.
2. **How KyPost knows the staff org.** *Recommend* `KYPOST_STAFF_ORG_ID` (K7). Also ask KyIdentity
   to refuse app roles named `*.admin` for accounts outside the staff org, so the rule holds on
   both sides.
3. **Hosted receiving profile.** Maddy is refused on PostgreSQL after R, and Cloudflare stops at
   5000 routes and sees every address. *Recommend* a receiver we run (Maddy) as its own container in
   R's replica overlay, handing deliveries to PostgreSQL rather than a local socket. This is a
   receiving design of its own; it must be decided before PR 7.
4. **Mail export while deletion is scheduled.** T's export-only session issues no app tokens.
   - (a) KyIdentity issues an `export_only` claim during `deletion_scheduled`, and KyPost admits
     only the export routes.
   - (b) No export in the window: billing resumes the org first.
   - *Recommend (a)*, because no lock-in is a product promise. It needs a T change. Until it lands,
     (b).
5. **When a stale entitlement cache fails closed.** *Recommend* 15 minutes for sign-in and sending
   (the token lifetime). Receiving keeps answering from the last known status, which is the
   Lifecycle table's rule, so a poller outage changes no SMTP answer. That is safe:
   - `deleted` is terminal (T "Account holder guard"), so its `550` cannot become wrong.
   - Leaving `deletion_scheduled` or `suspended` takes a billing call, which the next good poll
     applies.
   - An `active` org keeps receiving, so no mail is lost.
   - An org never seen by a good poll gets a temporary refusal.
6. **Mail held during suspension.** *Recommend* holding queued sends without sending them. On
   resume, a scheduled send whose time has passed returns to Drafts, as R's Send delay does, rather
   than going out late without the user knowing.
7. **Backup retention after a purge.** *Recommend* a fixed capsule retention stated in the hosted
   privacy terms, and no per-org backup surgery.
8. **Capacity target.** *Recommend* measuring orgs, mailboxes, schemas and messages per minute on
   R's single host before launch, as T owner answer 8 does for KyIdentity.
9. **The purge versus the never-delete triggers.** *Recommend* K10: scrub to tombstones, with one
   named trigger exception keyed on a live `org_purges` row, plus HMAC address tombstones. This
   relaxes an invariant that `docs/NATIVE_ADDRESSING_V2.md` relies on, so the owner should confirm
   it.
10. **Per-customer custom domains (later).** As M "Per-customer custom domains", the domain proof
    must bind the org as well as the issuer. `native-domains.json` holds one issuer
    (`sso/native_domain.go:68-74`) and would need a per-domain `org_id`. Out of scope for v1.
