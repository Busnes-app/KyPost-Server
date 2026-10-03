# Native provisioning reconciliation

Internal foundation following merged PR #249. Production directory webhooks
retain verified desired state and enforce account access as before. They do not
call this reconciler. No worker, new endpoint, domain setting or native selector
is enabled by this change.

`LifecycleStore.ReconcileNativeMailbox` reads the retained signed SCIM resource
under the same cross-process lock used by directory updates. The trusted caller
must prove domain authority and the NEW local account's issuer/subject binding
before publishing that account to ordinary state consumers. Token email/name
and generic SCIM emails provide neither proof nor alias authorization.

The operator's domain is a lowercase ASCII DNS name. Active desired state must
contain exactly one explicit primary bare ASCII dot-atom address in that domain.
Address comparisons are case-insensitive; quoted and SMTPUTF8 addresses are
refused until supported end to end. Extra non-primary emails never create aliases.

`$CONFIG_DIR/native-provisioning.json` is an owner-only atomic JSON ledger:
immutable issuer/subject/localID, primary address, absolute state root and limits;
desired revision/digest/activity; pending/applied/failed status, bounded failure
code and acknowledged mailbox source. Failed primary validation can retain an
account reservation with no address. Successful address reservations are never
freed implicitly, including after failure or deactivation. Renaming or transferring
an assigned address requires a future explicit reconciliation contract.

Reserve pending before touching files. Preparation publishes the mailbox,
prebound state and manifest atomically; applied records its source afterward.
Killed writers retain a pending reservation; retries reuse the exact published
namespace after lost acknowledgement. An acknowledged source uses read-only
`ValidatePreparedAccount`, refusing missing, legacy or damaged files instead of
creating another namespace. Preparation failures persist failed status and keep
reservations. Conflicting desired-primary updates retain current-revision failed
status and the original address/source. Persistence failures return explicitly;
a stored pending record is repaired by retry, never treated as completion.

An initialization fence in `sso-lifecycle.json` is durable before the first ledger
write. Missing ledger after initialization, or a ledger with an unfenced restored
lifecycle, refuses reconciliation/status reads. A crash between first fence and
ledger publication requires explicit ledger recovery, sacrificing availability
rather than releasing unknown reservations. Valid but older paired files cannot
be detected here: whole-stack restore reconciliation remains mandatory.

Offboarding retains storage/reservations. Existing signed directory handling
revokes access; reconciliation itself grants or revokes no user credentials.
`DesiredActive` and `Status` are historical preparation evidence, never live
access or receiver readiness. Consumers must compare live directory revision,
digest and activity under its lock before using them. A subject disabled before
its first assignment does not create storage. Deactivation of a pending,
unprepared assignment may complete without a source; reactivation prepares it.

The instance-wide lifecycle lock covers local disk preparation and serializes
revocation entirely before or after that work. It currently has unbounded flock
and filesystem waits; bounded/cancellable acquisition and bounded disk work are
activation gates. Do not promise prompt deactivation with a stalled worker.
Whole-file ledger rewrites and address scans scale with assigned account count;
measure before activation, move to SQLite if this becomes a bottleneck.

Verification (Linux, real SQLite/filesystem and subprocess SIGKILL):

```sh
cd backend
GOTOOLCHAIN=go1.26.6 go test -race ./internal/sso ./internal/mailbox -count=1 -timeout=20m
GOTOOLCHAIN=go1.26.6 go test -race ./internal/api -run '^TestDirectory' -count=1 -timeout=20m
```

Checks cover primary/domain ambiguity, unverified/legacy desired state, owner,
root and quota conflicts, concurrent collisions, lifecycle ordering, retained
raw mail and namespaces through disable/rehire, current failed-state repair,
legacy/missing storage, missing/corrupt ledger, partial lifecycle restore and
actual kills before preparation and after publication but before acknowledgement.
The signed webhook/API test resolves the existing account and explicitly calls
reconciliation as qualification; no production webhook calls it.

Local merge qualification: full backend race suites pass (API 277.369s),
including the pinned embedding model and Maddy proof in the other-package suite.
Formatting/vet/pinned lint/build/vulnerability checks pass. Frontend typecheck,
918 tests/build/runtime audit, both worker typechecks/76 relay tests, script/
workflow/compose checks and actual Docker build/health/heartbeat/cleartext refusal
checks pass. Independent hostile review repeated provisioning/preparation and
signed-directory race checks; no blocker remains for disabled internal scope.
Its surviving medium findings are unbounded lock/disk waits and restore
consistency; whole-file scaling is a low finding. Remote CI and the autonomous
reviewer must independently clear the pushed PR head before merge.

Before activation: persist/prove domain ownership; integrate new-account
allocation before any ordinary state opener (never adopt legacy accounts);
provide operator diagnostics and periodic repair; bound lock/storage work;
qualify backup/restore across lifecycle, ledger and prepared mailbox/state;
coordinate receiver routes/import commits with revocation. Source selection,
client deltas, aliases and transport readiness remain separate work.

Rollback keeps ledger, lifecycle fence and all prepared account data. Disable
workers before changing binaries. Older writers can discard the initialization
fence; use a compatible binary and reconcile retained lifecycle/ledger state
with a fresh verified directory revision before resuming. Never erase a ledger
or switch sources to make a failed preparation succeed. No new dependency,
network call, secret or client wire contract is introduced.
