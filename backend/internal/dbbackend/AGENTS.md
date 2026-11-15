# dbbackend

## Purpose

Chooses where every store keeps its databases: SQLite files (default) or one Postgres database named by `KYPOST_DATABASE_URL` / `KYPOST_DATABASE_URL_FILE`.

## Ownership

`dbbackend.go` (selection, startup checks, schema names, catalog, opens, staging for mailbox preparation), `copy.go` (the SQLite↔Postgres table copy, export, import, fingerprint, row counts), `testing.go` (the test hook), and `pgdialect/` (KyIdentity's translator and `kypg` driver).

## Local Contracts

- The URL is a secret: errors and logs name only host and database (`Describe`, `scrub`); a URL that does not parse is `pgdialect.ErrBadURL`. A configuration error still reports `Postgres()` true, so no open falls back to SQLite. `Check` refuses both variables, an unreadable or empty file, Postgres < 16 and a non-UTF8/non-C database before creating anything.
- One schema per database file: `SchemaName` maps a path under `STATE_DIR` / `CONFIG_DIR` to `state/<rel>` / `config/<rel>` and a schema (readable for the install-wide files, `kypost_f_<hash>` otherwise, at most 41 bytes, only `[a-z0-9_]`, always quoted). `kypost_catalog.stores(path, schema, kind, ddl)` records each. A test binary maps other paths to `kt_<hash>` schemas.
- Each schema's pool takes its own writer lock (`pgdialect.LockKeys` of its search_path) for every write transaction: one writer per database, as SQLite.
- `Held(path)` decides every open of an existing database and returns any catalog error, so callers fail closed: on Postgres a path under STATE_DIR/CONFIG_DIR (outside `backup-scratch`) is always Postgres; `Open` with `Create` is for openers that create. `Stage`/`Settle`/`Unstage` publish a prepared directory's schemas; pending rows are never opened.
- The copy matches tables and columns by name: a SQLite side with an unknown table or column is a newer release and refused; one lacking some is older and upgraded on a private copy (`Kind.CreateSQLite`). Store packages register their `Kind`.
- `bool` arguments bind as SQLite stores them (1/0 into BIGINT, a boolean where one is expected).

## Work Guidance

- Add a translator rule only when it is true token by token; otherwise give the statement a Postgres text at its call site.

## Verification

- `go test -race ./internal/dbbackend/...` checks the configuration refusals and that no error carries the URL or password, schema names, the translator; with `KYPOST_TEST_DATABASE_URL` also the per-schema single writer (`TestSingleWriterPerSchema`), the catalog, bool binding and the C-locale refusal before any write. `KYPOST_TEST_OLD_DATABASE_URL` (a Postgres 15) runs `TestOldServerRefused`.

## Child DOX Index
