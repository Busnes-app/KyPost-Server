# Accounts

## Purpose

Local account records, credentials, roles and authority epochs for file and table mode.

## Ownership

Account validation, mutations, indexes, deep-copy snapshots, table transactions and account mount selection. `../volumes/` owns schema registration; `../app/documents.go` installs the table mount before stores open. API owns verified identity/org admission and provisioning/purge fences.

## Local Contracts

- Ordinary constructors retain `users.json`, its bytes, bootstrap, stat cache and gated file locks. Table mode uses the startup-mounted `volumes.db` accounts pool and never bootstraps a local administrator or writes account files.
- Table records keep `User` JSON. SQL `id`, normalized username and nullable issuer/subject must agree with the record. Unbound identities use two NULLs; partial issuer/subject bindings are refused. MH8 must bind historical links before migration. Identity lookups use the full issuer/subject pair; a subject-only lookup refuses ambiguity.
- Each table lookup reads fresh data; `WithCurrentUsers` reads a private read-only REPEATABLE READ snapshot. Its callback can outlive another replica's write and must treat the snapshot as a past observation, not a commit fence.
- Account mutations run SERIALIZABLE, lock changed rows and update only changed records. Retry a whole attempt at most five times on 40001; exhaustion uses the existing busy admission error class. Keep retry counters attempt-local and preserve org/staff rules and native send epochs at the shared writer. New customer-org accounts hold `FOR SHARE` on the existing tenancy cache row through commit and refuse a deleted org or an existing whole-org/member purge record. Starting a purge touches the cache row, closing publication after fence loss even when a member's org remains active. The configured staff org remains exempt from customer cache requirements and is never purged. Provisioning retains its request context through retries/commit.
- Recovery duplicate validation retains global subjects in file mode and uses `(issuer, subject)` in table mode. Targeted table repairs match the SSO issuer and native owner/source; a true duplicate pair refuses before durable intent.
- Account pools use row writes; document pools keep the schema writer lock. Never pass an account pool to a document table. Commit fences returned by recovery callbacks remain held through the account commit. Restore deactivation commits before its external finalizer; a finalizer failure must not roll it back or replay it.
- The document writer locks the retained account FOR KEY SHARE before any derived body/key creation. Purge deletes accounts before documents. Offline capsule readers validate account routing columns and use exactly one file/table authority.

- Device slots have one writer, `SetPGPDeviceEnvelope`: v4 for legacy single-key material, v5 for converted complete rings, or v6 X-Wing for either. Validate v6 framing and the canonical enrollment key before mutation; preserve revision, fingerprint and generation binding. The wire contract is [device enrollment](../../../docs/DEVICE_ENROLLMENT.md).

- Existing keyring metadata versions 1 and 2 accept whole-ring password/recovery rewraps with revision guards and preserve the version, inventories and generation. Legacy single-key writers refuse either ring. P4 CommitPGPKeyring adds the internal v6 30/35 certificate and writes metadata v2 while retaining v4 exactly; it requires both sealed envelopes, preserves history, clears device slots and refuses overwriting v6. Readiness is computed from current device reports outside the account record. Each opaque envelope is capped at 512 KiB.

## Work Guidance

Keep product identity policy in the existing store methods. Use the registered volume schema and existing database copy path; no second account database or serialized account-list document.

## Verification

`go test -race -p 2 -count=1 ./internal/users` runs file behavior plus PostgreSQL account tests when `KYPOST_TEST_DATABASE_URL` is set. Table cases cover identity pairs/NULLs, uniqueness/admin races, fresh snapshots, corrupt columns, bounded serialization retry, server-observed isolation, two admin writes held concurrently at their row updates, and durable restore-finalizer behavior. A controlled Go overlay that forces READ COMMITTED must fail the isolation/admin-overlap tests; never count a start-channel race alone as proof of overlapping writes. API `TestTablePurge*`, vol `TestPurgedOwner*`, and backup `TestTableDocumentsBackupRestore` cover cross-store lifecycle.

## Child DOX Index

None.
