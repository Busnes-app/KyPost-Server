# worker

## Purpose

Runs each background role (`daemon`, `api-sweepers`, `mail-runtime`) on one process at a time, so PostgreSQL replicas on one host never run the same loop twice at once.

## Ownership

`worker.go`: the role names, `Run` (elect, wait the grace, run the jobs, wait for them on loss) and `Job`.

## Local Contracts

- The locker is `state.Store.WithLock` on the global state store: a PostgreSQL session advisory lock named `role:<role>` on a dedicated, discarded connection, or a flock beside `state.db` on SQLite.
- Losing the lock cancels every job's context; `Run` waits for every job to return before it competes again, so one process never runs a job beside a later owner's.
- On PostgreSQL a new owner waits `Grace` (callers pass `state.ProbeBound()+1s`) before its first job, so a predecessor whose session the database dropped has already seen its lock lost. SQLite passes 0: a flock cannot be lost while the process lives.
- Single ownership is not exactly-once. Each job keeps its own fence: ingress leases, outbox claim tokens, scheduled-send claims with claimant liveness, idempotent deletes.

## Verification

- `go test -race ./internal/worker/` checks one runner per role across three servers (both drivers), that a new owner starts only after its predecessor's job stopped (`TestNewOwnerStartsAfterThePredecessorStops`; a grace of 0 fails it), and, with `KYPOST_TEST_DATABASE_URL`, takeover after the owner shuts down.

## Child DOX Index
