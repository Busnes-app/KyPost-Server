# Ingress

## Purpose

Durable receiving buffer and immutable envelope ownership, separate from permanent mailboxes.

## Ownership

All files in this package. Opt-in app receiving commands and daemon import select it; no bundled public listener starts it. See [runtime contract](../../../docs/NATIVE_PROVISIONING.md#direct-receiving-runtime-qualification-profile).

## Local Contracts

- Callers are trusted local receiver/importer processes with exclusive access to an owner-only directory. Gateway IDs are supplied by authenticated transport configuration, never SMTP headers. This package is not a network authentication boundary.
- Route writers must verify KyIdentity provenance before calling `SetRoute`. Versions are monotonic per address; same-version refresh cannot change ownership or activation.
- `RefreshRoute` is accepted-mail recovery only: existing identical issuer/subject/mailbox/generation/active tuple, no insertion or reactivation. App import selects it only without fresh admission proof.
- Bind at RCPT; accept bounded raw MIME transactionally before SMTP success. Staged records and accepted/quarantined payloads never expire automatically: age cannot prove an SMTP transaction abandoned. Explicit fenced cleanup remains an operating gate.
- A claim revalidates the bound issuer/subject/mailbox/generation against current active routes. Conflicts quarantine without reassignment. Leases fence acknowledgments; the importer commits every intended local delivery before acknowledgment.
- `Import` resolves frozen issuer/subject/mailbox owners, checks the returned store identity, groups aliases, and commits permanent mailbox receipts before acknowledgment. The opt-in app importer preopens all stores and holds domain/directory/users fences through commits and acknowledgment. Partial failure retains payload and retries after claim expiry; recipient reassignment quarantines rather than resolving the new owner.
- `OpenExisting` validates persisted limits/schema before enabling WAL and never initializes missing storage. `QuarantinePending` retains bytes/bindings and refuses active competing claims; its caller must prove current ownership/generation conflict under authority fences.
- SQLite FULL synchronization and immediate writer transactions protect receipt/payload atomicity. Finite payload/record limits and physical admission estimates refuse growth; file lengths and filesystem free-space plus next-write allowance are checked inside the writer transaction. Near-budget checkpoints run outside it. Exact Bind/Accept replays and accepted-mail recovery remain available; physical/volume hard quotas remain deployment gates. Receipt records also consume capacity. No automatic receipt deletion or silent eviction.
- Raw mail, envelope metadata, WAL and local database backups may contain plaintext. Do not claim end-to-end confidentiality or write correspondence to application logs.

## Work Guidance

- Reuse installed SQLite and fsutil; preserve raw bytes without MIME reconstruction.
- Keep public deployment gated by receiver abuse/capacity, TLS, provenance and recovery qualification; app command integration does not install a receiver.

## Verification

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/ingress -count=1` checks retention, fencing, immutable ownership, quotas, physical admission/recovery, pinned-reader WAL reclamation, competing-process refusal and transaction replay.
- Set `MADDY_PROOF_BINARY` to the pinned binary in `docs/RECEIVING_GATEWAY_ASSESSMENT.md` to run the real receiving integration test; absence skips that test explicitly.
- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/app -run '^TestNativeReceiving' -count=1` checks actual receiving commands/import, deadline, revocation, quarantine and quota recovery. Supply the same pinned binary to exercise actual Maddy command dispatch.

## Child DOX Index

None.
