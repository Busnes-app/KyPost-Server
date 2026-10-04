# Native restore message references

## Shipped groundwork and remaining gate

Native mailboxes now retain a strict UUID-v4 `reference_generation` singleton. Ordinary reopen preserves it. Held offline restore rotates it per exact-source mailbox after whole-snapshot historical validation and before publication. Older databases with no table gain one; a present empty, malformed or non-table object is corruption. Mutation failures retain unpublished staging and the restore hold. Retry rotates again while held.

This generation is separate from the immutable namespace used by incoming-encryption recovery and encrypted outbox keys. Rotation preserves that namespace, raw mail, numeric IDs, receipts and queued ciphertext. **HTTP message references remain numeric; this layer does not prevent stale client references from resolving to reused IDs. No native restore hold release is supported.**

Rollback requires stopped workers and a compatible binary. Do not drop the generation table or run an older writer to repair metadata. Direct raw-volume rollback and mixed-root restores remain unqualified.

## Client source evidence

Read-only research pinned KyPost `5387324816fb9ee4f3ed663b2d97eab066ad3489`, Android `94ec827624a0804977a3eb15b8d0951685aad9f3`, Linux `544648b1d7d32469ff0763bc28f641959eb52a6b`, and Mac `2c1af70913017694c26d5a8beb921c6938ce32bd`. No sibling repositories were edited or compiled; Android had an unrelated pre-existing version-catalog modification.

All three client wire/cache message identifiers are strings. Android reconnect retains downloaded mail, and full snapshots can preserve a cached body with the same folder/message ID. Re-pairing and full sync alone therefore do not prove stale-reference isolation. See Android's [wire models](https://github.com/Busnes-app/kypost-android/blob/94ec827624a0804977a3eb15b8d0951685aad9f3/app/src/main/java/org/kysecurity/mail/mail/RelayModels.kt), [push repository](https://github.com/Busnes-app/kypost-android/blob/94ec827624a0804977a3eb15b8d0951685aad9f3/app/src/main/java/org/kysecurity/mail/push/PushRepository.kt) and [mail merge](https://github.com/Busnes-app/kypost-android/blob/94ec827624a0804977a3eb15b8d0951685aad9f3/app/src/main/java/org/kysecurity/mail/mail/MailRepository.kt). Linux uses [string IDs](https://github.com/Busnes-app/kypost-Linux/blob/544648b1d7d32469ff0763bc28f641959eb52a6b/core/models/Email.h); Mac uses [string wire models](https://github.com/Busnes-app/kypost-for-Mac/blob/2c1af70913017694c26d5a8beb921c6938ce32bd/KyPost/Data/Mail/RelayMailSource.swift).

The compatibility proposal is a generation-bound opaque native wire message ID, with prior-generation and bare numeric references rejected before reads or mutations. Keep numeric internal IDs, source identities, encryption bindings and legacy IMAP behavior. Native full snapshots retain `delta:false,cursor:0`; Linux cursor parsing is numeric and cannot safely be changed to an opaque string as part of this fence. Stored historical notification references must never be reinterpreted as current-generation references.

## Runnable evidence and follow-up

`TestNativeReferenceGenerationRollbackAndFailure` uses `VACUUM INTO` to retain an earlier real SQLite snapshot, advances the live database, then demonstrates ID reuse on the restored snapshot. It checks fresh generation, unchanged source/raw/receipt, wrong-source refusal, failed-write rollback and missing-file refusal. `TestNativeReferenceGenerationOlderSchemaAndCorruption` checks older absent tables and refuses empty, malformed and view-shaped metadata. `TestPrepareAccountReferenceGenerationReadOnly` exercises actual preparation validation and runtime migration without repairing corrupt metadata. The sealed native credential-restore regression checks repeated rotation and live-source isolation while preserving mail and the hold.

Before activation, implement and test every wire read/write boundary, ordinary and PGP body/attachment reads, actions, keyword changes, inbox/search serialization, notification creation/history and cached-body identity separation. String DTOs are source evidence, not physical client qualification. Update the cross-repo wire contracts in the implementation that changes those references.
