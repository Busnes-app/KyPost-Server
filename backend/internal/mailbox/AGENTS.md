# Permanent Mailbox

## Purpose

Per-owner permanent raw mail, indexed metadata, folders, flags, labels, delivery receipts, durable change history and encrypted native outbox intent/claims/Sent obligations.

## Ownership

All files in this package. Internal storage and complete `imap.Client` implementation; runtime selection requires explicit native mode and current admission. `NewClient` borrows the store; its caller owns the store lifetime and verifies the supplied sender address. Runtime `OpenClient` opens existing-only storage, requires per-operation admission and owns cleanup; KeepAlive protects active borrowers from garbage collection.

## Local Contracts

- Internal native outbox encrypts complete immutable intent with a relay-master HKDF key bound to owner/namespace/job ID; reserve delivery/Sent quota atomically. Claims are durable and never reclaimed after a crash; ambiguous acceptance remains excluded from automatic work. Only definite 4xx retries, at most six attempts. Sent filing has its own atomic receipt and cannot authorize resubmission. Snapshot validation is read-only historical evidence, with key/domain (relay `domains ∪ retiredDomains`), shape, foreign-key and quota checks. `QueuedOutboundFrom` lists the `From` of jobs with a queued or retryable delivery for domain-retirement and relay-removal guards; callers hold the domain fence. The shared native coordinator fences current authority through claims; `FromGeneration` (≥ 0; 0 on jobs queued before per-address generations) records the `From` ledger generation for that fence. Native compose/client-PGP routes and recovery workers are enabled only in explicit native mode. Follow-on discovery requires accepted primary evidence; uncertainty/crashed claims are never replayed. See [outbox contract](../../../docs/NATIVE_OUTBOX.md).

- `PrepareAccountContext` cancels account-lock contention and checks cancellation before no-replace publication. The legacy wrapper keeps blocking flock semantics; filesystem open/fsync and read-only SQLite validation remain a healthy-volume activation gate.

- Internal `PrepareAccount` publishes an empty mailbox, prebound account state and `native-mailbox.json` together at `$STATE_DIR/users/<localID>/` (`mailbox/mailbox.db` plus `state.db`). Caller proves verified new-account issuer/subject ownership, domain authority and unique primary address. This is storage preparation, never receiver readiness or a migration. The opt-in native allocator calls it before account publication.
- `PrepareMailboxContext`/`ValidatePreparedMailbox` are the same preparation under an explicit parent (`$STATE_DIR/mailboxes` for extra mailboxes); the created `state.db` must stay mail-only, which restore validation enforces.
- `ValidatePreparedAccount` is read-only preparation validation for acknowledged provisioning sources; unlike `PrepareAccount`, it must never recreate a missing account directory.
- Preparation serializes cooperating writers and uses Linux `RENAME_NOREPLACE` to refuse even an empty or concurrently created destination. Other platforms refuse explicitly. Retry reads existing regular databases without creating schemas, checks required tables/owner/source/namespace/address/limits, and refuses incomplete, legacy or recreated state. Preserve published data; crash-abandoned staging directories contain only empty preparation and need bounded cleanup before activation.

- Open only an owner-only directory selected from verified provisioning. The durable issuer/subject/mailbox tuple is immutable; opening the database for another owner fails. Validate user path components with fsutil before constructing paths.
- SQLite immediate writer transactions, FULL synchronization and raw BLOBs keep bytes, metadata, receipt and change atomic. Reuse installed dependencies.
- Import identity is gateway/delivery within this owner, with a canonical frozen envelope and SHA-256 digest. Exact replay returns the original numeric ID even after move/deletion; conflicts retain the holding copy. Aliases of the same owner produce one receipt/copy.
- Numeric IDs never reuse within an uninterrupted database history; older snapshots can rewind allocation. A separate strict UUID-v4 `reference_generation` singleton persists across reopen and rotates only in stopped, whole-snapshot-qualified restore staging. Missing tables in older databases migrate; present malformed/empty metadata fails rather than resetting. Preserve namespace/source and outbox/incoming-encryption bindings. Native HTTP/notification IDs are opaque `n1:<reference-generation>:<id>` references. Shared adapter resolution rejects stale/foreign/bare IDs before reads/actions; internal cache/journal/receipt/encryption IDs remain numeric. The generation is not authorization or hold release.
- IDs never reuse within that history. Moves preserve IDs and emit removal/arrival together. Folder-scoped lookups reject stale references. Permanent deletion clears live raw/metadata while retaining receipts, IDs and tombstones; earlier WAL/pages/backups may still contain plaintext.
- List uses descending-ID keyset pages. Changes retains all events and returns a bounded page plus snapshot high-water mark; advance through every event before high-water. Future cursors require full resync. Native HTTP currently forces fresh full snapshots (delta:false, cursor:0). Durable scoped HTTP deltas remain an activation gate.
- Persisted limits must match across writers. Payload quota counts live raw bytes; record quota includes tombstones. SQLite triggers maintain live-byte/record counters atomically, with serialized backfill at open. Physical storage, change-history/receipt growth, quota adjustment, checkpoint/backup and restore suitability remain operating gates. No automatic eviction.
- `Update` replaces complete flags/labels atomically. Client read/label edits update only their fields in a writer transaction, preserving concurrent edits. Labels are case-insensitive, capped at 100 per message and 1,000 catalog entries at the shared write boundary.
- Client folder deletion moves messages to the parent atomically; the lower-level store deletion refuses live messages. INBOX matching is case-insensitive. Drafts retain Draft without Seen.
- Reuse `imap.ParseRawContent` and `OverviewFromHeaders` for MIME display, attachment ordering, PGP detection and deterministic single-sender binding. Exact raw PGP bytes remain opaque. Incoming-encryption replacement/recovery reuses the existing poller journal and key reservation.
- `incoming_encryption.go` binds sources to immutable owner plus a durable random database namespace. Recreated same-owner databases reject old jobs. Replacement creates a new numeric ID, copies current flags/date, tombstones original live raw/header metadata and writes exact-ciphertext recovery receipt/change events in one transaction. Quota uses net live-byte growth plus one retained record; rollback preserves the original. Gateway receipts retain the original ID, so delivery retries never resurrect it.
- Encrypted action verification and mutations share a writer transaction. Exact bytes plus replacement receipt authorize replay; marker headers alone never do. Moves preserve IDs and retry only in the exact expected destination; a verified stop remains valid after a terminal move. Changed/deleted copies or wrong folders pause recovery for reconciliation.
- Bound raw reads before materializing BLOBs, body batches to 1,000 messages/192 MiB and poll pages to 200 messages. Malformed MIME sets ParseError per message: the poller records a failed decision and continues; lazy body reads answer 422 while original bytes remain retained.
- `export.go`: `ExportFolders`/`ExportCount`/`Export` page live messages in ID order (200 per page, admission per page, one raw message in memory, moved/deleted ones skipped) with the receipt envelope sender; `WriteMboxrd` and `WriteEML`/`ZipFolder` write exact stored bytes, never decrypting; `ZipFolder` also prefixes Windows device names. Format rules: [mail export](../../../docs/NATIVE_PROVISIONING.md#mail-export).
- `import.go`: `ReadImport` detects zip/mbox/EML by content and passes one message at a time (mbox: split at `From ` at file start or after a blank line, mboxrd unquoting, line endings kept; zip: `*.eml` entries only, names never paths, 10,000-entry, expanded-bytes and 100x-ratio guards). `ImportFolder` creates a missing target under an existing parent; `ImportMessage` refuses empty, header-less and over-limit bytes (`ErrUnimportable`), then `append(imported)` stores seen with no receipt and returns `ErrDuplicate` when the folder holds live identical bytes (`message_folder_digests` index). Seen keeps imported mail away from the poller and incoming encryption. Rules: [mail import](../../../docs/NATIVE_PROVISIONING.md#mail-import).
- Search uses bounded metadata/raw scans (five seconds/192 MiB), returning an explicit error on exhaustion. No decrypted PGP body index exists; representative large-folder search performance remains a release gate.

## Work Guidance

- `Client.MailSourceIdentity` binds immutable owner plus database namespace. API/daemon require the account state and cache to match before using numeric references. Only newly provisioned account state can be native; switching is refused.
- Existing linked IMAP accounts remain external. Opt-in native accounts use admitted existing-only stores and full HTTP snapshots; scoped deltas and held-restore release remain pending.
- Treat database paths as local process trust, not network authorization. Store correspondence in the database, never application logs.

## Verification

- `TestNativeReferenceGenerationRollbackAndFailure` reproduces actual SQLite snapshot ID reuse and proves generation/source/mail/receipt preservation, source refusal and failed-write rollback. `TestNativeReferenceGenerationOlderSchemaAndCorruption` checks historical migration and malformed/empty/view refusal. `TestPrepareAccountReferenceGenerationReadOnly` proves actual preparation validation leaves older schemas untouched, runtime migration succeeds and corrupt present metadata is refused without repair.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox ./internal/backup ./internal/cryptutil -run 'TestNativeOutbox|TestOpenRefusesMalformedNonce' -count=1 -timeout=5m` checks encrypted intent, real killed submitters, competing claims, actual loopback TLS/lost ACK, Sent recovery, quota and sealed snapshots/corruption.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox -run '^TestPrepareAccount' -count=1` checks publication, retained mail on retry, competing ordinary state creation, concurrent preparation, missing/corrupt files and subprocess SIGKILL before/after actual publication. Process crashes are not power-loss evidence.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox ./internal/ingress -count=1` checks durable IDs/metadata/receipts, conflicts, concurrent writers, quotas, complete pages beyond the cache window and interrupted multi-owner import.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/api -run '^TestNativeMailboxClientAPI$' -count=1` exercises the real Client through authenticated session/device routes, owner isolation, offboarding, actions, attachments, signed/ciphertext reads and encrypted draft storage.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox -run '^TestNativeIncoming' -count=1` covers replacement receipts, namespace/byte fences, quota/rollback, contested writers, exact original MIME and real killed writers after replacement/move commits. These are process-crash checks, not hardware power-loss evidence.
- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/processor -run '^TestNativeIncomingPollerJournalRecovery$' -count=1` checks the real Client with lost acknowledgements, journal/key/cache guards, classification and action recovery.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox -run 'TestMboxrd|TestZipFolder|TestNativeExport' -count=1` checks mboxrd quoting round-trip, hostile zip folder names, paging past one page and exact EML bytes; `go test -race ./internal/api -run '^TestMailExport$' -count=1` with `TestMailExportIdentities` and `TestExportWriteWindow` checks step-up, grant binding/single use/expiry, refusal redirects, HEAD and HTTP/1.0 refusals, mailbox selection, device/administrator/legacyMixedUse identities, concurrency limits, bounded streaming writes, abort on failure and, over a real HTTP/1.1 server, a mailbox disabled mid-export ending in an unterminated chunked body.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox -run 'TestReadImport|TestNativeImport' -count=1` checks mbox quoting/CRLF/LF/separators and round-trip with export, EML, zip names/bombs/entry and expansion caps, per-message caps, dedupe, seen storage that the poller never lists, and capacity; `go test -race ./internal/api -run '^TestMailImport' -count=1` checks step-up, CSRF, link binding, foreign/IMAP/administrator/device refusals, concurrency, upload caps, temp-file cleanup including startup, cancel, and incoming encryption refused up front and stopping a running job.

## Child DOX Index

None.
