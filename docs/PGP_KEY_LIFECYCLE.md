# PGP key lifecycle — proposed Tier 5 design

Status: design for implementation, **not shipped**. Baseline: server PR #189,
merged at `abb5e9fdeb773f86b66b996023a5b8034a72bd99`. The current wire contract
remains [E2E_PGP.md](E2E_PGP.md). Retirement requires re-enrolling every device
(user decision, 2026-09-13). Address reassignment remains deferred until an
admin user-delete route exists.

## Evidence and compatibility boundary

The browser currently holds one armored private key (`keyVault.ts`);
`pgpClient.ts` uses it for mail, saved drafts, local sealed autosaves and signing.
The users store holds one current public identity and opaque password/recovery/
device envelopes. Replacing its fingerprint clears device slots; a same-key
update does not. Fingerprint guards cannot detect concurrent same-key edits.

Native source inspection on 2026-09-14 contradicts the original punch list:

| Client checkout | Current behavior | Required before activation |
| --- | --- | --- |
| [Android `4e53200`](https://github.com/Busness-app/KyPost-for-Android/tree/4e53200967fbf5225620add61f14cc037587bce3) | `EnrollmentVault.store` overwrites one encrypted blob; `EnrollmentSession` replaces one key. | Durable complete keyring, historical decrypt, explicit active signing key. |
| [Mac `ff423f7`](https://github.com/Busness-app/KyPost-for-Mac/tree/ff423f7b62ae99b80d7539ebc5a0115fb1d1e252) | `EnrollmentVault` uses fixed Keychain envelope/fingerprint entries; `GopenPGPCrypto` parses one key. | Durable complete keyring, historical decrypt, explicit active signing key. |
| [Linux `34776ca`](https://github.com/Busness-app/KyPost-for-Linux/tree/34776ca4a2068170020c01cc107f86cc9a0667d4) | GnuPG retains imported keys, but `OpenPgpKeyImporter` rejects imports containing multiple primary fingerprints. | Validated bundle import and explicit active key. |

All three advertise only an enrollment public key and consume device-envelope
v2 with a single-key plaintext. These checkouts do not prove released binaries.
Previously enrolled Linux devices retaining history does not help a newly paired
device. Never silently put JSON or concatenated keys inside a v2 envelope.

## One sealed keyring

Keep the existing v2 PBKDF2-SHA256/AES-GCM password wrapper and its parameters.
Change its plaintext only after explicit account conversion:

```json
{
  "format": "kypost-pgp-keyring-v1",
  "activeFingerprint": "full-primary-fingerprint",
  "keys": [
    {
      "fingerprint": "full-primary-fingerprint",
      "privateKey": "ASCII-armored private key",
      "revocationCertificate": "ASCII-armored revocation certificate"
    }
  ]
}
```

`revocationCertificate` is optional for imported keys. Keep unpublished
certificates sealed: possession permits premature revocation. Derive and compare
each fingerprint against its parsed key, reject duplicate primary fingerprints,
and require exactly one matching active member. Preserve all private subkeys,
certifications and revocations. Every other member is historical; timestamps may
be displayed but do not authorize use. A public revocation record can disable the
active member without opening or modifying the sealed payload.

Initially cap a keyring at 16 primary keys and each serialized sealed envelope at
the existing 128 KiB bound, whichever is reached first. Refuse growth with an
explicit capacity error; never evict history. Large imported keys may hit the byte
limit earlier. A lifecycle request containing public material and two envelopes
needs an explicit aggregate body bound (proposed 1 MiB), with individual bounds
still enforced before expensive parsing. Revisit limits with representative RSA
and ECC fixtures before finalizing the wire schema.

Record a monotonic key-material generation and primary/subkey fingerprint inventory
in the server snapshot and inside each sealed ring/recovery payload. Increment the
generation whenever private-key membership changes, and commit it atomically with
both envelopes. Public UID edits and password rewraps do not change it. Compare
parsed inventory with the authenticated payload and current server snapshot; a
matching active fingerprint alone never proves that history is complete.

Legacy armor becomes a single-member ring in memory. Reading does not convert
stored data. Whole-ring wrapping avoids independent password updates that could
strand one historical key. No private material or recovery secrets enter logs,
server plaintext storage, browser persistent storage or analytics.

## Readers, writers and concurrency

Separate active-key access for new signing/self-encryption from historical-key
access for decryption. Apply the latter to received mail, Sent copies, saved
drafts and local sealed autosaves. Match recipient IDs against primary **and
subkey** IDs. IDs are hints, not authentication: try all matching keys on
collisions, and bounded all-member fallback for hidden/zero recipient IDs.
Let OpenPGP validate packets and authentication. Retired or revoked keys remain
available for historical decryption, never for new encryption/signing. Their
presence grants no sender trust and changes no contact pin.

Add an explicit account keyring version and monotonic PGP revision to bootstrap
and mutation responses. Every keyring/public-identity/recovery/credential writer
on a converted account must supply the expected revision, checked inside the
users-store mutation. Bump it for password changes, admin resets, recovery changes,
UID changes, retirement and revocation. Reject stale requests with 409 and preserve
all prior state. Legacy writers missing version/revision receive an explicit
upgrade-required response instead of overwriting a ring with one key.

Audit all entry points, including `/identity/client`, `/rewrap`, `/auth/password`,
wrapped-slot PUT/DELETE, identity deletion and admin password reset. Password
change atomically stores the new credential and the entire rewrapped ring;
admin reset preserves opaque material and invalidates stale writes. Device-slot
writes check the revision but need not increment it. Device authentication and
current slot membership remain authoritative. Do not bind device ciphertext to
this general revision: changing a password should not break a device's key copy.

## Retirement and recovery

1. Step up and unlock the complete current ring. Generate/import a distinct new
   key, retain every old member, and optionally certify the new identity with the
   uncompromised old key. Validate public/private correspondence and size limits.
2. Seal the complete result under the account password and a fresh random recovery
   secret. Serialize and reopen the recovery file, comparing every key and the
   active selection. Offer the file/secret and require saved-secret acknowledgement.
3. In one users-store mutation, compare the revision, commit active public material,
   password ring and recovery ring, and remove old device slots. Inventory cleanup
   follows; failure is reported and retried, while removed slots already deny fetch.
4. Only a confirmed commit changes the browser's active selection. On an uncertain
   network result, retain the prepared recovery material and reconcile revision/
   fingerprint before retrying; never silently generate another replacement.
5. Re-enroll every device using the complete ring, including historical keys.

Use a new `kypost-pgp-recovery-v2` file format for the ring. Keep v1 readers for
legacy accounts. A v1 or older backup must never replace a converted account's
ring and erase newer members. A v2 backup matching the current material generation
and complete primary/subkey inventory can restore a locked vault. An older
generation can only merge into a successfully opened current ring, preserving all
current members; otherwise refuse overwrite and explain the missing material.
Reconcile current public UID state and revocation records after restoration; use
the general revision guard to catch a concurrent change during recovery. A deliberate
identity deletion remains a separately confirmed destructive operation.

Password changes, recovery-slot refreshes and recovery drills validate the whole
ring. Drills change neither active selection nor server data. An older backup
cannot recover keys created after it; explain this limit beside backup export.
Retirement is not forward secrecy and cannot remove copies already held by devices
or correspondents. A rollback to old server/client binaries is unsafe after
conversion: preserve data and roll forward; a restore to an older snapshot must
explicitly account for newer keys before any destructive replacement.

## Device-envelope v3

Extend device-authenticated enrollment-key publication with supported envelope
versions; expose them to the browser. Converted accounts require v3 support and
otherwise show update-required. Legacy accounts may continue v2. Never downgrade a
converted account to active-key-only delivery.

V3 uses the existing installed ECDH/HKDF/AEAD primitives, with a distinct
`kypost-device-envelope/v3` HKDF/AAD domain and authenticated device ID and active
fingerprint, carrying the complete versioned ring. Retain the existing SAS check
before sealing; a capability advertisement is not proof of key ownership. Finalize
exact framing with shared cross-language vectors before implementing consumers.

Native clients validate every member in temporary state, persist the entire ring
before acknowledging enrollment, then switch the active pointer. Failures preserve
old keys and leave enrollment incomplete; partial GnuPG imports must never claim
success or delete history. Intentional device unpair/wipe still destroys all local
key material according to that client's existing teardown contract.

Clearing a device slot cannot erase a previously delivered key. Before new signing
or self-encryption, clients compare their active fingerprint/material generation
and revocation state with current bootstrap. Send requests from converted accounts
assert that generation; the server refuses stale or missing assertions. Record the
confirmed enrollment generation and require it for device sends after retirement,
so clearing a slot also leaves the device unable to send until re-enrolled. Browser
sends use the current unlocked ring. This prevents cooperative stale clients from
silently using an old key; it cannot revoke an exported key or prove what a
malicious client encrypted inside opaque mail.

## Revocation, transitions and aliases

Generate a revocation certificate for each generated key and offer a protected
offline copy. A valid certificate plus account step-up must support emergency
revocation even with a locked vault. Verify its signature and target fingerprint
server-side, persist a public revocation overlay, and stop advertising/using that
fingerprint for future encryption or signing. Keep the opaque ring for decryption.
Later recovery and UID writes must merge, never remove, accepted revocations.
Define a bounded public certificate download/distribution route before shipping;
keep historical decrypt keys out of ordinary WKD/Autocrypt encryption discovery.
Online clients refresh revocation state before new signing/self-encryption;
offline clients and recipients with cached keys cannot be remotely forced to
refresh. The UI must describe that limit instead of promising instantaneous
revocation everywhere.

Compromise differs from retirement: a compromise revocation makes prior signatures
suspect, while superseded/retired reasons preserve the interpretation of earlier
signatures. Use the correct reason and explicit user wording. See
[RFC 9580, reasons for revocation](https://www.rfc-editor.org/rfc/rfc9580.html#section-5.2.3.31).

For a routine transition, use a standard OpenPGP certification of the new identity
by the old key when available. Do not endorse a replacement with a compromised key
or automatically migrate recipient TOFU pins. Absence of the old key is displayed,
not synthesized into a signature. Tier 6 owns the trust presentation.

Add verified alias User IDs and revoke removed User IDs in the browser while
preserving the primary key, private subkeys, old certifications and revocations.
Packet deletion alone is not UID revocation. Reformatting alone is not evidence
that unrelated certifications survive. The server independently validates submitted
public material against currently verified send-as ownership; an IMAP username
is not proof. WKD/Autocrypt expose only currently owned, valid nonrevoked UIDs and
an eligible active key, respecting domain verification and publication opt-in.
Commit updated public material and sealed ring under the same revision. Recovery
of older material must merge current public revocations, never resurrect an alias.

## Delivery and proof

Implement in this order, keeping conversion unavailable until compatibility gates
pass:

1. Versioned ring parser and all browser decrypt consumers; shared fixtures for
   legacy import, duplicate/invalid members, subkey IDs, hidden recipients, revoked
   history, drafts and autosaves. Preserve active-only signing.
2. Revision-guarded server mutations, whole-ring password/recovery and retirement;
   test races with password reset, same-key edits, recovery and device publication,
   storage failures and uncertain HTTP outcomes. Prove no partial credential/ring
   commit and no history loss from legacy writers or old backups.
3. V3 vectors and Android/Mac/Linux persistence/import upgrades. Exercise fresh
   enrollment and re-enrollment with old/new encrypted mail, restart, failed import
   and intentional wipe on every platform. Verify released compatible versions
   before enabling conversion.
4. Public revocation distribution, signed transitions and UID editing; verify
   compromised-key refusal, immutable revocations, lost alias ownership and
   preservation of third-party certifications. Exercise GnuPG/Thunderbird interop.

The small [crypto probe](pgp-key-lifecycle-check.cjs) runs after `npm ci` in
`frontend`: `node docs/pgp-key-lifecycle-check.cjs`. It uses the pinned installed
OpenPGP.js and proves historical/hidden-recipient decryption, revoked-key historical
decryption with refusal of new encryption, an old-key certification of the new
identity, and UID addition without changing primary/subkey IDs. It does **not**
prove wire compatibility, persistence, revocation distribution, UID revocation or
preservation of arbitrary certifications. Those remain implementation gates.
