# End-to-end PGP: design, status, and the mobile plan

## Why this exists

KyPost advertised "PGP end-to-end mail encryption" while storing every user's
private key on the server, unlocked, sealed with a master key sitting on the
same volume. Anyone with the disk, a backup, a container shell, or a
path-traversal bug could decrypt every message every user had ever received,
retroactively. That is server-side encryption, not end-to-end, and the gap
between the claim and the design is the thing this work closes.

## The model

Two protection modes, recorded per user in `users.json` as
`pgpKeyProtection`:

| | `server` (legacy) | `client` (end-to-end) |
|---|---|---|
| Private key stored as | `pgpPrivateKeyEnc` — AES-GCM under `SECRET_DIR/pgp-private-key.key` | `pgpPrivateKeyWrapped` — AES-GCM under a key derived from the user's password |
| Server can decrypt mail | Yes | No |
| Decryption happens | Server (`api/pgp_receive.go`) | Browser (`lib/pgpClient.ts`) |
| Signing happens | Server (`handleMailSend`) | Browser, delivered via `POST /api/mail/send-pgp` |
| Send-as User ID reconcile | Daemon, automatically | Browser, when the vault is unlocked |

`users.User.PGPProtection()` is the single source of truth; every server-side
PGP path calls `HasServerReadableKey()` and refuses rather than assuming.

### Key wrapping

`frontend/src/lib/keyVault.ts`:

- PBKDF2-HMAC-SHA256, 600,000 iterations (OWASP figure for this
  construction), 16-byte random salt.
- AES-256-GCM, 12-byte random IV.
- Envelope records `kdf` and `iterations`, so a later move to Argon2id is a
  format addition, not a migration — unwrap dispatches on what the stored
  envelope says.

Argon2id would resist GPU attack better and is the eventual target. It is not
in WebCrypto, so today it would mean a WASM bundle on the critical path of
every login. The envelope is versioned specifically so that trade can be
revisited without touching stored data.

### Why the server can't cheat

The server holds an Argon2id hash of the password (scrypt for accounts not yet
rehashed), never the password. It therefore cannot derive the wrapping key,
and there is no code path that tries. `cryptutil.OpenString` uses `LoadKey`, not `LoadOrCreateKey`, so a
missing master key is a loud error rather than a silently-minted new one.

### Decrypted mail is never cached

`mailcache.json` is plain JSON on disk. In `server` mode the daemon's warm path
would otherwise write decrypted PGP bodies straight into it, so the sealed
private key sat next to a plaintext copy of everything it had ever opened — a
larger disclosure than the key itself.

`mailcache.Store.Upsert` now drops the body of any entry flagged
`PGPEncrypted`, keeping the flags so clients still render the badge. This is
enforced in the store rather than at the `internal/api` call sites, so a future
caller cannot bypass it. Correctness is unaffected: an empty `Body` has always
meant "not warmed, fetch if needed" rather than "empty message", and every read
path already falls back to a live fetch.

Until 2026-09-14 this had a cost: `Store.Snapshot` counted a window as warm
only when *every* entry had a body, so one encrypted message made the whole
mailbox read cold and every load took the live `ListUnreadMessages` path. The
entry now carries `pgpBodyOmitted`, set only by a classifying (API) write that
found the message encrypted with no decrypt error, and `Snapshot` counts such
an entry as warm. Serving it from cache emits `pgpEncrypted: true` with no
body and no `pgpDecryptError`, which is exactly what the live path emits for
the same message: the server decrypts nothing under any mode, and a
server-custody account's migration error is a decrypt error, so it never sets
the flag and stays on the live path. The flag is cleared with the verdict by
every invalidation (rules version, contact key generation), so a recompute
still forces the message back through the live path. A poller write never
classifies and never sets it; a failed decrypt is transient and never sets it.

### Session handling

The unwrapped key lives in module memory for the life of the page and is
never written to `localStorage` or `sessionStorage` — both survive a tab
close and are readable by any script that achieves XSS, which is exactly what
this protects against. A reload means re-entering the password. There is a
test asserting nothing lands in either store.

## What this costs, honestly

- **Admin password reset can strand the password envelope.** Recovery needs
  a recovery copy and its separate secret; an admin cannot reconstruct either.
  The untimed `recovery` slot survives a password reset, but identity deletion
  or replacement removes it. Keep a downloaded copy for server loss too.
- **Password change requires the browser.** It opens the old password envelope
  before submitting the new credential and rewrapped key together through
  `POST /api/auth/password`. Both commit atomically. The separate `/rewrap`
  route repairs an envelope using an old password or a recovery copy.
- **The pickup-link fallback is weaker than PGP.** See "Sealed pickup links"
  below for exactly how much weaker.
- **Subject headers are still cleartext** outside the encrypted part, as with
  any PGP/MIME mail. The real subject travels *inside* the ciphertext as a
  protected header (`buildProtectedContent` / `pgpmail.protectContent`); what a
  passing MTA sees is the placeholder.
- **Recipient addresses are cleartext.** SMTP needs them to route, they are in
  the send request as the envelope, and the Sent-folder listing is unusable
  without them. Nothing can be done about this within PGP.
- **The Sent copy is encrypted to yourself, so the server cannot read it —
  and neither can anyone without your key.** If you lose your key you lose your
  outbox along with your inbox. Until run-4 this copy was uploaded as
  *plaintext*, which meant the server received the cleartext and real subject of
  every message on a mode whose whole claim is the opposite; see M5. A client
  that has not been reloaded since that change will have its Sent copy refused
  rather than stored — the message still sends, and the warning says to reload.


## Sealed pickup links

A recipient with no PGP key has nothing to encrypt to, so KyPost falls back to
emailing them a one-time link. Originally the server stored that message's
plaintext (sealed with its own key, which it holds) for seven days — the exact
property client-side protection exists to remove.

**Behavior change (2026-07-25):** `/api/mail/send` no longer falls back to a
pickup link on its own. An encrypted send to a recipient with no usable key
refuses with 409 unless the request sets `allowPickupFallback: true`. Existing
callers that relied on the silent fallback will start seeing refusals — that
is the point: the fallback stores plaintext server-side for seven days. The
refusal happens before any SMTP delivery, so a client can safely re-send with
the flag set once the user confirms — see "What mobile apps must implement"
below for the exact shape of that 409. This gate applies to the server-side
encrypt path (`server`-protected senders, on web or mobile); a client-protected
sender's browser already sealed its own pickup links before this change and
is unaffected.

For client-protected accounts the message is now sealed in the sender's
browser instead:

1. The browser picks a random AES-256-GCM key and encrypts subject and body.
2. It uploads **only ciphertext** to `POST /api/pgp/pickup`.
3. The link is `…/pickup/<id>?t=<token>#<key>`. Browsers never transmit a URL
   fragment, so the key does not reach the server on the fetch.
4. The recipient's page (`/pickup-decrypt.js`) reads `location.hash` and
   decrypts locally, then strips the key from the address bar.

`?t=` still authenticates the fetch, as before. The key is separate and rides
only in the fragment.

### What this actually protects against

**Protects:** the server's disk, its backups, snapshots, a compromise after
the fact, and an operator reading files. The stored blob is ciphertext and the
key was never written anywhere.

**Does not protect:**

- **Anyone who can read the recipient's mailbox has the key**, because the key
  is in the email. Their provider, an attacker with mailbox access, an
  unencrypted hop. This defends against *your* server, not their mail path.
- **The server sees the key once, in flight**, when it relays the notification
  email over SMTP. Unavoidable while it holds the SMTP credentials. So a
  server compromised *at the moment of sending* still sees it; one compromised
  later does not.

Net: a seven-day plaintext-at-rest exposure becomes a momentary in-flight one.
That is a real improvement and it is not the same guarantee the PGP path
gives. The compose checkbox is off by default and says so.

### Operational hazard: link rewriting

Corporate mail security products (Outlook Safe Links, Proofpoint, Mimecast)
rewrite inbound URLs. HTTP redirects preserve fragments, but a rewriter that
*constructs* a new URL can drop everything after `#` — and then the message is
permanently unreadable. This is the most likely real-world failure, and it is
why the page checks for an absent fragment first and names that cause
specifically rather than reporting a generic decryption failure.

### Other notes

- The subject is inside the sealed blob, not stored alongside it. A subject in
  the clear gives away most of what the encryption was for.
- The page renders the decrypted body as **text**, never as HTML: it has no
  sanitizer available, and the content is written by the sender.
- Consumption happens on the blob fetch, not the page load, so a link-preview
  bot fetching the HTML does not burn the message.
- Server-protected accounts keep the original server-sealed path. The server
  can already read their mailbox, so client-sealing there adds machinery
  without changing what it can see. That page renders as text too, for the
  same reason — it stores the composed body's mode and flattens an HTML body
  before escaping it, rather than escaping the markup and showing the
  recipient the tags.

## Status

### Corrections to an earlier version of this document

The first version of this file claimed "Read path passes ciphertext through
untouched for client-protected accounts." That was true of the Go struct and
false end to end. `decryptPGPMessageContent` does leave
`PGPEncryptedPayload` populated, but `inboxEmail` has no field for it and
`mailcache.Entry` does not persist it, so the ciphertext was dropped at JSON
serialization and never reached any client. No client-protected account
could read encrypted mail, on web or anywhere else. Fixed by
`GET /api/mail/pgp-payload`.

It also listed the key endpoints as implemented without noting they were all
`withAuth` (session cookie only), which locked out every paired mobile
device — the clients this mode was designed for. All but `export-legacy` are
now `withMailAuth`.

Both defects came from verifying at the Go boundary and never at the HTTP
boundary. There are now HTTP-level tests
(`api/pgp_client_e2e_test.go`) for exactly that gap.

Implemented:

- Storage model, `PGPProtection()`, and refusal of every server-side PGP
  operation on a client-protected key (`users`, `api/pgp_receive.go`,
  `handleMailSend`, `processor/sendas_check.go`).
- Endpoints, all `withMailAuth` (device or session) unless noted:
  `GET /api/pgp/bootstrap`, `GET /api/pgp/identity/wrapped`,
  `POST /api/pgp/identity/client`, `POST /api/pgp/identity/rewrap`,
  `POST /api/mail/send-pgp`, `GET /api/mail/pgp-payload`.
  `POST /api/pgp/identity/export-legacy` is **session-only** on purpose: it
  is the one endpoint that returns a private key, and it re-verifies the
  account password, which a device secret is not. It has two callers in the
  web UI, both in `SecurityPage.tsx` and both one-shot: the migration to
  client custody, and the recovery backup a server-custody account takes
  before that migration makes the key unrecoverable. Neither writes the
  armored key anywhere — migration rewraps it under the account password and
  uploads the envelope, the backup wraps it under a one-time recovery secret
  and hands the file to the browser. There is no bare `.asc` download and
  should not be one.
- `PUT|GET|DELETE /api/pgp/identity/envelope/{slot}` write, read and delete one
  non-password sealing of the private key — a recovery code, or an enrolled
  device under the `device:` slot prefix. All three are **session-only**, unlike the
  `withMailAuth` routes above: a paired device authenticating with its own
  device secret must not be able to mint a sealing of the account's private
  key, which is the enforcement point a planned passphrase-only account tier
  depends on. `PUT` and `DELETE` additionally require the step-up credential
  (`pgp_stepup.go`, same standard as `rewrap` and `DELETE /api/pgp/identity`):
  installing a slot plants an envelope the server cannot validate, and
  deleting one destroys a sealing that cannot be re-minted without the
  unwrapped key, so neither is undoable from a session alone. `PUT` also
  refuses the `password` slot (400) — that field has exactly one writer,
  `POST /api/pgp/identity/rewrap`, because that route carries the guard
  against a server-custody account losing its only readable key. `GET` is
  **not** step-up-gated and serves whatever `WrappedEnvelopes()` returns for
  the requested slot name, including the synthesised `password` slot — it is
  not a disclosure, since `GET /api/pgp/identity/wrapped` already serves the
  same bytes under this same weaker auth. `GET` 404s only when no envelope
  exists under the requested slot name; `DELETE` of an absent slot succeeds
  (`users.Store.DeletePGPWrappedEnvelope` is deliberately idempotent).
- Slot `GET` also returns `fingerprint` and `publicKey` from the same user
  snapshot as `envelope`. Slot `PUT` and `/identity/rewrap` accept optional
  `expectedFingerprint`: a mismatch at the locked users-store mutation returns
  409 without changing key material. Omission preserves older clients; new
  recovery writes always supply the fingerprint derived from the private key.

- `POST /api/pgp/device/enrollment-key` and `GET /api/pgp/device/envelope` are
  the two **device-authenticated** routes of the enrollment ceremony
  (`pgp_device_enrollment.go`). They are neither `withAuth` nor `withMailAuth`:
  `withMailAuth` would admit a session, which has no device to scope to, and
  `withAuth` would exclude the device entirely. Both resolve the caller through
  `deviceAuthFromRequest`, which returns the **verified** device record, so
  neither reads an identity out of the request.
  - `POST .../enrollment-key` publishes the device's EC P-256 enrollment public
    key. A device may publish its own because a public key is not a
    capability — it only lets a browser seal **to** that device. The device id
    is taken from the credential and never from the body: a device that could
    name another device's id could overwrite the key a browser is about to seal
    to, which is precisely the substitution the verification code exists to
    catch.
  - `GET .../envelope` serves the one envelope sealed for the calling device and
    takes **no slot parameter** — the slot name is built from the verified
    device record, so there is no input to abuse. This is safe where the general
    `GET .../envelope/{slot}` would not be, because the payload is sealed to a
    key whose private half is non-extractable from that device's secure element.
    An expired transport copy reads as 404, since the handler iterates
    `WrappedEnvelopes()`, which filters on the `device:` slot TTL
    (`users.DeviceEnvelopeTTL`, 7 days).
  - Minting and destroying a sealing stay session-only. A device may publish a
    key and read what was sealed for it; only a session may seal.
- `POST /api/notifications/native/register` carries an optional
  `encryptionEnrolled` bool: the device's own answer to "can I still open my
  enrollment envelope". It is a pointer server-side because absent and `false`
  differ — an older client that omits it has no opinion and must not have the
  marker cleared, while `false` reports that the keystore key is gone. The
  marker is device-reported rather than a record of what the browser did,
  because those diverge: reinstalling the app destroys the keystore key.
- The server derives fingerprint and key ID from the uploaded public key
  rather than trusting the client's claim — otherwise a client could get its
  own key published under someone else's identity through WKD or Autocrypt.
- Browser crypto: `lib/keyVault.ts` (wrap/unwrap/lock, 12 tests) and
  `lib/pgpClient.ts` (generate, import, decrypt, encrypt, RFC 3156 wrapping
  with a full RFC 5322 envelope and protected Subject).
- Client-protected accounts fetch ciphertext per message from
  `/api/mail/pgp-payload` and decrypt locally.

### Browser recovery

Security → Encryption creates `kypost-pgp-recovery-v1` files with a random
128-bit secret. Creation restores the serialized file in memory and compares
its plaintext before offering either copy. Under client custody, the browser
validates the unlocked key against the current identity, requests the file
download, then shows the secret. Only **I saved the secret — store server copy**
can PUT the v2 wrapped envelope to `recovery` with account step-up. Merely
displaying the secret never starts a remote write: navigation can discard page
memory, so acknowledgement must precede replacement. No private key or recovery secret enters that request. Creating a
new copy replaces the server slot and its secret; old downloaded files still
open with their own secrets. The new file and secret stay in page memory,
including across Security tab switches, when storage fails or the response is
lost; **Download file again** remains available until **I saved the file and secret** dismisses it. Download
completion cannot be observed, so the UI asks the user to check the file.

**Use server recovery copy** fetches the sealed bytes without a file or an
unlocked vault. Both file and server restore open locally, parse the private
key and match its fingerprint against the current client-protected identity,
then rewrap under the current account password with an atomic identity guard.
This repairs the current identity; it does not restore deleted identities or
provide account sign-in recovery. Identity deletion always warns that the
server copy is deleted too. Password change warns when no server copy is
confirmed; absence of a slot says nothing about an offline file.

**Run recovery drill** opens the selected copy and checks the actual key's
fingerprint without unlocking the vault or changing any server data. Only
an ISO date and a SHA-256 hash of that envelope are stored in localStorage,
scoped to the user and identity. The date is labelled **in this browser** and
shown only for matching envelope bytes; a replacement cannot inherit a pass.
Storage failure is reported separately from a successful drill. Server-held
legacy keys still use export-legacy to create the same tested offline file;
they cannot have a client recovery slot until migration.

### Cold start

A client cannot keep the unwrapped private key across restarts — the web
vault holds it in page memory only, and the mobile apps are told not to put
it in the Keystore/Keychain, since anything recoverable without the password
defeats the model. Every launch is therefore a full reload.

`GET /api/pgp/bootstrap` is the single call that makes a client operational
from nothing:

| field | meaning |
|---|---|
| `hasIdentity` | whether this account has a key at all |
| `protection` | `client`, `server`, or `""` |
| `wrappedPrivateKey` | the self-describing envelope to unwrap (client mode) |
| `unlockRequired` | prompt for the password before reading mail |
| `canDecryptServerSide` | true only for legacy accounts |
| `migrationAvailable` | offer the one-time migration |
| `publicKey`, `fingerprint`, `keyId` | the identity itself |
| `signerKeys` | contact public keys, each with the addresses the address book binds it to, so signatures verify without waiting on a contacts sync |
| `payloadEndpoint` | where to fetch ciphertext; absent on older servers |
| `envelopeSlots` | names of every sealing that exists for this identity (e.g. `password`, `recovery`, `device:*`) — always an array, never `null` |

Doing this as separate calls gives four chances to render a
half-initialized UI — showing "no PGP identity" to someone who has one, or
treating mail as unreadable because the wrapped-key call was the one that
failed. The envelope carries its own `kdf`/`iterations`/`salt`/`iv`, so
clients must derive from the blob rather than hardcoding parameters; that is
what lets the KDF change later without stranding them.

Web UI (wired):

- **Cold start** in `App.tsx`: every authenticated page load fetches
  `/api/pgp/bootstrap` into `lib/pgpSession`. Nothing unlocks at login — the
  prompt appears the first time something needs the key, so a user who never
  opens encrypted mail is never asked. Logout clears the vault.
- **Security page**: browser-side generate and import, protection-mode
  status, unlock/lock, and the one-time migration for legacy keys. Both
  creation paths warn that an admin password reset destroys the key.
- **Read page**: fetches ciphertext from `/api/mail/pgp-payload` and decrypts
  locally; the signature verdict comes from that decrypt, not the server.
- **Compose**: resolves recipient keys via
  `/api/pgp/recipients/resolve`, encrypts per delivery group (BCC each in its
  own), posts to `/api/mail/send-pgp`. Refuses when a recipient has no usable
  key **unless** the "secure link if no key" checkbox is ticked, in which case
  it downgrades that recipient to a one-time link sealed in the browser — the
  server only ever stores ciphertext for this mode, unlike the `server`-custody
  fallback described below, which stores plaintext. The checkbox is off by
  default, so the refusal is what a client-protected sender gets unless they
  explicitly choose the weaker path.
- **Password change**: rewraps the key, unwrapping before the password write
  so a failure leaves nothing half-applied.

Still open:

- Browser-side send-as User ID reconcile. The daemon skips client-protected
  keys (adding a User ID re-signs the key and needs the private half), so an
  alias verified after key creation is not yet added to the key. Until that
  lands, regenerate the key after verifying a new alias if you need WKD or
  Autocrypt to serve it for that address.

Interoperability, as of 2026-09-13: a live mailbox of encrypted mail was
exercised against Proton Mail in both directions (KyPost to Proton, Proton
to KyPost) and worked. Not yet recorded for that run: attachments, sign-only,
protected Subject, Bcc, reply and forward, revoked or expired keys, and large
messages. A per-feature matrix is still owed before calling this a full
interoperability pass.

Sign without Encrypt, as of 2026-09-14: the browser builds an RFC 3156
`multipart/signed` message (`buildSignedDelivery`). The signed part is the
same protected-headers entity an encrypted send carries, with the body and
attachments base64 and any non-ASCII header as an RFC 2047 word so the part
is 7-bit safe; the detached signature is a binary signature over the part's
exact bytes, which is what `verifySignedMessage` and GnuPG both hash. One
delivery goes to every recipient (Bcc as envelope recipients only) and needs
no recipient keys, so nothing is resolved or refused for a missing key. The
real Subject is on the outside: nothing here is secret, only attributable.
`/api/mail/send-pgp` accepts `multipart/signed` for delivery only, checked by
the same two-part extractor the read side trusts (`pgpmail.ExtractSignedParts`);
drafts and the Sent copy keep the ciphertext-only check, and the Sent copy of a
signed-only message is encrypted to the sender's own key like every other
client-custody Sent copy. The four combinations: neither goes through
`/api/mail/send`; sign only is this path; encrypt only and both go through
`buildEncryptedDeliveries`, where signing is inline inside the ciphertext.

Signing defaults on for a new compose, reply, forward, or reopened draft when
the browser key is unlocked, and turns on if the key unlocks while composing.
An explicit Sign choice lasts for that message; locking keeps signing requested
so sending asks for unlock instead of silently sending unsigned. With Sign off,
compose shows `Unsigned`. After successful local decryption, a message without
a signature shows `encrypted but unsigned`. Locked, pending, or failed decrypts
do not establish signature absence. Encryption and sender verification remain
separate badges; only a successful sender-bound signature gets a verification pass.
A detached PGP/MIME signature inside encryption (including a nested multipart)
is detected and reported as unchecked. The browser does not yet verify that
nested detached signature; it must not label such a message unsigned.

Attachments in the browser path: compose attachments go inside the encrypted
entity as base64 parts of the protected-headers `multipart/mixed`, with the
same headers `mailmsg.Build` writes, and the Sent copy carries them too. On
read, `lib/mimeContent.ts` returns them decoded in memory; the reader offers
them as `download` links on `application/octet-stream` blob URLs and inlines
`cid:` raster images as `data:` URLs, so nothing decrypted is posted back or
navigated to in this origin. Limits: openpgp.js decrypts under a 25 MiB
decompressed cap (mirrors `mailmsg.MaxInboundMessageBytes`), the parser keeps
at most 200 attachments and 25 MiB of decoded attachment bytes and reports the
rest as a count, and a send refuses before encrypting when the attachments
would not fit every ciphertext copy (`encryptedAttachmentBudget`: about 13 MiB
with no Bcc, less per Bcc recipient). Recipients on the secure-link fallback
get no attachments, so a send with both is refused rather than trimmed.

Drafts and the compose safety net, as of 2026-09-14: on a client-custody
account, Save Draft encrypts the whole compose state to the user's own key
(`buildEncryptedDraft`) and posts it as `pgpDraft`, which the server appends
verbatim like the Sent copy after the same PGP/MIME shape check; the plaintext
fields of that request carry the placeholder subject and nothing else, and the
server ignores them. To, Cc, Bcc and Subject travel inside the ciphertext as
protected headers, so reopening a draft from the Drafts folder decrypts it in
the browser and restores recipients, subject, body and attachments. The
reader now shows the protected Subject for every decrypted message instead
of the outer placeholder. The compose autosave snapshot in `sessionStorage`
is sealed to the same key while the vault is unlocked; with the vault locked
nothing is written, and after a reload the snapshot waits for an unlock
before it is restored. Accounts with no PGP identity keep a plaintext
snapshot, since there is no key to seal to. Until the PGP bootstrap has
answered, the browser neither saves a draft nor writes a snapshot: an
unloaded or failed bootstrap must not read as "not client custody". The
server backstops this: a plaintext draft from a client-custody account is
refused with the same 409 `clientSideNeeded` shape as a plaintext send, so
a regressed or native client cannot put cleartext in that account's Drafts.
Native clients on a client-custody account therefore have to encrypt drafts
on device, the way they already encrypt sends; accounts in any other state
keep plaintext drafts, and the native gap is tracked per client.

Because the default *key-custody mode* (`client`) is unchanged, offering this
choice was safe to ship incrementally: existing installs keep generating
client-protected keys exactly as before, and no account is silently moved to
`server` custody. That is a separate claim from the pickup-link behavior
change above — that change is the opposite of "nothing silently downgrades"
for the case it targets: a `server`-protected send to a keyless recipient
used to downgrade silently, and now refuses instead unless the caller opts in.

## Mobile plan

**Superseded.** An earlier version of this section prescribed porting the
browser's crypto to Android and Qt. It ran into a constraint it had not
accounted for, described below, and the answer changed. The old prescription is
kept at the end of this section, marked as such, because the analysis behind it
is still the right starting point if the decision is ever revisited.

### Why porting the crypto was the wrong shape

A phone pairs by QR or deep link and never learns the account password. The
wrapped envelope is sealed under a key derived from that password, so unwrapping
on device means introducing password entry on the least-trusted device, for the
credential that also gates web login and admin.

Every way around that is worse:

- **Per-device PGP keys** break inbound mail. A sender encrypts to one key,
  whichever they discovered through WKD or Autocrypt. This is why Autocrypt's
  multi-device story transfers the same key rather than minting one per device.
- **A device key held in the Keystore/Keychain** makes mail recoverable without
  any user secret, which is the property "Cold start" above exists to remove.
- **Server-side re-encryption to per-device keys** requires the server to
  decrypt first, so it holds the account key anyway, in `SECRET_DIR` beside
  `users.json`. An attacker with the disk decrypts from IMAP directly. The
  layer sits downstream of the secret it would be protecting.

### What we do instead

Offer the user the choice, in the terms they can actually evaluate, and make the
mobile app honest about which one is in force. Both modes already exist; this is
a UI and copy problem, not new cryptography.

| | `server` | `client` (default) |
|---|---|---|
| Server can read your mail | Yes | No |
| Readable in the native mobile app | Yes | No — deep-links to webmail |

The question the Security page asks is "read encrypted mail on your phone?", not
"pick a key custody model". The mode follows from the answer. `client` stays the
default so nothing downgrades by inattention, and the `server` branch is never
described as end-to-end — advertising it that way is the defect this whole split
exists to close.

The choice is offered **at key creation only**. There is deliberately no
downgrade path: `export-legacy` already refuses once an account is
client-protected, and reversing that invariant to save a re-key is not a trade
worth making. Switching from `client` to `server` means generating a new key,
with the usual warning that mail encrypted to the old one stops being readable.

### What mobile apps must implement

Degradation for `client`-custody accounts, unchanged: the phone never holds
the unwrapped private key and cannot unwrap it without the account password,
which pairing deliberately never learns (see "Why porting the crypto was the
wrong shape" above), so it defers to webmail wherever that key would be
needed. `server`-custody accounts are different — the server already holds a
server-readable key, so a native encrypted send from the phone is the same
request the browser makes, not a degraded one.

1. Read `pgpEncrypted`, `pgpSigned`, `pgpVerified`, `pgpSignerFingerprint` and
   `pgpDecryptError` off the inbox row. They are `omitempty`, so absent means
   "no OpenPGP content".
2. `pgpEncrypted` with an **empty** `pgpDecryptError` means client-protected:
   there is no body, and the app cannot produce one. Say so, and offer a link to
   webmail. A **non-empty** `pgpDecryptError` is the different case where the
   server tried and failed — show that error.
3. `pgpEncrypted` **with** a body means the server decrypted it. Surface that
   too: the user should be able to tell that the server read their mail.
   Mark the *list* row for the first two cases only — a row that opens and reads
   normally does not need a marker, and marking it would decorate most rows of a
   `server`-mode mailbox with nothing the user can act on.
4. Sending for a `server`-custody account is native encrypted send, not a
   degradation: `POST /api/mail/send` with `encrypt`, `sign`, and (once the
   user has confirmed sending a pickup link to a keyless recipient)
   `allowPickupFallback` set on the request — the same fields and the same
   endpoint the web client uses. There is no separate mobile crypto path for
   this custody mode; the server does the encrypting either way.
5. That request can come back **409** two different ways. Both are the same
   status code, so discriminate by field, not by status:
   - `clientSideNeeded: true` means the account is `client`-protected and the
     server categorically cannot sign or encrypt for it — there is no retry
     from the phone that fixes this. Treat it as "not available here" and
     hand off to webmail (item 7).
   - `keylessRecipients` (the list of addresses with no usable key) plus
     `pickupFallbackAvailable: true` means the account is `server`-protected
     but at least one recipient has no key. Show the user which addresses,
     and if they confirm sending a one-time pickup link to those addresses,
     re-send the identical request with `allowPickupFallback: true`. Nothing
     was delivered on the first call, so the re-send is safe.
6. Preflight, so case 5's second 409 is a confirmation rather than a surprise:
   call `POST /api/pgp/recipients/check` before the send to learn, per
   address, whether the caller's contacts already have a usable key. Use
   `check`, **not** `POST /api/pgp/recipients/resolve` — `resolve` exists to
   hand a `client`-protected browser the recipients' actual public keys so it
   can encrypt locally, and it 409s for any account that is not
   client-protected. A `server`-custody mobile app asking `resolve` "does this
   recipient have a key" will always be refused; `check`'s yes/no answer is
   the one built for this question. Getting this backwards is the exact
   mistake this document is meant to prevent — an earlier draft of the design
   made it.
7. For a `client`-custody account, the hand-off to webmail is: `POST
   /api/mail/draft` with a `pgpDraft` (the PGP/MIME draft encrypted to the
   user's own key on the device, To/Cc/Bcc/Subject as protected headers) over
   the same paired-device credentials, then hand `/read?mailbox=Drafts` to the
   system as a normal https intent so the user finishes the send in webmail.
   A plaintext draft for such an account is refused with 409
   `clientSideNeeded` (as of 2026-09-14). Same "not an in-app WebView"
   reasoning as item 8.
8. The webmail deep link for reading a message is
   `/read?mailbox=<mailbox>&message=<messageId>`, the same route a web push
   click uses. Omit `mailbox` for INBOX. Hand it to the system as a normal
   https intent so an installed PWA or the user's browser handles it — **not**
   an in-app WebView, which shares no session and would put an account-password
   field inside the app.

This section specifies *when* to hand off. **`WEBMAIL_HANDOFF.md` covers
*how*** — the launch mechanism per client, the first-party origin guard, why
Android's move to Custom Tabs must not be ported to desktop, and why an
embedded web view stays ruled out everywhere. Read it before touching either
handoff site in any client.

Encrypting needs only the recipients' public keys, not the sender's private
one. `POST /api/pgp/recipients/resolve` and `POST /api/mail/send-pgp` are both
`withMailAuth`, so an encrypt-only (unsigned) send built on device is possible
with no account password and no private key on the phone. Not built: it costs
an OpenPGP stack in two clients and produces silently unsigned mail. Recorded
so the option is not rediscovered from scratch.

`kypost-android` implements 1-3 and the read deep link (8). Native
`server`-custody send, the two 409 shapes, the `recipients/check` preflight,
and the `client`-custody draft handoff (4, 5, 6, 7) are new to this branch and
not yet wired up in any mobile client. `kypost-Linux` / `kypost-for-Mac` still
need all of it.

### Superseded: the original port-the-crypto plan

Retained for the analysis, not as instructions.

Both apps need the same three capabilities. Neither can keep using the
server-side decrypt path once its user migrates, because there will be
nothing on the server to decrypt with.

### Shared contract

1. `GET /api/pgp/bootstrap` on every launch — see Cold start above. This
   replaces the older advice to call `/api/pgp/identity/wrapped` directly;
   that endpoint still exists for a re-unlock after an explicit lock, where
   pulling the whole address book again would be wasteful.
2. If `protection == "client"`, unwrap locally with the account password:
   PBKDF2-HMAC-SHA256, iterations and salt from the envelope, AES-256-GCM.
   The envelope is self-describing; do not hardcode 600,000.
3. Messages arrive with `pgpEncrypted: true` and an empty `pgpDecryptError`.
   The ciphertext is **not** inlined in the inbox row — fetch it per message
   from `GET /api/mail/pgp-payload?mailbox=&messageId=<uid>`, which also
   returns `signerKeys` for verification. An earlier version of this
   document said the payload arrived inline; it never did.

   **A signature is verified only by a key the ADDRESS BOOK binds to the
   sender.** Each entry in `signerKeys` is `{addresses, publicKey}`, where
   `addresses` are the owning contact's own email addresses and the key has
   been checked against that contact's TOFU fingerprint pin. Offer them all to
   the OpenPGP library so the actual signer can be identified, then report
   "verified" only when the key that produced the signature is bound to the
   sender you are displaying.

   Do **not** re-derive that binding from the keys themselves. A User ID is
   free-form and self-certified, and a key may carry arbitrarily many, so "this
   key claims the sender's address" is forgeable with one extra User ID — that
   defeated two successive versions of this check. It is also parser-dependent:
   openpgp.js and go-crypto disagree on adversarial User IDs in both
   directions, so a client deriving its own answer can vouch for a key the
   server's binding rejects. The server ships the binding precisely so no
   second parser participates in the trust decision.
4. Sending encrypted: build the ciphertext on device and POST to
   `/api/mail/send-pgp` with one delivery per recipient group, BCC recipients
   each in their own. Each delivery must be a **complete RFC 5322 message** —
   From, To, Subject, Date, MIME-Version, Content-Type — because the server
   relays the bytes verbatim and synthesizes nothing. It now rejects a
   delivery missing any of those rather than sending malformed mail. Put the
   real subject inside the encrypted part as a protected header and use the
   placeholder `[Encrypted] Email Sent by KyPost` outside, matching both
   other send paths.

### kypost-android

- **KDF/AEAD:** `javax.crypto.SecretKeyFactory` with
  `PBKDF2WithHmacSHA256`, then `Cipher.getInstance("AES/GCM/NoPadding")`.
  Both are in the platform; no new dependency.
- **OpenPGP:** Bouncy Castle (`org.bouncycastle:bcpg-jdk18on`), which the
  Android ecosystem already uses widely. Do not attempt to reuse OpenKeychain
  via intents — that puts the key in another app's custody and reintroduces
  the same "who actually holds it" question.
- **Key storage at rest:** keep the wrapped envelope in app-private storage.
  The unwrapped key should live in memory only, cleared in `onTrimMemory`
  and on logout. Do *not* put the unwrapped key in the Android Keystore
  "for convenience" — that makes it recoverable without the password, which
  is the property being removed.
- **Unlock UX:** prompt at first PGP operation after app start, not at
  launch, so users who never touch encrypted mail never see it. Optionally
  gate a short-lived in-memory cache behind `BiometricPrompt`.
- **Push:** already generic by default; nothing to change. The notification
  carries only `messageId`, so tapping it syncs and then decrypts on device —
  which is the correct flow anyway.
- **Order of work:** wrapped-key fetch and unwrap → decrypt on read →
  encrypt on send → migration prompt.

### kypost-Linux / kypost-for-Mac (Qt)

- **KDF/AEAD:** Qt has no PBKDF2 primitive worth using here; link OpenSSL
  directly (`PKCS5_PBKDF2_HMAC` with `EVP_sha256`, then
  `EVP_aes_256_gcm`). Both desktop targets already ship against OpenSSL for
  TLS.
- **OpenPGP:** GPGME via `gpgme++`, or Sequoia's C API. GPGME is the lower
  friction path on Linux; on macOS it means bundling GnuPG, so Sequoia may
  be the better single choice for both.
- **Key storage:** the wrapped envelope goes in the app config dir. Do not
  put the *unwrapped* key in the platform keychain (Secret Service /
  Keychain Access) — same reasoning as the Android Keystore note.
- **Unlock UX:** a modal on first PGP operation, with an explicit "this
  is your account password, not a separate PGP passphrase" line, because
  users who imported a passphrase-protected key will expect the old one.
- **Order of work:** identical to Android.

### Migration for existing mobile users

A user who migrates on the web will find their phone unable to read encrypted
mail until the app is updated. The apps should detect `protection == "client"`
with no local unwrap support and show a clear message ("this account's key is
end-to-end protected; update the app to read encrypted mail here") rather
than surfacing a generic decryption failure. Add that check first, ahead of
the crypto work, so an old app fails legibly.
