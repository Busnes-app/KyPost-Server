# KyPost OpenPGP v6 and post-quantum keys, design

Date: 2026-11-25. Status: **approved** (see "Owner answers"). Only Q12 is unanswered. P0 is being built. Owner request (2026-11-25): move KyPost to OpenPGP v6 (RFC 9580) with post-quantum keys,
then move the whole Ky stack to post-quantum cryptography. The suite-wide plan is
`ky-primitives/docs/superpowers/specs/2026-11-25-pqc-stack-plan.md` (**B**). The primary-source research both
specs rely on is `ky-primitives/docs/superpowers/specs/2026-11-25-pqc-research.md` (**R**).

## Owner answers (2026-11-25)

- **Approved as recommended:** Q1, Q2, Q3, Q4, Q6, Q8, Q11.
- **Q5, changed: pin an openpgp.js v7 pre-release now, and keep it updatable by Dependabot.**
  - No v7 is published to npm: `dist-tags` is `{latest: 6.3.3}` and there are no `7.*` versions, checked
    2026-11-25. The pin is therefore a git dependency on an exact openpgpjs `main` commit, the line that carries
    RFC 9980 (PR #2022).
  - The PR that adds it proves two things: Dependabot opens an update for that git dependency (unproven so
    far), and the build passes against the RFC 9980 fixtures.
  - Browser **read** support moves forward into P3 (below).
  - **Where openpgp.js cannot do a job, the browser runs gopenpgp compiled to WebAssembly**: the same library
    and KyPost profile as the server and the Mac. A spike on Go 1.27.3 with gopenpgp v3.5.2 `profile.PQC()`,
    run under Node 26 (not yet in a browser), passed:
    - it generated a v6 key with algorithm 30 and a 13,680-byte public certificate (the estimate was ~13.8 KB);
    - it encrypted, signed, decrypted and verified;
    - each operation took 4-18 ms;
    - the module is 8.8 MB, 2.4 MB gzipped.
  - It is lazily loaded, and used only for what openpgp.js lacks. Today that is ML-KEM-1024+X448 and
    ML-DSA-87+Ed448 (openpgp.js PR #2077, still open), and any RFC 9980 case where the cross-implementation
    fixtures show openpgp.js v7 disagreeing with go-crypto and Bouncy Castle.
- **Q9, overturned: a person's mail is encrypted to their v6 certificate, and KyPost uses v6 whenever
  possible.** Mail already stored is re-encrypted (new phase P6 below), and so is any message that still
  arrives encrypted to the v4 key.
- **Second round (2026-11-25, later):**
  - **Q5 extended:** since gopenpgp compiles to WebAssembly, *test it as a full replacement for openpgp.js*, not
    only as a fallback. A draft evaluation PR (`feat/pqc-wasm-engine`) runs the existing browser test suite
    against both engines and reports size, load time, interop, the CSP change and the secret-handling posture.
    The choice between openpgp.js v7 and gopenpgp WASM is made on those numbers.
  - **Q7 approved:** category 3 (ML-KEM-768 / ML-DSA-65) for now.
  - **Q9 approved**, as overturned above (re-encrypt to v6, P6).
  - **Q10 approved:** sign with the key that matches each delivery; no dual signing.
  - **Q12 not answered:** until it is, P3-P6 wait for the owner's merge approval; P0-P2 merge on the usual gate.

Line references are to KyPost-Server `origin/main` at `928ba0be`. Server paths are relative to the repo
root. Client references are to KyPost-for-Android `01f304bb` (**A:**), KyPost-for-Mac `165148e` (**M:**,
one target for macOS and iOS) and KyPost-for-Linux `ae383bf` (**L:**). Server references were read in
code; the client ones were read by a sub-agent and spot-checked at the headline level, so treat a client
line number as "reported". **Nothing here was run.** "Unverified" marks a claim no one checked.

## Summary

- **The standards are final.** RFC 9580 (OpenPGP v6, July 2024) and **RFC 9980** (Post-Quantum Cryptography in
  OpenPGP, Standards Track, June 2026; was draft-ietf-openpgp-pqc-17). RFC 9980's MUST algorithms are
  **ML-KEM-768+X25519 (ID 35)** and **ML-DSA-65+Ed25519 (ID 30)**. ML-DSA is v6-only. ID 35 is the one
  PQ algorithm also allowed as a subkey of a v4 key (R §2).
- **The Go side is ready today.** `backend/go.mod:10-11` pins ProtonMail go-crypto v1.5.2 and gopenpgp v3.5.2.
  go-crypto has the final RFC 9980 IDs (`openpgp/packet/packet.go:524-534 @ v1.5.2`, checked) with no build
  tag, and gopenpgp has a `profile.PQC()` preset that makes v6 ML-DSA-65+Ed25519 keys with AEAD and Argon2
  (`profile/preset.go:84-117 @ v3.5.2`, checked). Every KyPost call site uses `crypto.PGP()`, the v4
  default profile (`backend/internal/pgpmail/mime.go:179`).
- **The browser is the critical path.** The browser is the only place a KyPost account key is generated
  (`frontend/src/lib/pgpClient.ts:63-68`), and it decrypts and signs every client-custody message.
  openpgp.js 6.3.3 (`frontend/package.json:20`) has no PQC. RFC 9980 support exists only on the unreleased
  **openpgp.js v7** line (openpgpjs PR #2022, merged to `v7` on 2026-07-10). Its 6.x `pqc_*` enum
  declarations use pre-RFC code points 105/107 and must never be used.
- **The ecosystem is not ready for v6 keys from strangers.** GnuPG speaks LibrePGP and cannot process v6
  certificates; shipped Thunderbird (RNP 0.18) has no RFC 9980; Gmail has no OpenPGP; GPGTools, OpenKeychain
  and Mailvelope are v4-only (R §8). Per the 2026 OpenPGP Email Summit minutes, a v6 certificate or signature
  can **break** a v4 client, not just be ignored by it. Proton, the one production PQC deployment (opt-in since
  2026-05-05), does not expose v6/PQC keys to outside users.
- **Therefore: two certificates per account.** Keep each account's existing v4 certificate as its public,
  interoperable identity. Add a **v6 PQC certificate** (ML-DSA-65+Ed25519 primary, ML-KEM-768+X25519
  subkey) used for everything KyPost controls on both ends: mail stored encrypted to the user, Sent and
  drafts, and mail between KyPost users. Encrypt to outsiders per recipient with the best certificate they
  publish, and mark any delivery that is not post-quantum.
- **The biggest harvest-now-decrypt-later (HNDL) target is not OpenPGP.** The device-enrollment envelope
  that carries the account private key or ring through the server is HPKE DHKEM(P-256)
  (`backend/internal/users/pgp_device_delivery.go:78`, `frontend/src/lib/deviceEnrollment.ts:380-397`).
  Anyone who records it and later breaks P-256 gets every message, past and future. Moving it to X-Wing
  (ML-KEM-768+X25519), which ky-primitives already uses for backups, is phase P2 and does not wait for
  openpgp.js v7.
- **Mail already stored gets re-encrypted (owner, Q9).** Every incoming-encrypted INBOX message, Sent copy and
  draft so far is X25519 ECDH with SEIPDv1. The user's own client re-encrypts it to the v6 certificate in P6,
  because only the client holds the key.

## What changes for whom

| Who | Today | After the rollout |
|---|---|---|
| KyPost user, own mail at rest (incoming encryption, Sent, drafts) | v4 X25519, SEIPDv1 | v6 ML-KEM-768+X25519, SEIPDv2 (AEAD). PQ-confidential |
| Two KyPost users on one install | v4 X25519 | v6 PQC both ways, signed ML-DSA-65+Ed25519. PQ-confidential and PQ-authenticated |
| KyPost user ↔ a PQ-capable outside client (Proton with exported v6, Thunderbird once it ships ID 35, GnuPG `--allow-9980`) | v4 | PQ where the correspondent publishes PQ material (Q3, Q4) |
| KyPost user ↔ a v4-only outside client (GnuPG 2.4/2.5, GPGTools, OpenKeychain, Mailvelope, older Thunderbird) | v4 | Unchanged v4 delivery, marked "not quantum-safe" in the compose UI (Q1) |
| Gmail and other non-OpenPGP recipients | Secure-link pickup or plaintext | Unchanged (the pickup is AES-256-GCM, symmetric, `frontend/src/lib/pickupCrypto.ts:77-98`) |
| A new device | P-256 HPKE envelope | X-Wing HPKE envelope (P2) |

## Inventory: every OpenPGP use in KyPost

Each row: where, what it does today, what v6 + PQC needs. Numbers in brackets are the build phases below.

### 1. Key generation

| Item | Location | Today | v6 + PQC |
|---|---|---|---|
| Browser generator, the only one | `frontend/src/lib/pgpClient.ts:43-75` (call at `:63-68`; rationale comment `:36-41`) | openpgp.js `type:"ecc", curve:"curve25519Legacy"`: v4 EdDSA-legacy primary (algo 22) + v4 ECDH cv25519 subkey (algo 18). No passphrase (S2K usage 0); protection is the keyVault envelope. One key per account, one User ID per account address | Generate a second, v6 certificate: ML-DSA-65+Ed25519 (30) primary, ML-KEM-768+X25519 (35) subkey, needs openpgp.js v7 [P4]. Keep `passphrase` unset: all three native clients refuse protected keys by design (A:…/pgp/PgpKeyring.kt:142-145; M:…/GopenPGPCrypto.swift:301-309) |
| Server generation and import | `backend/internal/api/server.go:928-929`; `docs/E2E_PGP.md:22-27` | Retired; the routes refuse | No change |
| Legacy server-custody keys and the test helper | `backend/internal/pgpmail/pgpmailtest/pgpmailtest.go:30-40` | `crypto.PGP().KeyGeneration()`: v4 EdDSA | Test helper gains a v6 PQC variant [P0] |
| Public-key upload validation | `backend/internal/pgpmail/identity.go:103-128`; refusal of revoked or expired `backend/internal/api/pgp_client_keys.go:96-106` | Parses, refuses private, re-armors, derives fingerprints itself. **No algorithm or version allowlist**: anything go-crypto parses is stored and published | Add an explicit allowlist: v4 {Ed25519-legacy, cv25519, RSA ≥ 2048, ID 35 subkey}, v6 {27, 25, 30, 35} (31/36 per Q7). Refuse pre-RFC 105/107 and anything else [P0] |
| Native generation | A: none in `app/src/main`; M:docs/STRONG-CRYPTO.md:61-62; L:docs/STRONG-CRYPTO.md:76 | None. Keys arrive by enrollment | No change |

### 2. Key storage

| Item | Location | Today | v6 + PQC |
|---|---|---|---|
| User record | `backend/internal/users/users.go:133-200`; table mode `accounts.record` BYTEA/BLOB, `backend/internal/volumes/store.go:30-50` | One `pgpPublicKey`, `pgpFingerprint`, `pgpKeyId`; `pgpPrivateKeyWrapped` (client envelope, opaque); `pgpWrappedEnvelopes[]` recovery and `device:<id>` slots; legacy `pgpPrivateKeyEnc` (AES-256-GCM under `SECRET_DIR/pgp-private-key.key`) | Add the v6 certificate beside the v4 one (`pgpPublicKeyV6`, fingerprint, created). The account keeps one v4 and at most one v6 *active* certificate [P3] |
| Ring plaintext `kypost-pgp-keyring-v1` | `docs/PGP_KEY_LIFECYCLE.md:39-91`; `frontend/src/lib/pgpKeyring.ts:5-7,56-78`; server `backend/internal/users/pgp_keyring.go:16-17,85-102` | One `activeFingerprint`; ≤ 16 primaries, ≤ 256 fingerprints, 128 KiB per sealed envelope | `kypost-pgp-keyring-v2`: `active` becomes `{v4, v6}`. v1 rings stay readable [P3] |
| Browser wrapping (keyVault) | `frontend/src/lib/keyVault.ts:33-37,48-49,66-71,87` | PBKDF2-SHA256 600k, AES-256-GCM; key in page memory only | Symmetric, already PQ-adequate at 256 bits. The Argon2id move is separate and the envelope is versioned for it |
| Native storage | A:…/pgp/EnrollmentVault.kt:150-228 (AndroidKeyStore AES-256-GCM, StrongBox or TEE); M:…/Security/EnrollmentVault.swift:107-131 (HPKE P-256 to a Secure Enclave key); L: user's GnuPG | Hardware-bound | Android: symmetric, fine. Mac vault record: local P-256 HPKE, low HNDL (never leaves the device); move to X-Wing with P2. Linux: see Q6 |
| KyIdentity zero-knowledge keyring | none found in KyPost (`git grep -i -E 'zero.?knowledge\|identity keyring'`) | Not integrated | Out of scope; B covers KyIdentity |

### 3. Publishing

| Item | Location | Today | v6 + PQC |
|---|---|---|---|
| WKD serve | `backend/internal/api/pgp_wkd_publish.go:188-313`; hash `backend/internal/api/pgp_wkd.go:18-39`; domain proof `backend/internal/wkdpublish/verify.go` | Advanced and direct; serves the whole v4 certificate (all User IDs, not minimized), opt-in per user | Serve v4 only by default. GnuPG breaks on v6 in a WKD answer (summit minutes). Concatenated v4+v6 is allowed by draft-koch-openpgp-webkey-service-22 §3.1 and becomes a per-user or later default option (Q3) [P5] |
| WKD fetch (other domains) | `backend/internal/api/pgp_wkd.go:126-210` (1 MiB cap) | `crypto.NewKey(body)` reads one key; a multi-certificate answer gives the first one or an error (unverified which) | Read every certificate in the answer and pick by policy: v6 PQC > v4 with ID 35 > v4 [P0] |
| Autocrypt out | `backend/internal/api/pgp_autocrypt_out.go:17-69`; folding `backend/internal/pgpmail/mime.go:59-75` | Full v4 certificate, no minimization, no size cap, opt-in | Minimize (one User ID, current encryption subkey) and cap at Autocrypt's 10 KiB. A v6 PQC certificate (~13.8 KB binary, R §10) can never fit; Autocrypt keeps the v4 certificate, with its ID 35 subkey if Q4 adds one (~2 KB) [P0] |
| Autocrypt harvest | `backend/internal/processor/autocrypt_harvest.go:195-367` | DKIM-gated, one header, any size | Cap at 10 KiB; accept v4+ID 35. No gossip exists |
| Keyserver | `backend/internal/api/pgp_keyserver.go:20-63` | keys.openpgp.org VKS by-email lookup, user-confirmed, no upload | Whether VKS serves v6/PQC certificates is unverified; keep confirm-only |
| vCard KEY | `backend/internal/api/dav_server.go:586,748-765,883-890` | Armored v4 | Validate through the P0 allowlist; serve v4 |
| QR share | `backend/internal/api/pgp_qr_handlers.go:14-87`; client checks A:…/PgpKeyActivity.kt:414-433, M:…/PgpQrClient.swift:118-139, L:core/net/PgpQrClient.cpp:80-104 | `#fp=` accepted as 40 hex only on every client | Accept 64-hex v6 fingerprints (RFC 9580 §5.5.4.3) [P0] |

### 4. Encrypt, sign, decrypt, verify

| Item | Location | Today | v6 + PQC |
|---|---|---|---|
| Recipient resolution | `backend/internal/api/pgp_resolver.go:125-190`; contacts `backend/internal/contacts/contacts.go:53-73` | Tiers: contact pin → WKD → keyserver (confirm) → none. **One key and one TOFU pin per contact**; a new fingerprint is `key_changed` | A contact holds up to one v4 and one v6 key, each pinned. A correspondent adding v6 is not `key_changed` [P0]. Resolve returns the capability: `pq`, `v6`, `v4` |
| Required vs opportunistic | `docs/E2E_PGP.md:168-187,849-857` | Encryption is required per send; keyless recipients need the secure-link fallback | No change |
| Browser encrypt and sign | `frontend/src/lib/pgpClient.ts:488-493,525-530,577-628` | Per delivery group (To+Cc once, each Bcc alone), signed inside the ciphertext; sign-only is RFC 3156 detached; protected Subject | Split delivery groups by capability as well (Q1): RFC 9580 Table 26 forbids one message mixing a v6 PKESK/SEIPDv2 with a v3 PKESK/SEIPDv1, and RFC 9980 §8.1 says a mixed-recipient message is not PQ-confidential for anyone [P4] |
| Server `send-pgp` relay | `backend/internal/api/pgp_send_client.go:25-33,105-116,514-690` | Checks the MIME shape only; never rewrites | No change |
| Server incoming encryption | `backend/internal/processor/incoming_encryption.go:81-140`; `backend/internal/pgpmail/mime.go:139-200` | `EncryptStoredMIME(raw, u.PGPPublicKey)` with `crypto.PGP()` default: always v3 PKESK + SEIPDv1 | Encrypt to the user's v6 certificate when one is active, with an explicit KyPost profile (below); v4 path byte-identical (golden test) [P1] |
| RSA floor | `backend/internal/pgpmail/keyfloor.go:17-36`; browser `frontend/src/lib/pgpClient.ts:18-20` | RSA ≥ 2048; anything else passes | Folded into the P0 allowlist |
| Decrypt | server never (`backend/internal/api/pgp_receive.go:37-63`); browser `frontend/src/lib/pgpClient.ts:271,639`; A:…/PgpDecryptor.kt:120-178; M:…/GopenPGPCrypto.swift:41-120; L:core/pgp/OpenPgpDecryptor.cpp:277-278 | Every ring member tried; hidden-ID fallback; 25 MiB (browser), 32 MiB (Mac, Linux) | Every client must decrypt v6 PKESK + SEIPDv2 and ID 35 before the server encrypts anything to a v6 key (gate for P1) |
| Signer binding and badges | `backend/internal/api/pgp_receive.go:242-356`; `backend/internal/api/pgp_bootstrap.go:107-122`; `docs/E2E_PGP.md:924-934,1134-1165` | The server ships contact keys bound to their addresses; **parses every contact key on every bootstrap and every message open** | Ship both keys per contact. Fix the hot spot first: with ~18 KB PQC keys, 1,000 contacts is an 18 MB bootstrap and ~1,000 ML-DSA verifications per opened message. Cache parse results by fingerprint and ship only the sender's keys [P0] |
| Native verify | A:…/PgpDecryptor.kt:204-340 (key-ID lookup, first one-pass signature only, SHA-256 fixed); M:…/GopenPGPCrypto.swift:187-192 (`userIDAddresses` returns `[]` for non-v4); L:core/pgp/OpenPgpDecryptor.cpp:138-205 (first signature only) | v4 assumptions | Fingerprint lookup, verify every signature, per-key hash, v6 User ID binding [P3] |

### 5. Web client

| Item | Location | Today | v6 + PQC |
|---|---|---|---|
| OpenPGP library | `frontend/package.json:20`, lock `frontend/package-lock.json:1999-2002` | openpgp.js 6.3.3, lazily imported (`pgpClient.ts:5-7,22-24`) | openpgp.js v7 (Q5). Cross-check its output against go-crypto and Bouncy Castle and the RFC 9980 test vectors |
| Device sealing | `frontend/src/lib/deviceEnrollment.ts:380-397`; `@hpke/core` 1.10.1 | HPKE `HPKE-Base-P256-SHA256-A256GCM` | X-Wing (P2). The browser needs an X-Wing HPKE implementation (`@hpke/hybridkem-x-wing`, or noble post-quantum that openpgp.js v7 uses; neither checked) |
| Probe | `docs/pgp-key-lifecycle-check.cjs` | Exercises the pinned openpgp.js | Extend to v6 and RFC 9980 |

### 6. Native clients

| Client | Library | v6 + PQC |
|---|---|---|
| Android | Bouncy Castle `bcpg-jdk18on` 1.87 (A:gradle/libs.versions.toml). Policy refuses unknown algorithms (A:…/pgp/PgpStrengthPolicy.kt:13-31) | BC 1.87 (tag `r1rv87`, 2026-11-12) has the RFC 9980 tags 30-36 and the composite key classes (R §9). End-to-end encrypt/verify is unverified. Allowlist 30/35, SEIPDv2, fingerprint lookup, all signatures, 64-hex QR, larger caps [P3] |
| Mac and iOS | gopenpgp v3.5.2 xcframework via gomobile (M:Dependencies/gopenpgp.env:8) | Same library as the server, so the KyPost profile applies directly if the gomobile binding exposes it (unverified). Fix the v6 User ID binding and the 40-hex QR check [P3] |
| Linux | gpgme → the user's GnuPG (L:core/CMakeLists.txt:247-257) | GnuPG cannot read v6 certificates or RFC 9980 ML-DSA. **Linux cannot read a v6-encrypted message without a new OpenPGP backend** (Q6). Until then Linux users must not get a v6 certificate, or they lose their own mail on that client [P3] |

### 7. Search and indexing

Encrypted mail is never indexed in plaintext. `mailcache` stores no body for `PGPEncrypted` entries or Sent
(`backend/internal/mailcache/store.go:590-608`); native search scans stored bytes, so ciphertext matches only on
outer headers (`backend/internal/mailbox/client.go:407-440`); IMAP mode uses server SEARCH
(`backend/internal/api/server_inbox.go:1340`). Classification sees plaintext before incoming encryption
(`docs/E2E_PGP.md:369-381`). Clients keep no local index. **No change for v6 or PQC.**

### 8. Backup and restore

- **Key backup:** recovery files `kypost-pgp-recovery-v1`/`-v2` sealed under a recovery secret with the same
  PBKDF2/AES-GCM wrapper; optional server `recovery` slot; restore through `/identity/rewrap`
  (`docs/E2E_PGP.md:718-802`; `docs/PGP_KEY_LIFECYCLE.md:139-175`). Symmetric. A v2 ring with PQC keys needs
  the larger caps below; the recovery file format gains the v2 ring [P3].
- **Server backups:** ky-primitives `capsule` + `recoverykey` seal to the KyRecovery key with **X-Wing HPKE**
  (`docs/STRONG-CRYPTO.md:37`). Already PQ. Contents include the opaque wrapped envelopes.
- **Mail at rest:** incoming-encrypted INBOX, Sent and drafts are OpenPGP to the user's v4 key today. That
  ciphertext is the HNDL exposure (Q9).

### 9. Import, export, revocation, rotation

- One current public identity per account; the ring (16 primaries) is for historical decryption. Conversion,
  retirement and older-backup merge are **designed, not shipped** (`docs/PGP_KEY_LIFECYCLE.md:1-37,139-153,
  253-329`, Tier 5). Replacing the identity clears device slots and forces re-enrollment
  (`backend/internal/api/pgp_client_keys.go:119-132`).
- Adding a v6 certificate is a Tier 5 conversion that keeps the v4 key active. **The PQC rollout depends on
  shipping Tier 5 conversion plus native ring persistence on all three clients** (Q11).
- Linking v4 to v6 publicly is what draft-ietf-openpgp-replacementkey-09 (WG document) is for. Proton is waiting
  on it before exporting v6 keys. KyPost should do the same for outside publication.

### 10. Other public-key crypto in KyPost

B owns these. Listed so this spec's phases do not miss the ones that carry OpenPGP secrets.

| Item | Location | Today | Plan |
|---|---|---|---|
| **Device-enrollment envelope** | `backend/internal/users/pgp_device_delivery.go:32,60-84` (`len(enc) != 65` at `:78`); `docs/DEVICE_ENROLLMENT.md:182-259`; `frontend/src/lib/deviceEnrollment.ts:380-397`; A:…/pgp/HpkeRecipient.kt:9-115; M:…/DeviceEnvelope.swift:22-213; L:core/pgp/DeviceEnrollmentCrypto.cpp:125-200; vectors `testdata/device-envelope-hpke*.json` | HPKE DHKEM(P-256)/HKDF-SHA256/AES-256-GCM, envelope v4/v5, 7-day TTL, carries the private key or ring through the server | **v6 envelope: HPKE KEM 0x647A X-Wing** (same suite as ky-primitives `recoverykey`, Go `crypto/hpke.MLKEM768X25519()`), `enc` 1120 B; enrollment key publish 1216 B (fits the 4 KiB cap at `backend/internal/api/pgp_device_enrollment.go:25-29`); SAS preimage gains a version tag and the length-prefixed X-Wing key [P2] |
| Enrollment SAS | `frontend/src/lib/deviceEnrollment.ts:30-57` and the three clients | SHA-256 over the raw P-256 key, 70 bits | Covered by P2 |
| Relay identity | `backend/internal/api/relay_identity.go:40-83` | Ed25519 TLS-pin statements | B |
| Inbound TLS | `backend/internal/api/tls.go:170-190`; hosted receiving `backend/internal/app/receiving_tls.go:315-450` | Go defaults, so X25519MLKEM768 already negotiated | B (confirm under `fips140=on`) |
| DKIM | `backend/internal/mailmsg/dkim.go:80-119` | RSA-2048 | No PQ DKIM standard; B |
| Web Push, UnifiedPush, FCM, APNs | `backend/internal/config/config.go:802-850`; `worker/src/fcm.ts`; `worker-apns/src/apns.ts` | VAPID ES256, RFC 8291 P-256 | Fixed by the standards; B |
| OIDC | `backend/internal/sso/sso.go:158-162`; `backend/internal/sso/jwks_strength.go:16-57` | RS/ES/PS | Follows KyIdentity; B |

### 11. Tests and fixtures

Every fixture key in every repo is v4. The only v6 fixture is an RSA-2048 v6 key in
`backend/internal/api/contact_key_replay_test.go:48,161-171`. No ML-KEM or ML-DSA fixture exists anywhere.
Shared fixtures that clients consume with SHA-256 provenance: `testdata/pgp-keyring-v1.json`,
`testdata/device-envelope-hpke*.json`, `testdata/hpke-cfrg-p256-aes256.json`, `testdata/mime-corpus.json`.

### 12. Sizes and limits

| Bound | Today | Location | Proposed |
|---|---|---|---|
| Wrapped envelope / identity upload | 128 KiB | `backend/internal/api/pgp_client_keys.go:14-18` | 512 KiB |
| Ring | 16 primaries, 256 fingerprints, 128 KiB sealed | `backend/internal/users/pgp_keyring.go:16-17`; `frontend/src/lib/pgpKeyring.ts:5`; `frontend/src/lib/keyVault.ts:87`; A:…/MemoryBudget.kt:82-91; M:…/PgpKeyring.swift:19-21; L:core/pgp/OpenPgpKeyImporter.cpp:170 | 512 KiB sealed, same counts, raised in lockstep on server and all clients |
| Recovery file | 512 KiB | `docs/E2E_PGP.md:729-732` | 2 MiB |
| Device envelope `enc` | exactly 65 B | `backend/internal/users/pgp_device_delivery.go:78` and clients | 65 (v4/v5) or 1120 (v6) by version |
| Autocrypt header | no cap | `backend/internal/pgpmail/mime.go:59-75`; `backend/internal/pgpautocrypt/autocrypt.go:21-69` | 10 KiB both ways (Autocrypt 1.1) |

PQC sizes (RFC 9980 tables, FIPS 203/204; certificate totals are arithmetic estimates, R §10):

| Object | Classical | PQC |
|---|---|---|
| Certificate, one User ID | v4 Ed25519/cv25519 ~0.5 KB | v6 ML-DSA-65+Ed25519 / ML-KEM-768+X25519 ~13.8 KB binary, ~18.5 KB armored; each extra User ID ~+3.5 KB |
| v4 certificate + ID 35 subkey | — | ~2 KB |
| PKESK per recipient | ~100 B | ~1.2 KB |
| Signature | ~0.15 KB | ~3.45 KB |
| Ring of 16 PQC keys (private + certificate) | — | ~300 KB |

Timings: Go ML-KEM-768 encapsulation ~35 µs, decapsulation ~35 µs (Apple M2, golang/go commit 592775ec7d);
ML-DSA-44 sign ~245-390 µs. No primary-source ML-DSA-65 numbers; P0 measures them in CI. Per-message cost is
negligible; the size growth matters for bootstrap, the ring and `users.json` (rewritten whole under a global
lock on every mutation, `backend/internal/users/users.go:1061-1075`).

## Design

### The two certificates

- **v4 certificate (existing, public).** Unchanged fingerprint. Stays the account's published identity on WKD,
  Autocrypt, keyservers, vCard and QR. Optionally gains an ID 35 ML-KEM-768+X25519 encryption subkey next to
  its cv25519 subkey (Q4), which RFC 9980 §3.5 allows and which is the only way an outside v4 sender can
  encrypt to us post-quantum. Signatures from it stay classical: ML-DSA is v6-only.
- **v6 certificate (new, KyPost-internal at first).** ML-DSA-65+Ed25519 primary, ML-KEM-768+X25519 subkey,
  one User ID per account address, preferences AES-256 with OCB then GCM (RFC 9980 §7.1). Used for incoming
  encryption, Sent, drafts, and mail to any KyPost user on the same install. Published outside only per Q3.

### The KyPost profile

A custom gopenpgp profile in `backend/internal/pgpmail`, replacing every `crypto.PGP()` call:

- v6 recipients: v6 PKESK + SEIPDv2, AES-256, AEAD **OCB** (RFC 9580's mandatory mode; gopenpgp's `PQC()`
  picks GCM, which only works for recipients that advertise it), chunk size default.
- v4 recipients: today's behaviour, byte-identical (v3 PKESK + SEIPDv1, AES-256). A golden test pins it.
- A v4 recipient with an ID 35 subkey: v3 PKESK to the ID 35 subkey with AES-256, SEIPDv1 (RFC 9980 §4.3.1
  allows this); SEIPDv2 only if the certificate advertises the feature.
- Signing: the certificate that matches the delivery group (v6 → ML-DSA-65+Ed25519; v4 → Ed25519).

### Mixed recipients

One delivery group never mixes certificate classes. The browser and native senders already encrypt per
delivery group (To+Cc together, each Bcc alone); P4 splits To+Cc further into a PQ group and a classical group.
Each group gets the same visible headers. A PQ recipient never shares a message body with a classical PKESK,
so its copy stays PQ-confidential (RFC 9980 §8.1). The compose screen shows which recipients are "not
quantum-safe" before send (Q1).

### Generating and rolling out the v6 certificate

The rollout is a Tier 5 conversion (`docs/PGP_KEY_LIFECYCLE.md`) that keeps the v4 key active:

1. The browser generates the v6 certificate (openpgp.js v7) on the next unlock, adds it to a v2 ring with both
   keys active, re-seals the password and recovery envelopes, and uploads the public certificate.
2. The server validates it through the P0 allowlist and stores it beside the v4 key.
3. Each enrolled device gets a new envelope (v6 X-Wing) with the v2 ring. Devices that cannot read v6 are
   held back by capability; see the Linux gate.
4. Only when every enrolled device of the account reports v6 capability does the server switch incoming
   encryption, Sent and drafts to the v6 certificate. Until then they stay v4, so no device loses mail.

### Linux gate

A Linux client on GnuPG cannot decrypt v6. Devices already publish the envelope versions they read
(`envelopeVersions`, `backend/internal/api/pgp_device_enrollment.go:53`, tested in
`backend/internal/api/pgp_device_capabilities_test.go`). P3 adds an OpenPGP capability beside it (`openpgp:
["v4","v6","rfc9980"]`). The switch in step 4 waits for every device, so an account with a Linux device stays
v4 until the Linux backend (Q6) ships. The browser has no enrolled-device record; it reads v6 once P4 ships,
and P4 is the last step before any switch.

### Existing stored mail (P6, owner Q9)

The server never holds the private key, so re-encryption runs in the user's own client, after the step-4 switch:

1. The client lists stored messages whose PKESKs name only the v4 key. The server can read PKESK recipient IDs
   without decrypting anything. This covers incoming-encrypted INBOX, Sent, drafts, and mail outsiders encrypted
   to the published v4 certificate.
2. For each message the client decrypts with the v4 ring member, re-encrypts the same plaintext (the full MIME,
   so any inner signature survives) to the account's v6 certificate, and uploads it with compare-and-swap on the
   old ciphertext's SHA-256.
3. The server accepts the replacement only if its PKESKs name only the account's current v6 encryption subkey.
   It then replaces the ciphertext atomically, keeping the message ID, flags and folder.
4. The job is resumable, throttled, and runs on any unlocked client. Mail that keeps arriving for the v4 key (from
   outside senders) is picked up on the next unlock.

Limits:

- Backups taken before re-encryption still hold the v4 ciphertext until they expire. They are sealed to the
  X-Wing KyRecovery key, so a recorded backup alone is not exposed.
- Plaintext mail on accounts without incoming encryption is out of scope; that is the incoming-encryption setting,
  not this spec.
- The v4 key stays in the ring for as long as outside correspondents use it.

## Phases

Each phase is one PR or two, at most two stacked. P0-P2 change no user's keys and can merge on the
usual gate. P3-P5 change what users' keys and mail can do, so each needs the owner's merge approval.

| Phase | Scope | Depends on |
|---|---|---|
| **P0 groundwork** | Server algorithm/version allowlist; multi-certificate WKD fetch; contacts hold v4 + v6 keys and pins; signer-binding parse cache, sender-only `pgp-payload` keys; Autocrypt minimization and 10 KiB cap both ways; 64-hex QR on all clients; shared v6 Ed25519 and RFC 9980 fixtures in `testdata/` (keys, ring, both message rows, signatures), made with go-crypto and cross-checked with BC 1.87; golden test for v4 incoming encryption; ML-DSA-65 timings in CI; fix the doc drift (SAS length in `docs/DEVICE_ENROLLMENT.md:101-117`, advertised versions at `:220-221`, the `GOTOOLCHAIN=go1.26.9` pin at `docs/PGP_KEY_LIFECYCLE.md:247`) | Nothing |
| **P1 server profile** | The KyPost profile; `EncryptStoredMIME` encrypts to the v6 certificate when the account's switch is on (nothing can turn it on before P4, because no account has a v6 certificate until then) | P0 |
| **P2 PQ device envelope** | Envelope v6 with X-Wing HPKE on server, browser and three clients; SAS v2; X-Wing device keys (Q8); v4/v5 stay accepted until every client ships v6, then the server stops issuing them | P0; ky-primitives HPKE helper from B |
| **P3 clients read v6** | Android (BC 1.87), Mac/iOS (gopenpgp profile), Linux backend (gopenpgp c-archive, Q6), **browser (pinned openpgp.js v7 commit, gopenpgp WASM fallback, Q5)**: decrypt v6 PKESK + SEIPDv2 and ID 35, verify ML-DSA composites, v2 rings, larger caps, device capability report | P0 |
| **P4 browser keys** | v6 generation; v2 ring; Tier 5 conversion keeping v4 active; capability-split delivery groups; "not quantum-safe" marking | P1-P3 |
| **P6 archive to v6** | Re-encrypt every stored message still addressed to the v4 key to the v6 certificate (Q9) | P4 |
| **P5 outside world** | Q4's ID 35 subkey on v4 certificates; Q3's v6 publication on WKD; Autocrypt v2 when it is more than an individual draft; key-replacement links (draft-ietf-openpgp-replacementkey) | An interop matrix run (below); P4 |

**Interop matrix** (P5 gate, scripted, re-runnable): GnuPG 2.4 and 2.5 (default and `--allow-9980`),
Thunderbird current release, Proton (import), Sequoia `sq` 1.5, OpenPGP.js 6.3 and v7, Mailvelope, OpenKeychain,
GPGTools 2023.3, Delta Chat. For each: import our v4+ID 35 certificate, import a v4+v6 WKD answer, read a
v3-PKESK/ID 35 message, read a v6 message, verify a v6 signature. A tool that errors instead of ignoring
blocks that publication default.

### Security properties to prove (each with a test that fails without the code)

- P0: the allowlist refuses a pre-RFC 105/107 key, a v6 key with a v4 subkey, and RSA < 2048.
- P0: a contact adding a v6 key keeps its v4 pin and does not raise `key_changed`; a changed v4 fingerprint
  still does.
- P1: a message to a v6 recipient has only v6 PKESKs and a SEIPDv2 packet; a v4 recipient's output is
  byte-identical to today's (golden).
- P2: the server refuses a v6 envelope whose `enc` is not 1120 bytes; a v5 envelope still opens; the SAS
  changes when one byte of the X-Wing key changes.
- P4: no delivery group carries both a v6 PKESK and a v3 PKESK.
- Step 4 switch: incoming encryption stays v4 while any enrolled device lacks v6 capability.
- P6: the server refuses a replacement whose PKESK names any key but the account's current v6 encryption
  subkey, and one whose compare-and-swap hash does not match the stored ciphertext.
- P3: the browser decrypts the shared RFC 9980 fixtures with the pinned openpgp.js v7 and with the WASM fallback,
  and both agree with go-crypto and Bouncy Castle.

## Owner questions

Each has a recommendation. The first is the big one.

1. **A recipient cannot read v6 or PQC. What do we send?**
   (a) Per recipient, the best certificate they publish: PQ to PQ-capable recipients, v4 to the rest, split into
   separate delivery groups, with a "not quantum-safe" mark on those recipients in compose and in Sent. No
   refusal. (b) Refuse to encrypt to classical-only recipients (PQ-only), which cuts off almost every outside
   OpenPGP user today. (c) One message to everyone with mixed PKESKs, which RFC 9580 forbids across v6/v4 and
   which leaves nobody PQ-confidential. (d) As (a), plus an org admin setting "require PQ" that turns the mark
   into a refusal.
   **Recommend (a) now, (d) as a later admin option.**
2. **Certificate model.** (a) Keep the v4 certificate and add a v6 PQC certificate (two certificates). (b) Replace
   the v4 key with a v6 PQC key, which breaks every outside correspondent's pin and their ability to read our
   signatures. (c) v4 only, adding an ID 35 subkey, which gives PQ confidentiality but never PQ signatures.
   **Recommend (a).**
3. **Publishing the v6 certificate outside KyPost** (WKD, vCard, QR; Autocrypt cannot carry it).
   (a) Internal only at first: the v6 certificate is used between KyPost users on the same install and to self;
   outside, the v4 certificate only. (b) Serve v4+v6 concatenated on WKD now, which GnuPG users then fail to
   import. (c) Per-user opt-in to publish v6 on WKD.
   **Recommend (a), then (c) in P5 after the interop matrix, then default-on once GnuPG and Thunderbird
   handle it.**
4. **Add an ML-KEM-768+X25519 (ID 35) subkey to existing v4 certificates?** It lets PQ-aware outside senders
   (Thunderbird's planned v4 PQC, GnuPG `--allow-9980`) encrypt to us post-quantum, and fits Autocrypt (~2 KB).
   The risk: per the summit minutes, older Thunderbird and go-crypto failed on unknown subkey algorithms.
   **Recommend yes, in P5, default-on only if the interop matrix shows no tool that errors; otherwise
   per-user opt-in.**
5. **Browser library.** (a) Build P0-P3 now and wait for an openpgp.js v7 release for P4. (b) Pin an
   openpgp.js v7 pre-release commit from `main` now. (c) Compile gopenpgp to WebAssembly so the browser runs
   the same library as the server and the Mac (a bundle of several MB, and the browser would no longer share
   openpgp.js's interop testing).
   **Recommend (a), and track v7. If v7 has no release by the time P3 merges, revisit (b) with the
   cross-implementation test suite as the gate.**
6. **Linux backend.** GnuPG cannot read v6. (a) Replace gpgme with gopenpgp built as a Go c-archive, the same
   library and profile as the server and the Mac. (b) Sequoia (Rust, RFC 9980 since 2.4.0) through its C or
   Rust bindings. (c) Keep GnuPG; Linux users stay on v4 for their own mail, and accounts with a Linux device
   never switch.
   **Recommend (a).** One OpenPGP implementation across server, Mac/iOS and Linux, which the shared fixtures
   then test once.
7. **Security level.** (a) ML-KEM-768 / ML-DSA-65 (RFC 9980's MUST, NIST category 3). (b) ML-KEM-1024+X448 /
   ML-DSA-87+Ed448 (SHOULD; CNSA 2.0 asks for level 5), bigger and less widely implemented. openpgp.js v7 has
   them only in an open PR (#2077).
   **Recommend (a) for everyone. Offer (b) as an Enterprise option later, if a customer needs CNSA 2.0.**
8. **Device keys without hardware PQ.** AndroidKeyStore has no ML-KEM (unverified for current KeyMint).
   Apple documents `SecureEnclave.MLKEM768` and `XWingMLKEM768X25519` in CryptoKit (pages exist; availability
   not read, believed iOS/macOS 26; unverified). Android's rule is "non-extractable keys only"
   (A:app/src/main/AGENTS.md).
   **Recommend: Android keeps an X-Wing private key that exists only encrypted under the existing StrongBox/TEE
   AES-256-GCM key with per-use user authentication, so it is never usable without the hardware. Apple uses
   a Secure Enclave ML-KEM-768 key where the OS offers one; otherwise a CryptoKit X-Wing key in the Keychain with
   `ThisDeviceOnly` and user-presence access control. The Secure Enclave has no AES keys and cannot wrap an
   arbitrary key, so "Secure Enclave-wrapped X-Wing", as first written, is not possible. Corrected after the
   owner's approval; the intent (hardware-bound where the platform allows) is unchanged. Linux uses its existing
   secret store.**
9. **Mail already encrypted to the v4 key.** (a) Leave it classical and say so in the security docs. (b) Later,
   offer an opt-in "re-encrypt my mailbox" job that runs in the user's own client, at real cost in time and
   battery.
   **Recommend (a) now, (b) as a later phase if users ask for it.**
10. **Signatures to outside recipients.** (a) Sign with the certificate that matches the delivery: ML-DSA
    composite for v6 recipients, Ed25519 v4 otherwise. Sign-only mail to the world gets the v4 signature only.
    (b) Dual-sign everything with both keys, which per the summit minutes can choke Thunderbird.
    **Recommend (a).**
11. **Dependency on Tier 5.** The rollout needs Tier 5 key conversion and native ring persistence on all three
    clients (`docs/PGP_KEY_LIFECYCLE.md`), which are designed, not shipped. **Recommend building the Tier 5
    conversion as part of P3/P4 rather than as a separate project first.**
12. **Merge approval.** P0-P2 change no user's keys or mail. **Recommend: P0-P2 merge on the usual gate
    (CI, security reviewer, Guide), and P3-P5 each wait for your explicit approval.**
