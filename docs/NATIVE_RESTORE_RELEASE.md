# Native restore hold release

Status: proposed; PR 1 implemented (release floors). No release operation exists yet: `native-restore-hold.json` still has no supported removal. Read [restore authority](NATIVE_RESTORE_AUTHORITY.md), [reference boundary](NATIVE_RESTORE_REFERENCES.md) and [restore procedure](RESTORE.md) first.

## Threats the hold covers

- Offboarded or deleted subjects returning from an old backup with active local accounts or active directory rows.
- Directory-only subjects (row, no account) and unpublished/pending/failed reservations allocated from a stale active row.
- ID tokens issued before the restore.
- Message-reference reuse: old IDs resolving to newly allocated mail.
- Restored device/push registrations, pairing tokens and CardDAV app passwords.
- Old queued, retryable or interrupted outbound mail sent a second time.
- Receiving into addresses reassigned since the backup.
- Two live copies. Cloudflare is fenced by the host marker; Maddy has no fence.

## Release preconditions

- **P1** Fresh signed evidence covers every subject; repair completed and is still within its `ExpiresAt`.
- **P2** Repair `AfterDigest` equals the current `AuthorityDigest`; repair barriers match.
- **P3** A restore qualification marker `state/native-restore-qualification.json` `{epoch, createdAt, mailboxes:{id:referenceGeneration}}`, written as the last `QuarantineNativeRestore` step, matches at release; no queued or retryable outbox rows exist.
- **P4** Release floors (below, implemented).
- **P5** Administrator step-up.
- **P6** Only an active local-password administrator (non-native, non-KySignOn) may release. Owner to confirm.
- **P7** `qualification.createdAt <= challenge.CreatedAt`; every native `RevokedBefore >= createdAt`; release raises `RevokedBefore` to now + 31 s again.
- **P8** Fresh domain proof for at least one configured domain.
- **P9** Holds with zero subjects take a separate path later.

## Release floors (PR 1)

`sso-lifecycle.json` `releaseFloors` maps issuer/subject to `{revision, active}`: the signed evidence a release consumed. They are separate from `recoveryFloors`, which `nativeRecoveryInputs` treats as barriers that published accounts must clear, so writing release floors never refuses published legacy accounts in a later challenge.

- `LifecycleStore.RecordNativeReleaseFloors` raises floors monotonically: a lower revision never replaces a higher one; disagreeing evidence at one revision keeps `active:false`. Only tests call it until PR 4.
- `CheckNativeReleaseFloor` refuses active directory state whose revision is at or below the floor. The provisioning worker (`reconcileNativeSubject`) calls it before administrator provisioning, allocation and reallocation, after the legacy-account return. Inactive state passes, so the disable path stays open. `active` is reported, not trusted by the guard.
- `applyDirectory` refuses directory events at or below a release floor (`ErrDirectoryConflict`, 422), so the webhook cannot provision or reactivate from a stale queued event either.
- Floors survive restart and are sealed in backups with `sso-lifecycle.json`; `ValidateNativeSnapshot` refuses malformed floors.
- Lifting a floor needs a strictly newer KyIdentity revision. A resync allocates one for every active user; offboarded subjects stay floored.

Hold check, verified: `reconcileNativeSubject`, `AllocateNativeAccount` and the worker's address reconcile refuse while the hold exists. The webhook does not: newer ordinary events apply under the hold (fenced by `recoveryFloors`), and administrator accounts are provisioned (`TestNativeAdministratorProvisionedDuringRestoreHold`). Release floors therefore matter only after release, and are written by it.

## Release operation

`POST /api/admin/native-recovery/release`, step-up, API only. Read-only `restore status` CLI and API.

1. Verify P1-P8.
2. Audit intent.
3. One lifecycle write: `RestoreRelease`, release floors from the evidence, raised `RevokedBefore`.
4. Atomic rename `native-restore-hold.json` → `native-restore-released.json`, then `SyncDir`.
5. Audit completion.

A retry after the rename answers `alreadyReleased`. Still refused after release: Cloudflare takeover remains an explicit CLI step; receiving resumes only on restart; quarantined outbound and inbound mail stay quarantined; devices re-pair. With Maddy the request must carry the confirm token `original-host-decommissioned`.

## Mail during the hold

Maddy is down, so senders retry. Cloudflare holds mail in R2. Restored pending ingress imports after release under fresh authority. Quarantined outbound is never requeued.

## PR breakdown

1. Release floors (this change).
2. Qualification marker.
3. Read-only status.
4. Release core.
5. HTTP route and Maddy confirmation.
6. Optional zero-subject path.

## Gating

Release ships behind `KYPOST_NATIVE_RESTORE_RELEASE=true`, off by default, until the pending qualifications in [references](NATIVE_RESTORE_REFERENCES.md) and [restore](RESTORE.md) are done.

## Owner questions (defaults)

- **Q1** Keep the 5-minute evidence window; script the flow.
- **Q2** No KySignOn release.
- **Q3** Maddy requires the confirm token.
- **Q4** Zero-subject release path later.
- **Q5** Never requeue outbound.
- **Q6** Legacy SSO-linked accounts are out of scope; the release documents the review they need.
- **Q7** Relay check right after release, not a precondition.
- **Q8** The off-by-default flag.
