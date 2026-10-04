# Backup

## Purpose

KyPost's adapter over ky-primitives/recoveryclient v0.5.1: configuration, account/key collection, SQLite snapshots, drills and local audit storage.

## Ownership

This package owns payload selection and verification. API/CLI callers own credential checks and durable intent/completion audit. The library owns sealing, pairing transport, key pinning, retention, schedules and restore.

## Local Contracts

- Nonempty native outboxes require relay config/master key and `outbox:encrypted-jobs-claims-and-sent` recipe evidence. Decrypt actual frozen job bytes; check namespace/owner/domain, foreign keys, Sent receipts and quota. Databases with neither queue table remain compatible; a partial schema or orphan claims is corruption. Historical generations/claims are preserved, never reclaimed or replayed by restore.

- Collect `config/native-relay.json` with `private/native-relay.key`; validate decryption of the actual collected bytes and historical native-domain issuer/domain binding. Version-1 recipes add `relay:domain-credentials-and-authority`; drills require it when relay ciphertext exists. Relay-only restores persist the native hold without qualifying unowned mailbox databases. Fresh authority/provider evidence remains separate.

- Service name is `KyPost`. The token sealer uses the existing TOTP master key with HKDF label `kypost:setting:kyrecovery_token`; load it at operation time, never generate a replacement.
- Settings and flat `backup_audit` rows use the install-wide state.db. Pair, pin and unpair settings commit transactionally.
- Nonblocking `backup-operation.lock` coordinates API, CLI and daemon operations across processes. Busy operations return ErrInProgress. Keep lock files on stable inodes.
- Collect config/private/state, snapshot each state.db, mailbox.db and ingress.db with the library's SQLiteSnapshot, and refuse missing dependent keys or unsupported files. IMAP mail and rebuildable cache are excluded; local native databases and encrypted pending pickup messages are included. Collector and drill share the authoritative filename predicate. The version-1 recipe carries rules, never user-specific paths; older drills attest only state.db.
- Snapshots are consistent per database, not across files/stores. Collected bytes/drills validate historical native ownership/source/ledger and receiver bindings. Current authority, stale-ID fencing, power-loss qualification and the 64 MiB/file, 256 MiB/total caps remain activation gates; see docs/RESTORE.md.
- Local destination is outside all data roots or exactly STATE_DIR/backups. The dedicated destination, runtime supervisor files, lock files, migrated files and backup scratch are excluded. Other nonregular files are refused.
- Individual secret overrides must resolve to their default SECRET_DIR locations. Configured VAPID keys and existing tuning overrides must be inside collected roots. A missing optional TUNING_FILE is allowed only inside CONFIG_DIR, SECRET_DIR or STATE_DIR, matching the default container fallback; external overrides are refused even when missing, and required keys remain mandatory. Operator environment and external TLS mounts are restored separately; see docs/RESTORE.md.
- Drills serialize with all backup operations, use an opened authenticated manifest, validate its recipe and required files, verify SQLite integrity and decrypt stored IMAP credentials. Client-wrapped PGP stays opaque.
- Restore is CLI-only, shares on stdin. No running service holds the suite recovery private key. Extract privately; preserve failed staging. Native publication requires an absent Linux target, validated ownership and a persisted state/native-restore-hold.json. Allocation enforces the hold; no release path exists. Future native workers must enforce it.

## Work Guidance

- Use library helpers for transport and capsule behavior; adapt product storage rather than copying library implementations.
- Record an intent before mutations and a completion afterward. If completion auditing fails, state that the action may have happened.

## Verification

- `TestNativeOutboxSealedClaimsSentAndDependencies` checks sealed committed claims/Sent, retained holds, required recipe/key/config and wrong-key rejection.

- `TestDomainRelay` checks sealed credential/key/generation preservation, missing dependencies, corrupt restored keys and relay-only quarantine.

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/backup ./internal/state ./internal/config`
- `go test ./internal/api -run TestBackup` covers admin/CSRF/credential gates and audit outages.
- `TestNothingInTheServerDecrypts` scans backend source with guardtest, allowing only app.runRestore.
- `TestNativeDatabasesSurviveSealedRestore` proves WAL-only bytes/receipts survive sealing and restore, and drills reject corrupt native databases.

## Child DOX Index

None.
