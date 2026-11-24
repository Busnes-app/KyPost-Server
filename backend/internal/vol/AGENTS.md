# Volume documents

## Purpose

File and PostgreSQL document I/O for the multi-host design. Store packages retain their serialization and validation.

## Ownership

`Vol`, file/table implementations, bounded CAS retries, startup mounts, directory accounting, platform-marker moves and authenticated SQLite snapshot reads. Database creation belongs to `../volumes/`; identity resolution and startup selection belong to `../app/documents.go`.

## Local Contracts

- An empty dependency set delegates to the existing `Vol.Write` path, preserving its CAS retries and error classes (including the unchanged API 503/413 regressions). Nonempty dependency validation remains table-only.
- `Document.WriteWithReferences` validates same-volume/same-owner paths and checks deduplicated reference rows in the existing writer transaction, after the account guard and before key creation/sealing. Missing references return ErrStale and commit no body/key. Ordinary file writes keep their prior behavior. `RemoveIfUnchanged` compares the target incarnation and its owner's guard-document revision in one conditional delete; mismatch retries the whole decision, never only the delete. Contacts own photo-reference parsing; this seam remains format-agnostic.

- `Lock` is the cross-store lock seam: the mount's `FenceFunc` for a path under a `MountFenced` root, else the original flock for its `LockMode`. A plain `Mount` (offline capsule staging, older tests) keeps flocks. The `Fence*` names live here so `config` can name the settings fence; `state` validates and orders them.
- Ordinary installs resolve to files. Preserve each caller's directory modes, atomic file bytes and inter-process locks. Table mode is multi, or the explicit `volume_stores=postgres` state record; only startup installs mounts.
- A `Document` keeps its observed revision; its store mutex or attempt-local owner serializes its use. A mutation retries its complete read/check/change/write through `Update` or `Retry`, never just its final write. Callbacks may run five times and must not perform external side effects. `ErrBusy`/`ErrStale` are HTTP 503 at the API boundary; `ErrTooLarge` is HTTP 413 without retry.
- A missing table row clears cached content. Authentication, decode and storage failures propagate; they never mean an empty document.
- Every derived body is sealed with its org key and root/path/user/org associated data. Resolve mailbox and pickup owners from retained ledger/record data. Platform rows are plaintext. Keys are wrapped under provisioned `documents.key`; the seam never creates that master file.
- Table writes accept at most 64 MiB of plaintext (owner, 2026-11-23); file writes retain existing limits. MH8 must refuse oversized source files before publishing a migration. Ciphertext overhead is additional.
- Directory admission and the new row share one writer transaction. `kypg.BeginTx` takes the schema advisory writer lock before any usage query; do not bypass that driver or add a second quota lock. Marker moves compare the checked source revision in that transaction; only platform bodies may move without resealing. File callers sync the directory after a rename.
- Runtime tables require PostgreSQL. `NewSnapshotTable` is exclusively for private, stopped SQLite capsule staging. Live derived writes lock their retained account `FOR KEY SHARE` in the document transaction before creating a body or key, and compare its org with the trusted resolver. A missing/mismatched account refuses the write. Purge deletes accounts first; startup remains gated on later MH phases.

## Work Guidance

Use the existing seam for a document adapter. Keep wire parsing and product policy in its owning store.

## Verification

- `TestReferencePublicationAndCleanupShareWriterTransaction` blocks publication in an actual UPDATE trigger after dependency validation and before commit, observes cleanup waiting on the same schema writer key, then requires stale cleanup refusal. A Go overlay disabling only DELETE auto-wrap must fail it. Publication uses `Table.write` BeginTx and cleanup uses the normal driver's auto-wrapped DELETE; keep both paths on the shared transaction writer key. NewTable refuses the separate accounts row-writer driver.

- `TestDocumentReferencesAndGuardedRemoval` covers shared references, missing/foreign-owner dependencies, stale reference snapshots, deletion/recreation and unchanged body on refusal. `TestMissingReferenceCreatesNoBodyOrOrgKey` proves failed dependency validation creates neither ciphertext nor a wrapped data key. Contacts/API tests exercise the actual photo sweep and publication paths; sealed table restore retains the reference and its bytes.

`go test -race -p 2 -count=1 ./internal/vol` exercises files and, with `KYPOST_TEST_DATABASE_URL`, CAS/ABA, copied-ciphertext refusal, concurrent key creation, photo accounting/admission, marker moves and the table cap. API `TestCrossOrgMatrixTableDocuments` and `TestDocumentContentionAnswers503WithoutCreatingContact` exercise the boundary through handlers.

## Child DOX Index

None.
