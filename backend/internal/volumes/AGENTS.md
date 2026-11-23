# Shared volume database

## Purpose

Catalogued `volumes.db` storage for table-mode documents and wrapped org data keys.

## Ownership

Schema registration, existing-only/create opens, the dedicated account pool and purge operations. `app/documents.go` selects table mode and supplies identity resolvers; `vol/` owns document I/O and encryption. The `accounts` schema is owned here; `../users/` owns its record logic and dedicated row-writer pool.

## Local Contracts

- A normal single-tenant start creates neither this store nor `documents.key`. Table mode is selected before document stores open; absent or unreadable required storage fails closed.
- PostgreSQL runtime storage and SQLite capsule DDL have matching columns. Document revisions use PostgreSQL's identity sequence; database import advances it above restored rows.
- Delete account rows before their documents, closing the document writer's owner-row guard. Purge by the snapshot's user IDs, across both roots and every path, including photos and pickup. A whole-org purge removes its wrapped key after documents; erasing one member preserves the shared key. Both document rows and org-key rows participate in the final purge check.
- Backups include this database whenever it exists. Snapshot validation authenticates every wrapped key and body and checks nested credential keys. Older capsules retain historical keys until their normal retention expires.

## Work Guidance

Keep identity and application imports out of this storage package; supply trusted resolvers at the application boundary.

## Verification

`go test -race -p 2 -count=1 ./internal/volumes` runs shared-store tests with `KYPOST_TEST_DATABASE_URL`. API purge/matrix and backup table-document tests cover lifecycle and sealed restore. Required PostgreSQL CI includes this package.

## Child DOX Index

None.
