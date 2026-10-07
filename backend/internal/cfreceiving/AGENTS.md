# Cloudflare continuous receiving (KyPost side)

## Purpose

KyPost's half of the continuous Cloudflare wire contract: pickup credentials, the signed routing-table and rotation documents, the bounded Worker client and local pickup state. The app receiving runtime (`internal/app/receiving_cfcontinuous.go`) owns ownership decisions, scanning, import and fencing.

## Ownership

All files in this package. The contract is [continuous receiving](../../../docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md#wire-contract), implemented by `receiving-worker/continuous.mjs`; change both sides and the doc together.

## Local Contracts

- Signed documents are `{"<field>": "<payload json>", "signature": "<b64>"}`; the signature covers exactly `UTF-8(context) || UTF-8(payload)`. Owners never reach the Worker; routes carry address, generation and maxBytes only, addresses lowercase ASCII (A-label domains).
- `CredentialsFile` (SECRET_DIR, 0600) is sealed in backups. `HostFile` (SECRET_DIR, 0600) is the live marker and in-flight rotation and must stay excluded from backups (`backup.skip`): its absence is what makes a restore start fenced and never replay pending material. Pending equal to current means an interrupted promotion and counts as live. `Fence` drops pending.
- `Keys.Lock` serializes credential changes across processes: init, rotation, promotion and the daemon's fence decision after a 401. Daemon cycles read credentials without it.
- Bearers, seeds and pending material never appear in logs, errors, status or CLI output; `init` prints only the bearer's SHA-256 and the public key.
- `DBFile` sits beside `ingress.db` and is collected as a SQLite snapshot. Revisions strictly increase and are recorded before the table is sent. The ledger is an index of owed work; provider deletes are decided by the ingress store, never by a ledger row alone.
- Client: no proxy, no redirects, bounded bodies; error bodies never surface. 401 is `ErrUnauthorized`, 409 `ErrConflict`, a provider object that can never be held `ErrInvalid`.

## Work Guidance

## Verification

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/cfreceiving -count=1` checks contract bytes with Go's ed25519, route/key rules, file modes, the restored (host-record-less) copy starting fenced and the interrupted-promotion window.
- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/app -run TestCloudflareContinuous -count=1` drives the loop against a Go fake of the Worker.

## Child DOX Index

None.
