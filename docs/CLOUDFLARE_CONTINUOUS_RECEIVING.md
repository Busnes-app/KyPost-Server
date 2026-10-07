# Continuous Cloudflare receiving: design

Status: approved design, 2026-10-06. Worker (`receiving-worker/continuous.mjs`,
[wire contract](#wire-contract)) and KyPost side (`backend/internal/cfreceiving`,
`backend/internal/app/receiving_cfcontinuous.go`, [implementation](#kypost-implementation))
implemented and tested offline; nothing qualified live. Replaces the one-message
pilot in [CLOUDFLARE_RECEIVING.md](CLOUDFLARE_RECEIVING.md) once qualified. Cloudflare
is the primary receiving profile; bundled Maddy and external IMAP remain supported
alternatives (see root `AGENTS.md`). Both receiving profiles share the ingress
holding store and mailbox importer; only the transport differs.

## Goals

- Receive continuously for every assigned address, across several verified domains,
  into several mailboxes per KyIdentity user.
- No inbound port on the operator's network: KyPost only makes outbound HTTPS calls.
- Mail survives KyPost downtime: Cloudflare holds accepted mail until KyPost has
  committed it durably, then KyPost deletes the provider copy.
- All owners committed before provider deletion, exact replay idempotent, conflicts
  quarantine with bytes retained, never evict accepted mail, no correspondence in logs.

Non-goals: Cloudflare-side spam filtering, forwarding, sending through Cloudflare,
simultaneous Maddy and Cloudflare reception for one deployment.

## Approved contract changes

Two existing rules cannot hold for a hosted gateway that KyPost cannot reach
synchronously. Both were approved on 2026-10-06; `TURNKEY_MAIL_STACK_PLAN.md` and
`ingress/AGENTS.md` carry the amended rules.

1. **Stale routing.** `TURNKEY_MAIL_STACK_PLAN.md` requires a maximum routing
   staleness that pauses fresh acceptance, and rejection of disabled addresses "when
   its directory is current". The Worker can only reject permanently
   (`setReject`); whether a thrown exception gives senders a temporary failure is
   undocumented. Options:
   - **A (approved): accept against the last signed table**, bounded by a maximum age
     of 14 days, then refuse. Senders do not bounce during a home
     outage. A disabled or removed address can receive until the next publish; that
     mail is quarantined locally, never delivered.
   - B (rejected): refuse after a short maximum age, which bounces mail permanently
     during any longer outage.
2. **Binding time.** Today `Bind` runs at SMTP RCPT against a live local route. Here
   the Worker freezes `(address, generation, tableRevision)` at acceptance and KyPost
   binds later. A new ingress operation records that frozen binding: it creates a
   staged delivery when the generation still matches current authority, and a
   quarantined delivery with bytes retained when it does not. "Bind at RCPT" becomes
   "bind at gateway acceptance, verified at pickup" for the hosted profile.

## Prerequisites (separate change sets, both profiles)

- **Addressing model v2** ([spec](NATIVE_ADDRESSING_V2.md)). The single domain and single mailbox per user are encoded
  in `sso` (`native-domains.json`, held to one domain in phase 1a; assignments keyed by issuer/subject with one
  `Address`), `users` (`Mailbox == User.ID`, one native source per user) and
  `app/receiving.go` (single domain lock, `users/<Mailbox>` paths). v2 introduces a
  set of verified domains, mailboxes with their own IDs owned by a subject, and an
  address table `address → mailbox, generation, state` with primary address, explicit
  aliases and reservation of released names. **Generation is per address**, bumped
  only on reassignment, disable, re-enable or release, so ordinary profile edits do not fence
  waiting mail. Sending allows any address of the sending mailbox.
- **Receipt archival** (done). Acknowledgment archives an imported delivery into a
  replay tombstone outside the ingress record limit; see `NATIVE_PROVISIONING.md`.
  Tombstones are not pruned yet; pruning and capacity recovery remain a
  public-MX gate there.
- **Quarantine release** (done). Administrators list quarantined deliveries (envelope
  only) and release or discard them through the admin API or CLI, step-up confirmed
  and audited. Release goes only to the frozen mailbox, and only while it exists, is
  active and is still owned by the same issuer/subject; otherwise it refuses and
  discard remains. Mail waiting for a durably inactive owner quarantines instead of
  holding receiving capacity. See
  [quarantine release](NATIVE_PROVISIONING.md#quarantine-release).

## Cloudflare side

**Rules and DNS.** For each receiving domain, enable Email Routing on its zone and
point the catch-all rule at the operator's Worker; no per-address rules (200 per
domain). Enabling locks MX and replaces the apex SPF with Cloudflare's; the domain's
SPF policy becomes `~all` unless the operator merges further includes, and DMARC
`p=reject` still governs alignment. Disabling routing deletes the locked SPF
(observed 2026-10-06), so setup and rollback restore it. Up to 30 domains per zone.

**Worker** (`receiving-worker/`). Size cap: 25 MiB (Cloudflare's maximum, about ten
phone photos) for both receiving profiles; this raises the ingress 4 MiB message
limit and its live payload budget through a store limit migration.


- `email`: lowercase `message.to` and the envelope sender domain. A sender address or
  domain on the table's block list → `setReject` before reading the body. Then look the
  recipient up in the current table. Unknown address,
  or a domain absent from the table → `setReject` (permanent 550, intended). Over the
  address's `maxBytes` → `setReject`. Table older than the maximum age → per decision
  1. Otherwise persist raw bytes and envelope `{id, sender, recipient, generation,
  tableRevision, capturedAt, size, digest}` to R2 key `inbox/<uuidv7>` with
  `If-None-Match: *`, awaited before success. Storage failure throws rather than
  rejecting (temporary-failure behaviour to be qualified).
- **Routing table** `{revision, issuedAt, routes: [{address, generation, maxBytes}],
  blockedSenders: [{address or domain, until}]}`,
  signed by KyPost with an Ed25519 key; the Worker holds only the public key.
  Revision is KyPost's millisecond clock; the Worker refuses revisions not greater
  than the stored one or more than 5 minutes in the future, and replaces
  `routes.json` with an etag-conditional write. A stolen bearer secret therefore
  cannot forge routes, publish an empty table, or jam future revisions.
  `maxBytes = min(ingress MessageBytes, mailbox MessageBytes)`.
- **Pickup API**, HTTPS only, dedicated 256-bit bearer secret checked before any read:
  - `GET /messages?after=<key>&limit<=100` → keys (time-ordered), sizes, digests.
  - `GET /messages/<uuid>` → raw body plus envelope header.
  - `DELETE /messages/<uuid>?digest=<sha256>` → deletes only on digest match; absent
    key is success.
  - `GET /routes` / `PUT /routes` → read the stored table; install a signed table.
  - `POST /rotate` → install a new bearer-secret hash and signing public key, with an
    epoch one above the current one, signed by the **current** signing key; written
    with an etag-conditional replace of `credentials.json`. The Worker reads its live
    credentials from `credentials.json`; the deployed secrets only bootstrap epoch 1.
    Every other route checks the bearer against the current epoch, so rotation
    immediately fences the previous holder.
  - Paths are matched against a strict UUID pattern; nothing else is reachable.
- Each routed recipient is a separate invocation (one `message.to`), so a message to
  two addresses becomes two deliveries, including two copies when both belong to one
  mailbox. Deduplication by Message-ID stays forbidden. (Per-recipient invocation is
  inferred from the pilot interface; confirm in qualification.)

## Wire contract

Implemented by `receiving-worker/continuous.mjs`, selected by deploying it as the
Worker's `main` (the pilot stays `worker.mjs` until continuous is qualified). Times are
milliseconds since the Unix epoch. Every response carries `Cache-Control: no-store`;
error bodies are empty. The Worker never redirects and never logs.

**Plan.** Workers Paid is required. Free's 10 ms CPU per invocation cannot hash and
store a message near the 25 MiB cap, and the email handler peaks near twice the message
size in memory (about 50 MiB at the cap, within the 128 MiB isolate limit).

**Bindings.** R2 bucket `MAIL`. Secrets `PICKUP_TOKEN_SHA256` (lowercase hex SHA-256 of
the bearer string) and `ROUTING_PUBLIC_KEY` (standard base64 of the raw 32-byte Ed25519
public key) define epoch 1 and are read **only while `credentials.json` is absent**.
Generate a fresh key and bearer for each Worker and never share them between Workers:
signed documents name no deployment, so a table or rotation signed for one Worker is
valid on any other that trusts the same key.

**Public keys** (bootstrap and rotation) must be the canonical encoding of an Ed25519
point that is not the identity and lies in the prime-order subgroup; the Worker checks
this in BigInt because workerd's `verify` accepts small-order keys (the identity key
verifies the signature `01 00…00` on any message). An invalid bootstrap key makes every
route answer 503; an invalid rotation key is refused with 400.

**R2 objects.**

| Key | Content |
| --- | --- |
| `inbox/<uuid>` | Exact raw message; `customMetadata.envelope` = envelope JSON |
| `routes.json` | `{"table": "<json>", "signature": "<base64>"}` as installed; `customMetadata.revision` |
| `credentials.json` | `{"epoch": n, "tokenSha256": "<hex>", "publicKey": "<base64>"}` |

`<uuid>` is a lowercase UUIDv7 (`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
whose first 48 bits are the capture millisecond, so key order is capture order at
millisecond granularity.

**Bearer.** `Authorization: Bearer <64 lowercase hex>`, a fresh 256-bit random value.
The Worker hashes the 64 ASCII characters with SHA-256 and compares with
`tokenSha256` in constant time. Order: non-HTTPS → 403; a malformed or missing
header → 401 with no storage read; unreadable or invalid credentials → 503; wrong
bearer → 401 (after reading only `credentials.json`); then routing.

**Signed documents.** One Ed25519 key signs two document types, separated by a context
prefix. The signature (standard base64, 64 bytes, canonical encoding) covers exactly
`UTF-8(context) || UTF-8(payload string)`; the payload is carried as a JSON string, so
the verifier checks the bytes it received and no canonical JSON form exists.

| Document | Body | Context (with trailing LF) | Payload limit |
| --- | --- | --- | --- |
| Routing table | `{"table": "<json>", "signature": "<b64>"}` | `kypost-cf-routes/1\n` | 1 MiB |
| Rotation | `{"rotation": "<json>", "signature": "<b64>"}` | `kypost-cf-rotate/1\n` | 4 KiB body |

Both bodies are objects with exactly those two keys. Payload JSON has exactly the
listed keys:

- **table**: `{"revision", "issuedAt", "routes": [{"address", "generation", "maxBytes"}],
  "blockedSenders": [{"address", "until"} | {"domain", "until"}]}`. `revision`,
  `issuedAt`, `generation`: safe integers ≥ 1. `maxBytes`: 1 to 26214400. `until`:
  safe integer or `null` (no expiry). Route addresses are lowercase printable
  ASCII `local@domain` (internationalized domains as A-labels, `xn--…`), at most 254
  characters with a local part of at most 64, unique within `routes`. A blocked
  `address` is any non-null sender the email handler accepts (below), with A-Z
  lowercased and non-ASCII unchanged; a blocked `domain` is any such sender's
  domain, A-Z lowercased. Both sides compare senders after lowercasing A-Z only,
  so non-ASCII compares exactly (`receiving-worker/blocks.json` is the shared
  fixture). A domain block matches the sender's domain exactly, not its
  subdomains. At most 5000 routes and 5000 blocks; KyPost signs as many blocks
  as fit beside the routes, manual before automatic and newest first, and
  reports any it leaves out.
- **rotation**: `{"epoch", "tokenSha256", "publicKey"}` in the credentials format, with
  a `tokenSha256` and a `publicKey` that both differ from the current ones.

**Email handler.** One invocation per recipient. In order: lowercase `message.to`
and A-Z in the sender's domain (the local part keeps its case; a null sender, `""` or `<>`,
becomes `""`); a malformed or non-ASCII recipient, or a sender KyPost's holding
store could not hold → reject. A sender must be at most 320 UTF-8 bytes, well-formed
UTF-16, with a dot-atom local part (ASCII atext or any non-ASCII character) and an
ASCII dot-atom domain (a U-label could dodge an A-label block; domain literals and
quoted local parts are refused). KyPost's Maddy profile refuses exactly the same
senders at RCPT, so every sender either profile accepts can be blocked. `receiving-worker/senders.json` is the shared
fixture both sides test against. Read `routes.json` (absent → reject; storage error → throw); each isolate
keeps the parsed table and revalidates it on every message with a conditional get on
its etag, so a new table applies to the next message. Sender address or domain blocked with `until` null or in the future →
reject. `now - issuedAt` over 14 days → reject. Recipient not in `routes` → reject.
Announced or actual size over the route's `maxBytes`, or empty → reject. None of these
reads the body except the actual-size check. Otherwise put `inbox/<uuid>` with
`If-None-Match: *`, the SHA-256 checksum and the envelope, awaited; a failed put or a
key collision throws (never a reject, never an overwrite). The envelope, at most 2000
bytes, is exactly:

```json
{"id":"<uuid>","sender":"<normalized sender>","recipient":"<lowercase>","generation":3,
 "tableRevision":1790000000000,"capturedAt":1790000000123,"size":1234,"digest":"<sha256 hex>"}
```

**Routes.**

| Request | Success | Refusals |
| --- | --- | --- |
| `GET /messages[?after=<uuid>][&limit=1..100]` | 200 `{"messages":[{"key","size","digest"}],"truncated":bool}` in key order, starting after `after` | 400 bad `after`/`limit`, 404 other or repeated parameters |
| `GET /messages/<uuid>` | 200 raw bytes, `Content-Type: application/octet-stream`, `Content-Length`, `X-KyPost-Envelope: base64url(no padding) of the stored envelope's UTF-8 bytes` | 404 absent |
| `DELETE /messages/<uuid>?digest=<64 hex>` | 204 deleted, or already absent | 409 digest differs (object kept) |
| `GET /routes` | 200 the stored body, re-serialized as `{"table":…,"signature":…}` | 404 none installed |
| `PUT /routes` | 204 installed | 413 body over 2 MiB + 4 KiB; 400 malformed; 403 signature; 422 `revision` or `issuedAt` more than 5 min ahead, or `issuedAt` over 14 days old; 409 `revision` not above the stored one, or a concurrent install won |
| `POST /rotate` | 204 installed | 413; 400 malformed, invalid key, or a bearer hash or key equal to the current one; 403 not signed by the **current** key; 409 `epoch` ≠ current + 1, or a concurrent rotation won |

Any other method, path, trailing slash, upper-case UUID or query → 404. Paging:
continue with `after` = last key while `truncated` is true; the Worker follows R2's
cursor past an empty truncated page, so a page is empty only at the end. An `inbox/`
object without envelope metadata (only a Cloudflare account administrator can create
one) makes listing answer 503 until it is removed. Listing is not a durable
cursor: a capture in the same millisecond, or on a Worker instance with a slower
clock, can sort before a key already seen, so each pickup cycle lists from the start;
deleted keys drop out.

`PUT /routes` and `POST /rotate` replace their object conditionally on the etag read
in the same request (`If-Match`, or `If-None-Match: *` when absent). For rotate this is
the record the bearer was checked against, so of two concurrent rotations exactly one
installs. After a rotation the previous bearer gets 401 on every route and the previous
key gets 403 on `PUT /routes` and `POST /rotate`; requests already past authentication
when the rotation lands still complete. KyPost confirms a rotation whose
response it lost by any request with the pending bearer: 200 means it is installed,
401 means it is not (or someone else rotated: stay fenced).

## KyPost side

One consumer per Worker, enforced by credential rotation. A restored instance starts
**fenced**: no rotation, no pickup, no publish. Taking over receiving is an explicit
operator action (setup or admin, with step-up), never a side effect of restore. The
confirmation names the takeover and its effect: receiving moves to this host and the
current host stops receiving.

On confirmation, KyPost rotates the bearer secret and signing key:

1. Generate a new secret and key and persist them locally as `pending` (so a crash
   retries the same material rather than inventing a third).
2. `POST /rotate` signed with the restored key. A success, or a current epoch that
   already carries the pending public key, confirms it; persist as `current` and
   start pickup and publish.
3. A refusal means another instance rotated first: stay fenced, report it, and never
   pick up or publish with the old credentials.

After a takeover the previous instance is refused on list, fetch, delete and
`PUT /routes`; it stops its loop and reports that it has been fenced, rather than
retrying silently. A test or inspection restore that is never confirmed leaves
production untouched. The same rotation is the scheduled and on-demand path for the
live instance. If an attacker with a backup and its seal confirms a takeover, the
operator recovers through the Cloudflare account, in this order: generate a **new**
signing key and a **new** bearer secret, deploy them as the bootstrap secrets, and only
then delete `credentials.json`. Deleting first, or redeploying the old secrets, resets
the Worker to epoch 1 under the old key K1, and every rotation ever signed by K1 (the
attacker's own epoch-2 rotation included) carries epoch 2 = 1 + 1 and replays
successfully, handing receiving straight back to the attacker. Restore KyPost's signing
key from the new secrets, not from the compromised backup.

A daemon loop, outbound HTTPS only, every 30 seconds and on directory change:

1. **Publish.** Build the table from admitted assignments on currently verified
   domains under the authority fences; sign and `PUT` when it changed, and re-sign at least hourly so an
   unchanged directory never reaches the 14-day maximum age. Record each
   published revision locally (`revision → address, generation, owner`). A transient
   DNS failure never prunes a domain; only deliberate domain removal does.
2. **List.** Page `GET /messages` oldest first. Check a local provider ledger
   (`key → digest, state`) before fetching: keys already `archived`, `quarantined`
   or `refused` skip to step 6.
3. **Fetch and verify** size and digest, then record the frozen binding (decision 2):
   staged if the generation matches current authority, quarantined otherwise.
4. **Scan** with Rspamd (fixed Cloudflare settings, no peer IP/HELO). A reject verdict
   cannot bounce mail Cloudflare already accepted; it is delivered to the mailbox's
   Junk folder, never silently dropped. Soft reject or scanner failure retries later.
5. **Accept and import** under fresh authority with existing `importDelivery`, all
   owners committed.
6. **Delete at the provider** once the local record is durable in any terminal state
   (imported, quarantined with bytes, Junk-delivered), using the local digest. A lost
   delete is retried from the ledger without refetching. Lapsed domain proof or local
   capacity refusal stops fetching; mail waits in R2.

Restore: the ledger, published-revision records and signing key are in sealed
backups. Because tables are re-signed hourly, a restored `cloudflare.db` almost
never knows the revision mail waiting in R2 was captured under, so after a
takeover that mail is quarantined *unresolved* and needs an administrator's
release to the current owner (see KyPost implementation). Take over promptly
after a restore to keep that set small. A takeover needs the Worker's current
key: a backup older than the last rotation cannot take over; re-bootstrap
through the Cloudflare account instead ([setup](RECEIVING_SETUP.md#continuous-cloudflare-profile)).
Cloning a volume (as opposed to restoring a backup) copies the host marker too and
yields two live consumers; never clone a receiving instance. After restore, `GET /routes` reconciles: KyPost publishes only above the
stored revision, and R2 items frozen against revisions it no longer has are
quarantined, not guessed.

### KyPost implementation

Operator steps: [continuous Cloudflare profile](RECEIVING_SETUP.md#continuous-cloudflare-profile).

- **Selection.** `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN` (an HTTPS `*.workers.dev`
  origin; custom domains are not accepted yet) starts the loop in the daemon. It
  requires `KYPOST_NATIVE_RECEIVING=true` and `KYPOST_RECEIVING_RSPAMD=true` and
  refuses startup beside `KYPOST_NATIVE_RECEIVER=true`. Ingress gateway
  `cloudflare-continuous`; the pilot keeps `cloudflare-worker`.
- **Credentials.** `SECRET_DIR/cloudflare-receiving.json` (0600, sealed) holds the
  current epoch, bearer and Ed25519 seed. `SECRET_DIR/cloudflare-receiving.host.json`
  (0600, excluded from backups) holds this host's live marker (the current
  bearer's SHA-256) and any rotation in flight. A restored copy lacks it, so it
  starts fenced and never reuses rotation material the original may already
  have installed. `receiving cloudflare init` creates epoch 1 and prints only
  `PICKUP_TOKEN_SHA256` and `ROUTING_PUBLIC_KEY`; the bearer is never shown.
- **State.** `STATE_DIR/receiving/cloudflare.db` (sealed as a SQLite snapshot):
  published tables (`revision → address, generation, maxBytes, owner`), the
  provider ledger and the last status. Ledger states: `junk` (reject verdict;
  Junk delivery owed, durable before the bytes are accepted), `imported` and
  `quarantined` (provider delete owed), `refused` (an object KyPost cannot hold;
  kept in R2, dropped from the ledger once gone there). Rows leave after the
  provider delete.
- **Loop.** Every 30 seconds, and within 5 seconds of an address-ledger change.
  Publish compares the table digest with the last installed one and re-signs
  hourly; the revision is `max(now, last recorded + 1, Worker's + 1)`, recorded
  before the PUT. A route whose domain proof or mailbox admission fails this
  cycle is carried from the last installed table at the same generation.
  Pickup lists from the start, fetches at most 100 objects a cycle, binds with
  the frozen owner and generation through the shared `bindExpected`, scans,
  accepts and imports with the existing runtime, and quarantines with bytes
  (`ingress.Store.Quarantine`) when authority no longer matches, the message
  exceeds its mailbox's per-message limit, or the revision is unknown. An
  unknown revision leaves the owner empty (*unresolved*): an administrator can
  release it only to the recipient address's current owner, with an explicit
  confirmation that today's owner is not proven to be the original, or
  discard it ([quarantine release](NATIVE_PROVISIONING.md#quarantine-release)).
  Equal generations are never taken as proof after a restore. A key refused for
  a transient reason (domain proof, admission, scanner) backs off in memory
  from one minute to an hour, so stuck mail neither refetches every cycle nor
  uses the per-cycle fetch budget ahead of newer mail. Only a full receiving
  store stops fetching. An empty truncated listing page ends the cycle as an
  error. The provider
  delete uses the local digest and runs only when the holding store has the
  delivery archived or quarantined; a ledger row the holding store does not
  back (an older restored store) is dropped and the object picked up again.
- **Junk verdict.** The `junk` ledger row is the only record of a reject
  verdict until the delivery is archived; it is not stored with the ingress
  delivery (that would change its schema and restore validation). If
  `cloudflare.db` is lost or restored older while a delivery is pending, or an
  administrator releases a quarantined delivery that had a reject verdict, it
  is filed to INBOX. No mail is lost either way. A `junk` row whose provider copy
  has vanished and whose delivery is not pending is dropped.
- **Refused** objects are counted in status `refused`, not as waiting, and do
  not raise the oldest-unpicked warning.
- **Fencing.** A 401 is rechecked under the credential lock (which serializes
  init, rotation, promotion and this fence decision; cycles do not hold it): a rotation that
  landed meanwhile is not a fence, an in-flight rotation is confirmed with its
  own bearer, otherwise the host marker is cleared and the loop reports
  `fenced` and stops calling the Worker. `receiving cloudflare rotate` and
  `takeover --confirm move-receiving-here` persist pending material first, POST
  it signed by the current key, confirm a missing or refused answer with the
  pending bearer and then promote it; a refused pending bearer means another
  instance rotated first, and this one stays fenced.
- **Warning.** `oldestUnpickedWarning` when the oldest waiting capture is over
  one hour old (decision 5): 120 missed cycles means an operator problem, still
  far inside the 14-day table age.
- **Blocks.** `blockedSenders` carries the [sender blocks](NATIVE_PROVISIONING.md#sender-blocks)
  in force (manual, and automatic ones a Maddy profile recorded in the same store) (`until` in Unix ms or null). A block change republishes within
  one loop tick; an unreadable block list still publishes routes, with the
  last installed revision's blocks (recorded per revision in `cloudflare.db`;
  none if never published), and reports `error`.
- **Not built.** Automatic abuse blocks from Cloudflare evidence (pending SPF
  qualification), an admin takeover screen with step-up (CLI only),
  custom Worker domains, removing the pilot, live qualification.

## Abusive senders

Both receiving profiles block abusive senders with an escalating cooldown, before
storage: the Worker from the signed table, Maddy in its RCPT bind check (550).

- **Evidence counts only an authenticated sender identity.** A message counts
  against an address only when all three hold: the envelope sender equals the
  RFC5322 From address; DKIM passes with `d=` aligned to that address's domain; and
  SPF passes for the envelope domain on the receiving MTA's own result. A domain-level
  DKIM pass alone proves nothing about which address on a shared provider sent it, so
  without all three nobody can pin reject verdicts on someone else's address.
  Everything else is handled per message by the spam verdict.
- **Automatic blocks are per sender address**: 5 reject verdicts in 1 hour from
  that authenticated identity. DKIM alignment is strict (`d=` equals the
  address's domain exactly).
- **Profile coverage.** Maddy establishes all three through the Rspamd
  sidecar (`R_SPF_ALLOW` from the real peer IP and envelope, `R_DKIM_ALLOW`'s
  verified `d=`) and enforces automatic blocks.
  The Cloudflare Worker sees no peer IP, so Rspamd cannot evaluate SPF there.
  Automatic gateway blocks are disabled in the Cloudflare profile until
  qualification shows Email Routing hands the Worker a trustworthy SPF result for
  the envelope domain; until then it uses per-message spam verdicts plus manual
  blocks.
- **Automatic domain blocks are narrow.** A domain is blocked automatically only when
  at least 5 distinct addresses on it are automatically blocked (each meeting the
  identity rule above) within 24 hours
  **and** the deployment has never accepted non-rejected mail from that domain
  (a never-pruned record of accepted envelope domains, at most 10,000; when it
  is full, no domain is blocked automatically).
  Shared providers your users already receive from are therefore never blocked
  automatically; throwaway spam domains are.
- **Cooldown escalates** per repeat: 1 hour, 24 hours, 7 days; a clean period
  (30 days without a counted verdict) resets the level. Blocks expire automatically at `until`.
- **Administrators** see blocks with their evidence and can block or unblock any
  address or domain manually; manual blocks have no automatic expiry unless one is
  set. Manual blocks are implemented in both profiles (API and CLI:
  [sender blocks](NATIVE_PROVISIONING.md#sender-blocks)); the deployment's own
  mail domains and addresses on them cannot be blocked, and the null sender is
  never blocked. A manual unblock suppresses automatic re-blocking of that
  exact address or domain for 30 days, and an automatic block never replaces
  a manual one. Automatic blocks are implemented for Maddy
  ([automatic sender blocks](NATIVE_PROVISIONING.md#automatic-sender-blocks));
  evidence display and the admin UI are pending.
- Blocked mail is rejected, not stored; senders get a permanent 550 for the
  duration. Block state is durable, in sealed backups, and audited without message
  content.

## Security and trust

- Cloudflare and its account administrators can read ordinary mail in transit and in
  R2, and `routes.json` exposes the full address directory to them. Setup says so
  before enabling this profile; Maddy and external IMAP are the alternatives.
- A stolen bearer secret can read and delete waiting mail (listing reveals digests)
  and read `GET /routes`, which exposes the address directory and the block list,
  but cannot change routing. Both secret and signing key are runtime secrets in
  sealed backups and rotate together through `POST /rotate` (see KyPost side).
- Origins: verified HTTPS `*.workers.dev` or the operator's custom domain; no
  redirects, no proxy, bounded responses.
- R2: private, no public access, no lifecycle deletion. Provider copies are outside
  KyPost's backups and encryption until deleted. Whether Email Routing's own activity
  log retains envelope or subject data is not established.
- Anyone can mail a valid address; local ingress limits and the oldest-unpicked
  warning bound the damage, and R2 absorbs bursts while KyPost drains them.

## Migration from the pilot

The single slot, 15-minute `ROUTE` secret and `route`/`pickup` CLI are removed once the
continuous protocol is qualified. Bucket and Worker names may be reused; the pilot's
object is already deleted.
Until then both live in `receiving-worker/`: `worker.mjs` (pilot) and the self-contained
`continuous.mjs`, chosen by the deployment's `main`. Removing the pilot deletes
`worker.mjs`, its tests and runtime check, and the CLI.

## Qualification

Offline: Worker unit and workerd/R2 runtime tests (unknown-recipient reject, size cap,
conditional put, signed/ordered/future-bounded table replacement, list paging,
digest-checked delete, strict paths, rotation fencing) — done:
`node --test receiving-worker/continuous.test.mjs` and
`node receiving-worker/runtime-continuous-check.mjs`; backend race tests against a
Go fake of the wire contract for publish → capture → pickup → import → delete
interrupted at every boundary, replay, conflict, generation-change and
unknown-revision quarantine, spam-to-Junk, scanner retry, capacity refusal,
multi-owner deliveries, restore reconciliation; a restore started without takeover
confirmation makes no `POST /rotate` call and the original's list, fetch, delete
and `PUT /routes` keep succeeding; after a confirmed takeover the original is
refused on all four, reports that it is fenced, and no R2 item is deleted unless
the new owner has committed it — done: `GOTOOLCHAIN=go1.26.6 go test -race
./internal/app -run TestCloudflareContinuous` and `./internal/cfreceiving`. Boundaries
are simulated by aborting the cycle and restarting the loop over the same durable
state, not by killing a process.

Offline, abuse blocks: five reject-verdict fixtures that pass DKIM for a shared
domain but carry differing envelope senders and From addresses must not block any
address, in both the published table and Maddy's RCPT check — done against the
block store and Maddy's bind (`go test ./internal/ingress -run TestAutomatic`,
`./internal/app -run TestNativeReceivingAutomaticBlocks`), and against the
pinned Rspamd with real DKIM/SPF over test DNS (`RSPAMD_PROOF=true go test
./internal/app -run TestReceivingRspamdAuthenticationProof`). The Cloudflare
table carries no automatic evidence of its own.

Live, on the test deployment, each with its own bounded plan and approval:
1. Whether a thrown Worker exception gives the sender a temporary failure.
2. Several messages to several addresses and domains, including an unknown one.
3. KyPost stopped while mail arrives; restart drains R2 and deletes provider copies.
4. Address reassignment while mail waits in R2: old-generation mail quarantines.
5. Whether Email Routing gives the Worker a trustworthy SPF result (gates automatic
   sender blocks in this profile).
6. A message near the 25 MiB cap on Workers Paid: CPU time and memory stay within
   limits, and what the sender sees when the Worker exceeds its CPU limit
   (`EXCEEDED_CPU`): temporary failure, bounce, or silent loss.
7. Null-sender mail (a bounce): whether `message.from` is `""` or `<>`; both are
   accepted as the null sender.
8. Internationalized addresses: the form of `message.to` and `message.from` for an
   SMTPUTF8 or IDN sender and recipient (A-label or U-label); the Worker accepts only
   ASCII recipients and sender domains.

## Decisions (2026-10-06)

1. Stale routing: option A, 14-day maximum age.
2. Frozen binding at pickup for the hosted profile: approved.
3. Spam after acceptance: delivered to Junk.
4. Size cap: 25 MiB in both profiles, with the ingress limit migration.
5. Pickup interval 30 s; the oldest-unpicked warning threshold is one hour.
6. Abusive senders: blocked with an escalating cooldown, per authenticated address
   automatically (Maddy now; Cloudflare after SPF qualification) and per domain only
   under the narrow rule above; manual blocks in both profiles.
