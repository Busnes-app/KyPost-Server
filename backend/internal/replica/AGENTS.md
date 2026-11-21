# replica

## Purpose

The PostgreSQL replica startup checks: one volume set, one host, no single-consumer receiver, and one shared configuration. Replicas share local volumes on one Docker host, because host-local `flock`s fence the JSON stores, the authority and domain fences and DKIM keys.

## Ownership

`topology.go` (`Start`: receiver refusal, volume IDs, boot ID, instance registration) and `config.go` (`Shared`/`PerHost` classification, `Fingerprint`, `CheckConfig`, `Accept`).

## Local Contracts

- Only PostgreSQL starts call this package; SQLite records nothing.
- `Start` runs under the `replica-topology` lock before any listener or worker. The first start writes one random ID to `replica-volume.id` in CONFIG_DIR, SECRET_DIR and STATE_DIR (exclusive create) and records it as meta `replica_volume_id`; later starts refuse a root whose file is missing or different (`ErrVolumes`). A live instance with another `/proc/sys/kernel/random/boot_id` refuses the start (`ErrHost`). `KYPOST_NATIVE_RECEIVER` refuses (`ErrReceiver`, O2).
- Logs are per container in the replica profile, so LOG_DIR carries no volume ID.
- The fingerprint is HMAC-SHA256 per shared setting under the HKDF subkey `kypost:replica-config` of `totp-secret.key`, stored as meta `replica_config_fingerprint`. Refusals name settings, never values; a different key is reported alone. A setting the record lacks is merged in (upgrade); one the build no longer knows is ignored.
- Every environment variable the docs, `.env.example` or the backend's own reads name is in `Shared` or `PerHost`; `TestEveryEnvironmentVariableIsClassified` enforces it. Network lists compare parsed and masked.
- `KYPOST_TENANCY` and `KYPOST_STAFF_ORG_ID` are `PerHost` on purpose: adding them to `Shared` would merge two entries into every upgraded install's record. A multi-tenant install's own `meta` record (`internal/tenancy`) refuses a replica with another mode or staff org, and takes the same `replica-config` lock.

## Verification

- `go test -race ./internal/replica/` checks the classification on any driver; with `KYPOST_TEST_DATABASE_URL` also every refusal and allowed start of `TestTopologyGuard`, `TestReplicaConfigRecordsAndRefuses` and `TestFirstStartsRaceThroughTheRecord`.

## Child DOX Index
