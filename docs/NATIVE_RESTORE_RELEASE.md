# Native restore hold release

Status: PRs 1–5 implemented: release floors, qualification marker, read-only status, and the release core with its HTTP route and Maddy confirmation, behind `KYPOST_NATIVE_RESTORE_RELEASE` (off by default) pending the qualifications under Gating. The owner defaults below were set by the overnight coordinator and await owner confirmation. Read [restore authority](NATIVE_RESTORE_AUTHORITY.md), [reference boundary](NATIVE_RESTORE_REFERENCES.md) and [restore procedure](RESTORE.md) first.

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
- **P6** Only an active local administrator with no SSO link, on a password session (not native, not KySignOn, not generic SSO), may release. An SSO-linked one could be deactivated by the release itself.
- **P7** `qualification.createdAt <= challenge.CreatedAt`; every native `RevokedBefore >= createdAt`; release raises `RevokedBefore` to now + 31 s again.
- **P8** Fresh domain proof for at least one configured domain.
- **P9** Holds with zero subjects take a separate path later; the release refuses them.

Where each is checked: P1, P2 (`completedRepairReasons`), P3 (`CheckNativeRestoreQualification`), P7 (`nativeRestoreTokenFenceReasons`) and P9 in `LifecycleStore.PlanNativeRestoreReleaseHeld` (`backend/internal/sso/native_release.go`), the same functions status uses; P4 in `NativeRestoreReleasePlan.Commit`; P5, P6, P8 and the Maddy confirmation in `handleNativeRecoveryRelease` (`backend/internal/api/native_recovery_handlers.go`).

## Release floors

`sso-lifecycle.json` `releaseFloors` maps issuer/subject to `{revision, active}`: the signed evidence a release consumed. They are separate from `recoveryFloors`, which `nativeRecoveryInputs` treats as barriers that published accounts must clear, so writing release floors never refuses published legacy accounts in a later challenge.

- Floors rise monotonically: a lower revision never replaces a higher one; disagreeing evidence at one revision keeps `active:false`. The release writes one per evidence subject; `LifecycleStore.RecordNativeReleaseFloors` is the standalone form.
- One rule (`NativeReleaseFloor.refuses`) refuses active state below the floor revision, or at it when the evidence said inactive. KyIdentity bumps the revision on every desired-state change, so equal revision and equal activity is the evidence's own state and passes: unchanged active subjects keep working without a resync. Inactive state always passes; it can only fail closed.
- `reconcileNativeMailboxLocked` applies it to every allocation and preparation, so no caller bypasses it. The worker (`reconcileNativeSubject`) checks first, which also covers administrator provisioning, and treats a refusal as a quiet no-op logged once per subject per process.
- `applyDirectory` refuses events the rule refuses (`ErrDirectoryConflict`, 422). A deactivation or deletion still queued upstream when the floor was written applies; resync re-sends only active users, so refusing it would leave the subject active for good.
- Floors survive restart and are sealed in backups with `sso-lifecycle.json`; `ValidateNativeSnapshot` refuses malformed floors.
- A refused subject needs a strictly newer KyIdentity revision. A resync allocates one for every active user; offboarded subjects stay floored.

Hold check, verified: `reconcileNativeSubject`, `AllocateNativeAccount` and the worker's address reconcile refuse while the hold exists. The webhook does not: newer ordinary events apply under the hold (fenced by `recoveryFloors`), and administrator accounts are provisioned (`TestNativeAdministratorProvisionedDuringRestoreHold`). Release floors therefore matter only after release, and are written by it. A newer event drops the receipt and repair, so a release racing it is refused.

## Release decisions (implemented)

- **Non-native accounts (a).** Active accounts without a native mailbox whose SSO subject the evidence shows inactive or deleted (directory-provisioned administrators, legacy SSO links) are deactivated. Active administrators whose subject the evidence shows active without the `kypost.admin` role are demoted to user, local password included. Both are signed out. The release never activates or promotes anything.
- **Directory rows (b).** An active row for an evidence-inactive subject, or a missing one, becomes inactive at the evidence revision, with the evidence profile as its resource (`eventId` `native-restore-release`). The same revision is allowed: live expiry need not allocate one. A reservation at that revision follows the row, and address states and inactive routes are recomputed, so `desiredActive`, address state and the Cloudflare route table cannot revive repair-deactivated subjects. The directory's own inactive event at that revision is then already applied; an active one there is refused. This is the one place KyPost writes a directory resource from recovery evidence.
- **Resync step (c).** After release, status lists `resyncSubjects`, the active floored subjects without a newer revision, and the "run a KyIdentity resync" step until the list is empty.
- **Validate before mutating.** Every input, floors and account changes included, is computed and validated before the first write.

## Release operation

`POST /api/admin/native-recovery/release` with `{password}` or `{authSecret}`, plus `"confirm": "original-host-decommissioned"` on the Maddy profile (`KYPOST_NATIVE_RECEIVER=true` or `config/receiving.conf` present; an unreadable config state refuses). Exact-action CSRF and step-up, API only. Off: `404`; an invalid flag value: `409`; no hold and no release: `409` "no native restore hold".

1. Flag, P6 session check, step-up (P5), then P6 on the confirmed account.
2. Maddy confirmation; a fresh `VerifyDomain` for every configured domain, outside the fences (P8).
3. Fences: domain → settings → directory → users → session. At least one proof from step 2 is still current; P1, P2, P3, P7 and P9 are recomputed; no release intent for this epoch exists (a `restoreRelease` from another epoch, restored from a post-release backup, is replaced).

The writes, in order, each a crash point:

4. **Audit intent** (`admin.native_restore_release`, `started`, naming the accounts to deactivate and demote). If it fails nothing was written: `503`, retry.
5. **Intent**: `restoreRelease {epoch, nonce, receiptDigest, afterDigest, actor, at, deactivated, demoted}` in `sso-lifecycle.json`.
6. **Accounts** (a) in `users.json`.
7. **Ledger**: reservations at a rewritten row's revision follow it, address states are recomputed and non-active routes written inactive. A route still pending (receiving database busy) is not a failure: every later commit and the worker retry it, and import already quarantines on the ledger generation.
8. **Lifecycle**: one write of the rows (b), release floors for every evidence subject, and `RevokedBefore` raised to now + 31 s for every evidence subject with a directory row and every published native mailbox. An evidence-active subject with no row has no ID-token fence to raise; evidence-inactive ones get a row in this write.
9. **Rename**: re-read the hold epoch; rename `native-restore-hold.json` → `native-restore-released.json` (epoch kept), `SyncDir`. `RequireNativeRestoreReleased` looks only for the hold, so this is release.
10. Completion audit under the release's original actor and time, naming the changed accounts, then `restoreRelease.completedAt`.

Crash safety:

- Before the rename (steps 5–8) the hold stays and the intent refuses a retry (`409`, "interrupted") until a new challenge clears it; that needs fresh evidence and repair, then a release (tested end to end from each point). Every write so far only deactivates, demotes or raises: accounts stay changed and floors only go up.
- After step 7 alone the ledger is ahead of the lifecycle for a subject deactivated at its own revision, so the snapshot check, and with it backups, refuse until that recovery completes. The hold covers the window; address states only got stricter.
- After the rename the host is released. A later failure (`SyncDir`) answers `200 {released:true, confirmed:false}` without a failure audit; repeating the request answers `200 {alreadyReleased:true}` when the released marker's epoch matches `restoreRelease`, and writes the missing completion audit and `completedAt`.
- Backups never collect the released marker.

Still refused after release: Cloudflare takeover remains an explicit CLI step; receiving starts only on restart; quarantined outbound and inbound mail stay quarantined; devices re-pair.

## Rollback

An older binary that re-persists `sso-lifecycle.json` drops `releaseFloors` silently, reopening allocation from stale rows. After a release, do not downgrade below the version that introduced floors; if forced to, keep a copy of the file and treat the floors as lost.

## Mail during the hold

Maddy is down, so senders retry. Cloudflare holds mail in R2. Restored pending ingress imports after release under fresh authority. Quarantined outbound is never requeued.

## PR breakdown

1. Release floors.
2. Qualification marker.
3. Read-only status.
4. Release core.
5. HTTP route and Maddy confirmation (shipped with 4).
6. Optional zero-subject path (not implemented).

## Gating

Release ships behind `KYPOST_NATIVE_RESTORE_RELEASE=true`, off by default, until the pending qualifications in [references](NATIVE_RESTORE_REFERENCES.md) and [restore](RESTORE.md) are done.

## Owner decisions (overnight coordinator defaults, owner to confirm)

- **Q1** Keep the 5-minute evidence window; script the flow.
- **Q2** Release only by an active local-password administrator: not native, not KySignOn.
- **Q3** On the Maddy profile the request carries `original-host-decommissioned`.
- **Q4** No zero-subject release in this version (P9 refuses).
- **Q5** Never requeue quarantined outbound.
- **Q6** Legacy SSO-linked accounts are otherwise out of scope; decision (a) still deactivates those the evidence shows inactive.
- **Q7** No relay precondition; check the relay right after release.
- **Q8** Everything behind `KYPOST_NATIVE_RESTORE_RELEASE=true`.
