# KyPost tenancy: single-tenant and multi-tenant (SaaS), design

Date: 2026-11-20. Status: proposed design; build plan PRs 1 to 5 and PRs 6a and 6b are built ("PR 1 as built" to "PR 6 as built"), and so is KyPost's side of the org storage pool ("Org storage pool as built"); the rest is not. Revised the same day with the
owner's answers to open questions 1, 9 and 11, the Delete everything button, and a recommended
receiving tier for open question 3. Revised 2026-11-21 with the owner's answers to open questions 3
(the receiving tier gets its own spec, `docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`) and 4 (option (a) approved).
Revised 2026-11-22 with the owner's answers to PR 3's and PR 4's defaults ("PR 3 as built", "PR 4
as built") and to MH's open questions: self-hosted multi-host as an opt-in migration, and
multi-region as one cell per region (MH, `docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md`).
Revised 2026-11-23 with the owner's plan decisions: every paid account is an org on the shared
install, businesses included, and only Enterprise gets a dedicated install ("Plans and orgs").
Revised again 2026-11-23 with the storage decisions: one storage pool per org across all apps,
held by KyIdentity ("Quotas"), and message bodies in object storage on the hosted install (MH
"Object storage for message bodies").

KyIdentity's tenancy spec (`KyIdentity-server` `docs/superpowers/specs/2026-11-19-tenancy-design.md`,
cited as **T**) makes every hosted customer an *organisation* (org) under one issuer. Since the
owner's 2026-11-23 decision that is every paid plan: Personal, Couple, Family and Business
("Plans and orgs"). Tokens carry an opaque `org_id`. This note is KyPost's half: how one KyPost binary runs
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
   members. Single-tenant installs (self-hosted and Enterprise) keep their own administrators.
5. **Every paid account is an org on the shared install (2026-11-23, supersedes the earlier
   decision 5 and T decision 7).** Personal is an org of 1 member, Couple 2, Family up to 6, and
   Business N seats. All four run on the shared, multi-tenant, multi-server install. Only
   **Enterprise** gets a dedicated install, in the cloud or on premises, which is a single-tenant
   install (SQLite is fine there). The earlier rule, that businesses get their own single-tenant
   install and the shared service holds only people and households, no longer applies.
6. Stripe lives only in a separate billing service, built later. KyPost has no billing code.
7. Hosted masked addresses come from one shared platform relay domain, unique across customers.
   Per-customer custom domains come later. Self-hosted keeps its own domain (M).

Line references are to KyPost `origin/main` at `2c7ecc09`, to KyIdentity `origin/master` at
`2e304a4` for T, and to PR #677 at `746cdfd` where marked **R**. Unless a path says otherwise,
KyPost paths are relative to `backend/internal/`. "Unverified" marks a claim this note did not
check in code or by running it. The revision's new references are to KyPost `origin/main` at
`be66da6d` (which includes R) and KyIdentity `origin/master` at `49e8aa6`, marked **KI**. T's
PR 3 has merged as KyIdentity #276.

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
| K1 | `KYPOST_TENANCY=single` (default) or `multi`. A multi install records `tenancy_mode=multi` and `staff_org_id` (K7) in the root `state.db` `meta` at first start. A start whose mode or `KYPOST_STAFF_ORG_ID` differs from the record refuses to start. Single-tenant writes nothing: no record means single. It stays out of R's replica fingerprint, and the record is what refuses a mismatched replica (Replicas). | Install-time, as T D1. Writing nothing keeps upgrades byte-for-byte unchanged. A replica started in the wrong mode must not serve. |
| K2 | **Multi-tenant requires** PostgreSQL (`KYPOST_DATABASE_URL`, `docs/CONFIGURATION.md:124`), native mail (`KYPOST_NATIVE_MAIL=true`), the v3 authority database, a configured KyIdentity issuer, and an **empty install** at first start. Converting a single-tenant install is out of scope, as in T. | T requires Postgres too (T D2). The external-IMAP profile and local accounts have no place in a hosted mail service (K6). Starting empty avoids a migration with no org to assign. |
| K3 | **No existing table changes shape.** The org mapping lives in new, multi-only side tables that a multi install's first start creates: `user_orgs(user_id PK, org_id)` and `subject_orgs(issuer, subject, org_id, PRIMARY KEY(issuer, subject))`, in a multi-only store of their own, `STATE_DIR/tenancy.db` (PostgreSQL schema `kypost_tenancy`; see "PR 1 as built"). Both are write-once, refusing an update. Shared tables keep one schema in both modes, so no shared query or migration branches on mode. Only multi-mode code reads the side tables. `users.User.OrgID` is a JSON field with `omitempty`, so it is absent from single-tenant `users.json`. A single-tenant install never gets any of this, and its databases, `users.json` and every file stay byte-for-byte as they are. | This is the zero-change argument. Columns added only in one mode would give one table two schemas, and every query on it would need a branch. A side table is a join that only multi mode makes. |
| K4 | The org is recorded on two roots only: the user (`users.User.OrgID`, mirrored in `user_orgs`) and the KyIdentity subject (`subject_orgs`), both write-once. Every other store is **derived**: it inherits the org of the user, mailbox or subject that owns it. That includes the root-store aggregates (`send_ledger`, `agent_tokens`, `scheduled_sends`), which join `user_orgs` on `user_id` for per-org counts. The purge uses its own snapshot instead (Lifecycle). | Per-user data is already separated by user ID (above). Adding `org_id` to hundreds of per-user schemas would add a column nobody could set wrongly. The risk sits on the hops *between* users, so that is where the checks go (K5). |
| K5 | `AuthContext.OrgID` is set by `currentUser` from the re-read user record, never from the request. Every **cross-user hop** goes through one function, `orgAdmit(ac, target)`: from the caller to another user, mailbox, address, device, delivery or peer subject. On a mismatch it gives the same answer as a missing target. The hops form a named list (Isolation). | One choke point, as KyIdentity's `OrgStore`. KyPost has no single SQL layer to bind, but it does have a single per-request identity. |
| K6 | Off in multi-tenant v1, answering `404`: local password sign-in, local user creation, password reset, SSO link and unlink, `AutoProvision`, the external-IMAP mail source, administrator-assigned IMAP and CardDAV accounts, and HR mailbox reservation (`/api/sync/hr-mailbox`). The first-run bootstrap administrator (`app/bootstrap.go:41-42`) is not created. | Accounts come only from KyIdentity (T decision 5). Staff must never hold a customer's credentials. T switches HR off in multi mode (T "Per-org features"). |
| K7 | **Staff = `kypost.admin` role *and* a token whose `org_id` equals `KYPOST_STAFF_ORG_ID`.** A customer account is never an administrator, whatever roles a token or sync event carries. That holds in every plan: in a Business org the person who manages its seats is a member too, and seat changes go through billing and KyIdentity, not KyPost ("Plans and orgs"). | `HasAdminRole` (`sso/directory.go:24-31`) trusts the role alone. T's D8 refuses KyIdentity's *own* admin role outside the staff org, but app-role assignment is "per org, run by staff" (T "Per-org features"). So a staff mistake could give a customer `kypost.admin`. KyPost pins the org independently. |
| K8 | Entitlements are pulled from `GET /api/suite/orgs` (T D6) by one elected worker about every 60 s. They are cached in a platform table, `orgs(id, status, revision, enabled, quotas_json, polled_at)`, in the multi-only `tenancy.db`, so every replica reads the same answer. An org missing from the cache is not served. **The staff org (`KYPOST_STAFF_ORG_ID`) is exempt**: it is never gated by the cache, so staff keep the console during a poller or KyIdentity outage. | T's contract point 6. A shared table keeps replicas consistent (R `worker.Run` elects one runner per role). The staff exemption is safe: KyIdentity still decides whether a staff account signs in at all (T `active_principals`). It is unverified whether `/api/suite/orgs` lists the staff org. |
| K9 | Hosted mail domains, the platform primary domain(s) and the relay domain (M D2) are **platform assets**: staff configure, prove, sign and retire them through today's admin routes. No customer configures a domain in v1. | No customer administrators (decision 4). Per-customer custom domains come later (M "Per-customer custom domains"). |
| K10 | Org deletion **scrubs** rather than deletes in the authority database. Account and mailbox rows stay as tombstones with storage removed and no personal fields. Addresses become keyed hashes, as M open question 4 recommends. Everything else the org owns is deleted. | `authority_v3.go:89-99` forbids deletes, so that "never reuse" holds. M D5 needs the tombstone and the privacy rule needs the content gone. One named purge path with its own migration is the only exception to those triggers (open question 9, confirmed by the owner). |

## Plans and orgs (owner decisions, 2026-11-23)

| Plan | Org members | Pooled storage, all apps (owner decision of 2026-11-23 on storage) | Install |
|---|---|---|---|
| Personal | 1 | 20 GB | the shared multi-tenant install, run on several hosts (Replicas) |
| Couple | 2 | 40 GB | the shared install |
| Family | up to 6 | 100 GB | the shared install |
| Business | N seats (small teams) | 30 GB per seat, pooled across the team | the shared install |
| Enterprise | its own directory | no pool: a custom per-mailbox size (Quotas, single-tenant) | a dedicated single-tenant install, cloud or on premises; SQLite is fine |

Every plan sells all seven apps (email, contacts, calendar, tasks, notes, drive, messages), so
KyPost is in every one.

- **KyPost is plan-blind.** The suite directory gives KyPost an org's `id`, `status`, `revision`,
  `enabled` and its own opaque `quotas` only (ky-primitives `suitedir/client.go:185-191` at the
  `backend/go.mod` pin). KyPost never sees a plan, an org kind or a seat count, and no rule in this
  note branches on one. Each rule is about one account, one mailbox or the whole org. What a plan
  allows reaches KyPost only as `quotas_json` (Abuse limits per org).
- **Storage is one pool per org, held by KyIdentity** (owner decision, 2026-11-23, required for
  launch). The number a customer buys is shared by every member and every app. KyPost checks the
  pool before a write that adds bytes and reports its own usage ("Quotas"). It still sees no plan:
  the pool reaches it as a number.
- **Seats are KyIdentity's.** `max_seats` and the `seat_limit` refusal are enforced there (T
  "Entitlements"). KyPost adds no seat check: an account appears only from a signed directory
  event (Identity binding), and KyIdentity refuses the member add that would pass the seat count.
- **No customer administrators, in any plan** (decision 4, K7). A Business org's extra mailboxes
  and aliases are added by staff on request, bounded by its `mailboxes` quota (Mail). The business
  features T switches off in multi mode (inbound SCIM, HR provisioning and HR mailbox reservation,
  per-org federation: T "Per-org features"; K6 for HR) stay off. A business that needs them buys
  Enterprise (open question 12).
- **Enterprise is single-tenant.** Nothing in this note applies to it: K1 records nothing, K2's
  PostgreSQL requirement is multi-only, and the single-tenant upgrade test covers it as it covers
  self-hosted.
- **Rules that said "household"** now mean any org with more than one member: Couple, Family or
  Business. Delete everything's paths (Lifecycle) depend only on whether the account holder has
  other live members, which KyIdentity decides (T "Delete everything", `liveHolder`).
- **Owner decisions of 2026-11-23, later the same day (ky-launch#4):**
  1. Incoming mail while an org is over its pool: open question 16, now answered.
  2. A Business pool counts **paid** seats (`max_seats`), and seats are capped at 1000 per
     org. Both are KyIdentity's. KyPost sees neither.
  3. Delete everything and shared data: shared content passes to the longest-standing
     admin, then an editor, then any member. A person's messages in shared rooms become
     tombstones, and nothing other members wrote is deleted. KyPost holds no shared content
     between members (each mailbox has one owner), so its Delete everything paths are
     unchanged.
  4. No automatic version pruning at launch. KyPost keeps no versions.
  5. Object storage is multi-tenant only, with no volume fallback: a hosted install refuses
     to start without it (MH "Object storage for message bodies", MH11). Single-tenant
     installs keep their storage.
  6. KyCalendar keeps attendee display names readable, KyMessages does not count message
     text in the pool, and staff audit keeps usernames. None of these is KyPost's.
- **KyIdentity changes this needs** (T, not built; unverified how T will name them):
  - T's `orgs.kind` allows only `default`, `person`, `household` and `staff` (T "Data model"), and
    the `org_erasures_holder` trigger admits only `person` and `household` (T "Delete
    everything"). Couple, Family and Business orgs need kinds T accepts on both. KyPost reads no
    kind, so the naming does not touch KyPost.
  - T decision 7, "Per-org features" and "Business platforms" still say businesses get their own
    install. The 2026-11-23 decision supersedes them.

## Store inventory

Classes, as in T's appendix: **O** (org-owned: carries `org_id`), **D** (derived: inherits the org
of the parent named), **P** (platform: install-wide, staff only), **–** (not present in multi mode,
K6). "Multi-only" means the store or column exists only in a multi install (K3).

### SQL databases

| Store | Defined | Class | Reasoning |
|---|---|---|---|
| Root `state.db` `meta` | `state/schema.go:74-290` (SQLite), `state/pg_schema.go:11-69` | P | Settings, health, update dedup. Adds `tenancy_mode` and `staff_org_id` (multi-only, K1): together they refuse a replica with another mode or staff org. |
| Root `state.db` `agent_tokens` | `state/schema.go:269` | D (`user_id`, via `user_orgs`) | Suspension refuses them through the org check. The purge deletes them for the snapshot's users. |
| Root `state.db` `scheduled_sends` | `state/scheduled_sends.go`, `state/schema.go` | D (`user_id`, via `user_orgs`) | Keyed by `user_id` and `mailbox`. A suspended org's due sends are held (open question 6). |
| Root `state.db` `send_ledger` | `state/send_ledger.go:27-38` | D (`user_id`, via `user_orgs`) | Counts the per-user quota today (`api/send_quota.go:25`). Per-org quotas sum it through `user_orgs`. |
| New `tenancy.db` `user_orgs` | (multi-only) `tenancy/tenancy.go` | O | K3. The user root: one row per account, written once with `users.User.OrgID`. |
| Root `state.db` `backup_audit` | `state/schema.go` | P | Whole-install backups. |
| New `tenancy.db` `orgs` cache | (multi-only) `tenancy/tenancy.go` | P | K8. The registry every org check reads. |
| New `tenancy.db` `receiving_gateways` | (multi-only) `tenancy/tenancy.go`, `tenancy/gateways.go` | P | The hosted receiving tier's MX key pins (receiving tier spec, PR 2). Rows are never deleted. |
| New `tenancy.db` `sender_blocks`, `sender_evidence` | (multi-only) `tenancy/tenancy.go`, `ingress/senders_table.go` | P | The sender block list and its automatic-block evidence in multi mode (receiving tier spec R7, PR 3), with the file rules. Multi mode never has the two files. |
| New `tenancy.db` `import_leases`, `retired_bindings` | (multi-only) `tenancy/tenancy.go`, `tenancy/imports.go` | D (the mailbox named in the row) | Receiving import's per-mailbox leases and the bindings recorded as never importable (receiving tier spec R8, PR 4). The purge deletes an org's rows with its bindings. |
| New `tenancy.db` `org_purges`, `org_purge_members` | (multi-only) `tenancy/tenancy.go`, `tenancy/purge.go` | P | Purge progress, one row per deleted org, plus the snapshot of its user, mailbox and subject IDs taken at purge start, so a purge that died halfway resumes (Lifecycle). The snapshot is deleted when the purge is marked done. |
| Per-user / per-mailbox `state.db` (`processed`, `decisions`, `notifications`, `native_devices`, `pull_notifications`, `deferrals`, `sorter_*`, `snoozes`, `ooo_*`) | `api/server_userscope.go:86-92`; same schema as root | D (user or mailbox directory) | The directory, or on Postgres the `kypost_f_*` schema, is the owner. |
| `mailbox.db` (messages, folders, labels, receipts, usage, `outbox*`, …) | `mailbox/store.go:118-132`, `mailbox/pg_schema.go:17-32` | D (mailbox) | The `identity` row names `(issuer, subject, mailbox)` (`mailbox/store.go:118`). It gains no `org_id`: the mailbox's account carries it. |
| `native-authority.db` `directory`, `directory_primary_emails` | `sso/authority_v3.go:39-48` | D (`(issuer, subject)`, via `subject_orgs`) | The KyIdentity directory mirror. |
| `native-authority.db` `accounts` | `sso/authority_v3.go:49-55` | D (`(issuer, subject)`, via `subject_orgs`) | The ledger root of a person's mailboxes. |
| New `tenancy.db` `subject_orgs` | (multi-only) `tenancy/tenancy.go` | O | K3. The subject root, written once from the signed webhook (Identity binding). |
| `native-authority.db` `mailboxes` | `sso/authority_v3.go:56-63` | D (`(issuer, subject)` FK to `accounts`) | |
| `native-authority.db` `addresses`, `address_history`, `routes_pending` | `sso/authority_v3.go:65-76` | D (via `mailbox`) | Address keys are global (M D4). The purge deletes an org's rows after writing their hashes to `address_tombstones` (K10). |
| New `tenancy.db` `address_tombstones` | (multi-only) `tenancy/tenancy.go`, `tenancy/purge.go`; not in `native-authority.db`, whose exact-schema check refuses an extra table | P | K10. HMACs of every address a deleted org held, under the tombstone key. No org or owner, never deleted, consulted first by the conflict check. |
| `native-authority.db` `meta`, `directory_events`, `logouts`, `sign_ons`, floors, barriers, `lifecycle_records` | `sso/authority_v3.go:77-88` | P | Replay and recovery ledgers keyed by event or JTI. Restore authority is whole-install. |
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
| `receiving.conf`, `sender-blocks.json`, `sender-evidence.json`, `quota-<id>` locks | `api/native_recovery_handlers.go:364`, `ingress/blocks.go:23`, `ingress/autoblocks.go:26-31`, `app/receiving.go:37-82` | P (quota locks D, per mailbox; the two sender files `-` in multi mode, receiving tier PR 3) | Sender blocks stay platform-wide (Mail). Multi mode keeps them in `tenancy.db` instead. |
| Per-user `config.yaml` (settings, signatures, masked records) | `config/config.go:199-245,410` | D (user) | M D8 moves masked records out in hosted mode. |
| Per-user `imap-config.json`, `tuning.md` | `config/config.go:402,406` | – / D | No external-IMAP source in multi mode (K6). `tuning.md` is derived. |
| Per-user `carddav-auth.json`, `carddav-client.json` | `api/server_userscope.go:247,254` | D (user) | The user's own CardDAV password and outside contacts server. Administrator assignment is off (K6). |
| Per-user `contacts.json`, `groups.json`, `rules.json`, `send_as_aliases.json`, `pgp-discovery*.json`, `kycalendar-*.json`, `incoming-encryption.json` | `contacts/store.go:64`, `groups/store.go:44`, `rules/store.go:43`, `sendas/store.go:62`, `pgpdiscovery/settings.go:23`, `pgpdiscovery/suppress.go:28`, `kycalendar/invitations.go:32`, `kycalendar/flags.go:35`, `processor/incoming_encryption.go:58` | D (user) | |
| Per-mailbox `mailcache.json`, `native-mailbox.json`, `outbox-idle` | `mailcache/store.go:105`, `mailbox/prepare.go:26`, `mailbox/outbox_idle.go:22` | D (mailbox) | |
| `$STATE/pickup/<id>.json` | `api/server.go:476`, `pgpmail/pickup.go:88-92` | D (owner user) | One install directory, one record per owner. |
| `$STATE/imports/upload-*` | `api/mail_import.go:37,315` | D (user, through the grant) | Temporary. Grants and jobs are in memory (`api/server.go:164-172`) or in R's `auth_challenges` and `import_jobs`. |
| Native restore hold, qualification and release files, `mfa-clear-all.*` | `sso/native_restore.go:21`, `sso/native_restore_qualification.go:24`, `sso/native_release.go:95`, `app/app.go:574,586` | P | Whole-install recovery. |
| Backups, `mail-bulk.json` | `backup/backup.go:92`, `backup/bulk.go:38-43` | P | Whole-install capsules, as T. |
| `replica-volume.id`, `server-instance.lock`, `backup-operation.lock`, `sender-evidence.damaged.json` | `replica/topology.go:19`, `state/coordination.go:319`, `backup/backup.go:524`, `ingress/autoblocks.go:29` | P (`sender-evidence.damaged.json` `-` in multi mode, receiving tier PR 3) | Process coordination and a quarantined copy of damaged evidence. Added by PR 1's inventory test. |
| `tenancy.db` | `tenancy/tenancy.go` | P (tables O and P, above) | Multi-only (PR 1). |

### Keys (`SECRET_DIR`, `config/config.go:44,52`)

All keys are **P**: one per install, used for every org. Each is listed with what it seals:
`native-relay.key` (relay settings and DKIM keys), `imap-config.key` (per-user IMAP and CardDAV
client credentials), `totp-secret.key` (TOTP secrets, recovery-code digests and R's fingerprint
HMAC), `pgp-private-key.key` (users' sealed PGP private keys), `pickup-store.key`, `pairing.key`,
`pow.key`, `relay-identity.key`, `scheduled-send.key`, `push_relay_key` and `apns_relay_key`,
`notifications-vapid-private.pem`, `cloudflare-receiving.json`, `bulk-backup.key` and `.repo`, and
R's `imap-import.key`, and the hosted receiving tier's `receiving-tls.key` (multi-only, created by
`receiving serve --tls` or `receiving gateway server-pin`, `app/receiving_tls.go`) (`api/server.go:384-426`, `api/scheduled_send.go:161`,
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
- **Every one of those builders also checks the org's status** in the `orgs` cache (K8), with the
  staff exemption. An org that is not `active` is refused like a revoked credential. That is what
  keeps paired devices, CardDAV passwords and agent tokens unusable during a suspension without
  deleting them (Lifecycle). One shared function makes the check, so the four paths cannot drift.
  PR 5's tests call each path for a suspended org.
- **Signed public links** (`/pickup/{id}`, export and import links) resolve to an owning user.
  For a user whose org is not `active`, they answer as an expired link does.
- **Per-user stores** are opened by `AuthContext.UserID` (`api/server_userscope.go:308-603`). A
  handler cannot reach another user's store without naming that user, and naming another user is
  a cross-user hop.
- **Per-mailbox stores** are admitted by `admitMailbox` from `X-KyPost-Mailbox`
  (`api/native_mailbox_handlers.go:19,31-68`). It already checks that the mailbox belongs to the
  caller. Multi mode adds `orgAdmit` there, so a foreign mailbox ID answers like an unknown one.
- **Root-store rows** (`agent_tokens`, `scheduled_sends`, `send_ledger`) are keyed by the writer's
  `user_id`, as today, and reach their org through `user_orgs`.
- **Background workers** (provisioning, sweeps, outbox recovery, scheduled sends, receiving and
  Cloudflare pickup import) work from the ledger, where every mailbox resolves to an account and
  so to an org. Each one checks the org's status in the `orgs` cache before it writes:
  - A worker skips a mailbox whose org is not `active`. Receiving import is narrower, per the
    receiving tier spec (`docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`,
    "Org status"). Mail already accepted imports for `suspended` and `deletion_scheduled` orgs,
    whose data K keeps. It waits only for an org missing from the cache, and a `deleted` org's
    mail is dropped by the purge. So one org's status never holds the shared ingress limits.
  - A waiting import leaves that mailbox's part of the delivery pending, so it is never lost.
    Other recipients' mailboxes still import. An import write is fenced against the purge by the
    mailbox's quota lock: the writer re-checks for `deleted` under that lock, and the purge holds
    it while removing the mailbox (Purge, step 3). So nothing is written into storage the purge is
    removing.
  - Outbox and scheduled sends are held (Lifecycle).

  The purge and the entitlement poller are the only workers that act on an org as a whole.

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
| Signed sync webhook | `sso.DirectoryUser` gains `OrgID`, covered by `syncauth` (T "Outbound sync"). A missing `org_id` is refused. The first event for a `sub` writes its `subject_orgs` row. A later event with a different `org_id` is refused, audited and changes nothing. | `api/sync_handlers.go:29-133`, `sso/directory.go:60-74,181` |
| Admin role | K7: `kypost.admin` in the token or event, **and** `org_id == KYPOST_STAFF_ORG_ID`. In multi mode `KYPOST_STAFF_ORG_ID` is required, and startup refuses without it. A customer event naming `kypost.admin` is applied as a member and logged. | `sso/sso.go:334-345`, `sso/directory.go:24-31`, `api/sync_handlers.go:128-131` |
| Session | Unchanged, except that `currentUser` also kills a session whose user's org is not `active` in the `orgs` cache (Lifecycle). | `api/server_auth_session.go:1224` |
| URL, form or header | Never a source of org. The one exception is the staff org selection on admin routes (Admin surface). | — |

The org therefore comes only from a verified token, a signed webhook or a suite answer (T point 4).
`users.User.OrgID` is written only by provisioning from the signed directory event, and only once.
A later write with a different value returns an error from the users store. The two records are written in a
fixed order, so a retry always converges:

1. `subject_orgs` first, from the signed event. Its `(issuer, subject)` key is the idempotency key.
2. Then the account in `users.json`, with `OrgID`. This write mints the user ID
   (`users/users.go:56`). A retry looks the account up by `(issuer, sub)` (`GetBySSOSubIssuer`)
   before creating one, so it never mints a second ID. Whether today's native provisioning reuses
   an ID across a crash in its own steps is unverified; PR 2 must prove it with a crash test.
3. Last, `user_orgs`, keyed by the ID now in `users.json`. A crash before it is repaired on the
   retry, or by a reconciliation sweep run by the elected `api-sweepers` worker. The sweep adds
   the missing row for any multi-mode account that has `OrgID` and deletes any `user_orgs` row
   whose user does not exist.

A user whose two records disagree is refused at sign-in, as a mismatch is. The purge snapshot
(Lifecycle) runs the sweep for the org itself, then unions all three sources, so a half-provisioned
account is never missed.

Hosted members are provisioned the native-mail way. There is no JIT account at sign-in
(`api/sso_handlers.go:629-631`). An account appears when the signed `user.created` event has
been applied and a mailbox address has been allocated (Mail, open question 1).

**Staff accounts** come from the path administrators already use in native mode. A signed
directory event whose roles name `kypost.admin` skips mailbox allocation and creates a
mailbox-less, non-native account (`provisionDirectoryUser`,
`api/sync_handlers.go:138-141,175-190`; administrators own no mailbox,
`sso/native_provision.go:686-689`). Multi mode keeps that path and adds two rules:

- The event's `org_id` must equal `KYPOST_STAFF_ORG_ID`. It is written to `OrgID` exactly as for
  members. A `kypost.admin` event from any other org is applied as a member (K7).
- A staff-org account without `kypost.admin` gets no KyPost account. Staff-org accounts never get
  mailboxes, so such an event is a no-op, and that person's sign-in is refused like an
  unprovisioned one.

The first staff administrator therefore needs only KyIdentity's own bootstrap (T "Emergency
access and recovery") and the `kypost.admin` app role in the staff org. KyPost has no bootstrap
administrator of its own in multi mode (K6). Losing the role does **not** use today's role-change
path (`api/sync_handlers.go:155-166`). That path would leave a staff-org member, which the rule
above forbids. In multi mode a staff account that loses `kypost.admin` is deactivated and its
credentials revoked (`deactivateAndRevoke`, `api/sync_handlers.go:199-208`), and it never gets a
mailbox. Regaining the role reactivates it. The last-administrator refusal (`:213`) still
applies, so the last staff administrator cannot be demoted this way.

## Mail

### Mailbox addresses and domains in hosted mode

- **Domains.** One or more platform primary domains (`<platform-mail>`) and the separate relay
  domain for masked addresses (M D2). All are platform assets (K9), proved with today's
  `_kypost-mail` TXT bound to the one issuer (`sso/native_domain.go:52-63,369-372`).
- **Primary address.** Today it is the KyIdentity resource's single primary email, which must sit
  on a proven domain (`sso/native_provision.go:721-728,798-811`). In hosted mode a member's
  KyIdentity email is their sign-in address (T D5: username = email). It is usually on someone
  else's domain, so it cannot be the mailbox.
  **Owner answer (open question 1): KyIdentity is given the address, and KyPost provisions the
  matching mailbox.** Today KyIdentity has one `Email` per user (KI `internal/store/models.go:13`)
  and sends only that, as the primary SCIM email (KI `internal/sync/sync.go:147-155`). Inbound
  SCIM keeps only one (KI `internal/api/scim_inbound_handlers.go:572-590`). So KyIdentity gains a
  separate **mailbox address** per user:
  - Set when the member is added (billing's `POST /orgs` for the account holder, the member
    endpoints for the rest; T "Platform API"), and by staff. It must be on one of the suite email
    domains (below) and unique in KyIdentity.
  - Sent in the directory resource as a second `emails` entry of its own type, beside the sign-in
    email, under the org's signed events.
  - In multi mode, KyPost allocates the mailbox from that entry instead of the primary email,
    through today's `AllocateNativeAccount` (`sso/native_allocate.go:43-60`). KyPost's ledger is
    the final word on uniqueness: a taken address is the durable allocation failure it records
    today (`:107-110`), shown to staff. Single-tenant keeps the primary email, unchanged.
  This replaces the HR-style reservation this note first recommended (HR stays off in multi mode,
  K6).
- **Suite email domains.** KyIdentity #277 already holds the domain list: the
  `suite_email_domains` setting (KI `internal/store/suite_settings.go:18`), set by staff through
  `PUT /api/admin/suite-settings/email-domains` (KI `internal/api/server.go:359`) and read by apps
  at `GET /api/suite/email-domains` (`:248`). It is informational today, and KyPost does not read
  it: KyPost keeps its own `native-domains.json` (`sso/native_domain.go:68-74`). In multi mode the
  mailbox address field is limited to that list, and KyPost serves a domain only when it is both
  on the list and proved by its own `_kypost-mail` TXT (`sso/native_domain.go:52-63`). A domain on
  the list but not proved is refused, so a KyIdentity setting alone never makes KyPost accept mail
  for a domain. Single-tenant adopting the list is a separate change.
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
    **Mail sent during `deletion_scheduled` is lost if billing later resumes the org.** A `451`
    would not save it. Sending servers typically give up after about 5 days (unverified per
    sender), and the grace period is 7–90 days (T `schedule-deletion`), so most of it would bounce
    anyway, only later and less clearly. **The owner accepted this** (open question 11).
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
  the last phase. The 5000 is KyPost's own constant (`cfreceiving/wire.go:32-33`). Cloudflare's
  own documentation names no such limit; it documents 200 routing rules per domain and an
  apex-only catch-all (checked in the receiving tier spec, "Cloudflare Email Routing's real
  limits"). Raising it would not fix the disclosure.
- **Size and quota.** The 25 MiB cap is unchanged (`mailmsg/limits.go:38`). The per-mailbox quota
  is reserved at bind (`app/receiving.go:458-479`). Hosted mode applies the install-wide
  `KYPOST_MAILBOX_QUOTA_BYTES` and the optional `mailboxBytes` and `storageBytes` keys as hard
  limits (Abuse limits per org), then the org's storage pool ("Quotas"). The pool alone has
  open question 16's grace window (decided): a valid pool keeps accepting mail for 7 days or up
  to 10 % over (at least 2 GB), whichever ends first, then `452` for 3 days, then `552 5.2.2`. A lower quota refuses
  only new mail, never locks a mailbox (today's rule, root `AGENTS.md` "Mailbox limits").

### Recommended hosted receiving tier (open question 3)

**Answered (2026-11-21): the receiving tier has its own spec,
`docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`.** That spec is authoritative; the summary below is the
recommendation it expands.

What self-hosted uses today (`AGENTS.md:157`; `docs/guides/NATIVE_MAIL.md:22-37`): one of three
profiles per install.
- **Cloudflare Email Routing and a Worker** (recommended). The Worker stores each raw message in
  R2, and KyPost fetches it over outbound HTTPS, so no inbound port is open
  (`receiving-worker/continuous.mjs:304-312`, `docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md:16,109-110`).
  Continuous mode has been tested offline only (`docs/guides/NATIVE_MAIL.md:37`).
- **Maddy inside the KyPost container** on port 25, off by default (`Dockerfile:175`). It calls
  `kypost-server receiving bind` per recipient and `receiving accept` per message
  (`app/receiving_config.go:188-207`), which hand off to `receiving serve` over a same-user Unix
  socket (`app/receiving_serve.go:24-30,143-145`). The socket and the one port-25 container are
  why R refuses it on PostgreSQL (`replica/topology.go:33-34,62-64`, `docs/RUNNING.md:252-254`),
  not the ingress store, which is already a shared PostgreSQL schema (`ingress/pg_schema.go:12-28`).
- **An external IMAP account**, polled (`docs/RUNNING.md:271`). Off in multi mode (K6).

All three land in the one ingress store, and one replica holding the `daemon` role imports from it
(`docs/RUNNING.md:271`).

For thousands of mailboxes on more than one server, *recommend* a receiving tier we run, as its
own spec:
1. **Stateless MX hosts.** Two or more small hosts, each an MX record, run today's pinned Maddy and
   Rspamd. They hold no mailbox data.
2. **The socket becomes an authenticated internal call.** `bind` and `accept` reach `receiving
   serve` on the KyPost replicas over mutually authenticated TLS on a private network instead of
   `receiver.sock`. The checks, their order and the answers stay as they are (Inbound routing).
3. **`250` only after commit.** The MX host answers `250` only once `accept` has committed the
   message to the PostgreSQL ingress store, so losing an MX host loses no accepted mail. A replica
   that cannot be reached gives `451`, and the sender retries another MX.
4. **Import scales out.** Import moves from the single `daemon` replica to leases per mailbox, so
   several replicas import at once. Whether one importer keeps up at hosted volume is unverified;
   open question 8 measures it.
5. **More than one host.** R pins replicas to one Docker host (`replica/topology.go:31-32,80`). Going
   past one host needs R's follow-up (accounts and per-user files into PostgreSQL, Replicas), the
   JSON files beside `ingress.db` (sender blocks and evidence) moved into PostgreSQL, and
   PostgreSQL run as its own replicated service.
6. **The relay domain** for masked addresses uses the same tier, which answers M open question 5
   without a third party.
7. **Outbound** stays one relay profile per install (Outbound relay and DKIM). At hosted volume
   that is a sending provider or a pool of warmed direct-delivery IPs, decided with the
   reputation limits in Abuse limits per org.

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

Every limit an org can be sold, except the storage pool (KyIdentity's, "Quotas"), is a key in
that org's KyPost `quotas_json` (T `org_apps`), so a
plan, or one org, can be given any value without a code change. Today's per-user and install-wide
limits stay: they are the only limits in single-tenant mode (self-hosted and Enterprise), and in
multi mode they remain the per-account and per-mailbox layer under the org caps and the default
when a key is missing. Lines are at `origin/main` `e46f01c2`.

| Limit | Today, and where it stays | Per-org keys in `quotas_json` | A missing key |
|---|---|---|---|
| Sending, per account | 60 messages an hour and 1000 recipients a day, install-wide (`config/config.go:573`), with an optional per-account override (`users.User.SendQuota`, `users/users.go:65-66`; `sendQuotaFor`, `api/send_quota.go:26`). Kept in both modes. | `memberSendPerHour`, `memberRecipientsPerDay`: the per-account limit for that org's members, in place of the install default. A per-account override set by staff still wins. Not built (PR 7). | The install default |
| Sending, whole org | None | `sendPerHour`, `recipientsPerDay`, summed from `send_ledger` through `user_orgs` (built in PR 5, `chargeOrgSend`, `api/tenancy_lifecycle.go:173`) | No org cap (`:179-181`) |
| Masked creation | Per user, 100 live and 20 a day (`api/masked_addresses.go:31-37`). Kept as the single-tenant limit and the hosted default. | M D7: `maskedPerUser`, `maskedPerDay`, `maskedPerOrg`, plus M's platform-wide circuit breaker. PR 7. | 100 and 20 per user; no org cap |
| Mailbox storage | Per mailbox, the install's `KYPOST_MAILBOX_QUOTA_BYTES`, 256 MiB to 1 TiB (`config/native_mail.go:33-52`). Kept, and in multi mode it is the **ceiling**: receiving checks the mailbox's own limit first (`a.Limits`, `app/receiving.go:82`), then the org's (`:85`). That `a.Limits` always carries the install quota, which `app/app.go:159-163` reads at start, is unverified. | **The org's storage pool, from KyIdentity** (owner decision of 2026-11-23 on storage, "Quotas"): the number the customer buys. Under it, two optional safety keys stay: `mailboxBytes` per mailbox and `storageBytes`, KyPost's own share across the org, counting mail held for import (built in PR 5, `orgRoom`, `app/receiving.go:98`). Billing does not set them for a plan. No org value can raise a mailbox past the install's. That ceiling bounds one mailbox, not the pool, so a hosted install sets `KYPOST_MAILBOX_QUOTA_BYTES` to the largest single mailbox it allows, at most 1 TiB. A larger pool (Business from about 35 seats at 30 GB each) stays usable across the team's mailboxes, but no one mailbox grows past the ceiling. That is a per-mailbox safety limit the plan terms must state. | The pool has its own default ("Quotas"). A missing safety key: the install quota only. |
| Extra mailboxes | Administrator's choice | `mailboxes` (built in PR 5, `orgMailboxRoom`, `api/tenancy_lifecycle.go:220`) | No cap |
| Import and export | 2 at a time per process and 1 per user (`api/mail_import.go:34-35`, `api/mail_export.go:30-31`). Kept. | `transfersPerOrg`: imports and exports one org may run at once. Not built (PR 7, with the receiving tier's per-mailbox leases). | 1 per org, so one org cannot hold a server's slots |
| Seats | Not KyPost's | None: KyIdentity's `max_seats` (Plans and orgs) | — |
| Inbound connections | Maddy per IP and install-wide (`app/receiving_config.go:184-189`) | None. Recipients are per address, not per org, and M adds a refused-recipient budget per source. | — |
| Sender blocks | Install-wide, manual and automatic (`ingress/blocks.go:22-23`, `ingress/autoblocks.go:36-57`) | None. Platform-wide, staff only, no per-org blocks in v1 (no customer administrators). | — |

The quota names are proposals for KyIdentity's opaque `quotas_json` (T "Entitlements"). The
billing service sets them per plan, and staff may set them for one org. Parsing is PR 5's
(`parseQuotas`, `tenancy/orgs.go:111`): a known key that is not a whole non-negative number reads as
0, which refuses new mail and creations, never existing data. PR 7 adds the new keys to
`tenancy.Quotas` (`tenancy/orgs.go:33-38`) with the same rule.

**Storage is pooled in every plan** (owner decision, 2026-11-23; this answers open question 13).
The pool is KyIdentity's, not a KyPost `quotas_json` key ("Quotas"). A per-member allowance
inside a pool is not sold. If it ever is, `mailboxBytes` already expresses it with no code change.

## Admin surface

In multi mode the KyPost admin UI is a **staff console**. A route is admitted only for K7 staff,
with today's step-up (`withAdminStepUp`, `api/server_users.go:30`; `withActionDigest`,
`api/sso_stepup.go:56`). Every admin route declares one class. A route with no class fails the
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
records that a purge started and its last finished step. The purge is **idempotent and
resumable**:
- **Snapshot first.** The purge starts by running the `user_orgs` reconciliation for the org
  itself. It then records the org's user IDs in `org_purge_members`, taking the **union** of three
  sources: `user_orgs`, the `users.json` accounts whose `OrgID` is the org, and the accounts of the
  org's `subject_orgs` subjects. A crash between provisioning's steps can leave one source
  incomplete, but not all three. It records the mailbox IDs and `(issuer, subject)` pairs
  alongside, in the same transaction that creates the `org_purges` row. Every step and the final check are driven by that snapshot, never by
  `user_orgs`. A purge that dies after step 5 deletes `user_orgs` can therefore still find
  everything.
- A resumed purge re-runs every step from the start. Each step deletes whatever still exists,
  re-listing schemas, directories and rows for the snapshot's IDs rather than trusting the
  progress row.
- Before the purge is marked done, a final check walks the store classification and confirms that
  nothing of the org remains.
- No worker can write for the org once it is `deleted` (the Background workers rule in
  Isolation), so nothing reappears between steps.

Steps:

1. **Block new use.** Bump R's `session_revocations` for each of the org's users, which ends every
   session on every replica as suspension does. Delete the `agent_tokens` and
   `scheduled_sends` rows of the snapshot's users, and cancel their import jobs (R `import_jobs`).
2. **Receiving and quarantine first.** Delete `ingress.db` `routes` and `bindings` for the org's
   mailboxes. A delivery whose every binding was in the org is deleted. For a delivery shared with
   another org, only this org's bindings go.
   This comes before storage, so no pending delivery is left that an import could write back.
3. **Remove storage.** Each mailbox's removal runs under its receiving quota lock
   (`quota-<id>`), which fences a concurrent import write (Isolation, "Background workers"). For
   each of the org's users and mailboxes, drop the per-user and per-mailbox
   PostgreSQL schemas through the catalog (`dbbackend/dbbackend.go:205-247`). Then remove their
   directories under `CONFIG_DIR` and `STATE_DIR` (`users/<id>`, `mailboxes/<id>`), and delete
   their pickup records and import uploads. This step deletes the remaining credentials:
   - paired devices (`native_devices`, in the per-user `state.db`);
   - CardDAV app passwords (`carddav-auth.json`);
   - sealed IMAP and CardDAV client credentials (`imap-config.json`, `carddav-client.json`).
4. **Ledger tombstones (K10).** Each table in `native-authority.db`, by name:
   - `accounts`, `mailboxes`: kept as tombstones, with state `purged`, `state_root` cleared and
     the subject kept (an opaque KyIdentity ID, not personal data).
   - `addresses`, `address_history`, `routes_pending`: the org's rows are deleted. First, each
     address is written as an HMAC under the tombstone key into a new `address_tombstones` table,
     which the conflict check consults first (M open question 4). No plain address remains.
   - `directory`: kept, with `resource` (the SCIM body, which holds names and emails) set to NULL,
     so the revision fence still refuses a replayed event.
   - `directory_primary_emails`: the org's rows are deleted.
   - `subject_orgs`: kept. It holds only opaque IDs, and it stops a subject from ever joining
     another org.
   - `directory_events`, `logouts`, `sign_ons`, floors, barriers, `lifecycle_records`: platform
     replay and recovery ledgers keyed by event or JTI. They expire on their own and are not
     touched.

   Only the deletes and updates above use the named trigger exception (open question 9).
5. **Root store.** For the snapshot's users, delete their rows in `send_ledger` and in R's
   `sessions`, `auth_challenges` (by owner `user_id`), `import_jobs` and `session_revocations`.
   Then delete their `users.json` records, which hold the password hashes, sealed TOTP secrets,
   recovery codes and PGP keys, and their `user_orgs` rows.
6. **Record the purge.** Run the final check over the snapshot. Write a platform audit line with
   the org ID and the row and file counts, and no content. In one transaction, mark the
   `org_purges` row done and delete its `org_purge_members` snapshot.

- **One-way.** Step 4 is the only write that touches rows the authority triggers protect. It needs
  a named trigger exception keyed on a live `org_purges` row. It cannot be undone, which is the
  point. **The owner confirmed this** (open question 9).
- **Backups.** Whole-install capsules taken before the purge still hold the org's data until they
  age out of retention (Residual risks, open question 7).

### Delete everything (owner requirement)

The owner wants a **Delete all your stuff** button that someone can click and that sticks.
Today nothing does this. KyIdentity has no self-service deletion: its user `DELETE` routes cover
devices, launcher preferences, access requests, sessions and passkeys only (KI
`internal/api/server.go:181-203`). Org deletion is billing's `schedule-deletion` then `DELETE`,
with a 7–90-day grace and `resume` allowed until it ends (T "Platform API"). KyPost never deletes
a user's data: a disabled or deleted user only loses access (`api/sync_handlers.go:121-125`).

- **Where.** On KyIdentity's account page, because one click must reach every suite app. This is
  a T change.
- **What it deletes.** The clicker's own stuff, never anyone else's without warning. For the
  account holder, that is the whole org; a Personal org is just them. For any other member of an
  org with more members (Couple, Family or Business), it is only their own account. The rule is
  the same in every plan, because KyPost sees no plan (Plans and orgs). For a Business org the
  holder's click ends every seat's mailbox after the 7-day window (open question 14).
- **Confirming.** Step-up (password and second factor) and typing the mailbox
  address. The dialog offers the export first (Export), because nothing can be got back.
- **No undo.** Once confirmed, nobody can reverse it: billing, staff and the clicker cannot. A
  member's deletion sends a new signed `user.erased` event; today's `user.deleted` keeps meaning
  "disable", as now.
  - **A holder with no other live members:** the org goes straight to `deleted`.
  - **A holder with other live members** (the "household path" of the build plan and PR 6, which
    covers every plan with more than one member): the holder's own access ends at once, and the
    org enters `deletion_scheduled` with a 7-day grace that `resume` refuses (T allows `resume`
    today). Each other member is mailed a notice and can export their own mail in that window
    through the `export_only` claim (open question 4 option (a), approved by the owner; a T change
    that this path needs). The org is then `deleted`. Their mail is refused with
    `550` from the click, as for any scheduled deletion (open question 11).
  - T's own grace and `resume` stay for billing-driven deletion only.
- **In KyPost.** The org's next poll shows `deleted`, and the purge runs (Purge). `user.erased`
  runs the same steps for one user: its `org_purges` row is scoped to that user, and the snapshot
  holds only that user, their mailboxes and their subject.
- **A restore does not bring it back.**
  - **Deleted orgs stay listed.** KyIdentity keeps a deleted org's row and lists it to every app
    with status `deleted`, "so the app learns to drop its data" (KI
    `internal/store/suite_orgs.go:15-20,49-53`). A trigger forbids leaving `deleted` (KI
    `internal/store/tenancy.go:83,96`). So a poll after a restore reads the org as `deleted`
    and the purge runs again.
  - **Nothing is served until KyIdentity confirms.** A restored `orgs` cache predates the
    deletion and may still say `active`, so the restore empties it. Restore already leaves the
    server held until a release step checked against KyIdentity (`docs/RESTORE.md:290`,
    `:400-415`). In multi mode the release also needs a full poll from revision 0 and a read of
    the erased-subject list (a T change), both succeeding after the restore. Until then every
    customer org is missing from the cache, so sign-in is refused and receiving answers with a
    temporary refusal. A KyIdentity outage after a restore keeps the server unserved, which
    loses no mail.
  - That release step needs a local administrator today, which K6 refuses in multi mode. A
    staff-run release on a platform route is part of PR 6.
- **Nothing is reused.** The address tombstones (K10) refuse the address for ever. The kept
  `subject_orgs` row and `accounts` tombstone refuse provisioning that subject again.
- **Backups.** Older capsules still hold the data until they age out (open question 7). The hosted
  terms state that retention.
- **Tests.**
  - Click, purge, restore a capsule taken before the click, release, and poll: the purge runs
    again and the final check finds nothing. Do the same for one erased member of an org with
    other members.
  - Restore with KyIdentity unreachable: sign-in is refused, receiving answers `451`, and the
    hold cannot be released.
  - A click by a holder with other live members: the members are notified, can export for 7 days, and `resume` is
    refused.

### Export

Users export their own mailboxes and folders as mbox or EML zip, self-service, as today (root
`AGENTS.md`). There is no org-wide or staff export (Admin surface). During `deletion_scheduled`,
KyIdentity's export-only session issues no tokens today (T "Views"). **The owner approved open
question 4 option (a):** KyIdentity issues an `export_only` claim in that window, and KyPost admits
only the export routes under it. Every other route answers as for a non-`active` org.

### Quotas

Every limit but storage comes from each org's `quotas_json` in the cache (Abuse limits per org,
which lists every key and the per-user limit each sits on). A lower quota refuses new mail and
new creations only, never existing data. That matches today's mailbox-quota rule and T's
"over_limit, nobody is disabled".

**Storage: the org pool (owner decision, 2026-11-23, required for launch).** One number per org,
shared by every member and every app, is held by KyIdentity. Each app checks it before a write
that adds bytes and reports its own usage. KyPost's side is built against KyIdentity's contract,
except at receiving ("Org storage pool as built"). KyIdentity's half is T's
amendment (board `kyidentity-business-orgs`). It has no PR yet, so the field and route names
below are this note's proposals, and how T names them is unverified.

*What KyPost needs from KyIdentity.*
- **The pool.** Each org's limit in bytes and the usage the other apps last reported. It arrives
  with the org entry in `GET /api/suite/orgs`, which KyPost already polls (K8), and is cached in
  the `orgs` row, so no write waits on a network call.
- **A usage report.** One authenticated call per poll, carrying `{org_id, used_bytes, as_of}`
  for each org KyPost holds data for. KyIdentity binds the report to the calling client, so one
  app can never report for another, and keeps only the newest `as_of` per app and org, so a
  replayed or reordered report cannot lower a usage figure.

*What KyPost counts* (exact, from PostgreSQL, summed over the org's mailboxes and members):

| Counted | Source |
|---|---|
| Every stored message in every folder: Inbox, Sent, Drafts, Trash, archive, and incoming encryption's replacements. Attachments are inside the message. | `usage.payload_bytes` per mailbox, kept by the triggers (`mailbox/pg_schema.go:43-51`) |
| Queued sends and the Sent copy reserved for each | The same counter (`mailbox/store.go:133-134`, `mailbox/pg_schema.go:34-39`) |
| Mail held for import | `HeldBytes` (`app/receiving.go:77`), as `orgRoom` counts it today |
| Contact photos | The per-user total `contactPhotoBytesUsed` reads (`api/contacts_photo.go:196`) |
| A body moved to object storage | Counted at its size, as if it were in `raw` (MH "Object storage for message bodies") |

Not counted: exports (streamed, nothing stored, `api/mail_export.go:22-26`), import uploads
waiting to run (scratch, deleted with the job), settings and other small documents, indexes,
logs and backups. Counting a backup would charge a customer for the platform's own safety copy.

*Where KyPost checks.* The room is the pool limit, minus the other apps' last reported usage,
minus KyPost's own usage now. Every write that adds bytes checks it, at the points that already
check the mailbox quota:
- receiving acceptance, after the mailbox's own check (`orgRoom`, `app/receiving.go:85`);
- every new message, which covers drafts, Sent copies, client uploads and imports
  (`mailbox/store.go:428`). **Not** the held-mail importer's move of a delivery receiving
  already accepted (a gateway receipt, `gateway != ""`). Receiving charged it while it was held,
  and refusing the move would only strand mail that got `250`. This is the rule `checkReserve`
  already follows (`mailbox/store.go:431-434`), so open question 16's grace mail always reaches
  the mailbox;
- **Not** filing a Sent copy that its send's enqueue already reserved (`FileOutboundSent`,
  `mailbox/outbox.go:456`). The reservation was charged against the pool at enqueue (the
  enqueue check below), and today's code already never refuses this filing
  (`mailbox/outbox.go:218-219`). So a send enqueued under the pool still files its Sent copy if
  the org goes over before delivery;
- a send's enqueue, which reserves its Sent copy (`mailbox/outbox.go:216`);
- a contact photo upload (`storeContactPhoto`, `api/contacts_photo.go:141`).

PR 5's statement that imports, drafts and Sent copies are bounded by the install quota only
("PR 5 as built") is superseded for the pool.

*What over-pool does.* The owner approved the rule on 2026-11-23 (ky-launch#4): new uploads and
new stored content are blocked until the org is back under the pool, no data is ever deleted, and
the holder is warned before a seat change that would leave the org over. The warning is
KyIdentity's and billing's, because the seat change happens there. Receiving follows open
question 16, decided by the owner on 2026-11-23.
Each surface takes the safest default:

| Surface | Over the pool |
|---|---|
| Receiving | Open question 16, decided 2026-11-23: 7 days' grace with up to 10 % of the pool over (at least 2 GB), whichever ends first; then `452 4.2.2` "Mailbox full" (exit 9) for 3 days; then `552 5.2.2` (exit 14, hosted profile only). Org status answers win. |
| Sending | A new send is refused at enqueue with today's capacity answer and a message naming storage, before anything is sent. A send already queued goes out, because its Sent copy was reserved at enqueue. Sending without a Sent copy is not offered: it would lose the copy silently. |
| Drafts, copies, client uploads, imports | A write that adds bytes is refused with the capacity answer. An import stops at the first refused message and reports how far it got. The existing draft is kept. |
| Sync, read, search, export | Unchanged, always allowed. |
| Delete, move, flag, empty Trash | Always allowed, so a user can always free space. The room KyPost frees counts at once in KyPost's own checks, and in other apps after the next report. |

The UI shows the pool's usage with today's 80% and 95% warnings (root `AGENTS.md` "Mailbox
limits"), and names the pool, not the mailbox, as what is full.

*Failure defaults.*
- **No pool in an org's entry** (KyIdentity not upgraded, or a malformed value): the pool reads
  as 0, the same rule as a malformed key (`parseQuotas`). Nothing new is accepted, receiving
  answers the temporary `452` at once with no grace, and no data is lost. The pool is a launch requirement, so K PR 7
  must not accept `multi` until KyIdentity publishes it and KyPost's checks are built (open
  question 15).
- **A stale cache** keeps the last known pool and usage, as receiving keeps the last known
  status (open question 5). Other apps' growth since their last report can then overshoot the
  pool by at most one poll interval of writes. KyPost's own part is always exact.
- **A report that fails** is retried on the next poll. KyIdentity keeps the last good one.
- **Concurrent writes** in one org can pass the check together, as `orgRoom`'s ponytail note
  says (`app/receiving.go:94-97`). The overshoot is bounded by the writes in flight. The pool is
  a billing limit, not a security boundary, and the per-mailbox ceiling still holds.

*The staff org* (`KYPOST_STAFF_ORG_ID`) is exempt from the pool, as it is from the cache (K8).
Its mailboxes, which include abuse and complaint handling on the platform domains, are bounded
only by the per-mailbox limits. Whether `/api/suite/orgs` lists the staff org with a pool is
unverified, and the exemption makes the answer irrelevant.

*Single-tenant* (self-hosted and Enterprise) has no pool: there is no `tenancy.db`, so every check
above is skipped, as `orgRoom` skips today (`app/receiving.go:99-101`). Enterprise's custom size
is the install's `KYPOST_MAILBOX_QUOTA_BYTES`, a per-mailbox limit. An Enterprise contract
sold as a pooled figure cannot be enforced by KyPost as written: that would need the pool on a
single-tenant install, which is not designed.

*Negative tests the build must carry:* wrong org (a report or a check never counts another org's
mailbox), a wrong client (KyIdentity refuses a report signed by another app), a replayed or older
`as_of` (KyIdentity keeps the newer figure), a stale cache, a missing or malformed pool, an
oversize or negative usage figure, an org at exactly the limit, mail accepted during grace that imports while the org is over, a send enqueued under the pool whose org goes over before delivery (its Sent copy still lands), a deletion that frees room, and
single-tenant mode, where the pool is never read and the upgrade test is unchanged.

## Replicas (PR #677)

R runs several app replicas on one host, with one PostgreSQL and shared volumes. Multi mode must
also run across hosts (below, "Several hosts"). The `.db` stores
are in PostgreSQL. Accounts (`users.json`), per-user JSON and `config.yaml`, DKIM keys and the
secret keys stay on the volumes behind host-local file locks (`docs/RUNNING.md:240-245,287` at `origin/main` `3e467f2b`).
Tenancy fits on top:

- **Mode and staff org stay out of R's fingerprint.** Adding them would make R's `Diff` merge new
  entries into every upgraded install's fingerprint record (R `replica/config.go:138-152`). That
  write would break decision 1. Instead, K1's own record guards replicas:
  - A multi install records `tenancy_mode=multi` and `staff_org_id` in `meta` at first start. A
    replica started with `KYPOST_TENANCY` unset or `single`, or with another
    `KYPOST_STAFF_ORG_ID`, refuses.
  - A start with `multi` and no record writes the record only if the install is empty (K2). It
    refuses when the database or volumes already hold data. The check and the write run as one
    step under R's `replica-config` lock (R `replica/config.go:167`), so the first replica writes
    the record and the others then find it. On SQLite, which multi mode refuses anyway (K2),
    nothing is written.
  - With no record, unset and `single` are the same, so explicit-versus-unset can never disagree.
  - A single-tenant install writes nothing: no `meta` key and no fingerprint entry.
  - PR 1's upgrade test starts an R-format replica install under the new binary, once with
    `KYPOST_TENANCY` unset and once with `single`. Both start with no refusal and leave every
    database row, the fingerprint record included, and every file byte-for-byte unchanged.
- **Shared org state.** The `orgs` cache and `org_purges` live in the multi-only `tenancy.db` on
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
- **Per-replica meters** allow up to N times their limit with N replicas (`docs/RUNNING.md:282` at `3e467f2b`,
  "O4"). Per-org quotas must not use those in-memory meters. They read PostgreSQL (`send_ledger`)
  or the ledger, so they hold across replicas. MH "Per-replica meters" lists every meter and moves
  the ones that reach a third party or unauthenticated callers into R's shared windows (MH6).
- **Several hosts (owner decision, 2026-11-22).** A multi install must run as several replicas
  on several hosts, not only on one. R's follow-up, which moves `users.json`, the per-user files
  and the other volume files into PostgreSQL, is therefore a hard requirement of multi mode, not a
  launch prerequisite outside the plan. It is specified in
  `docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md` (**MH**): the volume inventory
  with a target per file, every `flock` and host check and what replaces each, and PRs MH1–MH10
  (MH1–MH7 multi-host, MH8 the self-hosted opt-in, MH9 and MH10 regions).
  In multi mode, R's boot-ID check (`replica.ErrHost`) and volume IDs give way to MH's
  volume-free assertion. Single-tenant installs keep them unless an operator migrates the install
  with `migrate-volumes-to-postgres` (MH "Self-hosted multi-host", owner decision 2026-11-22). The
  isolation phases (PRs 1–6) do not depend on MH.
- **Several regions (owner decision, 2026-11-22).** Multi mode runs as one cell per region, each a
  complete install with its own PostgreSQL, and each org lives in one home region (MH "Regions").
  K's design holds per cell. A cell caches and serves only its own region's orgs.
- **Receiving.** R refuses Maddy on PostgreSQL (Mail, open question 3). The receiving tier spec
  (`docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`) replaces it in multi mode.

## Downstream contract, point by point (T "Tokens and suite contracts")

| # | T requires | KyPost multi mode | Proof |
|---|---|---|---|
| 1 | Refuse a token without `org_id` | ID token, device-assertion token and logout token without `org_id` are refused. KyPost accepts no access token. | Token tests per input (Identity binding) |
| 2 | Key every row by `org_id` | `org_id` on the two roots (`user_orgs`, `subject_orgs`). Every other store is derived from its owning ID (K3, K4, Store inventory). | `TestEveryStoreIsClassified` (Phased build plan, PR 1) |
| 3 | Bind accounts to `(sub, org_id)` and refuse a known `sub` with a different `org_id` | Write-once `OrgID`, `user_orgs` and `subject_orgs`. A mismatch at sign-in, device sign-on, logout, webhook or peer call is refused like a wrong issuer. | Mismatch tests per input, `TestCrossOrgMatrix` |
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
refused at startup until PR 7. Until then multi-mode code runs only in tests. PR 2 onward needed
KyIdentity's T PR 3 (`org_id` in tokens, logout tokens and webhooks, and `/api/suite/orgs`),
which has merged as KyIdentity #276. So PRs 1–5 and PR 6's org purge can start now. Four parts
wait, each on a KyIdentity change listed under the plan:
- PR 6's one-user purge needs `user.erased`.
- PR 6's household path needs the `export_only` claim, and KyPost admitting only the export
  routes under it (open question 4 (a), approved).
- PR 6's restore release needs the erased-subject list.
- PR 7 needs the mailbox address field, and also the receiving tier (open question 3, specified in
  `docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`). Its per-mailbox import leases are part of that tier. PR 7 also
  needs multi-host replicas, PRs MH1–MH7 of
  `docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md` (amendment, 2026-11-22, below),
  and MH9, the cell record (second amendment, below).

1. **Mode and inventory.** `KYPOST_TENANCY` (refused if `multi`), the multi-only `meta` record,
   the multi-only side tables (`orgs`, `user_orgs`, `subject_orgs`; PR 6 adds the purge tables), `users.User.OrgID`
   (`omitempty`), and `KYPOST_STAFF_ORG_ID`. Tests:
   - `TestEveryStoreIsClassified` requires every table in every schema (`state`, `mailbox`,
     `ingress`, `cfreceiving`, `sso/authority_*`, and R's tables once merged) and every registered
     file store to have exactly one class. A new store with no class fails the build.
   - A single-tenant upgrade test: a fixture install started under the new binary keeps
     `users.json`, every schema dump and every file byte-for-byte identical. That covers a SQLite
     install and an R replica install, with `KYPOST_TENANCY` unset and with `single`.
   - The full `go test -race ./...` and the `postgres` job.
2. **Identity binding.** `org_id` parsing and refusal for ID tokens, device-assertion tokens,
   logout tokens and sync events. Write-once `OrgID`, `user_orgs` and `subject_orgs`, and the K7 admin rule.
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
6. **Purge.** Creates `org_purges` and the `org_purge_members` snapshot table (Lifecycle), the purge steps, the tombstone key, the `address_tombstones` table, and
   the named trigger exception (confirmed, open question 9), and the one-user purge for
   `user.erased`, and the multi-mode restore release (Delete everything). Tests: a purge killed at every step resumes and finishes;
   nothing of the org remains, checked against the classification; tombstones refuse reuse,
   including after a restore with the same key; the Delete everything tests.
7. **Hosted mail.** Allocation from KyIdentity's mailbox address field, limited to the suite email
   domains (Mail), M's masked changes 1–7, per-org abuse limits, and the hosted receiving tier
   (open question 3; built as that spec's PRs 1–5, `docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`). Startup then accepts `multi`. Tests: M's hosted verification list, and a native-mail harness run
   (`docs/DEVELOPMENT.md`) against KyIdentity in multi mode.

**Amendment, 2026-11-22 (owner decision: multi-tenant needs multi-server).** PR 7 must not
accept `multi` until multi-host works. Startup keeps refusing `multi` until **all** of MH1–MH7
(`docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md`, "Phased build plan") and RX
PRs 1–5 have merged. Where it accepts `multi`, PR 7 adds MH's prerequisites to the K2 checks:
`sslmode=verify-full` for a database that is not on loopback, and a provisioned, complete
`SECRET_DIR` (MH5). It also adds the container two-host harness, two containers with separate volumes on one database, which needs a startable multi mode. MH7 has only the in-process two-host test. MH sits against this plan as follows:
- MH1 can start now.
- MH2 needs PR 6's addition of `tenancy.db` to the backup set.
- MH4's catalog tombstone is the purge's database-level fence, so PR 6's final check asserts it
  once MH4 has merged.
- PR 6's purge must also cover `contact-photos/`, which `TestEveryStoreIsClassified` does not list
  yet (MH inventory).

**Second amendment, 2026-11-22 (owner answers to MH's open questions).**
- **Multi-region is required.** PR 7 accepts `multi` only after MH9 as well: `KYPOST_REGION`
  recorded with K1's `meta` record from the first multi start, the `orgs` poller serving only
  the cell's own orgs (other regions' orgs kept as foreign ID and region only), and sync events
  for a foreign org refused permanently without a write (an org the poll has not returned yet
  gets a temporary refusal until the next poll). PR 7
  builds M's D3 relay local part with MH9's region character, so every hosted masked address
  carries its region from the first one. PR 7 owns that test: masked allocation only under the cell's code, with the
  code outside the 6 random characters. MH10 (cross-region relay routing on the MX hosts) must
  merge before a second region opens, not before PR 7.
- **Self-hosted multi-host is opt-in.** MH8 (`migrate-volumes-to-postgres` and its reverse) is not
  a gate for PR 7. Every MH PR from MH2 on selects its tables by table mode (multi, or a migrated
  single install), and the single-tenant upgrade test asserts that an un-migrated install has no
  `volumes.db` and no `volume_stores` row. MH2 first moves `documents` from `tenancy.db` into
  `volumes.db`, so `tenancy.db` keeps meaning multi mode.
- **Encryption on every hop.** PR 7's K2 checks also include every refusal in MH's "Encryption in
  transit" table, which is the authoritative list. Today that is: a plain HTTP listener off
  loopback; `sslmode=verify-full` on the database, PgBouncer and replication connections; and TLS
  to KyIdentity, the peers, KyRecovery, the push relays, Ollama and the outbound SMTP relay,
  each unless on loopback.
- **Staff step-up per org** (PR 4 owner answer 5) has merged (#703).

Launch prerequisites outside this plan: a measured capacity target (open question 8), PostgreSQL
as its own replicated service with a synchronous standby (RX R5, RX "Past one host" step 3), and
these KyIdentity changes:
- the mailbox address field, sent as its own `emails` entry (Mail);
- the Delete everything button, the holder's paths (straight to `deleted`, or a 7-day member
  export window that `resume` refuses, with a notice to each member), `user.erased`, and the list
  of erased subjects (Delete everything);
- the `export_only` claim during `deletion_scheduled` (open question 4 (a), approved), which the
  household path needs;
- for MH9: a home `region` on each org in `/api/suite/orgs`, sync events sent only to the org's
  home cell, a home region on each suite email domain, and mailbox addresses given only on the
  org's own region's domains (MH "Regions").

## PR 1 as built (2026-11-21)

PR 1 follows the plan with these readings. Each is the safer one where K was ambiguous or, for
the first, unworkable as written. References are to the PR's own head.

- **The side tables are one multi-only store, `STATE_DIR/tenancy.db`** (kind `tenancy`, schema
  `kypost_tenancy`, `tenancy/tenancy.go`), not tables in the root `state.db` and
  `native-authority.db`. Read in code, not run:
  - the authority on PostgreSQL opens only when its catalog fingerprint equals its DDL exactly
    (`sso/authority_postgres.go:162`, finding S3, as SQLite at `sso/authority_v3.go:34`), and its
    rebuild drops the schema (`:233`), so an extra `subject_orgs` would make every open fail and
    a rebuild lose it;
  - a PostgreSQL export to a SQLite capsule refuses a source table the binary's SQLite schema
    lacks (`dbbackend/copy.go:189,451`), so an extra table in `state.db` would break backups of a
    multi install.
  `tenancy.db` is not in the backup set yet (`backup/collect.go:36`); PR 6 adds it with the purge
  tables, which belong in the same store. Moving the tables loses no atomicity K relied on: the
  three provisioning writes were already separate, ordered steps (Identity binding).
- **Write-once** is a `BEFORE UPDATE` trigger on `user_orgs` and `subject_orgs` on both backends.
  Deletes stay possible for the reconciliation sweep and the purge. SQLite's `INSERT OR REPLACE`
  deletes and inserts, so it would bypass the trigger: writers use plain `INSERT`.
- **The record.** `tenancy_mode` and `staff_org_id` are inserted in one transaction and never
  replaced (`state/tenancy.go`). A record with only one key, an unknown mode or a malformed staff
  org is refused as damaged, in either mode. A single start reads the record without the lock and
  writes nothing.
- **`KYPOST_STAFF_ORG_ID`** must be T's `org_id` in canonical lowercase UUID form; `single` (or
  unset) refuses a set value, so a half-configured hosted install cannot start self-hosted.
  Neither variable is in `.env.example` or the Compose file while `multi` is refused.
- **Empty install** (K2, Replicas) means no account in `users.json`, no entry under
  `CONFIG_DIR/users`, `STATE_DIR/users` or `STATE_DIR/mailboxes`, and no `native-authority.db` or
  `receiving/ingress.db` (`tenancy.Empty`). An error deciding it refuses.
- **K2's other prerequisites** (PostgreSQL, native mail, v3, an issuer) and the SQLite refusal
  are not checked in PR 1: `config.CheckEnv` and `bootstrap-admin` refuse `multi` outright, so
  `tenancy.Check` with `multi` runs only in tests, on both backends. PR 7 adds them where it
  accepts `multi`.
- **The upgrade test** (`TestSingleTenantUpgradeIsByteForByte`) builds the fixture with the same
  binary (bootstrap, first start, one sign-in), then restarts it with `KYPOST_TENANCY` unset and
  `single`. It compares every file's bytes except SQLite's `-shm` index, every SQLite schema and
  row, and on the PostgreSQL job every catalogued schema's columns, indexes, triggers and rows,
  the fingerprint record included. The children's HTTPS proxy is a closed port: the release
  check would otherwise record the newest GitHub release in `meta` on whichever start first
  reaches GitHub, which is a restart effect, not tenancy. A fixture from an older binary is not
  built (unverified against a released image).

## PR 2 as built (2026-11-21)

PR 2 follows the plan with these readings, each the safer one where K was ambiguous. Paths are
under `backend/internal/` at the PR's head; "KI" here is KyIdentity `origin/master` at `39d7d30`.
Every multi-mode path is reached from tests, because startup still refuses `multi`: the tests set
the server's tenancy store and staff org, and the users store's `StaffOrg`, directly
(`api/tenancy_binding_test.go`).

- **Where `org_id` is read.**
  - **ID tokens.** `SSOTokenClaims` gains `OrgID` and `ExportOnly` as raw JSON, decoded only by
    multi-mode code (`TenantClaims`, `sso/sso.go:325-338`). A typed string field would make a
    single-tenant install refuse every token whose provider sends a non-string `org_id`, and
    single mode must not change.
  - **Sync events.** The `org_id` is read from the signed body in multi mode only
    (`bindDirectoryOrg`, `api/tenancy_binding.go:91`). KyIdentity sends it at the top level of
    the SCIM body (KI `internal/sync/sync.go:130,475`). `sso.DirectoryUser` does **not** gain
    the field, as K said it would. The directory state stores the parsed struct
    (`sso/directory.go:227`), so a field there would change what a single install stores when
    an event carries `org_id`. The org lives in `subject_orgs` instead.
  - **Logout tokens.** The `org_id` is read from the verified token's payload (`sso.LogoutOrg`),
    because ky-primitives `oidcverify.LogoutClaims` does not return it.
- **Valid means canonical.** A token, event or logout `org_id` must match PR 1's lowercase UUID
  form (`config.ValidOrgID`), so a case or format variant is refused like a missing claim.
- **Sign-in.** After the code exchange, a missing, malformed or export-only token gets the answer
  a failed exchange gets today (`api/sso_handlers.go:525`), so the refusal covers step-up too.
  `resolveSSOUser`, shared by the browser and device sign-on, then reads the token's org before
  it looks the account up (`:656`). It refuses a bad org, or an account bound to another org,
  with the wrong-issuer answer (`ErrNativeAccountConflict`), whether or not the account exists.
  The refusal also writes a security log line that names no org, subject or address. For device
  sign-on the `jti` is spent first, as it is for every refusal after the token is verified.
- **No account from a token.** In multi mode `resolveSSOUser` never auto-provisions, even with
  `AutoProvision` set (`:704`), because `OrgID` is written only from a signed event. K6's `404`
  for the setting itself is PR 4's.
- **SSO link and unlink answer `404` in multi mode now** (`api/sso_handlers.go:307,621,775`), not
  in PR 4 with the other K6 refusals. An org-bound account's subject is write-once, and relinking
  a staff account to another org's subject would strand that subject's events.
- **`export_only` is refused** at sign-in and device sign-on. K assigns the export-only route gate
  to PR 6's household path (Phased build plan). Admitting the claim before that gate exists would
  give an export-only token a full session.
- **Logout** (`logoutOrgStatus`, `api/sso_logout.go:181`). Before any session is touched, a multi
  logout token needs a valid `org_id` **and** a `sub`. A `sid`-only token cannot be checked
  against an account's org, so it is refused. KyIdentity always sends `sub` (KI
  `internal/oauth/backchannel.go:33`). The `sub`'s account must be bound to the token's org. A
  `sub` with no account under this issuer is admitted, as today, because no session can be live
  for it.
- **Three records, one check.** `checkAccountOrg` (`api/tenancy_binding.go:45`) requires
  `users.json` `OrgID`, `subject_orgs` and `user_orgs` to name the same org. It writes a missing
  `user_orgs` row only after the first two agree. It runs at sign-in, device sign-on, logout and
  every directory event for an existing account. Any disagreement is refused and changes nothing,
  as K says, even for a deactivating event. A disagreement means the records are damaged, and
  KyIdentity alone cannot repair them.
- **Write order and repair.**
  1. A verified event for an existing account is compared with the account's `OrgID` first, so an
     event for another org never writes a binding.
  2. The event then binds `subject_orgs` (`tenancy.BindSubject`) before the revision fence. So an
     event the fence then refuses as stale still binds its subject. That is harmless, because
     KyIdentity's `org_id` is immutable.
  3. Provisioning writes the account with `OrgID` taken from `subject_orgs` (in
     `provisionDirectoryUser`, and through `AllocateNativeAccount`'s new `org` argument).
  4. `user_orgs` is written last. A row a crash left unwritten is written by `checkAccountOrg` on
     the next event or sign-in. That is K's "repaired on the retry".

  **The reconciliation sweep is not built.** It has no reader before PR 5's per-org aggregates
  and PR 6's purge snapshot, which runs it itself. A sweep that deletes rows must list `user_orgs`
  before it lists accounts, or it races a new account's step 4.
- **Plain `INSERT`.** The binds use `INSERT … ON CONFLICT DO NOTHING`, then compare the stored org.
  That statement never replaces a row, unlike `INSERT OR REPLACE`, so PR 1's `BEFORE UPDATE`
  triggers still guard every row. Another org is `tenancy.ErrOrgMismatch`. A directory event gets
  `422`, not `409`, which the sender counts as success on `user.created`.
- **The users store holds the account rules itself** (`orgInvariants`, `users/users.go:570`),
  because many writers bypass the generic mutation path. Every `users.json` write in multi mode
  compares the new file with the one on disk (`writeFileUnlocked`, `:995`):
  - `OrgID` never changes (`ErrOrgIDWriteOnce`).
  - An org-bound account keeps its issuer and subject.
  - No write makes an administrator outside the staff org (`ErrCustomerAdmin`). That covers
    `SetRole` from the admin routes, a local `Create`, and the restore repair that copies
    KyIdentity roles (`sso/native_recovery_repair.go:55`), which now fails closed.

  A retried native publication naming another org is `ErrNativeAccountConflict`. Single mode
  sets no `StaffOrg`, so it reads and checks nothing more.
- **K's crash test.** Allocation reserves the mailbox ID in the ledger before `users.json` is
  written. A retry after a crash between the two publishes that ID, never a second one
  (`TestNativeAllocationRepairsAcknowledgedUnpublishedReservation`, and with an org,
  `TestNativeAllocationKeepsItsIDAndOrgAcrossACrash`).
- **K7 on directory events is applied by rewriting the event before the fence**
  (`api/tenancy_binding.go:125-132`):
  - A customer event naming `kypost.admin` is stored and applied with `roles: []`. Native
    allocation, HR and address code read the stored roles, so they all see a member and allocate
    a member's mailbox.
  - A staff-org event without the role is applied as `active: false`. An unknown subject then gets
    no account. An existing staff account goes through `deactivateAndRevoke`, so the
    last-administrator refusal answers `422`. Regaining the role reactivates the account.
- **K7 at sign-in.** In multi mode an SSO session's administrator ceiling (`ssoAppAdmin`,
  `api/tenancy_binding.go:35`) needs the token's own `kypost.admin` role and the staff org. There
  is no group fallback.
- **Not covered, by phase.**
  - The restore release reads KyIdentity profiles' roles (`sso/native_release.go:204`). The
    multi-mode restore release is PR 6's.
  - Peer bodies are PR 3's ("PR 3 as built").
  - HR and the remaining K6 refusals are PR 4's.
  - KyIdentity's `group.*` bodies carry no `org_id` (KI `internal/sync/sync.go:455`). KyPost's
    webhook refuses every type except `user.*` (`sso/directory.go:104`).

## PR 3 as built (2026-11-21)

PR 3 follows the plan with these readings, each the safer one where K was ambiguous. Paths are
under `backend/internal/` at the PR's head. Peer specs were read at KyCalendar `origin/main`
`08bd045` and KyDrive `origin/master` `7fa91dc`. Startup still refuses `multi`, so the tests
build the multi paths as PR 2's do (`makeMultiTenant`).

- **`AuthContext.OrgID`** (`api/server_auth_session.go:125`) is copied from the re-read user
  record by the four builders: the session (`currentUser`, `:1325`), the paired-device arm of
  `resolveMailAuthContext` (`deviceOrg`, `api/tenancy_gate.go:119`), CardDAV Basic
  (`api/dav_auth.go:258`) and MCP agent tokens (`api/mcp.go:181`). In multi mode an account
  without an org is refused as each path refuses a dead credential.
  - The refusal is also made in `deviceAuthSnapshot` (`api/device_auth.go:161`). The
    `withDeviceAuth` handlers never build an `AuthContext`, so a check only in the builders
    would have missed them.
  - The org status check K puts in the same builders is PR 5's.
- **`orgAdmit(ac, target users.User)`** (`api/tenancy_gate.go:26`) compares the caller's org
  with the target account's `users.json` `OrgID`. It reads no `tenancy.db` row, because PR 2
  keeps the three records in step. In single mode it admits everything and compares nothing.
  Every hop's refusal is the answer the route gives a missing target.
- **The hops, as built:**
  - **Extra mailbox:** `admitMailbox` (`api/native_mailbox_handlers.go:57`) adds `orgAdmit`
    after the ledger's owner check. The ledger already refuses another user's mailbox, so this
    is the second, independent check K5 asks for. `TestTenancyMailboxHopNeedsTheOrg` reaches it
    with a forged `AuthContext`.
  - **People search and the KyDrive recipient check:** `suitedir.Person` has no `org_id`
    (ky-primitives at the `backend/go.mod` pin, `suitedir/client.go:85-89`), so KyPost cannot
    "drop any answer whose `org_id` is not the caller's" from the answer itself.
    `sameOrgPeople` (`api/tenancy_gate.go:47`) keeps only people whose subject this install
    has bound to the caller's org in `subject_orgs`. An unknown subject is dropped, so a
    member KyPost has not been told about yet is not suggested.
  - **KyMessages address resolution** is not filtered. It is unreachable: see the peer list.
  - **Address reassign and quarantine release to the current owner:** `addressOneOrg`
    (`api/tenancy_gate.go:60`) requires every mailbox in the address's history, and the target,
    to belong to accounts of one org. The history comes from `NativeAddressMailboxes`
    (`sso/native_address.go:86`).
    - A reassign refused this way answers like an unknown mailbox (`404`,
      `api/native_address_handlers.go:83`).
    - A release answers `409`, as K says (`quarantineOneOrg`,
      `api/native_quarantine_handlers.go:136`).
    - The check runs before the ledger lock. That is safe because every reassign makes the
      same check, so the history cannot gain another org in between.
    - Both routes are administrator-only. Their tests are function-level
      (`TestTenancyAddressMovesStayInOneOrg`, which also proves that an unreadable users store is
    an error, not an org refusal); the handler path in multi mode is unproven until
      PR 4 can make a staff administrator with a step-up.
  - **Staff on an org-bound admin route** is PR 4's.
- **Peer bodies, in step with each peer's own spec:**
  - **KyCalendar** (its D10, and its contract table at spec lines 468-470). KyPost adds
    `org_id` to `kypost.itip.inbound` (`processor/kycalendar_forward.go:111`),
    `kypost.itip.respond`, `kypost.ooo.set` and `kypost.task.flag` (`withOrg`,
    `api/tenancy_gate.go:100`). `kycalendar.itip.send` and `kycalendar.task.state` must carry
    the account's `org_id` (`peerOrgAdmit`, `api/kycalendar_handlers.go:135`,
    `api/flag_tasks.go:247`). KyCalendar at `08bd045` sends none, so a multi install refuses
    both with `404 no_user` until it does.
  - **KyDrive** (its spec lines 489-490): every body adds `org_id` (`driveEvent`,
    `api/kydrive_handlers.go:179`).
  - **KyMessages** has no tenancy spec, and KyMessage-Server sends and reads no `org_id`.
    Multi mode answers every `/api/kymessage/*` route `409 kymessage_off`
    (`api/kymessage_handlers.go:82`) and sends no presence body (`api/kymessage_ooo.go:27`).
    It sends nothing rather than guess a field.
  - **KyPulse** has no tenancy spec. Multi mode requires the account's `org_id` as for
    KyCalendar (`api/kypulse_handlers.go:135`), so every alert is refused today. Its decoder
    rejects unknown fields, so single mode still answers `org_id` with `400 bad_body`
    (`strictPeerOrg`, `:114`). Multi mode reads the field.
  - The `org_id` field is added only for an account with an `OrgID`, which exists only in
    multi mode, so a single-tenant body is byte-for-byte unchanged.
- **HR mailbox reservation answers `404` in multi mode now** (`api/hr_mailbox.go:16`), not in
  PR 4 with the other K6 refusals. Its signed body names a subject with no `org_id`, so it is a
  peer hop this PR could not check.
- **The call-site scan** is `TestOrgAdmitCallSites` (`api/org_admit_scan_test.go`), a go/ast scan
  of every non-test file in `api/`.
  - **What it looks for:** the users store lookups (`Get`, `GetBySSOSubIssuer`, `GetBySSOSub`,
    `GetByUsername`) and the per-user store openers (`userStore`, `userContactsStore`,
    `userMailClient`, `nativeMailboxAssignment` and the rest). K's `mailFor` and `contactsFor`
    take the request and read its `AuthContext` themselves (`api/server_userscope.go:325,590`),
    so they need no check; there is no `storeFor`.
  - **What admits an ID:** an `AuthContext`'s `UserID`, a user read by such an ID, a device
    credential's owner, or a parameter. A function that passes its parameter on becomes an
    accessor itself, so the check moves to its callers.
  - **What fails:** any other ID, unless that ID, the variable it is a field of, or the
    call's result is passed to an org gate in the same function, or the function is in
    `orgAdmitExempt`. A gate covers only the hop it checks, so a second, ungated lookup in a
    gated function still fails.
  - **The exemption list** gives a reason per function in four classes. Authentication is a
    credential naming its own account. Signed links name one account. Workers are PR 5. Admin
    routes are PR 4. Each entry must name a live function.
  - `TestOrgAdmitScanFailsAnUngatedHandler` runs the scan on a fixture. It fails a handler that
    reads a path ID without `orgAdmit`, follows a helper's parameter to its caller, and fails
    a second, ungated lookup in a function that gates its first.
- **`TestCrossOrgMatrix`** (`api/tenancy_matrix_test.go`) runs in every `internal/api` job. That
  is SQLite in the race legs and PostgreSQL in `ci-backend-postgres`, whose stores
  `KYPOST_TEST_DATABASE_URL` selects.
  - **Org-bound means:** every route wrapped in `withMailbox` (the `X-KyPost-Mailbox` hop), the
    three routes that admit a mailbox from their body (export, import, IMAP import), and every
    non-administrator route with a path parameter. Administrator routes are PR 4's staff
    variant.
  - A route needs a case or a written reason in `crossOrgNotBound`. The reasoned routes are
    the PGP envelope `{slot}` (a fixed slot name) and the public `/pickup/{id}` links (a
    bearer link by design).
  - **Each case makes three calls as org A's member:** with A's own object, with org B's, and
    with an ID that never existed.
    - B's answer must equal the missing one byte for byte, after removing `Date` and putting a
      placeholder for the ID used.
    - No log line may name B's account ID, subject, mailbox or username.
  - **B's digest:** B's own reads of every per-user store, every file under B's user and
    mailbox directories, and B's `users.json` record without `updatedAt`. The test's own
    session helper moves `updatedAt`. On PostgreSQL the per-user SQLite stores are schemas;
    B's reads cover them, but no schema dump is taken (narrower than K's "digest of every B
    store").
  - **Where the own-object control cannot succeed without a live step-up or peer,** it must
    still answer unlike a missing object. Two routes confirm the caller's credential before
    reading the object (device MFA, contact PGP key confirm), so only B and missing are
    compared there.
  - **The non-HTTP inputs:**
    - The sync event is PR 2's tests.
    - The inbound peer bodies are `TestTenancyInboundPeersNeedTheAccountOrg`.
    - SMTP `RCPT` is `TestCrossOrgMatrixSMTPRcpt` (`app/receiving_hosted_test.go`), on RX PR 5's hosted profile over TLS.
  - **Proof that the gates bite:** replacing `orgAdmit` with `return true` fails
    `TestOrgAdmit` and the mailbox, inbound-peer, people-search and address-move tests. It
    does not fail the matrix, because the per-user stores and the ledger already separate
    users.
- **Owner answers to PR 3's defaults (2026-11-22).** The defaults, numbered as in #687's hand-off:
  1. **KyMessages off in multi mode.** KyMessages is being rebuilt as KyMessages-Native, which
     will define `org_id` in its own spec. Multi mode keeps answering `409 kymessage_off` until
     that spec exists and KyPost is built against it.
  2. **KyPulse requires `org_id`.** KyPulse gets a multi-tenant, multi-region replacement. That is
     outside KyPost's scope; this is a note only. KyPost keeps refusing alerts without the
     account's `org_id` in multi mode until the replacement defines its contract.
  3. **HR reservation refused in multi mode now:** approved.
  4. **People search filtered by `subject_orgs`:** approved.

## PR 4 as built (2026-11-21)

PR 4 follows the plan with these readings, each the safer one where K was ambiguous. Paths are
under `backend/internal/` at the PR's head; T was read at KyIdentity `origin/master` `39d7d30`. Startup
still refuses `multi`, so the tests build the multi paths as PRs 2 and 3 do (`makeMultiTenant`),
with a staff administrator provisioned from a signed staff-org event.

- **Route classes are the wrappers themselves** (`api/tenancy_staff.go`). Every administrator
  route in `api/server.go` names one: `withPlatformAdmin` (53 routes), `withOrgAdmin` (15) or
  `withRefusedAdmin` (9). Each calls `withAdmin`, so the class cannot disagree with what the
  route does. Every other route is a member route, whose auth model
  `TestEveryRouteDeclaresItsAuthModel` already requires.
  - **The check:** `TestEveryAdminRouteHasAClass` (`api/route_class_test.go`) fails a route that
    uses a bare `withAdmin` or names two classes.
  - `TestRouteClassCheckFailsAnUnclassifiedRoute` runs the same check on a fixture, which must
    fail on both kinds of route.
  - The classes are K's Admin surface table. `GET /api/admin/kycalendar`, `kydrive` and
    `kymessage` are the "suite peer status" rows. Native recovery repair is platform: it repairs
    the accounts the restored ledger names, whole-install.
- **K7 where routes are admitted.** `withAdmin` also needs `isStaff` (`api/tenancy_staff.go:165`,
  called at `api/server_users.go:19`): in multi mode the caller's `AuthContext.OrgID` must equal
  `KYPOST_STAFF_ORG_ID`. PR 2's users-store rules already refuse a customer administrator. This
  is the second, independent check K7 asks for. Single mode compares nothing.
- **`X-Ky-Org`** is read by `withOrgAdmin` (`:40`) only in multi mode, and only after `withAdmin`
  has proved K7. Every refusal comes before the handler, the step-up and any audit row:
  - **No header** (or an empty one): `400 {"error":"org_required"}`, as T says.
  - **A malformed value:** `400 {"error":"org_invalid"}`. That covers more than one header, or a
    value that is not PR 1's canonical lowercase UUID (`config.ValidOrgID`).
  - **An unknown org:** `404 {"error":"org_unknown"}`. The org must have a row in the `orgs`
    cache (`tenancy.KnownOrg`, `tenancy/tenancy.go:243`), in any status. A failed read answers
    `503`. The staff org is exempt (K8).
  - **Before PR 5 the cache is empty**, so only the staff org can be selected outside tests.
  - **A customer member never reaches the header.** Every administrator route gives them
    today's `403 {"error":"admin access required"}`, with or without it. A member route never
    reads it ("URL, form or header" in Identity binding).
- **Every query is limited to the selected org.** Each handler's own "missing" answer refuses
  an object of another org. The gates are in `api/tenancy_staff.go`, and each is in the call-site
  scan's `orgGates`:
  - `staffTarget` covers the `/api/users/{id}` routes.
  - `staffMailboxAdmit` covers a mailbox in the path or body.
  - `staffAddressAdmit` covers an address's holding mailbox.
  - The lists drop other orgs' accounts and mailboxes (`staffOrgMailboxes`). The mailbox list's
    storage summary is summed over the selected org only, not the install.
  - The address routes check the holder before the masked-address `409` and the step-up. A
    staff member therefore cannot tell another org's masked address from an unknown one.
  - The reassign keeps PR 3's `addressOneOrg`, and its target must also be in the selected org.
    So the handler path PR 3 left unproven is now in the matrix.
  - On a refusal, the operational log line's `target` is empty, so it names no other-org
    mailbox.
- **Staff audit.** Before the handler runs, `withOrgAdmin` writes one row to the audit log
  (`backup_audit`, through `backup.Service.Audit`):
  - `staff.read` for `GET`/`HEAD`, `staff.write` otherwise;
  - actor: the staff account; target: `org:<org_id>`; details: `{"route":"<pattern>"}`;
    outcome: `requested`.
  - It names no subject, address, account or object ID. A request that cannot be audited
    answers `503` and runs nothing.
  - Writes keep their existing log lines unchanged. The org reaches them through the paired
    `staff.write` row, not by a new field on each line, which would have changed each line's
    single-mode bytes or needed a mode branch per handler.
  - A KySignOn step-up's first call (the challenge) and its confirmed retry each write a row.
- **K6, all answering `404 page not found` (`http.NotFound`) in multi mode:**
  - `withRefusedAdmin` (`:32`): local user creation, password reset, administrator IMAP and
    CardDAV assignment, and poll-now. It answers after the administrator check, so a customer
    member still gets `403`.
  - `offInMulti` (`:82`) on member routes:
    - local password sign-in: `POST /api/auth/login`, `GET /api/auth/login-params` and the Play
      reviewer's `POST /api/notifications/review-pairing`, which signs in with a password;
    - the external-IMAP source: `GET|POST|DELETE /api/imap/config` and `POST /api/imap/test`.
    The IMAP import tool is not a mail source and stays.
  - **AutoProvision:** `PUT /api/admin/sso` with `autoProvision: true` answers `404`
    (`api/sso_handlers.go:92`). `false` is saved. `sso.Store.Load`'s default of `true` for a
    missing file is unchanged. PR 2 already makes sign-in ignore the setting in multi mode.
  - The bootstrap administrator is still refused by `bootstrap-admin` refusing `multi` (PR 1).
    PR 7 must keep it off when it accepts `multi`.
  - PRs 2 and 3 already turned off SSO link and unlink and HR.
- **Step-up is per org** (owner decision 2026-11-22, overturning this PR's first reading that
  one step-up covered any org). The action digest (`withActionDigest`) does not cover
  `X-Ky-Org`, so the grant itself records the org `withOrgAdmin` selected when it was minted
  (`ssoStepUp.Org` in `api/sso_stepup.go`). `confirmSSOStepUp`, the one sink behind every
  KySignOn step-up, spends a grant only when that org equals the org now selected.
  A grant made with org A, or with no org selected, presented with org B is not spent: the
  request gets the existing `403 sso_step_up_required` with a new challenge for B, and nothing
  runs. That covers every org-bound route with a step-up: the `/api/users/{id}` writes
  (`withAdminStepUp`) and the mailbox and address writes (`nativeDomainGate`).
  Platform routes and single mode select no org, so their grants carry `""`, which `omitempty`
  keeps out of the stored row: their bytes and behaviour are unchanged. A password session
  re-proves on every request, so it has nothing to bind. Tests: `TestStaffStepUpIsPerOrg`
  (A on A allowed; A on B and none on A refused with a fresh challenge for the selected org;
  an expired grant keeps today's answer; a customer member on a KySignOn session, holding a
  live grant for the exact action and its own org, gets `403` and the grant stays unspent) and
  `TestStepUpGrantWithoutOrgKeepsItsBytes`.
- **Tests** (`api/tenancy_staff_test.go`, in every `internal/api` job, SQLite and PostgreSQL):
  - **The staff variant:** `TestCrossOrgMatrixStaff`. Every `withOrgAdmin` route needs a case in
    `staffCases` or the build fails. As staff with org A selected, each case makes three calls:
    - with A's object: the control must succeed;
    - with B's object: it must equal the missing answer byte for byte, leave B's digest
      unchanged, write audit rows naming only org A, the staff actor and the route, and write
      no log line naming B or org B;
    - with an ID that never existed.
  - `TestStaffListsKeepTheSelectedOrg`.
  - `TestStaffOrgSelection` covers no header, an empty one, and malformed, oversize, case,
    whitespace and repeated values. It also covers an unknown org, a customer member's header
    on a member route, and the missing audit log.
  - `TestEveryAdminRouteRefusesACustomerMember` calls every administrator route (77) as a
    customer member with no header, A, B and the staff org. Each gets `403`, nothing changes,
    and no audit row is written.
  - `TestTenancyK6RefusalsAnswer404`, `TestIsStaff`.
  - `TestSingleTenantIgnoresTheOrgHeader`: a single install answers the same with any
    `X-Ky-Org` and writes no audit row.
  - **Proof that the gates bite:** making `staffOrgAdmit` admit everything fails every staff
    matrix case except reassign, which `addressOneOrg` still refuses, and fails the list test.
- **Not built, by phase.**
  - **The admin UI's org selector.** Nothing the SPA calls changes in single mode, and multi
    cannot start, so there is no changed screen. The selector needs PR 5's `orgs` cache to
    offer orgs.
  - **Staff reading the audit across orgs by actor** (T "Audit"). KyPost's audit log is
    whole-install, and no route reads it by org yet.
  - The org status effects on staff routes (a suspended org's accounts) are PR 5's.
- **Owner answers to PR 4's defaults (2026-11-22).** The defaults, numbered as in #688's hand-off:
  1. **Unknown org answers `404 org_unknown`**, any cached status counting as known: approved.
  2. **Write audit through the paired `staff.write` row**, existing log lines unchanged: approved.
  3. **Mailbox storage summary over the selected org only:** approved.
  4. **`autoProvision: true` answers `404`, `false` saves:** approved.
  5. **The step-up digest not covering `X-Ky-Org`:** *overturned.* Staff step-up is per org: a
     grant obtained with one org selected must not authorise a request under another. Built in
     #703 ("Step-up is per org", above). In a multi-region install it is also per cell (MH
     "Regions").

## PR 5 as built (2026-11-21)

PR 5 follows the plan with these readings, each the safer one where K was ambiguous. Paths are
under `backend/internal/` at the PR's head. ky-primitives is bumped to `9deb288` (#72), the
first commit with `suitedir.Client.Orgs`. Startup still refuses `multi`, so the tests build the
multi paths as PRs 2 to 4 do (`makeMultiTenant`, which now also caches orgs A and B as active).

- **The cache** (`tenancy/orgs.go`) is PR 1's `orgs` table; no table is added, so
  `TestEveryStoreIsClassified` is unchanged.
  - `ApplyOrg` (`:159`) writes one listed org in one statement. A lower revision than the
    cached one changes nothing, and `deleted` is terminal (T "Account holder guard"). An entry
    whose ID is not PR 1's canonical form, whose status is unknown, or whose quotas are over
    4 KiB or not a JSON object (`null` included) is refused (`ValidOrg`). `pollOrgs` checks every listed entry first, so a malformed
    one fails the whole poll before any session is revoked, as `suitedir.Orgs` fails on one.
  - **The cursor** (`OrgCursor`, `:134`) is the highest cached revision minus one, so orgs that
    share it are listed again. An equal revision applies as the same state. No cursor row is
    stored.
  - **The poll stamp.** After every listed org applied, `MarkPolled` (`:182`) sets every row's
    `polled_at` to the poll's time. A `suitedir.Orgs` call cut short at its 100-page bound
    returns a cursor below its last revision; `pollOrgs` keeps calling until one is not cut
    short, so no row is stamped fresh while its change is unread
    (`TestOrgPollerReadsPastThePageBound`). A failed poll stamps nothing. `polled_at` therefore means
    "the last good poll", which is what the stale bound reads.
  - `LookupOrg` (`:81`) reads a row; a missing row is not known.
- **What "served" means** (`Org.Served`, `:54`): known, `active`, `enabled` for KyPost, and a
  last good poll no more than 15 minutes old (`StaleAfter`, `:19`; a stamp in the future is
  stale). `enabled: false` is not in K's table. It is read as "not served" (owner question 1
  below).
- **The gate** is `tenancy.Gate.Serve` (`:215`). The staff org is always served (K8). A nil gate,
  one without a database, or an empty org serves nobody, so an unwired caller fails closed. The
  api's gate is `orgGate` (`api/tenancy_lifecycle.go:29`); single mode never asks it
  (`orgServe`, `:34`).
- **The poller** (`StartOrgPoller`, `api/tenancy_lifecycle.go:52`) runs about every 60 s on the
  `api-sweepers` owner (`app/app.go:477`), and only when the server holds `tenancy.db`.
  - `pollOrgs` (`:75`) calls `suitedir.Orgs` with the SSO settings' suite directory
    credentials.
  - **Session revocation.** For an org that stops being served (it was served or unknown, the
    entry is not stale, and it is not the staff org), `revokeIfStopping` (`:112`) runs
    `revokeUserSessions` for every `user_orgs` member before the row changes. That is R's
    per-user generation on PostgreSQL, and the in-memory sessions on SQLite. A failed
    revocation fails the poll before anything is applied, so the next poll retries it.
  - The failure log line names no org: `suitedir`'s error can quote an org ID.
- **Sign-in and credentials.** Each check answers like the dead credential on that path:
  - The session (`currentUser`, `api/server_auth_session.go:1330`) is dropped when not served.
    A cache read error answers `503` and keeps the session.
  - The paired device (`deviceAuthSnapshot`, `api/device_auth.go:164`, and `deviceOrg`,
    `api/tenancy_gate.go:119`).
  - CardDAV, on a cache hit and a miss, and MCP agent tokens, through `davAccountActive`
    (`api/dav_auth.go`), which reads the org once and returns `(active, err)`, so the answer
    never comes from two reads that disagree. A refused CardDAV password still pays the KDF
    timing.
  - **An unreadable or stale cache is "try again later", never a dead credential.** A
    stale cache whose last known state is served is `tenancy.ErrStale`, which is still
    `ErrNotServed` for sign-in and sending. An org whose last known state is not served
    answers as before. Device auth
    answers busy. CardDAV, after the password is verified, and
    MCP answer `503`. A `401` would make a client drop a working password or re-pair.
  - Browser sign-in and device sign-on (`resolveSSOUser`, `api/sso_handlers.go:668`): `403`
    "this account is not available right now". It is reached only after the token's org and
    the three binding records agree, so it tells only the account's holder. The security log
    line names no org.
  - Agent tokens, devices and CardDAV passwords are kept, and they work again on resume with
    nothing re-issued. Sessions do not come back, because they were revoked.
  - **A stale cache drops sessions too.** After a KyIdentity outage longer than 15 minutes,
    every customer signs in again (owner question 3). Devices, CardDAV and MCP keep their
    credentials and get busy or `503` until the cache is fresh.
- **Sending.** Every send is charged through `chargeSend`, so the org check sits there
  (`api/send_quota.go:59`): immediate sends (`403` "sending is paused for this account"),
  scheduled sends, and KyCalendar and KyPulse mail.
  - **A scheduled send is held** (`api/scheduled_send.go:498`). It waits with `reasonHeld` and is
    retried until its one-hour retry window ends. Then it returns to Drafts keeping that reason
    (`:453`), as open question 6 recommends.
  - **A queued native send is held, not sent.** `sso.NativeOutbound.Submit` refuses an org-bound
    owner whose org is not served with `ErrNativeOutboundHeld`
    (`sso/native_outbound.go:355`), before the claim, so nothing is consumed or quarantined.
    `Recover` treats the refusal as a quiet skip (`:518`).
  - `NativeOutbound.Orgs` is an interface, because the `tenancy` tests import `sso`. Only the
    api sets it (`api/native_outbound.go:20`). The daemon's outbox worker and the processor's
    out-of-office replies leave it nil, so they hold every org-bound send until PR 7 wires the
    gate where it accepts `multi`.
- **Receiving.** The status decision is `Org.Receiving` (`tenancy/orgs.go:68`), from the last
  known status whatever the cache's age, as open question 5 recommends:
  - accept for `active` and enabled;
  - temporary (`451`, RX exit 12) for `suspended`, disabled or not cached;
  - unknown (`550`, exit 3) for `deletion_scheduled` and `deleted`.

  Wiring it into `receiving bind` is RX PR 5's exit 12 (the brief keeps exit codes 12 and 13
  out of this PR). RX PR 5 built it (`orgReceives`, RX "PR 5 as built").
- **Quotas** (`tenancy.Quotas`; masked quotas are M change 3, PR 7):
  - **Parsing.** A missing key keeps today's default. A known key that is not a whole
    non-negative number reads as 0, and a body that does not decode zeroes every key. 0 refuses
    new mail and creations, never existing data.
  - **`sendPerHour` and `recipientsPerDay`** (`chargeOrgSend`, `api/tenancy_lifecycle.go:139`).
    The user's own charge is written first. Then every member's charges, this one included,
    are summed from `send_ledger` (`state.OrgSendUsage`, `state/send_ledger.go:128`, members
    from `user_orgs` plus the sender). Past a cap, this charge is deleted and the send gets
    the quota answer. Two concurrent sends can both be refused but never both admitted past the
    cap. Only a charge this call wrote is deleted: a retry of an admitted send that a lowered cap
    now refuses keeps its earlier charge. The org refusal's `Retry-After` is one hour, not computed.
  - **`mailboxes`** (`orgMailboxRoom`, `:186`) counts the org's extra mailboxes in any state.
    It refuses a new one on `POST /api/admin/mailboxes` with `409 mailbox_quota`
    (`api/native_mailbox_handlers.go:201`). Two staff creations at once can pass together.
  - **`mailboxBytes` and `storageBytes`** are applied at receiving acceptance (`orgRoom`,
    `app/receiving.go:94`, after the mailbox's own check at `:83`), with mail held for import
    counted. At DATA, a delivery to several mailboxes of one org counts one copy per mailbox
    (`sameOrgCopies`). They answer the existing mailbox-full exit 9.
    - The owner's org comes from `subject_orgs` and the members from `user_orgs`, because the
      check runs inside the users fence. A subject with no recorded org is refused.
    - It reads the runtime's `tenancy.db` handle, which RX PR 3 opens when
      `KYPOST_TENANCY=multi` (`app/receiving.go:449`); single mode has none and checks nothing.
    - Imports, drafts and Sent copies stay bounded by the install's mailbox quota only.
- **Staff** keep the console with the poller failing and the staff org absent from the cache.
  PR 4's `X-Ky-Org` still accepts any cached org in any status, so staff reach a suspended org's
  accounts.
- **Tests:**
  - `api/tenancy_lifecycle_test.go` runs in every `internal/api` job:
    - `TestOrgStatusOnEverySurface`: ten cache states, including both sides of the stale bound,
      against session, device, CardDAV, MCP, sending, a scheduled send and the receiving
      decision, with org B unaffected and credentials working again;
    - `TestOrgStatusAtSignIn` (browser and device);
    - `TestOrgPoller`: revocation, cursor, stale revision, terminal `deleted`, KyIdentity down,
      wrong credentials, malformed, non-canonical and oversize entries, no directory, and the
      poll stamp;
    - `TestStaffKeepTheConsoleWithThePollerFailing`;
    - `TestOrgQuotas`;
    - `TestSingleTenantHasNoLifecycle`;
    - `TestOrgCacheOutageIsRetryable`;
    - `TestTwoReplicasSeeOneSuspension`, PostgreSQL only.
  - Elsewhere: `tenancy` `TestOrgCache`, `TestApplyOrgRefusesMalformedEntries` and
    `TestParseQuotas`; `sso` `TestNativeOutboundHoldsANotServedOrg`; `app`
    `TestReceivingOrgStorageQuotas` and `TestReceivingOrgStorageCountsEveryCopy`.
  - **Proof that the gates bite:** each of these mutations fails its test:
    - the session check, the sign-in check and the gate's staleness;
    - the outbox hold;
    - session revocation and the staff exemption;
    - the revision fence;
    - the org send cap, the mailboxes cap and each storage cap;
    - the gate itself, on PostgreSQL.
- **Not built, by phase.**
  - **Import and export, one per org at a time** (Abuse limits). It is not a `quotas_json` key,
    and today's slots are per process. PR 7, with the receiving tier's per-mailbox leases.
  - **Masked-address quotas** (M D7): PR 7.
  - **`receiving bind` refusing by org status:** RX PR 5 (built).
  - **The org gate in the daemon's outbox worker and out-of-office replies:** PR 7. Until then
    they hold, never send.
  - **The admin UI's org selector:** still not built. No screen changed, so there are no
    screenshots.
  - Whether `/api/suite/orgs` lists the staff org is still unverified. The staff exemption makes
    the answer irrelevant here.
- **Owner questions, each with the default now in code:**
  1. `enabled: false` is treated like `suspended` for sign-in, sending and receiving. The data
     is kept.
  2. A malformed or negative quota reads as 0: nothing new is accepted.
  3. A stale cache drops sessions, not only refuses them.
  4. `mailboxes` counts extra mailboxes only.
  5. Storage quotas apply to received mail only.
  6. A held scheduled send returns to Drafts after the one-hour retry window, not at resume.

## PR 6 as built (2026-11-22)

PR 6 is split into three stacked PRs, because one would pass what a reviewer can read: **6a**
(purge, tombstones, backups), **6b** (one-user purge for `user.erased`, the `export_only` route
gate) and **6c** (the multi-mode restore release). This section records 6a; 6b and 6c add their
own parts. Paths are under `backend/internal/` at 6a's head unless marked. Startup still refuses `multi`, so the
tests build the multi paths as PRs 2 to 5 do (`makeMultiTenant`, now through the test helper
`useTenancy` in `api/tenancy_binding_test.go`).

- **Tables** (`tenancy/tenancy.go:67-75`, PostgreSQL `:112-121`), all class P:
  - `org_purges(org_id, subject, step, started_at, done_at)`, keyed `(org_id, subject)`. A whole
    org's purge has `subject = ''`; 6b's one-user purge names the erased subject.
  - `org_purge_members(org_id, subject, kind, issuer, id)` is the snapshot: `user`, `mailbox` and
    `subject` rows.
  - `address_tombstones(hmac)`: triggers refuse every update and delete.
- **When it runs.** After every org poll, whether or not the poll succeeded, on the `api-sweepers`
  owner (`runPurges`, `api/tenancy_purge.go:33`, called at `api/tenancy_lifecycle.go:66`).
  - A purge starts for each cached org whose status is `deleted` and has no purge row yet. The
    staff org is never purged.
  - Every live purge then runs.
  - The failure log line names no org, path or database detail.
- **The snapshot** (`purgeSnapshot`, `:77`) is K's union: `user_orgs`, the `users.json` accounts
  bound to the org, and the accounts of the org's `subject_orgs` subjects. Then every ledger
  mailbox of those subjects (`sso.NativePurgeMailboxes`, `sso/native_purge.go:62`).
  - Reading: the union is the reconciliation's outcome, so the snapshot writes no `user_orgs` row
    that step 5 would delete.
  - An account bound to another org is never taken. A `user_orgs` row naming one stops the purge
    with an error.
  - `StartPurge` (`tenancy/purge.go:43`) writes the row and the snapshot in one transaction, once.
- **Steps** (`purge`, `api/tenancy_purge.go:131`). Every run executes all five, re-listing what
  still exists. `step` records the last one finished, and nothing reads it to skip.
  1. Revoke every session, delete agent tokens and scheduled sends, cancel a running import on
     this replica.
  2. `ingress.PurgeMailboxes` (`ingress/purge.go:17`) deletes the mailboxes' routes and bindings.
     - A delivery bound only to them is archived as `discarded`: its bytes go, and an exact replay
       is still recognised.
     - A delivery shared with another org keeps its other bindings.
  3. Storage, under each mailbox's `receiving/quota-<id>` lock:
     - On PostgreSQL, the schemas under each user and mailbox directory go (`dbbackend.Remove`).
     - The directories go: `CONFIG_DIR/users/<id>`, `STATE_DIR/users/<id>` and
       `STATE_DIR/mailboxes/<id>`. That covers paired devices, CardDAV passwords, sealed IMAP and
       CardDAV client credentials, and every per-user file.
     - Then the lock files go, and the users' pickup records (`PickupStore.SenderRecords`).
     - Every ID is checked as a safe path component first.
  4. The ledger, under the named trigger exception (below). Then `ingress.ScrubArchived`
     (`ingress/purge.go:65`) drops every tombstoned address from the receiving tombstones'
     recipient lists.
  5. The users' root-store rows (`state.PurgeUsers`: `agent_tokens`, `scheduled_sends`,
     `send_ledger`, `sessions`, `session_revocations`, `auth_challenges`, `import_jobs`). Then
     their `users.json` records (`users.Store.RemoveUsers`), then their `user_orgs` rows.

  Afterwards the final check runs. Then one `tenancy.purge` row is written to the audit log
  (`backup_audit`): target `org:<org_id>`, with the scope and the account, mailbox and subject
  counts, and no content. Last, `FinishPurge` marks the row done and deletes the snapshot in one
  transaction. Without the audit log or the root state store the purge refuses to run.
- **The named trigger exception** (`sso.PurgeNative`, `sso/native_purge.go:96`, owner answer to
  open question 9).
  - **Why not a trigger change.** `native-authority.db` opens only when its schema equals the
    binary's DDL exactly, on every connection, on both backends. A trigger that read a live
    `org_purges` row, or a new `purged` state, would change every single-tenant install's schema,
    and `org_purges` is in another database anyway.
  - **How it works instead.** Inside its one write transaction, under the directory fence:
    1. It calls `live`, which must find the scope's live `org_purges` row.
    2. It lists every address the owners' mailboxes ever held, current or in history.
    3. It calls `tomb`, which records their HMACs.
    4. It suspends three triggers: `mailboxes_no_delete`, `addresses_no_delete` and
       `address_history_no_delete`. SQLite drops each and recreates it from its exact text in
       `authoritySchemaV3`; PostgreSQL uses `DISABLE` and `ENABLE TRIGGER`.
    5. It deletes and updates the rows below, restores the triggers, and bumps the Cloudflare
       change mark.
  - **What other connections see.** Every other connection, and the exact-schema check after the
    commit, sees the triggers unchanged. A failure at any point rolls the whole transaction
    back, triggers included.
- **The ledger, by table** (readings where K could not hold as written):
  - **`addresses`, `address_history`, `routes_pending`:** the owners' rows are deleted, after
    their tombstones.
  - **`mailboxes`:** primary mailboxes stay as `disabled` tombstones. **Extra mailboxes are
    deleted**, because the ledger's own validation (`validateNativeLedger`) refuses an extra
    mailbox without its primary address. A kept extra mailbox would make every later ledger
    write fail. `state_root` is not cleared: it is the install's `STATE_DIR`, shared by every
    mailbox, not the user's, and a trigger fences it. The schema has no `purged` state, so
    `disabled` stands for it.
  - **`accounts`:** kept, with `desired_active = 0`.
  - **`directory`:** kept as the revision fence, with `resource` NULL and `active = 0`.
    Reconcile refuses a NULL resource (`sso/native_provision.go:666`), so a purged subject is
    never provisioned from it.
  - **`directory_primary_emails`:** the owners' rows are deleted.
  - **`subject_orgs`:** kept.
- **Tombstones.**
  - **The key.** `SECRET_DIR/address-tombstone.key` is created exclusively (`cryptutil.LoadOrCreateKey`),
    but only while no tombstone exists. With tombstones and no key, `tenancy.ErrTombstoneKeyMissing`
    refuses every allocation, rather than mint a key under which every tombstoned address would
    be free again.
  - **The HMAC.** HMAC-SHA-256 of the lowercased address.
  - **Who consults it.** The conflict check reads the tombstones before the ledger, through
    `LifecycleStore.Tombstoned` (`sso/lifecycle.go`). Only the test helper `useTenancy` sets that
    hook today, so a single install never consults it. **PR 7 must set it wherever it sets
    `Server.tenancy`**, as `useTenancy` does; without it allocation would not see tombstones. Restore qualification
    asks `LifecycleStore.Purged` (`tenancy.MailboxPurged` over `purged_mailboxes`, written by
    `FinishPurge`), which `api.NewServer` sets and which answers false without `tenancy.db`. The two allocation paths are new aliases and reassignment
    (`changeNativeAddress`, `sso/native_address.go:343`) and a primary allocation
    (`sso/native_provision.go:742`, the durable `address_conflict` failure). HR reservation is
    off in multi mode.
- **Nothing is reused.** A signed directory event for a deleted org, or for a subject with a purge
  row, applies as inactive, whatever its `active` field says (`api/tenancy_binding.go`, in
  `bindDirectoryOrg`). Removals still succeed, and no account is created. An event with no
  `active` field is refused earlier, as malformed (`sso/directory.go:92`).
- **Pickup records.** A pickup record that cannot be parsed stops the purge with an error,
  because its owner cannot be ruled out. A corrupt file therefore needs an operator before the
  purge can finish (`TestOrgPurgeFailsClosedOnAnUnreadablePickup`).
- **The final check** (`purgeLeft`, `api/tenancy_purge.go:326`) has eight parts:
  - `users.json`, `user_orgs`, `root-state`;
  - `directories` (with the quota lock files) and `schemas` (PostgreSQL catalog entries under
    them);
  - `pickup`, `ingress` (routes, bindings, and tombstoned archived recipients);
  - `ledger` (`sso.NativePurgeLeft`).

  `tenancy.PurgeCoverage` maps every O and D entry of the Store inventory to one part.
  `TestPurgeCoversEveryOrgStore` (`tenancy/inventory_test.go:256`) fails when a new
  org-owned or derived store has no part. `TestPurgeFinalCheckCoversTheClassification` fails
  when a part is named but not run. Reading: import uploads (`imports`) live only while their
  job runs; the purge cancels the job, whose exit removes the file. They have no owner on disk
  to check, so they map to the `import_jobs` rows.
- **Backups.**
  - **In the backup set.** `tenancy.db` is a snapshotted database (`backup/collect.go:38`):
    `VACUUM INTO` on SQLite, the catalog export on PostgreSQL. Before this, SQLite copied the
    raw file without its WAL. So sender blocks, gateway pins, the org roots, purge rows and
    tombstones survive a restore.
  - **The key travels with the tombstones.** A capsule with tombstones but without
    `private/address-tombstone.key` is refused (`tombstoneKeyCollected`, `:326`).
  - **Native or not.** `tenancy.db` alone does not make a capsule native
    (`backup/native.go:74`). A multi install is held on restore by its ledger, as before.
  - **The restore fence.** `runRestore` calls `tenancy.FenceRestored` (`app/backup.go:214`) on
    the staged SQLite file, before any PostgreSQL import:
    - it empties the `orgs` cache, so no org is served until a poll after the restore;
    - it deletes the finished purge rows, so every deleted org is purged again once KyIdentity
      lists it;
    - live purges keep their snapshot and resume.
  - **Reading.** The brief's "org state survives a restore" holds for the org roots and purge
    records. The cache is emptied on purpose, as K says.
- **Tests:**
  - `api/tenancy_purge_test.go`, in every `internal/api` job:
    - `TestOrgPurgeResumesFromEveryStep`: a fixture of two orgs. Org A holds sessions, contacts,
      devices, agent tokens, scheduled sends, send charges, messages, pickup records, its own
      delivery, one shared with B and one already imported. The purge is killed before and
      after each of the five steps, then resumes. Each time nothing of A remains, B's digest is
      unchanged, the tombstones refuse all four addresses, and neither the purged subject nor a
      new subject of the deleted org gets an account.
    - `TestOrgPurgeFinalCheckRefusesLeftovers`.
    - `TestOrgPurgeRefusals`: the staff org, no live row, another org's account, a lost key.
    - `TestOrgPurgeKeepsTheLedgerTriggers`.
    - `TestOrgPurgeRunsAgainAfterARestore`: after the fence, B is refused until polled, and A's
      purge runs again and passes its final check. The tombstones still refuse reuse under the
      same key.
    - `TestPurgeFinalCheckCoversTheClassification`.
  - Elsewhere:
    - `backup/tenancy_test.go`: `TestTenancySurvivesARestore` seals and restores, and on the
      PostgreSQL job imports into PostgreSQL. `TestTombstonesNeedTheirKeyInTheCapsule`.
    - `app/backup_test.go`: `TestRestoreCLIFencesTheOrgCache`, through `runRestore`.
    - `tenancy/inventory_test.go`: `TestPurgeCoversEveryOrgStore`.
  - **Proof that the gates bite:** removing each of these fails the purge tests:
    - the alias tombstone check;
    - the archived-recipient scrub;
    - the deleted-org rule on directory events;
    - restoring the triggers;
    - the receiving step;
    - the root-store step.
- **6b: the one-user purge (`user.erased`).** Paths at 6b's head.
  - **Admitted in multi mode only.** In multi mode the webhook accepts `user.erased`
    (`api/sync_handlers.go:53`). A single install still refuses the type as unsupported (`400`),
    as before.
  - **Validated as a removal.** The event must be inactive, and `bindDirectoryOrg` checks its
    `org_id` exactly as for `user.deleted`.
  - **The purge is recorded first** (`eraseMember`, `api/tenancy_purge.go:397`). It writes the
    purge row and snapshot for scope `(org, subject)`, so a failure after it still purges.
    Then the event applies as `user.deleted`, which revokes the account's credentials at once.
  - **The fence does not decide.** The fence's digest still names `user.erased`. A fence
    refusal (`ErrDirectoryConflict`) answers `200 erased`, because the purge does not depend
    on the revision. A failure to record the purge answers `500`, so KyIdentity retries.
  - **The purge is 6a's, scoped to one subject.** The snapshot holds only that subject, its
    accounts and its ledger mailboxes. The next poll runs it, and the audit row's scope is
    `member`.
  - **What else applies.** A whole-org purge row covers its members, so no member row is added
    under it. A later active event for the subject applies as inactive (6a's rule).
- **6b: the `export_only` route gate** (K open question 4 (a)). Before this, PR 2 refused the
  claim at sign-in.
  - **Parsing.** `tokenOrg` now returns the claim (`api/tenancy_binding.go:43`).
  - **Where the token is accepted.** In the browser sign-in only, and only while the account's
    org is cached, fresh, enabled and `deletion_scheduled` (`orgExportable`,
    `api/tenancy_lifecycle.go:254`). The staff org never qualifies.
    - Device sign-on still refuses it like a token without an org (`resolveSSOUser`'s
      `admitExport`, `api/sso_handlers.go:670`).
    - A link refuses it.
    - A step-up admits it only for an export-only session (`:537`).
    - An ordinary token for a `deletion_scheduled` org is still refused.
  - **The session** carries `ExportOnly` (`Session`, `api/server_auth_session.go:74`, omitted
    when false, so a single install's stored sessions keep their bytes). It is never an
    administrator, and it lands on `/settings/account?tab=export`.
  - **The gate is in `currentUser`** (`:1294`), which every session wrapper calls. Outside
    `exportOnlyRoutes` (`api/tenancy_lifecycle.go:245`) the session is treated as absent, so
    every other route answers exactly as without a session. The session itself is kept.
    `exportOnlyRoutes` is:
    - `/api/auth/me`, `/api/auth/csrf` and logout;
    - the three OIDC step-up routes and the SSO callback, which confirm the export;
    - `/api/mailboxes` and the four export routes.

    Once the org is no longer `deletion_scheduled`, or the cache goes stale, every route
    refuses the session.
  - **`GET /api/auth/me`** adds `exportOnly: true` only for such a session.
  - **The SPA** renders `pages/ExportOnlyPage` (the deletion notice, the existing `MailExport`
    and Sign out) before any route or shell, so it calls nothing the server refuses.
    Screenshots are in `docs/ui-tenancy-2026-11-22/screenshots/`, captured against a mocked
    API, because `multi` cannot start yet. "Before" is the generic sign-in failure PR 2 gave
    the same token.
- **6b tests:**
  - `TestMemberErasurePurgesOnlyThatMember`: the member's account, mailboxes, addresses and
    data go; the other member and org B are byte for byte unchanged; a repeat and a later
    active event change nothing.
  - `TestMemberErasureRefusals`: another org, active, no org, and single mode.
  - `TestExportOnlySession`:
    - org active: refused;
    - an ordinary token while deletion is scheduled: refused;
    - another org's member: refused;
    - `/api/auth/me` returns `exportOnly`;
    - every route of `server.go` outside the list answers exactly as with no session, and the
      session survives;
    - deletion ends it.
  - `TestSingleTenantIgnoresExportOnly`, and frontend `App.exportOnly.test.tsx`.
  - PR 2's sign-in test now expects `403` for an export-only token while the org is active.
  - **Proof that the gates bite:** removing each of these fails its test:
    - the route gate (244 failures);
    - the session's org window;
    - the sign-in org window;
    - the erasure's purge record;
    - the multi-mode condition on `user.erased`.
- **Not in 6a or 6b:** the multi-mode restore release with the full poll and erased-subject read (6c). The end-to-end
  Delete everything tests (click, purge, restore a capsule from before the click, release,
  poll) need 6c's release, so they are 6c's.

## Org storage pool as built (2026-11-23)

KyPost's side of "Quotas", built from this note's defaults before the owner merged it. Paths are
under `backend/internal/`. KyIdentity's half is its tenancy spec's "Org storage pool" (KyIdentity
#288, not merged; another worker builds it), cited here as **S**. Startup still refuses `multi`,
so only tests run any of this.

- **The contract is S's grant model.** It replaces this note's draft, which computed the room
  from the pool and the other apps' reports. KyPost does not read `storage` from
  `/api/suite/orgs` (S: `pool_bytes`, `over_since`). The only other use of `over_since` is
  receiving grace, which is not built.
  - `POST /api/suite/storage` carries `{reports: [{org_id, used_bytes, seq, want_bytes}]}`.
    The answer is `{results: [{org_id, result, seq, ceiling_bytes, ttl_seconds, …}]}`.
  - The client is `tenancy.ReportStorage` (`tenancy/storage.go:120`). It uses the SSO settings'
    suite-directory credentials, the same HTTPS-or-loopback rule as `suitedir` and no
    redirects. It sends 1 to 500 reports and accepts an answer of at most 1 MiB.
  - `ky-primitives` has no POST client, so this one lives in KyPost. Moving it into `suitedir`
    is the upgrade once S merges.
  - `matchResults` (`:177`) refuses the whole answer when any of these hold, so nothing of it
    is applied:
    - an org is missing, repeated or not in the request;
    - a result other than `ok`, `stale` or `unknown_org`;
    - an `ok` for another `seq`;
    - a ceiling below 0 or over 2^53;
    - a negative TTL.
- **The grant** is a new `tenancy.db` table, `org_storage` (`tenancy/tenancy.go:87`, `:143`).
  Its columns are `org_id`, `seq`, `used_bytes`, `reported_bytes`, `admitted_bytes`, `ceiling_bytes` and `expires_at`. `used_bytes` includes new reservations; `reported_bytes` is the last measured usage; `admitted_bytes` is a monotonic reservation watermark.
  - It is platform class, like the `orgs` cache: byte counts keyed by org
    (`TestEveryStoreIsClassified`). A single install has no `tenancy.db`, so it never has
    this table.
  - `NextStorageSeq` (`tenancy/storage.go:63`) draws each report's `seq` from the shared
    database with an upsert, so two replicas never send the same one.
  - `SetGrant` (`:74`) writes an answer only while its `seq` is still the org's latest. A late
    answer to an older report changes nothing.
- **The report** runs about every 60 s on the `api-sweepers` owner, in `StartOrgPoller` after
  the poll, whether or not the poll succeeded (`api/tenancy_lifecycle.go:75`, `reportStorage`,
  `api/tenancy_storage.go:130`).
  - It covers every cached org that is not `deleted` and not the staff org. `want_bytes` is
    the larger of 256 MiB and twice the growth since the last accepted measurement (`n - last.Reported`, never the reservation-inflated `last.Used`).
  - The expiry is the time the request was sent plus `min(ttl_seconds, 15 min)`.
  - `stale` keeps the current grant. `unknown_org` zeroes it.
  - A failed batch logs one line naming no org and is retried at the next poll. The grant
    stands until it expires.
  - An org whose usage or sequence cannot be read (an unreadable mailbox or photo
    directory, say) is skipped. Its sequence does not advance and its grant is left to
    expire, so it alone fails closed. Every other org is still reported, and one line logs
    only the number skipped. A failure that touches every org still stops the report: the
    org list, the suite directory, the ledger, the spool scan, a batch or a grant write.
- **What KyPost counts** (`orgUsage`, `api/tenancy_storage.go:30`), over every `user_orgs`
  member of the org:
  - `usage.payload_bytes` of every mailbox the member owns in the ledger (primary and extra).
    That covers stored mail in every folder, queued sends and their reserved Sent copies.
  - Mail held for import. The report opens the spool once and asks `HeldBytes` only for each org's own mailboxes. Mailbox ownership is queried per member (`NativeMailboxesOf`), never from the install-wide ledger. Writes never open the spool or scan storage.
  - Contact photos (`contactPhotoBytesUsed`).
  - Exports, import uploads and backups are not counted, as above.
- **The check** is `orgPoolRoom` (`api/tenancy_storage.go:76`). A write of `n` bytes fits only
  while the grant is unexpired and usage plus `n` is at most the ceiling (`tenancy.ReserveStorage`). Its edge cases:
  - A missing grant fits nothing. That covers an org never reported, an unknown org, or
    KyIdentity unreachable past the grant's life. This is open question 15's default.
  - An account with no org is refused.
  - The staff org is exempt.
  - A read error refuses the write with that error.
  - It runs at four points:
    - **Every new message** (`mailbox/store.go:437`): drafts, client uploads, copies and user
      imports. A gateway receipt is exempt: held mail receiving accepted always imports.
    - **A send's enqueue** (`mailbox/outbox.go:223`), for the sealed job plus its Sent copy.
      `FileOutboundSent` is never checked, and neither is an exact replay, which returns before
      the check.
    - **A new contact photo** (`api/contacts_photo.go:188`). A photo already stored adds
      nothing.
    - **Not receiving.** Open question 16 is the owner's, so `orgRoom` and `receiving bind` are
      unchanged.
  - The mailbox checks run through one process-wide hook, `mailbox.SetPoolCheck`
    (`mailbox/pool.go:30`), which `Server.StoragePoolCheck` (`api/tenancy_storage.go:115`)
    implements. Nothing installs it outside tests, so `backend/deadcode.allow` lists it. PR 7
    must install it wherever it sets `Server.tenancy`, in every process that writes mail, and
    drop that entry. That joins the multi start gate.
  - The check reserves bytes with one conditional `UPDATE org_storage`, inside the mailbox
    transaction after replay checks. Every mailbox and replica spends the same cached
    headroom atomically. A failed write stays charged until the next report; deletions
    free room at that report. No per-message storage scan runs.
  - **Report overlap bound.** The report reads `admitted_bytes` before scanning, then its
    answer adds every reservation since that watermark to measured usage. A slow answer
    cannot reset concurrent spending; some writes may be counted twice until the next
    report. Reservations already in flight at the watermark can be missing from the scan:
    at most one per mailbox writer and one per member's photo lock. The worst-case local
    overshoot is the sum of `(128 MiB + mailbox MessageBytes)` per org mailbox (the maximum
    sealed send plus its Sent copy), plus `maxContactPhotoBytes` per member. Receiving is
    exempt in this PR and retains its existing mailbox/holding/safety caps; #707 adds the
    shared reservation to DATA acceptance. Other apps' stale reports retain KyIdentity's
    grant/TTL bound. Failed reservations overcount, never refund uncommitted writes.
  - **Regression evidence.** `TestOrgStorageBatchImportUsesCachedUsage` imports 1 and 64
    messages with 0 and 100 unrelated orgs, counts zero mailbox storage reads per write,
    and disables authority storage after the report. `TestOrgStorageConcurrentWriters`
    admits exactly one of two mailbox transactions competing for one message's headroom.
    `TestStorageReservationsSurviveReport` covers shared admission, expiry and late answers.
- **What a member sees.** `mailbox.ErrPoolFull` (`mailbox/pool.go:11`) is `ErrCapacity`, so
  every existing capacity path still refuses. Its text names the organisation's storage:
  - an immediate send answers `507` with that text and no `outboxId`, since nothing was queued
    (`api/native_outbound.go:48`);
  - an import stops with "your organisation's storage is full" (`api/mail_import.go:508`);
  - a scheduled send returns to Drafts with that reason (`api/scheduled_send.go:518`);
  - a photo upload answers `400` with the text.
- **Tests:**
  - `mailbox` `TestPoolCheck`: what is checked and with how many bytes; that a refusal writes
    nothing; and that a replay, Sent filing and a held import are never checked.
  - `tenancy` `TestStorageGrant`: the ceiling to the byte, expiry, the zero grant, `seq`
    order, a late answer, and another org's grant.
  - `tenancy` `TestReportStorage`: credentials, every malformed answer, oversize, an insecure
    issuer, a redirect, and the batch bounds.
  - `api` `TestOrgStoragePool`, against a fake KyIdentity:
    - org A's figure counts both members' mailboxes and photos;
    - org B's figure holds none of A's, and the staff org is not reported;
    - no grant, an org at exactly the ceiling, one byte over, the send route's `507`;
    - org B unaffected by A;
    - a held import over the pool;
    - a deletion freeing room at the next report;
    - a `ttl_seconds` past 15 minutes, KyIdentity down, a `stale` answer, an expired grant,
      `unknown_org`, a zero grant, and the staff exemption.
  - `api` `TestOrgStoragePoolCountsHeldMail`: held mail counts in the report and in the write
    check, for its own org only.
  - `api` `TestOrgStorageReportSkipsAFailingOrg`: the first org's usage read fails; the second
    is still reported and granted; the first keeps its grant and sequence; the log names no
    org, user or path.
  - `api` `TestSingleTenantHasNoStoragePool`: no report, no check.
  - The single-tenant upgrade test is unchanged.
  - **Proof that the gates bite:** each of these mutations fails its test:
    - removing the append, enqueue, photo or send-answer check;
    - checking held imports;
    - `<` for `<=` at the ceiling;
    - ignoring expiry;
    - counting other orgs' mailboxes;
    - not counting held mail, in the report or in the check;
    - stopping the report at one org's failure, or logging its raw error;
    - not counting photos;
    - reporting the staff org;
    - dropping the staff exemption;
    - letting a stale or late answer overwrite;
    - checking in single mode.
- **Not built:**
  - Receiving grace is supplied by the stacked #707. Receiving is
    unchanged.
  - The UI's pool usage and its 80 % and 95 % warnings. No screen changed, so there are no
    screenshots.
  - An early report when half the grant is used. The 60 s report is the only one, so a fast
    import can stop at its grant until the next report.
  - Incoming encryption's replacement copies are not checked (they rewrite accepted mail).
  - The restore step that advances `org_storage.seq` past KyIdentity's stored `seq` (S "After
    a restore"). Until an operator advances it, every report answers `stale` and writes stop
    once the grant expires. It belongs with PR 7's restore runbook.
  - Object storage (MH11) is a later phase.
- **Owner questions, each with the default now in code:**
  1. A member's figure is every mailbox their account owns. An extra mailbox counts against
     its owner's org.
  2. `want_bytes` is at least 256 MiB per org per report, which reserves that much of an idle
     org's pool for KyPost. The alternative is to release it with `0` when idle.
  3. The staff org is exempt from the check and never reported.

### Incoming mail over the pool (#707)

The owner's 2026-11-23 rule replaces open question 16: incoming mail has 7 days of
receiving grace, limited to max(10% of the pool, 2 GB) over; then 452 4.2.2 for 3 days;
then 552 5.2.2 (hosted exit 14). Only stored usage exhausting overage ends grace early.
`first_over` and `grace_end` are write-once per overrun; a fresh report confirming the org
back within its pool clears them. Missing pools fail closed with 452; staff is exempt.
`pool_bytes`, `org_used_bytes`, `over_since`, `first_over`, `grace_end` extend the new,
unreleased `org_storage` table: multi startup is still refused, so there is no prior shape.

RCPT uses the cached grant as a probe. DATA reserves bytes once per org (all distinct
mailbox copies), in one shared tenancy writer transaction before holding acceptance.
Mailbox/photo writes use the same counter. A failed acceptance can overcount until the
next report. `reported_bytes` remains fixed when `used_bytes` grows, preserving other
apps' contribution to the grace ceiling. Exact accepted replay never reserves twice.
The report-overlap bound above also includes incoming writes: at most one message per
mailbox quota fence in flight at the watermark, each within the mailbox message limit.
No per-message full-org or install-wide pool usage scan runs.

## Residual risks

- **One install, one database, one host until MH7 ships.** Every org of a cell shares one
  PostgreSQL, and a restore rolls every org of that cell back (R, T). With several regions there
  is one cell per region (MH "Regions"). Until MH7
  (`docs/superpowers/specs/2026-11-22-multi-host-replicas-design.md`), R's replicas also share one
  host, and multi mode cannot start before then (Phased build plan, amendment 2026-11-22).
- **Backups outlive a purge.** A deleted org's mail stays in older capsules until they expire.
- **Delete everything cannot be undone.** Someone who holds the password and the second factor can
  delete the whole org. Step-up and the typed address stop a stray click, not a thief.
- **Mail during scheduled deletion is lost.** The owner accepted this (open question 11).
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

1. **Where a hosted member's mailbox address comes from.** *Answered:* KyIdentity is given the
   address, and KyPost provisions the matching mailbox. KyIdentity gains a mailbox address field
   on a suite email domain (#277), separate from the sign-in email (Mail).
2. **How KyPost knows the staff org.** *Recommend* `KYPOST_STAFF_ORG_ID` (K7). Also ask KyIdentity
   to refuse app roles named `*.admin` for accounts outside the staff org, so the rule holds on
   both sides.
3. **Hosted receiving profile.** *Answered (2026-11-21):* the receiving tier gets its own spec,
   `docs/superpowers/specs/2026-11-21-hosted-receiving-tier-design.md`. It covers stateless MX hosts running
   today's Maddy, `receiving serve` over mutually authenticated TLS instead of the local socket,
   `250` only after the PostgreSQL commit, and import leased per mailbox.
4. **Mail export while deletion is scheduled.** *Answered (2026-11-21): option (a) is approved.*
   KyIdentity issues an `export_only` claim during `deletion_scheduled`, and KyPost admits only the
   export routes under it. It needs a T change. Until that lands, there is no export in the window
   (the former option (b)). Delete everything's path for a holder with other members depends on it (Delete
   everything).
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
   R's single host before launch, as T owner answer 8 does for KyIdentity. With several regions
   the target is per cell (MH "Regions"). Business orgs (N seats, on the shared install since 2026-11-23)
   belong in the measured mix.
9. **The purge versus the never-delete triggers.** *Answered:* yes. K10 as written: scrub to
   tombstones, with one named trigger exception keyed on a live `org_purges` row, plus HMAC address
   tombstones. The owner also asked for a Delete everything button that sticks (Lifecycle).
10. **Per-customer custom domains (later).** As M "Per-customer custom domains", the domain proof
    must bind the org as well as the issuer. `native-domains.json` holds one issuer
    (`sso/native_domain.go:68-74`) and would need a per-domain `org_id`. Out of scope for v1.
11. **Receiving while deletion is scheduled.** *Answered:* keep the `550`. Mail sent during the
    grace period is lost if the org is resumed, and the owner accepts that. Billing's deletion
    notice tells the account holder that mail stops at once.
12. **Business features on the shared install (new, 2026-11-23).** Business orgs now share the
    install, but T keeps inbound SCIM, HR provisioning and per-org federation off in multi mode,
    and there are no customer administrators (decision 4). *Default:* they stay off. A Business
    org's people are added through billing and KyIdentity's member endpoints, and extra
    mailboxes and aliases by staff on request. A business that needs a directory sync or its own
    IdP buys Enterprise.
13. **Family storage: pooled or per member (new, 2026-11-23).** *Answered (2026-11-23):* pooled,
    in every plan, as one number per org across all apps, held by KyIdentity (Quotas).
14. **A Business holder's Delete everything (new, 2026-11-23).** The holder's click deletes the
    whole org, every seat included, after the 7-day member export window. *Default:* the same rule
    in every plan, because KyPost sees no plan. If Business should differ (for example, the
    button only removes the holder, or needs a second person), that is a T and billing change
    that KyPost does not need to know about.
15. **An org entry with no storage pool (new, 2026-11-23).** *Default:* the pool reads as 0, so
    the org accepts nothing new and receiving answers a temporary `452` at once (Quotas). Open
    question 16's grace does not apply: a missing or malformed pool has no over-quota-since
    date and no meaningful 110 %. K PR 7 must not accept
    `multi` before KyIdentity publishes the pool, so this happens only through a KyIdentity fault.
    The alternative, no pool cap, lets an org grow without limit during that fault.
16. **Incoming mail while an org is over its pool (new, 2026-11-23; KyPost owns this rule).**
    *Answered (owner, 2026-11-23, ky-launch#4):*
    - **Grace:** 7 days' grace and up to 10 % of the pool over it (at least 2 GB), whichever
      ends first.
    - **Then:** `452 4.2.2` for 3 days, then `552 5.2.2`.
    - **Org status answers win:** `550` during scheduled deletion.

    This replaces the recommendation of 14 days or 110 % and never a permanent refusal. As
    built ("Org storage pool as built"):
    - The overrun starts at the earlier of KyIdentity's `over_since` and KyPost's own first
      sighting, which is written once per overrun.
    - The 3 days run from the end of grace, whichever bound ended it.
    - An answer back within the pool clears both dates, so the next overrun gets a fresh
      window.
    - `552 5.2.2` is exit 14, rendered only in the hosted profile.
    - A missing pool is open question 15: `452` at once, no grace.
    - Grace never passes the per-mailbox install ceiling, the 25 MiB cap or a `mailboxBytes`
      key, which receiving checks first.
    - **Not built:** the banner ("Storage full: new mail is still accepted until <date>"),
      with the other UI.
