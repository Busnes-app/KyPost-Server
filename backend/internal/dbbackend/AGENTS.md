# dbbackend

## Purpose

Chooses where every store keeps its databases: SQLite files (default) or one Postgres database named by `KYPOST_DATABASE_URL` / `KYPOST_DATABASE_URL_FILE`.

## Ownership

`dbbackend.go` (selection, startup checks, schema names, catalog, opens, staging for mailbox preparation), `tombstone.go` (table-mode directory deletion markers and atomic schema removal), `copy.go` (the SQLite↔Postgres table copy, export, import, fingerprint, row counts), `testing.go` (the test hook), and `pgdialect/` (KyIdentity's translator and `kypg` driver), and `pgtest/` (test-only: a freezable TCP relay for outage tests).

## Local Contracts

- `volumes.db` maps to `kypost_volumes`. It holds documents and wrapped data keys independently of multi-only `tenancy.db`; export/import and sequence repair follow the existing registered-kind copy path.

- The URL is a secret: errors and logs name only host and database (`Describe`, `scrub`); a URL that does not parse is `pgdialect.ErrBadURL`. A configuration error still reports `Postgres()` true, so no open falls back to SQLite. `Check` refuses both variables, an unreadable or empty file, Postgres < 16 and a non-UTF8/non-C database before creating anything.
- One schema per database file: `SchemaName` maps a path under `STATE_DIR` / `CONFIG_DIR` to `state/<rel>` / `config/<rel>` and a schema (readable for the install-wide files, `kypost_f_<hash>` otherwise, at most 41 bytes, only `[a-z0-9_]`, always quoted). `kypost_catalog.stores(path, schema, kind, ddl)` records each. A test binary maps other paths to `kt_<hash>` schemas.
- Except for the explicit `RowWrites` volume-accounts pool, each schema's pool takes its own writer lock (`pgdialect.LockKeys` of its search_path) for every write transaction: one writer per database, as SQLite.
- `Held(path)` decides every open of an existing database and returns any catalog error, so callers fail closed: on Postgres a path under STATE_DIR/CONFIG_DIR (outside `backup-scratch`) is always Postgres; `Open` with `Create` is for openers that create. `Stage`/`Settle`/`Unstage` publish a prepared directory's schemas; pending rows are never opened.
- The copy matches tables and columns by name: a SQLite side with an unknown table or column is a newer release and refused; one lacking some is older and upgraded on a private copy (`Kind.CreateSQLite`). Store packages register their `Kind`; `KindsForTest()` lists them for `TestEveryStoreIsClassified` (`internal/tenancy`), which fails on a `Register` call whose package it does not import.
- Volume accounts use a separate row-writer pool with explicit SERIALIZABLE transactions and changed-row locks. Explicit isolation is preserved, and an unspecified row transaction uses the server/session default; only schema-writer transactions force READ COMMITTED when unspecified. Its driver identity is distinct, so `vol.NewTable` refuses it; directory admission continues to require the normal schema-writer pool.
- Table-mode purge atomically drops schemas and records an existing-catalog `kind=purged` directory marker. Guard all ancestors in `Open`, `CreateSchemaTx`, `Stage` and `Settle`; use indexed path candidates, not a catalog scan per open. Markers also cover unpublished child stores and survive ordinary cleanup. `Stores`/`RowCounts` omit them because they have no namespace and are not backup members. Single-mode catalog DDL remains unchanged.
- Purge takes the existing per-schema writer keys before dropping namespaces. Schema-writer connectors receive the required schema separately from the unchanged search_path/key; after the writer lock, compare the effective `current_schema()` rather than an MVCC `pg_namespace` row and refuse a missing namespace (`pgdialect.ErrSchemaGone`) before SQL can fall through to public. Keep explicit row-writer isolation and its distinct driver unchanged.
- `bool` arguments bind as SQLite stores them (1/0 into BIGINT, a boolean where one is expected).
- `ImportSQLite` verifies every table inside its transaction before the caller commits: the row count and an order-independent content digest (`verifyTable`: each row normalized by the Postgres column type, int8 as decimal, text as its bytes, bytea byte-exact, NULL distinct; row hashes sorted and hashed), so migrate-to-postgres and restore refuse a lost, changed or extra value (`ErrDigestMismatch`). `TestCopyVerifiesEveryTableByContent` drops a table's rows and flips one BLOB byte through the test-only copy hook, and checks that every identity ends above the copied maximum.

## Work Guidance

- Add a translator rule only when it is true token by token; otherwise give the statement a Postgres text at its call site.

## Verification

- `go test -race ./internal/dbbackend/...` checks the configuration refusals and that no error carries the URL or password, schema names, the translator; with `KYPOST_TEST_DATABASE_URL` also the per-schema single writer (`TestSingleWriterPerSchema`), the catalog, bool binding and the C-locale refusal before any write. `KYPOST_TEST_OLD_DATABASE_URL` (a Postgres 15) runs `TestOldServerRefused`.

- `TestDirectoryTombstone*` covers indexed ancestor/sibling boundaries, fresh/unpublished/staged opens, wrong cleanup, deterministic queued writers at default/REPEATABLE READ/SERIALIZABLE without isolation downgrade, cancellation and injected marker-insert rollback/retry. API `TestTableMailboxImportPausedPastPurgeFails` exercises a retained mailbox handle; `TestTablePurgeFinalCheckRequiresDirectoryTombstones` removes one marker and proves final refusal. Sealed table backup/restore proves markers remain control metadata rather than capsule stores.

## Child DOX Index
