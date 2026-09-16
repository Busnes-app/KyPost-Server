# PGP key lifecycle — proposed Tier 5 design

Status: lifecycle conversion, retirement and older-backup merging are **not shipped**.
Ordinary whole-ring password changes and matching recovery restore are implemented for already-converted records. Internal atomic
whole-ring storage and legacy-writer guards are implemented. The first reader
implementation accepts legacy armor and bounded versioned rings for historical
mail, draft and autosave decryption. Single-key writers refuse ring plaintext
until server/native lifecycle gates are ready. Baseline: server PR #189,
merged at `abb5e9fdeb773f86b66b996023a5b8034a72bd99`. The current wire contract
remains [E2E_PGP.md](E2E_PGP.md). Retirement requires re-enrolling every device
(user decision, 2026-09-13). Address reassignment remains deferred until an
admin user-delete route exists.

## Evidence and compatibility boundary

The browser vault holds opaque plaintext in memory (`keyVault.ts`).
`pgpKeyring.ts` validates legacy armor or the complete ring for mail, saved drafts
and local sealed autosave decryption. Existing signing/enrollment/legacy upload writers
still require single-key armor; no account conversion is exposed. Complete-ring
offline export, read-only drills and matching-ring restoration are implemented.
Verified recovery-slot uploads are implemented; older-backup merging remains gated.
The users store holds one current public identity and opaque password/recovery/
device envelopes. Replacing its fingerprint clears device slots; a same-key
update does not. Fingerprint guards cannot detect concurrent same-key edits.

Native source inspection on 2026-09-14 contradicts the original punch list:

| Client checkout | Current behavior | Required before activation |
| --- | --- | --- |
| [Android `4e53200`](https://github.com/Busnes-app/KyPost-for-Android/tree/4e53200967fbf5225620add61f14cc037587bce3) | `EnrollmentVault.store` overwrites one encrypted blob; `EnrollmentSession` replaces one key. | Durable complete keyring, historical decrypt, explicit active signing key. |
| [Mac `ff423f7`](https://github.com/Busnes-app/KyPost-for-Mac/tree/ff423f7b62ae99b80d7539ebc5a0115fb1d1e252) | `EnrollmentVault` uses fixed Keychain envelope/fingerprint entries; `GopenPGPCrypto` parses one key. | Durable complete keyring, historical decrypt, explicit active signing key. |
| [Linux `34776ca`](https://github.com/Busnes-app/KyPost-for-Linux/tree/34776ca4a2068170020c01cc107f86cc9a0667d4) | GnuPG retains imported keys, but `OpenPgpKeyImporter` rejects imports containing multiple primary fingerprints. | Validated bundle import and explicit active key. |

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
  "activeFingerprint": "FULL-PRIMARY-FINGERPRINT",
  "materialGeneration": 1,
  "keyFingerprints": ["FULL-PRIMARY-FINGERPRINT", "FULL-SUBKEY-FINGERPRINT"],
  "keys": [
    {
      "fingerprint": "FULL-PRIMARY-FINGERPRINT",
      "privateKey": "ASCII-armored private key",
      "revocationCertificate": "ASCII-armored revocation certificate"
    }
  ]
}
```

Writers use uppercase hex fingerprints derived from the parsed packets; the
reader compares case-insensitively and detects duplicates after normalization.
`keyFingerprints` is the unique complete primary/subkey inventory (order is
immaterial). `materialGeneration` is a positive safe integer. The reader rejects
unknown formats, duplicate/missing members, multiple keys in one armor entry,
public-only keys and, for JSON rings, any missing or still-passphrase-protected
private packet. Legacy armor keeps its prior decryption policy, including GnuPG
exports with a dummy primary and usable encryption subkey, large UID/certification
sets, and text surrounding armor. JSON ring plaintext parsing is bounded to
128 KiB before JSON/OpenPGP work; recovery creation also enforces the serialized
sealed-envelope limit below, including base64 expansion. Reader capacity is broader
than storage admission: both password and recovery envelopes must fit 128 KiB,
so the effective stored plaintext ceiling is below 96 KiB. Wrapping the same bytes
with either secret produces equal envelope sizes; keep the broader reader bound
for inspection and salvage.

`revocationCertificate` is optional for imported keys. Keep unpublished
certificates sealed: possession permits premature revocation. Derive and compare
each fingerprint against its parsed key, reject duplicate primary fingerprints,
and require exactly one matching active member. Preserve all private subkeys,
certifications and revocations. Every other member is historical; timestamps may
be displayed but do not authorize use. A public revocation record can disable the
active member without opening or modifying the sealed payload.

The internal store caps a keyring at 16 primary keys, 256 total primary/subkey
fingerprints, and each serialized sealed envelope at
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

The server revision foundation now persists `pgpRevision`, returns it with
bootstrap/identity/envelope snapshots and mutation responses, and checks optional
`expectedRevision` in existing PGP and password write APIs under the disk lock.
Browser writers supply snapshot-bound revisions, preserving vault provenance and
prepared recovery revisions across refreshes and failed uploads. Older clients
may still omit the guard on unconverted accounts. The internal whole-ring transaction
now stores versioned metadata, complete sealed password/recovery envelopes and an
optional derived credential atomically. Converted accounts require revision guards
and refuse legacy single-key writers. There is no HTTP conversion route. Every keyring/public-identity/recovery/credential writer
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

`kypost-pgp-recovery-v2` file creation and read-only drills are implemented for the ring.
The file carries public identity/metadata and seals the original complete plaintext;
creation roundtrips every byte. File opening checks encrypted generation/inventories
against file metadata; drills check a fresh post-decrypt server snapshot too, including
the complete active public packet multiset. Matching v2 restore preserves current metadata and confirms exact stored ciphertext before installation; verified uploads require saved-secret acknowledgement and exact slot confirmation; older-backup merges remain gated. Keep v1 readers for
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

Device-authenticated enrollment-key publication now accepts `envelopeVersions`
and exposes `enrollmentEnvelopeVersions` to the browser; see the exact request,
legacy defaults and bounds in `E2E_PGP.md`. Current delivery remains v2 and the
browser refuses devices that exclude v2. Converted accounts require v3 support and
otherwise show update-required. Legacy accounts may continue v2. Never downgrade a
converted account to active-key-only delivery.

V3 uses the existing installed ECDH/HKDF/AEAD primitives, with a distinct
`kypost-device-envelope/v3` HKDF/AAD domain and authenticated device ID and active
fingerprint, carrying the complete versioned ring. Retain the existing SAS check
before sealing; a capability advertisement is not proof of key ownership.
The preparation helper and shared vectors below pin the framing; native consumers,
v3 upload/acknowledgement and conversion remain gated.

### V3 framing and interoperability vectors

The JSON envelope is `{v:3, alg:"ECDH-P256+HKDF-SHA256+A256GCM", epk, iv, ct}`.
Binary fields use standard padded base64. `epk` is the ephemeral P-256 public
point in 65-byte uncompressed SEC1 form; `iv` is 12 random bytes; `ct` is
AES-GCM ciphertext followed by its 16-byte tag. Generate a fresh ephemeral key
and IV for every sealing. Importers must reject unsupported versions/algorithms,
invalid curve points, wrong lengths and authentication failures before parsing
plaintext. Never try v2 as a fallback for a v3 envelope.

Derive 32 shared bytes with P-256 ECDH. HKDF-SHA256 uses those bytes as IKM,
the device's raw 65-byte public point as salt, the UTF-8 bytes of
`kypost-device-envelope/v3` as info, and produces the 32-byte AES key.
AAD is the concatenation:

```
UTF8("kypost-device-envelope/v3") ||
uint16BE(byteLength(UTF8(deviceId))) || UTF8(deviceId) ||
uint16BE(byteLength(UTF8(activeFingerprint))) || UTF8(activeFingerprint)
```

The device ID is nonempty and unchanged (no trimming, normalization or case
folding), at most 65,535 UTF-8 bytes. The active fingerprint is uppercase hex
without whitespace, computed from the validated active key (40 or 64 bytes).
Lengths count UTF-8 bytes, not characters. Plaintext is the original UTF-8
`kypost-pgp-keyring-v1` JSON, including every historical private key and optional
revocation certificate. Do not re-export a parsed key to construct it. Apply the
ring bounds above and a 128 KiB UTF-8 limit on the serialized sealed envelope;
base64 expansion can reject a ring that fits the reader's 128 KiB plaintext cap.

The browser's `sealKeyringForDevice` requires caller-supplied current snapshot
metadata/public material, validates the entire ring, and verifies the existing
SAS against the same device key/ID immediately before sealing. It only returns
ciphertext. It does not upload, persist, install keys or establish that a native
client supports this format. A future delivery caller must bind preparation to
its original revision and recheck session liveness before writing.

[testdata/device-envelope-v3.json](../testdata/device-envelope-v3.json) contains
public test-only scalars, exact plaintext, ECDH/HKDF/AAD intermediate bytes and
v2/v3 ciphertext. The v3 ID includes multibyte UTF-8 and a pipe to catch character
counts and delimiter framing. V2 pins legacy byte compatibility. Fixed scalars
and IVs belong only in tests. WebCrypto sealing is checked by
`frontend/src/lib/deviceEnvelopeV3.test.ts`; an independent Go stdlib verifier
checks both directions and every intermediate in backend CI:

```sh
cd backend
GOTOOLCHAIN=go1.26.6 go test -v -run DeviceEnvelope ./internal/cryptutil
```

Passing these vectors proves crypto framing compatibility, not native durable
keyring import or released-client readiness. Each native port must also pass the
persistence and stale-device scenarios below before conversion can ship.

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
2. Revision guards, internal whole-ring storage, password changes, complete-ring
   recovery export/drills, matching restore and verified recovery-slot uploads are
   implemented. Older-backup merging, retirement and their transaction HTTP writer remain.
   Test races with password reset, same-key edits, recovery and device publication,
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

Shared synthetic reader vectors: [testdata/pgp-keyring-v1.json](../testdata/pgp-keyring-v1.json).
`frontend/src/lib/pgpKeyring.test.ts` exercises them through the real mail and
autosave readers, plus draft/Sent attachments, revoked history, malformed rings
and refusal of legacy writes. The fixture private keys are public test data.
Recovery drills now compare server generation/inventories and current public packets;
this does not authorize conversion. Retirement/merge transaction HTTP writers and
native compatibility remain implementation gates.

## Implemented password writer

`POST /api/auth/password` accepts `keyringVersion: 1` only for existing rings,
with the complete rewrapped password envelope, derived credential and expected
revision. Client validation binds exact plaintext to matching password/bootstrap
snapshots. The store mutation changes only credential and password ciphertext;
recovery presence/bytes/timestamps, public identity and inventories are preserved.
The revision increments, material generation does not. Forced changes retain the
existing opaque-preservation flow. All existing credential revocation applies.
No conversion or retirement is exposed by the password opt-in. Matching recovery restore uses the separate versioned rewrap opt-in described in E2E_PGP.md.
