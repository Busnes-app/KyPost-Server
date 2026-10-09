# Turnkey mail Phase 1 evidence

2026-10-03. The implementation plan is approved. This change set adds executable feasibility checks and a caller audit; it introduces no production mailbox mode, receiver or relay configuration. **Phase 1 remains open:** receiver ownership/retention gates and production storage measurements below are unresolved.

## Local storage through the existing API

[TestNativeMailboxProof](../backend/internal/api/native_mailbox_proof_test.go) uses the installed SQLite driver and MIME parser. It injects a test-only reader into the existing server's cached client slot, retaining the real HTTP router, authentication and handlers. Existing test scaffolding supplies unexercised methods. It is deliberately not an authoritative mailbox adapter or a migration.

Run:

```bash
cd backend
GOTOOLCHAIN=go1.26.9 go test -race ./internal/api -run '^TestNativeMailboxProof$' -count=1 -v
```

Verified: exact raw MIME survives database close/reopen; a rolled-back insert remains invisible; numeric IDs remain unchanged across reopen; owner and folder predicates prevent body retrieval across those boundaries. Browser sessions and paired-device headers both exercise classic inbox, `since=0&bodies=0`, unchanged cursor polling, lazy body, attachment metadata and byte-identical attachment download through existing routes. Unauthenticated body reads return 401. The proof does not test changed-message deltas, moves, labels, PGP verification/encryption, offboarding or source switches.

The disposable database uses WAL with `synchronous=FULL`, one connection, raw BLOBs and an owner/mailbox/UID index. Its shared test schema exercises owner predicates; the plan still proposes separate per-user production databases. Reopening is a process-lifecycle check, not a power-failure test.

Measured locally without race instrumentation:

| Workload | Observed result |
| --- | --- |
| 1.1 MB binary attachment | Exact bytes through authenticated download; raw MIME 1,505,895 bytes |
| 8 MiB binary attachment, insert plus native authenticated read | 161 ms; raw MIME 11,479,767 bytes |
| 11,001 small messages, one insert transaction plus 200-ID keyset pages | 63 ms; all 11,001 enumerated without MIME BLOB reads |
| Database and WAL at end of the proof | 16,027,648 and 11,573,112 bytes respectively |

These are feasibility observations, not throughput promises. Bulk rows are small test messages, not a representative full mailbox. The read spike reparses complete MIME even for overviews and buffers attachments; production must bound parsing and memory and index envelope metadata. BLOB storage remains the initial candidate, **not a completed suitability decision**. Remaining measurements: representative MIME mix beyond 10k messages, concurrent API/daemon writes, allocation/peak RSS, quota and disk-full recovery, checkpoint behavior, online backup size/time, and restored raw/receipt integrity. Keep these outside production until acceptance checks exist.

## Production caller inventory

Audited production callers of every `imap.Client` operation (excluding tests and underlying dependency calls). Paths below are relative to `backend/internal/`; the interface itself remains in `adapters/imap/client.go`.

| Operations | Current callers |
| --- | --- |
| `ListUnreadInbox` | `processor/poller.go` classification/checkpoint path |
| `ListUnreadMessages` | `api/server_inbox.go` cold/classic inbox |
| `ListOverviews` | `api/server_inbox.go` cursor/cache path; `api/rules_handlers.go` rule preview |
| `SearchMessages` | `api/server_inbox.go` search; `processor/sendas_check.go` alias proof lookup |
| `GetMessageBodies` | `api/server_inbox.go`, `api/mail_body.go`, `api/pgp_client_read.go`, `api/rules_handlers.go` |
| `ListLabels` | `api/server_settings.go` |
| `ListSubfolders`, `CreateFolder`, `RenameFolder`, `DeleteFolder` | `api/server_inbox.go` folder routes |
| `EnsureLabel` | `processor/poller.go`, `processor/phish_scan.go` |
| `ApplyLabel` | `api/server_inbox.go`, `processor/poller.go`, `processor/phish_scan.go`, `rules/engine.go` |
| `RemoveLabel` | `api/server_inbox.go`, `rules/engine.go` |
| `ApplyInboxAction` | `api/server_inbox.go`, `rules/engine.go` |
| `ListAttachments`, `GetAttachment` | `api/server_mail_attachments.go` |
| `SaveDraft` | `api/server_mail_send.go` ordinary/encrypted draft paths |
| `SaveSent` | `api/server_mail_send.go`, `api/pgp_send_client.go` |
| `FetchHeaderFields` | `processor/poller.go` rules; `processor/autocrypt_harvest.go`; `api/rules_handlers.go` |
| `FetchRawMessage` | `api/pgp_client_read.go` signed MIME; `processor/phish_scan.go`, `processor/sendas_check.go`, `processor/autocrypt_harvest.go` |

Incoming encryption has an additional capability interface in `processor/incoming_encryption.go`: `PrepareIncoming`, `ReplaceIncoming`, `ApplyIncomingAction`. Its persisted recovery jobs carry `imap.IncomingSource`, replacement UID and ciphertext. A native backend must implement equivalent verified replacement/recovery and update pending-job/source-mode guards; merely implementing `Client` is insufficient. API eviction also recognizes `io.Closer` in `api/server_userscope.go`.

Both construction paths currently require stored IMAP configuration: `api/server_userscope.go:userMailClient` and `processor/poller.go:userMailClient` (through `newMailClient`). Native selection must update both, plus configuration readiness checks; changing only the API leaves classification on the external service. Stable identity also reaches `state` decision/checkpoint records, cache keys and tombstones, sorter correction hashes, push payloads and client caches. Define source generations and stale-reference handling before enabling switches.

Sending is coupled to those per-user configuration files independently of the mailbox interface. `mailmsg.SMTPDeliver` and `SMTPDeliverPrepared` are the shared validated transport sinks. Callers include ordinary/server-PGP compose (`api/server_mail_send.go`), client-PGP compose (`api/pgp_send_client.go`), pickup notices (`api/pickup_handlers.go`), alias verification (`api/sendas_handlers.go`, `processor/sendas_self_probe.go`), oversized-mail notices (`processor/poller.go`) and model-update notices (`api/ollama_version.go`). Domain relay resolution must cover all of them. Persist outgoing wire MIME separately from the encrypted Sent copy, and preserve current send-success semantics; Sent retry must never trigger transport resubmission.

## Client evidence and limits

Source inspected in neighboring repositories; no client code was changed and no live Android, Linux or Apple app was launched.

| Client | Current evidence and implication |
| --- | --- |
| Android | `kypost-android/app/src/main/java/org/kysecurity/mail/mail/RelayMailSource.kt` sends device headers, `since`, `bodies=0`, then lazy mailbox/message-ID body requests. `data/EmailDao.kt` scopes stored rows by folder and message ID. The server proof exercises those read shapes, not the running app or its complete PGP lifecycle. |
| Web | `frontend/src/pages/ReadPage.tsx` uses `since=0`, `bodies=0`, mailbox/message-ID body lookup and cursor polling. Session-authenticated proof covers those read shapes; full browser interaction remains a release check. |
| Linux | `kypost-Linux/core/net/RelayMailSource.cpp` uses the same inbox/actions and optional cursor, currently requesting full bodies rather than `bodies=0`. Preserve full-body responses; do not make lazy-body adoption a prerequisite. No executable native-storage client acceptance run yet. |
| iOS/macOS | Shared `kypost-for-Mac/KyPost/Data/Mail/RelayMailSource.swift` uses string message IDs, flexible numeric/string cursors and full-body inbox reads. Its search method explicitly uses the local EmailDAO cache. Preserve this current behavior; server-wide search and device-level acceptance are separate gaps. No Apple build/device test here. |

This evidence supports retaining the existing API; it does not establish cross-platform feature parity. PGP enrollment, ciphertext reads, key lifecycle and signed-only behavior still require the existing contract checks plus native-backend parity tests.

## Receiver result and next work

[Receiving assessment](RECEIVING_GATEWAY_ASSESSMENT.md) records both the original queue experiment and the selected synchronous boundary. The receiver accepts SMTP and invokes the local holding store; it does not pull SMTP. The holding-store commit precedes upstream acknowledgment. Later, the importer must commit local mailbox messages and idempotency receipts before acknowledging the holding receipt and releasing its payload.

The follow-up now implements the internal `ingress` holding store and qualifies Maddy's synchronous command boundary against it. Mail is committed before SMTP acknowledgment, recipients are frozen at RCPT, and receipt identity comes from the trusted receiver rather than headers. The receiver's stock expiring queue is outside this path. See the [updated receiving assessment](RECEIVING_GATEWAY_ASSESSMENT.md#synchronous-receiving-boundary) for execution, guarantees and remaining adoption gates.

The binding/retention milestone passes isolated integration checks, including full-spool refusal and a holding-writer crash after commit. The permanent-store follow-up below adds actual mailbox import receipts. Next: connect verified directory routing and runtime integration, add safe fenced reservation cleanup, and complete physical capacity/backup/representative storage measurements. Phase 1 remains open for those production suitability gates. No production rollout, MX change or Mailflare fork is justified by these checks.

## Verification and handoff

Passed: native proof under the race detector; existing cold-inbox, first-delta, lazy-body and encrypted-attachment fetch-cost regressions; `go vet ./internal/api`; the repository-pinned golangci-lint 2.12.2 against `./internal/api` (zero issues); pinned Maddy qualification; optimized-Python refusal; documentation links and `git diff --check`. An independent reviewer repeated the native race check and cleared the artifacts after the script was changed to refuse disabled assertions. Full CI and release/device acceptance were not run; this is not a merge or release assertion.

Follow-up validation: ingress unit/concurrency and pinned Maddy integration checks pass under the race detector; package lint reports zero issues, vet passes, and the full backend builds. Independent review found an unsafe age-based staged-reservation sweep; it was removed and a regression now proves aged SMTP transactions retain every accepted RCPT. No automatic deletion replaces it. Full CI and release/device acceptance remain outstanding.

DOX: root/backend child indexes include `internal/ingress/`, whose own AGENTS.md owns its trust/retention contracts. README Project Structure describes the internal core without advertising enabled reception. Shipped PGP/platform wire contracts and container configuration remain unchanged because no runtime mode, route or environment setting selects it. The plan, assessment and this handoff are mirrored to the existing myslop work folder. Phase 2 storage and import work is recorded below; resume from its adapter/integration gates before promoting reception or freezing production storage.


## Permanent storage and import follow-up

The internal [mailbox package](../backend/internal/mailbox/AGENTS.md) now stores exact bounded RFC5322 bytes, indexed envelope header metadata, folders, read/starred/draft flags, validated labels, immutable delivery receipts and durable changes in a per-owner SQLite database. The issuer/subject/mailbox identity is fixed on first open. Incoming imports atomically commit raw bytes, metadata, receipt and arrival event. Canonical frozen recipients distinguish exact retry from sender, generation, recipient or payload conflict.

Numeric IDs persist across restart and moves and never reuse. Moves and subtree renames atomically emit old-folder removal and new-folder arrival. Permanent deletion clears live raw/metadata but retains ID, digest, receipt and tombstone; exact delivery retry returns the deleted ID without resurrecting mail. Earlier WAL/pages/backups can retain plaintext. Folder deletion refuses live messages or children. Drafts and Sent accept raw stored copies; MIME composition and send behavior have not been connected to these stores.

Lists use descending-ID keyset pages of indexed metadata, independent of the rebuildable mail cache. Change pages and high-water are read in one transaction; callers must consume each returned event before advancing. Full history is currently retained. These are internal storage cursors, not the HTTP inbox cursor contract; source generations, full-resync protocol, client cache collision fences and folder-generation handling remain required before activation. Metadata currently holds raw header values; the adapter must supply consistent MIME display decoding and deterministic single-sender binding.

The [internal importer](../backend/internal/ingress/import.go) claims a holding receipt, verifies payload digest, groups aliases by frozen owner, checks every resolved store identity and commits one receipt/copy per owner before acknowledgment clears the holding payload. Partial failure, quota refusal and importer death retain accepted bytes. A retry after claim expiry resumes already-committed receipts even if the user moved/deleted a copy. Reassignment before claim quarantines without resolving a new owner. Reassignment during import is detected again at acknowledgment and now durably quarantines the holding item; copies already committed remain with their frozen owners. Coordinating directory revocation with individual import commits remains an activation gate. The bridge is trusted local code; it supplies no network authentication, verified directory resolver or runtime scheduler.

SQLite-maintained usage counters update in the message transaction across processes, including deletion and rollback. Opening an existing database backfills missing counters under the same immediate writer lock. Live-byte and retained-record quotas are separate; deleting mail frees live bytes but does not reclaim retained identities. Quota changes, receipt/history retention, physical disk exhaustion, representative MIME memory use, WAL checkpoints and consistent backup/restore still require operator procedures and release evidence.

Runnable checks:

```sh
cd backend
GOTOOLCHAIN=go1.26.9 go test -race -count=1 -timeout=20m ./internal/mailbox ./internal/ingress
```

Storage checks cover reopen/raw preservation, owner refusal, conflicting receipts, retry after deletion, flags/labels, Drafts/Sent copies, subtree rename, stale-folder rejection, concurrent receipt/quota writers, counter backfill/rollback and complete 11,001-message metadata/change pagination. Import checks include aliases plus an envelope-only recipient, partial recipient failure with store restart, wrong-owner refusal, reassignment before/during import, and a real killed subprocess after its first mailbox commit. The subprocess does not simulate power loss or prove hardware fsync behavior.

Remaining Phase 2 work after the follow-ups below: both API/daemon source selection, source/cursor collision fences and guarded migrations. The original test-only API read spike remains separate evidence and uses no production adapter. KyIdentity provisioning, runtime receiving, domain relay/outbox, guided setup and release acceptance follow. No production route, source setting, container change or client wire behavior enables this foundation.

Validation: the complete mailbox/import suite passed under the race detector in independent review, including the 11,001-message check. Replacing per-import quota scans with transactionally maintained counters reduced that check from about 291 seconds to 20–22 seconds on this workspace; this is a synthetic developer check, not a production throughput promise. Final mailbox/ingress race checks also passed with the pinned Maddy binary; package vet/lint passed and the full backend built. Full repository CI, live client/device runs, representative disk/backup and release checks remain outstanding. DOX indexes and README structure now include the permanent store; PGP/platform wire docs remain unchanged because runtime behavior is unchanged.

## Native mail Client follow-up

The internal mailbox Client implements the complete existing `imap.Client` interface without a new dependency. Metadata decodes MIME headers consistently with the existing adapter; body/attachment parsing reuses its rendering, PGP detection and single-sender binding. Stored signed and encrypted MIME remain exact. Draft/Sent composition reuses the existing builder; client-encrypted drafts preserve bytes accepted by the existing API (which trims outer whitespace).

Read/label edits use atomic per-field SQL updates across writers. Label definitions are case-insensitive and capped at 1,000, with 100 labels per message. Folder deletion moves contents to the parent atomically; moves/renames preserve IDs. Batch body reads and full-text search have byte/time/count bounds; search currently scans raw mail and fails explicitly on budget exhaustion. Representative large-folder performance remains unqualified.

Malformed MIME fails only its own body read: metadata/raw bytes remain available, lazy body returns 422, and the poller records a failed decision without blocking later mail or consuming classifier capacity. The platform baseline documents that error. Existing IMAP parsing behavior is unchanged.

`TestNativeMailboxClientAPI` injects the real Client into existing session/device routes and checks owner isolation with coincident IDs, offboarding, metadata/delta reads, body/attachments, labels, actions, folder changes, search, drafts, exact signed payload verification and client-protected ciphertext reads that decrypt and verify with the generated recipient/sender keys. It also checks encrypted draft storage. This supersedes the test-only read spike as Client evidence; neither test enables runtime source selection.

```sh
cd backend
GOTOOLCHAIN=go1.26.9 go test -race -count=1 -timeout=20m ./internal/mailbox ./internal/ingress ./internal/adapters/imap
GOTOOLCHAIN=go1.26.9 go test -race -count=1 -timeout=20m ./internal/api -run 'TestNativeMailbox(ClientAPI|Proof)|TestServeInbox|TestMailBody|TestEncryptedAttachment|TestPGPPayload'
GOTOOLCHAIN=go1.26.9 go test -race -count=1 -timeout=20m ./internal/processor -run 'TestTickUser|Test.*Incoming'
```

Validation: package race checks, the authenticated API regression subset, focused poller regressions, changed-package vet and pinned lint pass; the backend builds. Required reviewer cleared internal use. Full CI, live clients, representative disks/search/backup and release checks remain outstanding. Production still selects external IMAP: source/cursor fences, verified provisioning and deployment are unfinished; native incoming-encryption recovery is covered by the next follow-up. No new route, environment setting or container behavior enables native mailboxes.

DOX pass: mailbox/adapters/backend contracts, README and changelog updated. `PLATFORM_BASELINE.md` gains the malformed-body error. `E2E_PGP.md`, WKD and `WEBMAIL_HANDOFF.md` stay unchanged because key custody, PGP payload shapes and launch contracts are preserved; root/ingress/scripts ownership and indexes remain valid. Rollback before activation is removal of the unused Client; preserve any experimental mailbox databases rather than dropping data or switching source flags.

## Native incoming-encryption recovery

The native Client now implements the existing narrow incoming-encryption interface. The poller continues to own eligibility, classification, ciphertext-only jobs, pending-key reservation, body-cache purging and processed decisions. No new key custody, endpoint, source setting or dependency is introduced. Existing IMAP behavior remains selected in production.

Sources bind immutable mailbox ownership and a durable random database namespace, which survives reopen but changes when a same-owner database is recreated. Replacement verifies the exact original hash and INBOX membership. In one SQLite writer transaction it creates a new ID containing exact PGP/MIME ciphertext and current flags/date/labels, clears original live plaintext/header metadata, records both change events and stores a bounded replacement receipt. Original gateway/import receipts retain their original IDs. Payload quota charges net live-byte growth; retained-record quota adds the replacement without dropping the original tombstone. Any failure rolls back all writes.

Recovery requires the original source binding, replacement receipt and exact ciphertext; a marker header alone grants nothing. Encrypted labels/read/moves verify and mutate within one transaction. A repeated committed move succeeds only for the verified copy in its expected destination. Valid move-plus-stop rules finish after restart. Changed/deleted copies and wrong namespaces pause for reconciliation, preserving the journal/key reservation rather than guessing or resurrecting mail.

Runnable checks:

```sh
cd backend
GOTOOLCHAIN=go1.26.9 go test -race -count=1 -timeout=20m ./internal/mailbox -run '^TestNativeIncoming'
GOTOOLCHAIN=go1.26.9 go test -race -count=1 -timeout=20m ./internal/processor -run '^TestNativeIncomingPollerJournalRecovery$'
```

Mailbox checks cover exact full original MIME inside protected ciphertext, current flags/date, receipt retry after replacement/deletion, same-owner database recreation, moved/changed sources, altered ciphertext, record refusal/net-byte quota, forced SQL rollback and quota counters, two competing writers, restart replay, and actual killed subprocesses after replacement and move commits. Killing a process does not prove power-loss/hardware fsync durability.

The real native Client also runs through the existing poller orchestration with faults limited to lost acknowledgments after actual replacement/move commits. Generated PGP ciphertext decrypts to exact original MIME. Recovery after opt-out/reopen does not rerun classification; it preserves labels/actions, clears plaintext cache rows, records both IDs as processed and one replacement decision, removes the completed journal and releases the key reservation. Pending jobs refuse key deletion. Enrollment wrapping remains an opaque test fixture; these tests make no enrollment-cryptography claim.

Validation: focused native/poller race checks pass independently in required review. Complete mailbox/ingress/IMAP race regressions passed (mailbox 25.141s, including complete pagination and process-crash checks); focused poller/incoming API checks, changed-package vet/pinned lint and backend build passed. Required reviewer cleared the final code and documentation. Full repository CI, live client/domain tests, representative disk limits, backup/restore reconciliation and release acceptance remain outstanding. Production activation still requires source/cursor/cache collision fences, both runtime selectors, guarded migrations, verified KyIdentity provisioning and access/revocation coordination.

DOX pass updates mailbox/backend contracts, README structure, changelog, approved plan and the incoming-encryption section of `E2E_PGP.md`. Platform/WKD/webmail launch contracts and root/ingress/scripts ownership stay unchanged because payloads, routes, setup and recipient routing are unchanged. The new schema is additive; pre-activation rollback retains experimental databases/journals with a compatible binary for export/reconciliation. Never drop data or change the source flag to bypass a pending job. Plaintext may remain in receiving storage, WAL/pages, earlier caches and backups; this milestone does not claim retroactive erasure.

## Source admission and safe native snapshots

Account state now has an immutable `meta.mail_source`. Normal `state.New` opens
preserve existing bindings and initialize unbound legacy or recreated state as
IMAP. Empty legacy state is never proof of freshness: clients may already hold
numeric references even when no checkpoint or cache survives. Internal
`state.NewNative` exclusively creates an absent state directory and binds its
native owner/database namespace before returning; it is reserved for verified
new-account provisioning, not migration or repair. Failure preserves the
created directory for reconciliation rather than guessing freshness on retry.

Both API and daemon preflight the cache, verify state admission, then bind the
cache before consuming mail IDs, checkpoints or pending encryption jobs. Cache
writes retain the binding. Mode changes and recreated/different native database
namespaces fail closed, including coincident numeric IDs in another owner's
store. Mail routes answer 409 with explicit restoration/migration remediation;
the inbox reports other state/configuration failures instead of claiming an
empty mailbox. No migration, directory provisioning or runtime source selector
is enabled by these guards. External IMAP endpoint/UIDVALIDITY changes remain a
separate existing limitation; the IMAP binding denotes mode, not a provider.

Native HTTP qualification keeps numeric message IDs and cursor types. Every
request uses a fresh live listing and full requested-window snapshot
(`delta:false`, `cursor:0`), even for positive/future `since` or a warmed classic
request. `bodies=0` stays metadata-only and default bodies remain available for
the desktop baseline. This deliberately trades repeated listing bandwidth for
safety until durable scoped deltas are implemented and tested. It does not
qualify incremental sync, full-history HTTP pagination, live client behavior or
source migration. Moves retain IDs; replacements allocate IDs; permanent
removals cannot resurrect through stale cache windows/cursors.

Runnable checks:

```sh
cd backend
GOTOOLCHAIN=go1.26.9 go test -race ./internal/state ./internal/mailcache ./internal/adapters/imap ./internal/mailbox -count=1 -timeout=20m
GOTOOLCHAIN=go1.26.9 go test -race ./internal/api ./internal/processor -run 'Test.*Inbox|Test.*MailBody|TestNativeMailbox|Test.*Incoming|TestTickUser' -count=1 -timeout=20m
```

Validation: complete state/cache/IMAP/mailbox race suites passed (31.418s,
7.906s, 2.083s, 28.834s); focused API/poller race suites passed (100.294s,
6.268s). Source tests exercise reopen, empty legacy state, deleted/recreated
state, changed namespaces/modes, cache preservation/preflight and a daemon
rejection with zero fetches/checkpoint writes. The real native Client API test
checks session/device requests, live snapshots, positive cursors, removals,
coincident owner IDs and refusal of reads/actions/drafts against a changed
source. Reviewer independently reran source/native API/poller recovery checks
with race detection and cleared the earlier provenance/cursor-lifetime blockers.
Changed-package vet, pinned golangci-lint v2.12.2 and backend build passed.
The final corrupt-cache/real-native API regression also passed under race
detection (2.090s), followed by API vet, pinned lint and backend build; the
reviewer independently passed that final subset (2.348s) with no surviving
blocker.
Full CI, live clients/domain delivery, storage/backup/restore acceptance and
production activation remain outstanding.

Next: verified KyIdentity new-account provisioning and crash/retry
reconciliation, shared API/daemon source selection, then durable scoped deltas
and restore/migration contracts before activation. Receiver access/offboarding
and operator relay/outbox work follow the approved plan. Do not implement
activation by deleting state, changing a source flag, or automatically falling
back to another source. Pre-activation rollback preserves experimental
mailboxes/state/cache/jobs with a compatible binary for export/reconciliation.

DOX pass updates backend/adapters/mailbox/mailcache contracts, README structure,
changelog, approved plan and the platform inbox contract. Root ownership/index,
ingress/scripts, E2E PGP/WKD/webmail and contribution gates stay unchanged:
no route, setting, deployment, key lifecycle or custody behavior changed.

## Durable directory desired state and native account preparation

The verified webhook now retains the supported `DirectoryUser` SCIM fields
alongside the issuer/subject revision, exact-body digest and access fence in one
lifecycle-file publication. Signature verification still covers original body
bytes; supported fields are not a copy of unknown SCIM extensions. Generic
`emails` entries are not implicitly aliases or evidence of domain ownership.
Directory acknowledgment retains desired data and applies existing access
changes; it does not assert mailbox provisioning or receiver readiness.

Stale/conflicting events cannot replace desired data. Failed account apply or
JSON publication remains an explicit failure; retries retain the existing
idempotent apply-before-fence semantics. Newer compatibility events without a
resource clear older desired data. Legacy resource-less records and exact
legacy retries do not invent provenance; repair needs a newer verified event.
Desired records persist per subject. The existing whole-file JSON store scales
with account count; SQLite becomes appropriate if that is a measured bottleneck.

Internal `mailbox.PrepareAccount` stages empty permanent mailbox storage,
source-bound native `state.db` and `native-mailbox.json` before publishing one
complete `$STATE_DIR/users/<localID>/` directory. Mail lives at its
`mailbox/mailbox.db`. It closes and syncs database files and publishes the
manifest before an atomic Linux no-replace directory rename, then syncs the
parent. The caller must independently prove verified new-account identity,
domain authority and unique primary address. No production webhook, worker or
source selector calls it. A preparation manifest grants no receiving readiness,
alias approval or user access.

Preparation uses the existing pinned x/sys v0.48.0 dependency (now direct)
for `RENAME_NOREPLACE`; there is no new dependency or version bump. The syscall
refuses even an empty destination created by an ordinary state-store opener
that does not share the preparation lock. Unsupported platforms/filesystems
fail explicitly, never fall back to replacing a directory.

Retries first require a protected directory, manifest and existing regular
nonempty database files. Read-only SQLite queries check required tables,
immutable owner, persisted limits, namespace/source and address. They never
call a constructor that could recreate missing state or adopt a partial
restore. Different identities/addresses/limits and missing/corrupt databases
refuse with existing files unchanged. Published preparation survives a lost
acknowledgment without allocating another namespace. A crash before publication
leaves no visible partial account; orphan staging directories contain empty
preparation only and need bounded cleanup before activation. These checks are
not an integrity proof for arbitrary partial restores or hardware power loss.

Runnable checks:

```sh
cd backend
GOTOOLCHAIN=go1.26.9 go test -race ./internal/mailbox ./internal/sso -count=1 -timeout=20m
GOTOOLCHAIN=go1.26.9 go test -race ./internal/api -run 'TestDirectory|TestSSO|TestNativeSign|TestNativeMailboxClientAPI' -count=1 -timeout=20m
```

Tests cover retained desired data across reopen/retry, stale events, failed
account apply and failed publication, compatibility invalidation, unsigned
webhook refusal and existing deactivation/rehire/access gates. A real signed
webhook drives explicit test-only storage preparation; offboarding retains its
immutable source. Preparation checks retain actual appended raw mail on retry,
refuse mismatched/incomplete/corrupt state, serialize concurrent creation and
preserve a competing ordinary state directory. Subprocesses are asserted to
die from SIGKILL immediately before/after the real publication syscall, then
recovery verifies absence before publication and unchanged namespace after it.

Validation: full mailbox/SSO race suites passed (26.703s/4.048s), and focused
API directory/SSO/native regressions passed (14.371s). The stricter SIGKILL
boundary assertion passed separately (1.218s). Changed-package vet, pinned
lint (0 issues) and backend build passed. Independent required review reran
preparation, desired-resource and signed-directory/API checks under race and
cleared final code and documentation with no surviving blocker. Linux amd64
and arm64 builds passed. Full CI, live
KyIdentity/client/domain tests, receiver readiness, storage/backup/restore and
release acceptance remain open.

Subsequent reservation/reconciliation, admin mail-domain proof and disabled
prepare-before-publication allocation are described in [the provisioning
contract](NATIVE_PROVISIONING.md). Runtime worker integration, whole-stack
restore, source selection, receiver routing/revocation and aliases remain gates;
preserve default external IMAP. Do not use this
primitive to convert existing accounts, bypass missing/corrupt state, erase
mail, or activate reception from a preparation manifest.

DOX pass updates backend/mailbox contracts and README Features, API Highlights,
persisted-file description and Project Structure, plus changelog and the
approved phase-3 plan. Root/index, ingress/adapters/cache/scripts,
PGP/WKD/platform/webmail and contribution docs stay unchanged: this milestone
adds no route, required wire field, environment variable, client selector,
key lifecycle or transport. Pre-activation rollback preserves prepared state,
mailboxes and manifests; keep a compatible binary for reconciliation/export.
Older lifecycle writers can discard retained resource fields, so obtain a new
verified directory revision before resuming provisioning after such a rollback.

## Merge qualification (2026-10-03)

This change qualifies the disabled internal foundation, not a production native
mail stack. Full backend race suites pass (API 210.436s); formatting, vet, pinned
lint and vulnerability checks pass. Frontend typecheck, all 918 tests in 73
files, production build and dependency audits pass. Both push workers typecheck
and all 76 relay tests pass. Script checks, compose validation, the pinned Maddy
receiver proof, and the actual Docker build/health/heartbeat/cleartext refusal
checks pass. Initial concurrent host runs hit existing frontend and login timing
assertions; isolated reproductions and complete suites on two dedicated CPUs
pass without changing the tests or their thresholds.

Independent hostile review finds no merge blocker for this disabled scope.
Surviving activation concerns: coordinate importer commits with directory
revocation; qualify retention, quota adjustments, physical disk limits and
backup/restore; implement durable client deltas/history pagination and qualify
live clients. Directory events currently rewrite the retained lifecycle JSON
file, which also needs a measured scaling limit. These concerns remain release
gates, not claims of shipped support.

The DOX pass confirms the updated root/backend/adapter/cache/ingress/mailbox/
script contracts and indexes, README, changelog and cross-repo source/PGP docs.
Contribution and CI contracts remain unchanged. Revert the change to roll back
the disabled foundation; retain any manually prepared storage and obtain a fresh
verified directory revision before resuming preparation with a compatible build.
Remote CI and the autonomous PR reviewer must independently clear the pushed
head. CONTRIBUTING.md requires the human contributor's own verification and
trust-boundary statement before merge; agent checks cannot supply that attestation.


## Android native-mail runtime qualification

The optional `device-android` case joins the existing native signed-directory,
receiving-store and actual TLS SMTP fixture to Android's production registration
and mail transports. Use a dedicated disposable emulator, build/install both Play
debug APKs from an Android checkout containing `NativeMailboxRoundtripTest`, then:

```bash
cd backend
KYPOST_NATIVE_ANDROID_TEST_SERIAL=emulator-5580 GOTOOLCHAIN=go1.26.9 \
  go test -race ./internal/api \
  -run '^TestNativeOutboundAPIActualTLSAndPGP$/device-android$' \
  -count=1 -timeout=4m -v
```

The bridge binds only loopback HTTPS and uses adb reverse. Its public test CA is
trusted only by the instrumentation client's TrustManager; hostname verification,
leaf pinning, redirects and response bounds retain the production factory's
behavior. All accounts, directory/DNS proof, push tokens and correspondence are
explicit synthetic fixtures. No operator configuration or provider is used.
The Android test clears pairing preferences on this disposable install and uses
an in-memory Room database; never run against an operator's app account.

Assertions cover real registration, persisted encrypted pairing proof/leaf pin,
native generation IDs through Room, body retrieval/cache preservation, keyword
labels, exact binary attachment bytes, read-state synchronization, wrong device
credentials and leaf pins, stale-reference refusal, and ordinary client sending
with its Sent result. The server independently checks one accepted outbox claim,
TLS/AUTH, Bcc envelope isolation, a single durable Sent copy/recovery and directory
offboarding. Without an explicit emulator serial the joined case is absent; green
ordinary CI is not evidence that this runtime check ran.

This uses the trusted-local durable ingress import, not Android-originated public
SMTP reception. The real Maddy/app/supervised receiving proofs remain separate.
Physical-device behavior, Android client PGP/enrollment, live provider receipt,
public-domain deployment, restore activation and power-loss qualification remain
pending. No production route, setting, wire field or dependency changes here.


Local evidence on Android 12/API 31: the joined seven-mode ordinary/PGP/device/
uncertainty/recovery race fixture passed (20.442s); the final Android-only run with
wrong-pin, wrong-secret and valid foreign-generation rejection passed (5.260s).
Existing pairing/pin/Room instrumentation passed 20 tests, and all 1294 JVM tests
in 144 suites passed. The full server API race suite passed (261.898s), and whole-
backend vet/build/pinned lint passed with zero issues. An independent reviewer
read both test implementations and production paths, ran the default six-mode
server race regression (12.880s), and found no blocker; its assertion/comment
precision findings were corrected. These results used a debug Android build
with the repository's public CI Firebase placeholder, not production credentials.
