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
  discard remains. See
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
operator recovers by redeploying the Worker's bootstrap secrets and clearing
`credentials.json` through the Cloudflare account.

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
backups. After restore, `GET /routes` reconciles: KyPost publishes only above the
stored revision, and R2 items frozen against revisions it no longer has are
quarantined, not guessed.

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
- **Automatic blocks are per sender address** (proposed trigger: 5 reject verdicts in
  1 hour from that authenticated identity).
- **Profile coverage.** Maddy establishes all three and enforces automatic blocks.
  The Cloudflare Worker sees no peer IP, so Rspamd cannot evaluate SPF there.
  Automatic gateway blocks are disabled in the Cloudflare profile until
  qualification shows Email Routing hands the Worker a trustworthy SPF result for
  the envelope domain; until then it uses per-message spam verdicts plus manual
  blocks.
- **Automatic domain blocks are narrow.** A domain is blocked automatically only when
  at least 5 distinct addresses on it are automatically blocked (each meeting the
  identity rule above) within 24 hours
  **and** the deployment has never accepted non-rejected mail from that domain.
  Shared providers your users already receive from are therefore never blocked
  automatically; throwaway spam domains are.
- **Cooldown escalates** per repeat: 1 hour, 24 hours, 7 days; a clean period
  resets the level. Blocks expire automatically at `until`.
- **Administrators** see blocks with their evidence and can block or unblock any
  address or domain manually; manual blocks have no automatic expiry unless one is
  set.
- Blocked mail is rejected, not stored; senders get a permanent 550 for the
  duration. Block state is durable, in sealed backups, and audited without message
  content.

## Security and trust

- Cloudflare and its account administrators can read ordinary mail in transit and in
  R2, and `routes.json` exposes the full address directory to them. Setup says so
  before enabling this profile; Maddy and external IMAP are the alternatives.
- A stolen bearer secret can read and delete waiting mail (listing reveals digests),
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

## Qualification

Offline: Worker unit and workerd/R2 runtime tests (unknown-recipient reject, size cap,
conditional put, signed/ordered/future-bounded table replacement, list paging,
digest-checked delete, strict paths); backend race tests for publish → capture →
pickup → import → delete killed at every boundary, replay, conflict, generation-change
quarantine, spam-to-Junk, capacity refusal, multi-owner deliveries, restore
reconciliation; a restore started without takeover confirmation makes no
`POST /rotate` call and the original's list, fetch, delete and `PUT /routes` keep
succeeding; after a confirmed takeover the original is refused on all four, reports
that it is fenced, and no R2 item is deleted unless the new owner has committed it.

Offline, abuse blocks: five reject-verdict fixtures that pass DKIM for a shared
domain but carry differing envelope senders and From addresses must not block any
address, in both the published table and Maddy's RCPT check.

Live, on the test deployment, each with its own bounded plan and approval:
1. Whether a thrown Worker exception gives the sender a temporary failure.
2. Several messages to several addresses and domains, including an unknown one.
3. KyPost stopped while mail arrives; restart drains R2 and deletes provider copies.
4. Address reassignment while mail waits in R2: old-generation mail quarantines.
5. Whether Email Routing gives the Worker a trustworthy SPF result (gates automatic
   sender blocks in this profile).

## Decisions (2026-10-06)

1. Stale routing: option A, 14-day maximum age.
2. Frozen binding at pickup for the hosted profile: approved.
3. Spam after acceptance: delivered to Junk.
4. Size cap: 25 MiB in both profiles, with the ingress limit migration.
5. Pickup interval 30 s; the oldest-unpicked warning threshold is set during
   implementation.
6. Abusive senders: blocked with an escalating cooldown, per authenticated address
   automatically (Maddy now; Cloudflare after SPF qualification) and per domain only
   under the narrow rule above; manual blocks in both profiles.
