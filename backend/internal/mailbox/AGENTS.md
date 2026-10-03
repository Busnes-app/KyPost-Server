# Permanent Mailbox

## Purpose

Per-owner permanent raw mail, indexed metadata, folders, flags, labels, delivery receipts and durable change history.

## Ownership

All files in this package. Internal storage and complete `imap.Client` implementation; runtime source selection remains disabled. `NewClient` borrows the store; its caller owns the store lifetime and verifies the supplied sender address.

## Local Contracts

- Internal `PrepareAccount` publishes an empty mailbox, prebound account state and `native-mailbox.json` together at `$STATE_DIR/users/<localID>/` (`mailbox/mailbox.db` plus `state.db`). Caller proves verified new-account issuer/subject ownership, domain authority and unique primary address. This is storage preparation, never receiver readiness or a migration. No production scheduler/selector calls it.
- Preparation serializes cooperating writers and uses Linux `RENAME_NOREPLACE` to refuse even an empty or concurrently created destination. Other platforms refuse explicitly. Retry reads existing regular databases without creating schemas, checks required tables/owner/source/namespace/address/limits, and refuses incomplete, legacy or recreated state. Preserve published data; crash-abandoned staging directories contain only empty preparation and need bounded cleanup before activation.

- Open only an owner-only directory selected from verified provisioning. The durable issuer/subject/mailbox tuple is immutable; opening the database for another owner fails. Validate user path components with fsutil before constructing paths.
- SQLite immediate writer transactions, FULL synchronization and raw BLOBs keep bytes, metadata, receipt and change atomic. Reuse installed dependencies.
- Import identity is gateway/delivery within this owner, with a canonical frozen envelope and SHA-256 digest. Exact replay returns the original numeric ID even after move/deletion; conflicts retain the holding copy. Aliases of the same owner produce one receipt/copy.
- IDs never reuse. Moves preserve IDs and emit removal/arrival together. Folder-scoped lookups reject stale references. Permanent deletion clears live raw/metadata while retaining receipts, IDs and tombstones; earlier WAL/pages/backups may still contain plaintext.
- List uses descending-ID keyset pages. Changes retains all events and returns a bounded page plus snapshot high-water mark; advance through every event before high-water. Future cursors require full resync. Native HTTP currently forces fresh full snapshots (delta:false, cursor:0). Durable scoped HTTP deltas remain an activation gate.
- Persisted limits must match across writers. Payload quota counts live raw bytes; record quota includes tombstones. SQLite triggers maintain live-byte/record counters atomically, with serialized backfill at open. Physical storage, change-history/receipt growth, quota adjustment, checkpoint/backup and restore suitability remain operating gates. No automatic eviction.
- `Update` replaces complete flags/labels atomically. Client read/label edits update only their fields in a writer transaction, preserving concurrent edits. Labels are case-insensitive, capped at 100 per message and 1,000 catalog entries at the shared write boundary.
- Client folder deletion moves messages to the parent atomically; the lower-level store deletion refuses live messages. INBOX matching is case-insensitive. Drafts retain Draft without Seen.
- Reuse `imap.ParseRawContent` and `OverviewFromHeaders` for MIME display, attachment ordering, PGP detection and deterministic single-sender binding. Exact raw PGP bytes remain opaque. Incoming-encryption replacement/recovery reuses the existing poller journal and key reservation.
- `incoming_encryption.go` binds sources to immutable owner plus a durable random database namespace. Recreated same-owner databases reject old jobs. Replacement creates a new numeric ID, copies current flags/date, tombstones original live raw/header metadata and writes exact-ciphertext recovery receipt/change events in one transaction. Quota uses net live-byte growth plus one retained record; rollback preserves the original. Gateway receipts retain the original ID, so delivery retries never resurrect it.
- Encrypted action verification and mutations share a writer transaction. Exact bytes plus replacement receipt authorize replay; marker headers alone never do. Moves preserve IDs and retry only in the exact expected destination; a verified stop remains valid after a terminal move. Changed/deleted copies or wrong folders pause recovery for reconciliation.
- Bound raw reads before materializing BLOBs, body batches to 1,000 messages/192 MiB and poll pages to 200 messages. Malformed MIME sets ParseError per message: the poller records a failed decision and continues; lazy body reads answer 422 while original bytes remain retained.
- Search uses bounded metadata/raw scans (five seconds/192 MiB), returning an explicit error on exhaustion. No decrypted PGP body index exists; representative large-folder search performance remains a release gate.

## Work Guidance

- `Client.MailSourceIdentity` binds immutable owner plus database namespace. API/daemon require the account state and cache to match before using numeric references. Only newly provisioned account state can be native; switching is refused.
- Keep external IMAP selected until verified provisioning, both runtime selectors, durable scoped deltas and restore reconciliation are qualified.
- Treat database paths as local process trust, not network authorization. Store correspondence in the database, never application logs.

## Verification

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox -run '^TestPrepareAccount' -count=1` checks publication, retained mail on retry, competing ordinary state creation, concurrent preparation, missing/corrupt files and subprocess SIGKILL before/after actual publication. Process crashes are not power-loss evidence.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox ./internal/ingress -count=1` checks durable IDs/metadata/receipts, conflicts, concurrent writers, quotas, complete pages beyond the cache window and interrupted multi-owner import.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/api -run '^TestNativeMailboxClientAPI$' -count=1` exercises the real Client through authenticated session/device routes, owner isolation, offboarding, actions, attachments, signed/ciphertext reads and encrypted draft storage.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox -run '^TestNativeIncoming' -count=1` covers replacement receipts, namespace/byte fences, quota/rollback, contested writers, exact original MIME and real killed writers after replacement/move commits. These are process-crash checks, not hardware power-loss evidence.
- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/processor -run '^TestNativeIncomingPollerJournalRecovery$' -count=1` checks the real Client with lost acknowledgements, journal/key/cache guards, classification and action recovery.

## Child DOX Index

None.
