# Continuous Cloudflare receiving: design

Status: approved design, 2026-10-06. Not implemented. Replaces the one-message
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

- **Addressing model v2.** The single domain and single mailbox per user are encoded
  in `sso` (`native-domain.json`, assignments keyed by issuer/subject with one
  `Address`), `users` (`Mailbox == User.ID`, one native source per user) and
  `app/receiving.go` (single domain lock, `users/<Mailbox>` paths). v2 introduces a
  set of verified domains, mailboxes with their own IDs owned by a subject, and an
  address table `address → mailbox, generation, state` with primary address, explicit
  aliases and reservation of released names. **Generation is per address**, bumped
  only on reassignment, disable or release, so ordinary profile edits do not fence
  waiting mail. Sending allows any address of the sending mailbox.
- **Receipt archival.** Imported receipts count toward the ingress record limit
  (10,000) and are never deleted, so continuous reception would stop. Fenced
  archival of imported receipts is required first (already a listed gate in
  `NATIVE_PROVISIONING.md`).
- **Quarantine release.** An operator tool to inspect, release to the current owner
  of the frozen mailbox, or discard quarantined deliveries. Without it, quarantine is
  a dead end.

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


- `email`: lowercase `message.to` and the envelope sender domain. A sender domain on
  the table's block list → `setReject` before reading the body. Then look the
  recipient up in the current table. Unknown address,
  or a domain absent from the table → `setReject` (permanent 550, intended). Over the
  address's `maxBytes` → `setReject`. Table older than the maximum age → per decision
  1. Otherwise persist raw bytes and envelope `{id, sender, recipient, generation,
  tableRevision, capturedAt, size, digest}` to R2 key `inbox/<uuidv7>` with
  `If-None-Match: *`, awaited before success. Storage failure throws rather than
  rejecting (temporary-failure behaviour to be qualified).
- **Routing table** `{revision, issuedAt, routes: [{address, generation, maxBytes}],
  blockedSenders: [{domain, until}]}`,
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
  - Paths are matched against a strict UUID pattern; nothing else is reachable.
- Each routed recipient is a separate invocation (one `message.to`), so a message to
  two addresses becomes two deliveries, including two copies when both belong to one
  mailbox. Deduplication by Message-ID stays forbidden. (Per-recipient invocation is
  inferred from the pilot interface; confirm in qualification.)

## KyPost side

One consumer per Worker: the bearer secret and signing key are per deployment, and a
restored second instance must not share them.

A daemon loop, outbound HTTPS only, every 30 seconds and on directory change:

1. **Publish.** Build the table from admitted assignments on currently verified
   domains under the authority fences; sign and `PUT` when it changed. Record each
   published revision locally (`revision → address, generation, owner`). A transient
   DNS failure never prunes a domain; only deliberate domain removal does.
2. **List.** Page `GET /messages` oldest first. Check a local provider ledger
   (`key → digest, state`) before fetching: keys already `imported`, `quarantined`
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
backups. After restore, `GET /routes` reconciles: KyPost publishes only above the
stored revision, and R2 items frozen against revisions it no longer has are
quarantined, not guessed.

## Abusive sender domains

Both receiving profiles block abusive sender domains with an escalating cooldown,
before storage: the Worker from the signed table, Maddy in its RCPT bind check (550).

- **Evidence counts only DKIM-authenticated mail.** The envelope sender is
  spoofable and the Worker sees no peer IP, so a domain accrues abuse only from
  messages whose Rspamd DKIM result passes for that domain. Unauthenticated abuse
  never blocks a domain; it is handled per message by the spam verdict.
- **Trigger:** a domain whose authenticated mail draws repeated reject verdicts
  within a window (proposed: 5 in 1 hour) is blocked.
- **Cooldown escalates** per repeat: 1 hour, 24 hours, 7 days; a clean period
  resets the level. Blocks expire automatically at `until`.
- **Administrators** see blocks with their evidence counts and can block or unblock
  any domain manually; manual blocks have no automatic expiry unless one is set.
- Blocked mail is rejected, not stored; senders get a permanent 550 for the
  duration. Block state is durable, in sealed backups, and audited without message
  content.

## Security and trust

- Cloudflare and its account administrators can read ordinary mail in transit and in
  R2, and `routes.json` exposes the full address directory to them. Setup says so
  before enabling this profile; Maddy and external IMAP are the alternatives.
- A stolen bearer secret can read and delete waiting mail (listing reveals digests),
  but cannot change routing. Rotation: install a new secret, switch KyPost, remove
  the old one. Both secret and signing key are runtime secrets in sealed backups.
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

## Qualification

Offline: Worker unit and workerd/R2 runtime tests (unknown-recipient reject, size cap,
conditional put, signed/ordered/future-bounded table replacement, list paging,
digest-checked delete, strict paths); backend race tests for publish → capture →
pickup → import → delete killed at every boundary, replay, conflict, generation-change
quarantine, spam-to-Junk, capacity refusal, multi-owner deliveries, restore
reconciliation.

Live, on the test deployment, each with its own bounded plan and approval:
1. Whether a thrown Worker exception gives the sender a temporary failure.
2. Several messages to several addresses and domains, including an unknown one.
3. KyPost stopped while mail arrives; restart drains R2 and deletes provider copies.
4. Address reassignment while mail waits in R2: old-generation mail quarantines.

## Decisions (2026-10-06)

1. Stale routing: option A, 14-day maximum age.
2. Frozen binding at pickup for the hosted profile: approved.
3. Spam after acceptance: delivered to Junk.
4. Size cap: 25 MiB in both profiles, with the ingress limit migration.
5. Pickup interval 30 s; the oldest-unpicked warning threshold is set during
   implementation.
6. Abusive sender domains: blocked with an escalating cooldown, as above.
