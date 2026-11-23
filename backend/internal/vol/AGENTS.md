# Volume documents

## Purpose

File and PostgreSQL document I/O for the multi-host design. Store packages retain their serialization and validation.

## Ownership

`Vol`, file/table implementations, bounded CAS retries, startup mounts, directory accounting, platform-marker moves and authenticated SQLite snapshot reads. Database creation belongs to `../volumes/`; identity resolution and startup selection belong to `../app/documents.go`.

## Local Contracts

- Ordinary installs resolve to files. Preserve each caller's directory modes, atomic file bytes and inter-process locks. Table mode is multi, or the explicit `volume_stores=postgres` state record; only startup installs mounts.
- A `Document` keeps its observed revision; its store mutex or attempt-local owner serializes its use. A mutation retries its complete read/check/change/write through `Update` or `Retry`, never just its final write. Callbacks may run five times and must not perform external side effects. `ErrBusy`/`ErrStale` are HTTP 503 at the API boundary.
- A missing table row clears cached content. Authentication, decode and storage failures propagate; they never mean an empty document.
- Every derived body is sealed with its org key and root/path/user/org associated data. Resolve mailbox and pickup owners from retained ledger/record data. Platform rows are plaintext. Keys are wrapped under provisioned `documents.key`; the seam never creates that master file.
- Table writes accept at most 64 MiB of plaintext (owner, 2026-11-23); file writes retain existing limits. MH8 must refuse oversized source files before publishing a migration. Ciphertext overhead is additional.
- Directory admission and the new row share one writer transaction. Marker moves compare the checked source revision in that transaction; only platform bodies may move without resealing. File callers sync the directory after a rename.
- Runtime tables require PostgreSQL. `NewSnapshotTable` is exclusively for private, stopped SQLite capsule staging. MH3 adds the account-row guard before any derived write or key creation; startup remains gated until it exists.

## Work Guidance

Use the existing seam for a document adapter. Keep wire parsing and product policy in its owning store.

## Verification

`go test -race -p 2 -count=1 ./internal/vol` exercises files and, with `KYPOST_TEST_DATABASE_URL`, CAS/ABA, copied-ciphertext refusal, concurrent key creation, photo accounting/admission, marker moves and the table cap. API `TestCrossOrgMatrixTableDocuments` and `TestDocumentContentionAnswers503WithoutCreatingContact` exercise the boundary through handlers.

## Child DOX Index

None.
