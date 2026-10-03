# Ingress

## Purpose

Durable receiving buffer and immutable envelope ownership, separate from permanent mailboxes.

## Ownership

All files in this package. No public listener, application mode or deployment configuration selects it yet.

## Local Contracts

- Callers are trusted local receiver/importer processes with exclusive access to an owner-only directory. Gateway IDs are supplied by authenticated transport configuration, never SMTP headers. This package is not a network authentication boundary.
- Route writers must verify KyIdentity provenance before calling `SetRoute`. Versions are monotonic per address; same-version refresh cannot change ownership or activation.
- Bind at RCPT; accept bounded raw MIME transactionally before SMTP success. Staged records and accepted/quarantined payloads never expire automatically: age cannot prove an SMTP transaction abandoned. Explicit fenced cleanup remains an operating gate.
- A claim revalidates the bound issuer/subject/mailbox/generation against current active routes. Conflicts quarantine without reassignment. Leases fence acknowledgments; the importer commits every intended local delivery before acknowledgment.
- `Import` resolves frozen issuer/subject/mailbox owners, checks the returned store identity, groups aliases, and commits permanent mailbox receipts before acknowledgment. Partial failure retains payload and retries after claim expiry; recipient reassignment quarantines rather than resolving the new owner. No production directory/scheduler invokes it yet.
- SQLite FULL synchronization and immediate writer transactions protect receipt/payload atomicity. Finite payload/record limits refuse acceptance; receipt records also consume capacity. No automatic receipt deletion or silent eviction.
- Raw mail, envelope metadata, WAL and local database backups may contain plaintext. Do not claim end-to-end confidentiality or write correspondence to application logs.

## Work Guidance

- Reuse installed SQLite and fsutil; preserve raw bytes without MIME reconstruction.
- Keep the Maddy helper test-only until directory authentication, operation logging and operator setup are integrated.

## Verification

- `GOTOOLCHAIN=go1.26.6 go test -race ./internal/ingress -count=1` checks retention, fencing, immutable ownership, quotas and transaction replay.
- Set `MADDY_PROOF_BINARY` to the pinned binary in `docs/RECEIVING_GATEWAY_ASSESSMENT.md` to run the real receiving integration test; absence skips that test explicitly.

## Child DOX Index

None.
