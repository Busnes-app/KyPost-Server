# Changelog

- Web appearance now defaults to Busnes light/dark according to the OS; existing theme choices remain available.

Versions are dotted-numeric (`MAJOR.MINOR.PATCH`) and published as GitHub
releases tagged `v<version>`. The tag, `serverVersion` in
`backend/internal/api/server_version.go`, and `frontend/package.json` must all
agree — `release-image.yml` refuses to publish a release where they do not.

Each release publishes an immutable `ghcr.io/<owner>/kypost-server:<version>`
image. `:stable` is moved to that image only after its build attestation has
been published *and* verified, and only when the release is the newest published
non-prerelease version.

## Unreleased

- Native mail: the outbound worker no longer logs "native outbox discovery deferred" when the API and daemon check the same idle mailbox (typically a disabled extra mailbox) at the same moment, such as at startup. Mailbox validation now waits up to five seconds for the other process's lock instead of treating the mailbox as unprepared. Disabled mailboxes still refuse new mail and new sends.

- Webmail: a user with additional native mailboxes now sees them under "Your mailboxes" in the sidebar and can read, search, file, draft and send from each, sending from that mailbox's addresses. The choice is kept for the browser tab; a mailbox that is disabled or removed falls back to the primary with a notice. An autosaved unsent message goes back to the mailbox it was written in, and is kept rather than restored elsewhere while that mailbox is unavailable. Settings → Filters says which mailbox Run rules now uses.

- KySignOn confirmation of a sensitive action (export, import, PGP, backup and the rest) now works in real browsers. KyPost's Cross-Origin-Opener-Policy made the sign-in window look closed as soon as it opened KySignOn, so the confirmation was cancelled within a second and the finished sign-in was refused with "restart the action". The page now waits for the sign-in until it is confirmed, cancelled with Cancel, or five minutes pass.

- A wrong password or two-factor code when confirming a sensitive action (sender blocks, mail addresses, quarantine, mail domain, relay, backup, recovery, export/import, PGP, SSO link) is now refused with 403 and shown on the page. It was a 401, which the web app took for an expired session and silently reloaded the page. Sign-in and password change keep their statuses.

- Native receiving: mail to a disabled extra mailbox is now refused as an unknown recipient (`550 5.1.1`), like a released or disabled address, instead of `451 4.3.0 Receiving storage unavailable`, which made senders retry for days. Hosted pickup quarantines such mail instead of leaving it waiting.

- Native mail: parallel deliveries, sends and account allocations no longer fail with a spurious "mail domain proof missing, expired or changed" error when another request refreshed the same domain proof at the same moment. Only a changed DNS challenge is refused.

- `POST /api/admin/native-recovery/release` releases a qualified, repaired native restore hold when `KYPOST_NATIVE_RESTORE_RELEASE=true` (default off). Only an unlinked local administrator on a password session with step-up may release; every precondition is rechecked under the fences with a fresh DNS proof, and the bundled receiver requires `"confirm": "original-host-decommissioned"`. The release deactivates non-native accounts the evidence shows inactive and demotes non-native administrators it shows without the administrator role, writes those directory rows inactive, records release floors and token fences, and renames the hold to `native-restore-released.json`. Status then reports `released` and the KyIdentity resync still needed. Do not downgrade below this version after a release.

- `kypost-server restore status` and `GET /api/admin/native-recovery/status` report, read-only, whether a held native restore meets each release precondition and why not, with next steps.

- Native restores now record `state/native-restore-qualification.json` as their last step (hold epoch, each mailbox's new reference generation). A read-only checker reports every reason a restore is not yet qualified for hold release; holds from older versions must be restored again with this version.

- Native restore: durable release floors in `sso-lifecycle.json` stop the provisioning worker, native allocation and the directory webhook from recreating, allocating or reactivating a subject from directory state older than, or contradicting, the evidence a hold release consumed; unchanged active subjects and deactivations still apply. The hold release writes them (design in `docs/NATIVE_RESTORE_RELEASE.md`).

- Native mailboxes now hold real mail: each has a 5 GiB quota (5 × 2^30 bytes; set `KYPOST_MAILBOX_QUOTA_BYTES`, 256 MiB to 1 TiB, for the whole deployment), a 25 MiB message limit and 1,000,000 records, up from 32 MiB, 5 MiB and 10,000. `migrate-native` raises existing mailboxes, primary and extra, at the next start (the ledger first, then each mailbox's database and preparation file, so a crash or a quota changed in between finishes at the following start), and migrates a restored older backup the same way. A quota lowered below a mailbox's usage never locks it: its mail stays readable, movable and deletable and backups still validate; only new mail is refused until it is back under. The receiving buffer is raised in place to 25 MiB per message and 512 MiB held (Maddy now accepts 25 MiB messages, matching Cloudflare). A full mailbox answers `452 4.2.2 Mailbox full` at RCPT and DATA, and new mail is refused with `452 4.3.1` before the state drive fills, keeping 10% of it or 5 GiB free, whichever is larger; both are temporary, so senders retry and nothing is lost. Hosted pickup leaves a full mailbox's mail in R2 and stops at the drive reserve. Imports refuse uploads that would cut into the reserve (checked up front for the declared length plus other uploads in flight, and every 4 MiB while streaming) and stop before the message that would; uploads may now take two hours. A zip whose central directory is over 8 MiB or 10,000 entries, whose headers do not end exactly at the (zip64) end record, or whose zip64 end record does not sit right before its locator, is refused before it is parsed, so the larger upload cap cannot turn into tens of GiB of memory. Settings → Mail → Storage shows each of your mailboxes' usage, warning at 80% and 95%; Server → Mail addresses shows every mailbox's usage and warns when unused quota passes 80% of the free space beyond the reserve. `GET /api/mailboxes` and `GET /api/admin/mail-addresses` carry `usedBytes`/`quotaBytes`, the latter a `storage` summary. Without `KYPOST_BULK_BACKUP_REPOSITORY`, backups fail once a mailbox passes 64 MiB: set it before mailboxes grow. Mail waiting in the receiving buffer counts against its mailbox's quota, and RCPT refuses a mailbox with less room than one 25 MiB message (for that recipient alone; Maddy gives no size at RCPT). Draft saves, appends, imports and outgoing mail are refused at the drive reserve by the mailbox store itself, so a quota larger than the free space cannot fill the shared drive; delivered mail, replays and filing a Sent copy after sending are never refused. An invalid `KYPOST_MAILBOX_QUOTA_BYTES` stops only native mail. `migrate-native` names any mailbox that could not take the limits and leaves the others working. A quota change during a native recovery repair voids its evidence; start a new challenge. Rolling back to an older image needs the pre-upgrade backup: older images refuse the raised receiving-buffer limits.

- Mail bulk backup: set `KYPOST_BULK_BACKUP_REPOSITORY` to a restic repository outside the data volumes (compose: mount it at `/kypost-bulk`) and every mailbox and receiving database is backed up there, streamed to disk rather than memory, instead of inside the sealed capsule, so mailboxes are no longer limited to 64 MiB each and 256 MiB together. The capsule seals the full snapshot ID and each file's SHA-256, and a new `private/bulk-backup.key` (created with the repository, sealed in every capsule) is the restic password. `private/bulk-backup.repo` records the repository ID: a different repository, or an empty directory where the key says one existed (an unmounted share), is refused rather than initialized. `rest:https://` repositories are accepted and recommended with an append-only rest-server, because a local repository is writable by KyPost; the URL is redacted wherever shown. Runs back up from a fixed path under `--host kypost`, so restic retention groups them. Each run first deletes scratch a killed run left behind and refuses to start unless the scratch filesystem keeps 10% or 5 GiB free after a copy of the mail (`KYPOST_BACKUP_SCRATCH_DIR` moves scratch elsewhere). Runs, drills and exports get 16 minutes plus 10 minutes and a minute per 2 GiB of mail (at most 4 hours more). Restore and drills restore exactly that snapshot with `--verify` and `--no-lock` (a read-only mount works), check the repository ID, re-hash every file and refuse missing, extra or altered files before any other check; restore needs the same repository and, when the capsule's recipe says so, refuses before shares are entered. Any restic failure fails the backup. Server → Backup shows the snapshot of the last delivered capsule. Nothing is pruned automatically; see [mail bulk backup](docs/RESTORE.md#mail-bulk-backup) for retention. Without the variable, mail stays in the capsule as before and a backup over the limits fails naming it. The image now includes Debian's `restic`.

- Admin → Server → Sender blocks manages sender blocks from the web UI: the blocks in force (kind, value shown as plain text with hidden characters as code points, source, level, expiry and creation in local time with zone, reason), with automatic domain blocks called out in words because one can cover a whole mail provider; the automatic-evidence status (damaged or reset evidence, when automatic domain blocks start, automatic blocking full, protected-domain record full); adding an address or exact-domain block with optional expiry and reason; and removing a block, which also suppresses automatic re-blocking for 30 days. Each change is confirmed and needs the account password or KySignOn; server refusals (own domain, list full, invalid value) are shown as returned. Adding names the block it replaces and says a full list evicts the soonest-expiring automatic blocks (the notice counts any removed); removing says whether a domain block or address blocks still refuse the sender. Past expiries are refused before sending.
- Untrusted names on the Quarantine, Sender blocks and IMAP import screens now show non-ASCII spaces (such as U+00A0 and U+3000) and the full-width and small @ lookalikes as code points, so a sender like `billing[U+FF20]partner.com` padded with U+3000 cannot pass as another domain.
- Maddy receiving with the Rspamd sidecar now blocks abusive senders automatically. A spam rejection counts against a sender only when the envelope sender equals the message's From address, SPF passes for it on the scanner's own check from the real connecting IP, and a DKIM signature verifies for exactly that domain; `Authentication-Results` headers in the message are ignored. Five counted rejections in an hour block the address for 1 hour, then 24 hours, then 7 days; 30 clean days reset the level. A domain is blocked only after 5 of its addresses were blocked within a day, only if no authenticated mail was ever accepted from it, and only after 30 days of collecting that history. Automatic blocks use at most half of the block list and are evicted, soonest-expiring first, before an administrator's block is ever refused; an automatic block never evicts another, so past that share new ones are refused and the block list reports `automaticFull`. Domains recorded as good are never evicted (at most 50,000, and 50 per parent domain); a full record stops recording and reports `goodFull`. Your own domains, the null sender and senders under a manual block are never blocked automatically; unblocking stops automatic re-blocking for 30 days and always removes the block, with a warning if that suppression cannot be recorded. Evidence (`STATE_DIR/receiving/sender-evidence.json`: block IDs, counts and times only, under 6 MiB) is sealed in backups when valid; a damaged copy is set aside and counting restarts, and the block list response reports it. Evidence work never delays an SMTP reply by more than 2 seconds. The CLI now audits with the API's action names (`block_sender`, `unblock_sender`). The Cloudflare profiles collect no evidence.
- Sender blocks: every sender either receiving profile accepts can now be blocked (non-ASCII local parts, underscores and other characters in domains), and both profiles accept the same senders: Maddy now refuses quoted local parts, domain literals and internationalized sender domains at RCPT with `550 5.1.7`, as the Cloudflare Worker already did. Blocks compare after lowercasing A-Z only, identically in KyPost and the Worker; the Worker no longer lowercases non-ASCII in sender domains, so a sender domain with the Kelvin sign is refused instead of read as `k`. The block list's size is measured exactly as the routing table encodes it, and blocks that no longer fit beside the routes are left out of the Cloudflare table (manual before automatic, newest first) with an error in status, so routes always publish. Maddy answers `451 4.3.0 Sender blocks unreadable` (rather than a storage error) while the list is damaged. Adding a mail domain that a block matches is refused until you unblock it, and backups refuse a malformed block list.
- Administrators can block abusive senders by address or by domain in both native receiving profiles: `kypost-server receiving blocks list|add|remove` (the value repeated after `--confirm`) or `GET|POST /api/admin/receiving/blocks` and `DELETE /api/admin/receiving/blocks/{id}` (account or KySignOn confirmation; the URL carries the block ID, never the address). Maddy refuses a blocked sender at RCPT with `550 5.7.1 Sender blocked` before storing anything; the Cloudflare Worker rejects it from the signed routing table, which is republished within seconds of a change. A block can expire at a set time (`--until`, or `until` in Unix ms) and carries a reason code (`spam`, `phishing`, `abuse`, `other`), never free text. Domain blocks match exactly, not subdomains; internationalized domains are given as A-labels. Your own mail domains and addresses on them cannot be blocked, and bounces (the null sender) are never blocked. Already accepted mail is unaffected. If the block file becomes unreadable, Cloudflare routes keep publishing with the last published blocks (status shows the error), so a damaged file can never let the routing table age out. Blocks live in `STATE_DIR/receiving/sender-blocks.json`, are sealed in backups and are audited with a block ID, never the address; at most 5000. Automatic blocks and an admin screen are later changes.
- Continuous Cloudflare mail that waited through a restore and takeover is no longer stuck: it arrives quarantined as "original owner unknown" (captured under a routing table the restored server never published, so its owner cannot be proven), and Server → Quarantine offers Release to current owner…, which delivers it to the recipient address's owner today, at its current generation and only while the address is active, after a confirmation saying that owner is not proven to be the original. The release names the mailbox the administrator reviewed and is refused if the address moved to another owner since. API: `{"toCurrentOwner": true, "currentMailbox": "<as listed>"}` on release, audited separately; CLI: `receiving quarantine release-to-current-owner <gateway> <id> <currentMailbox> --confirm <id>`. An ordinary release of such mail now explains this option instead of asking to resync. Pickup isolates stuck mail: a message refused for a passing reason (domain proof, admission, scanner) waits from one minute up to an hour before it is fetched again, so it no longer crowds out newer mail, and a message too large for its mailbox is quarantined with its bytes instead of stopping pickup; only a full receiving store stops it. The Worker now refuses envelope senders KyPost cannot hold (quoted or malformed local parts, domain literals, over 320 bytes), so the sender gets a bounce. Refused provider objects are counted separately in status and no longer trigger the oldest-mail warning. A corrupt credential file shows as an error in status.
- Continuous Cloudflare receiving, KyPost side (offline tests only; not qualified live). Set `KYPOST_CLOUDFLARE_RECEIVING_ORIGIN` to the Worker and the daemon publishes, every 30 seconds and within seconds of an address change, a table of every admitted active address on a proven mail domain, signed with KyPost's own Ed25519 key, re-signed at least hourly and always at a revision above both its own and the Worker's last one. An address whose domain proof or admission fails for a moment keeps its route; only a release, disable, reassignment, offboarding or domain removal drops it. Pickup reads the whole queue each cycle, checks every message against the owner and address generation frozen when Cloudflare accepted it (a mismatch, or a table revision this instance never published, is quarantined with its bytes and never delivered to the address's current owner), scans it with the local Rspamd sidecar (a reject verdict files it to Junk, because Cloudflare already accepted it; scanner failure or soft reject retries later) and imports it. The provider copy is deleted, by its digest, only after the local receiving store holds it imported or quarantined; a lost delete is retried. A lapsed domain proof leaves that domain's mail waiting in R2, and a full receiving store stops fetching. Credentials (bearer and signing key) live owner-only in `SECRET_DIR` and are sealed in backups together with the pickup ledger and published tables; a host-local marker that is never backed up makes a restored copy start fenced, contacting nothing. `kypost-server receiving cloudflare init` creates them and prints only the bearer's SHA-256 and the public key to deploy as Worker secrets; `rotate` replaces both; `takeover --confirm move-receiving-here` moves receiving to a restored host, and the previous host is refused on every route, stops and reports itself fenced. Status: `kypost-server receiving cloudflare status` and `GET /api/admin/receiving/cloudflare` (counts and times only). Mail waiting over an hour is flagged. Abuse blocks, the takeover screen and removing the pilot are later changes.
- Continuous Cloudflare receiving, Worker side only (`receiving-worker/continuous.mjs`; KyPost does not use it yet and nothing is deployed). It accepts mail only for addresses in a routing table signed by KyPost's Ed25519 key, refuses blocked senders and unknown addresses before reading the message, enforces each address's size cap and stops accepting once the table is 14 days old. Accepted mail is stored in private R2 under a time-ordered key with a frozen envelope, never overwritten; a storage failure is not a bounce. KyPost picks up over HTTPS with a dedicated bearer secret checked before any mail is read: list, fetch, delete only on digest match, read and install the table (newer revisions only) and rotate the bearer and signing key, which immediately locks out the previous holder. Signing keys must be valid prime-order Ed25519 points and a rotation must replace both the bearer and the key; addresses must be ASCII (internationalized domains as A-labels). Running it needs Workers Paid. The one-message pilot is unchanged. Wire contract: [continuous receiving](docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md#wire-contract).
- Users can import from another mail account over IMAP into their own native mailbox under Settings → Mail → Import Mail → From another mail account: server, port and security (TLS on 993 or STARTTLS on 143, certificate always verified), username and password or app password. After the account or KySignOn confirmation (which never carries the provider password) the server signs in, lists the folders (Gmail's All Mail and Starred start unchecked, since they repeat other folders) and copies the chosen ones under `Imported/<host>` as a background job with folder progress and cancel. The provider is opened read-only (EXAMINE, BODY.PEEK; nothing is changed or deleted there); each message keeps its exact bytes, received date (INTERNALDATE), read state and flag (as starred). The password is held in memory only until the job signs in and is never stored, logged or shown; the audit names the host but never the username. Only public addresses are dialled: a name resolving to any loopback, private, link-local, CGNAT or reserved address is refused, and the validated address is the one connected to. Messages announced over the per-message limit are skipped without downloading, and a message larger than announced, a folder announcing more messages than the job may handle, or a session reading more than twice the mailbox's storage (every byte from the server counted) stops the job, which runs at most 4 hours. Failed sign-ins are limited per user, across confirmations: six in an hour, then listing is refused for an hour; at most four listings connect at once server-wide. Duplicates are skipped, so importing again resumes. Same limits and incoming-encryption refusal as file import. API: `POST /api/import/imap`, `POST /api/import/imap/{token}/folders`, `POST /api/import/imap/{token}/start`.
- Users can import mail into their own native mailbox under Settings → Mail → Import Mail: an mbox file (mboxrd or mboxo, CRLF or LF; Thunderbird's extensionless folder files work), one EML, or a zip of EML files, into an existing folder or a new one (default `Imported`). Starting needs the account password or a KySignOn confirmation and gives a single-use upload link for that browser session; the upload shows progress and goes to a temp file under the state directory, at most the mailbox's storage quota (32 MiB today). A background job then stores each message as parsed from the file, marked read and never processed by the poller even if later marked unread, dated by its `Date` header; it survives the browser leaving, shows imported, duplicate and skipped counts, and can be cancelled. Messages already in the folder are skipped as duplicates, so re-importing after a restart (which loses the job; its temp file is removed at startup) is safe. Empty, header-less and oversized messages (over the mailbox's 5 MiB message limit) are skipped and counted without stopping the import; a zip with more than 10,000 entries or expanding beyond the quota is refused, and an entry inflating more than 100-fold is skipped. A job handles at most 20,000 messages (counting duplicates and skipped ones) and an upload at most 20 minutes, so floods and trickling uploads cannot hold a slot. mboxo files lose one `>` from a genuine `>From ` line, and an unquoted body line `From ` after a blank line splits a message. Imported mail is not scanned for spam, not run through rules and sends no notifications. Import is refused while incoming encryption is on or pending, and stops if it is turned on mid-import, because imported mail would be stored unencrypted. One import runs per user and two per server; each is audited without subjects or addresses. Users whose mail stays on an external IMAP server, and administrators, cannot import. API: `POST /api/import`, `POST /api/import/{token}`, `GET /api/import`, `POST /api/import/cancel`.

- Users can export their own native mailbox, or one folder of it, under Settings → Mail → Export Mail, as an mbox file (mboxrd) or a zip of EML files. Both hold the exact stored bytes, so encrypted messages stay encrypted; flags and labels are not included. Exporting needs the account password or a KySignOn confirmation and gives a single-use download link valid for five minutes in that browser session only. One export runs per user and two per server; each is audited without subjects or addresses. The screen shows how many messages are being downloaded; a refused download returns to the screen with the reason, and Retry when another export was still running. mbox needs HTTP/1.1 between a reverse proxy and KyPost (nginx: `proxy_http_version 1.1;`), because over HTTP/1.0 a cut-off download looks complete; choose the EML zip otherwise. A slow connection gets time in proportion to each message's size, and a stalled one gives up its slot. Windows device names (`CON`, `NUL`, `COM1`…) among folder names are prefixed with `_` in the zip. External IMAP accounts are told to use their provider's export, and administrators cannot export anyone's mail. API: `GET /api/export/folders`, `POST /api/export`, `GET /api/export/{token}`.

- Server → Quarantine reviews quarantined native mail: received time, envelope sender, recipients with the mailbox and user each was frozen to, size and gateway/ID, 100 at a time with Load more, and never the body or subject. The sender and every other field render as plain text, with control, bidi, zero-width and blank-letter characters and stacked combining marks shown as `[U+XXXX]` (a run of one code point as `[U+XXXX ×N]`); a hostile sender wraps inside its own column and cannot squeeze or paint over the rest of the table. A recipient with no user reads "mailbox gone or owner changed; release will be refused". Times show their time zone. A cancelled KySignOn or local credential failure says nothing changed and keeps the screen usable. Release confirms the frozen mailboxes and that the mail never goes to an address's current owner; Discard confirms the bytes are deleted for good and that an interrupted release may leave it partially released, which is shown as a warning. Both need account or KySignOn confirmation. Refusal reasons are shown as returned and the list re-read; an unanswered or mismatched answer locks the screen until reload; with native mail off the tab says so.

- Quarantined native deliveries can now be released or discarded instead of waiting forever. Administrators list them with `GET /api/admin/receiving/quarantine` or `kypost-server receiving quarantine list` (envelope sender, recipients, frozen mailbox and owning user, received time and size; never the body, subject or headers), then release or discard each one with account or KySignOn confirmation (API) or the delivery ID repeated after `--confirm` (CLI). Release goes only to the mailbox the mail was frozen to, never to an address's new owner, and only while that mailbox exists, is active and still belongs to the same KyIdentity subject; otherwise it is refused with the reason and discard remains. Release reuses import, so an interrupted attempt never duplicates or loses mail. Discard removes the bytes and keeps a tombstone so a replayed receipt or re-pickup cannot bring it back. Both are audited without message content and refused under a restore hold. Receiving tombstones now record whether a delivery was imported, released, discarded or partially released (discarded after an interrupted release that may already have reached some mailboxes); stores from before this change gain the field on open. Release refusals say whether the frozen mailbox is durably gone, disabled or offboarded (discard remains) or only not admitted right now (resync and retry). Accepted mail waiting for a disabled mailbox, or for an owner KyIdentity offboarded or promoted to administrator, is now quarantined instead of retried forever, so it no longer holds the receiving budget; re-enable the mailbox and release it if it is still wanted. The CLI must run as the state owner (`docker compose exec --user kypost kypost-server kypost-server receiving quarantine ...`), logs its actions as `cli:<uid>` to the terminal only, and leaves the tombstone and mailbox receipt as the durable record.

- Native receiving no longer stops after 10,000 imported messages. Once every recipient's mailbox has committed a delivery, the acknowledgment replaces its receiving record with a small tombstone in the same transaction; the 10,000-record limit now counts only waiting (staged, pending and quarantined) mail. Tombstones keep an exact receiver or pickup replay from delivering the message again; sealed backups carry them while `ingress.db` stays under the 64 MiB per-file cap (about 360,000 typical tombstones). Receipts acknowledged by an earlier or downgraded version are converted when the receiving store opens. Tombstones are not pruned yet: typically 190-210 bytes (about 2 million in the receiving storage budget), but a flood to many aliases with long senders fits about 170,000, and a full budget stops reception, so pruning remains a public-MX gate. Backups now snapshot the receiving store before any mailbox, so a message imported while a backup runs is never restored as a tombstone without its mail. Receiving audit lines record `replayed` for an already archived delivery.

- Server → Mail addresses manages additional mailboxes: New mailbox creates one for a user, and each additional mailbox offers Disable or Enable, with account or KySignOn confirmation. Creating confirms that the address is held permanently and the mailbox cannot be deleted, that the user selects it in their mail client and that incoming encryption is unavailable to them while it is active (disabling it restores the option); disabling confirms that delivery and sending stop at once, mail is kept, the user cannot open it until it is enabled again and queued outgoing mail is quarantined for good. Each mailbox shows its kind and state; an unfinished one shows how to finish it and offers no actions, and primary mailboxes offer none either. Server refusals (such as an owner with incoming encryption on) are shown as returned, and every answer is validated before display.

- Extra native mailboxes (backend): administrators give an everyday user further mailboxes, each with its own primary address on an established domain, through `/api/admin/mailboxes`, and disable or re-enable them; administrator and `legacyMixedUse` subjects get none. Each mailbox has its own storage under `$STATE_DIR/mailboxes/`, its own classification state, outbox and Sent folder; devices, notifications and sorter learning stay with the user's primary mailbox. Disabling a mailbox stops its delivery, sending and access and keeps its mail; deactivating or promoting the KyIdentity subject disables all of its mailboxes. Clients list their mailboxes with `GET /api/mailboxes` and select one with the `X-KyPost-Mailbox` header; without it every route serves the primary, so existing clients are unchanged, and an unknown, foreign or disabled mailbox answers one 404. Native notifications carry the mailbox ID. Sorter learning covers the primary mailbox only, and incoming encryption and extra mailboxes exclude each other: an extra mailbox cannot be created or re-enabled while its owner has incoming encryption on (or a replacement pending), incoming encryption cannot be enabled while the user has an active extra mailbox (the Security page shows why; an administrator disabling every extra mailbox lifts this); both checks are made under the user's settings lock, so concurrent requests cannot leave both on, and the poller never polls an extra mailbox past that rule. `POST /api/rules/run` honours the mailbox selection. Disabling an extra mailbox quarantines its queued outbox jobs for good. A creation interrupted before its storage exists is skipped by polling and outbox recovery until it is repeated. Backups carry extra mailboxes; restore validation refuses device or notification rows in one and rotates its message references, and the recovery authority digest includes them, so creating, disabling or re-enabling one needs a new recovery challenge.

- Server → Mail addresses lists native mailboxes with each address's kind, state and generation, filterable by user, and adds, releases and reassigns aliases with account or KySignOn confirmation. Adding, releasing and reassigning ask first and name the consequences (an added address is held permanently); primary addresses offer no actions. A change saved while its receiving route is still pending shows the server's warning beside the success; server refusals are shown as returned and the list is re-read; only a request that never got an answer locks the screen until reload. The list and every answer are validated before display. Here and on Mail domain, a change that was saved but whose follow-up read failed now says so instead of showing only the read error.

- Native aliases: administrators add aliases to an everyday user's mailbox, release them and reassign released ones through `/api/admin/mail-addresses`; administrator and `legacyMixedUse` subjects get none. Mail to an alias lands in the owning mailbox (once per message, however many of its addresses it names), and native compose and client-PGP sends accept an owned active alias as `from`; anything else stays 403. Every address now has its own generation, changed only on release, reassignment, disable and re-enable: ordinary KyIdentity edits no longer quarantine accepted mail, while deactivation, reactivation and promotion disable or re-enable a subject's addresses inside the directory update itself (a failed ledger or route write fails the webhook and KyIdentity retries). Mail and queued outbox jobs frozen against an older generation are quarantined, never delivered to a new owner. Released addresses stay reserved, never deleted. Restore validation checks routes and bindings against each address's history, and the recovery authority digest includes every address, so an outstanding recovery challenge needs a new one after upgrade. Existing ledgers are switched over safely: generations are raised once to the directory revision their routes carried.

- Add `Date` and `Message-ID` to server-composed mail (compose, Sent copies, drafts, pickup and system notices, send-as probes). Each message is stamped once, so the delivered bytes, native outbox retries and the Sent copy share one Message-ID; PGP envelopes carry them outside the signed/encrypted part. The plaintext IMAP Sent copy is now the delivered bytes plus Bcc rather than a rebuild, so it also keeps the sent From. Client-supplied PGP MIME is unchanged. AWS SES replaces both headers, so the delivered Message-ID differs from the Sent copy there.

- Support several verified native mail domains under one KyIdentity issuer (backend; the admin screen still manages the founding domain). New admin routes list, add, verify and retire domains (`/api/admin/mail-domains`); the existing `/api/admin/mail-domain` routes keep serving the first configured domain. Each domain keeps its own DNS proof: re-verifying or a lapsed proof on one domain never blocks allocation, receiving or sending on another. A primary address may sit on any configured domain. Retiring a domain is refused while an address on it is active or a queued/retryable outbox job sends from it; its address records are kept. The relay accepts a `domains` set: adding or removing a domain alone keeps the relay generation, removal is refused while queued/retryable jobs use it, and removed domains stay valid for backup validation of older jobs. The generated Maddy profile lists every configured domain; regenerate it and restart the receiver after a domain change. A version-1 or phase-1 binary refuses a set with several domains. Retirement is also refused while accepted (pending) incoming mail is bound to the domain, while the relay still sends for it (remove it from the relay first) or under a restore hold. A retired domain can be re-added: it starts with a new challenge and carries no mail until verified again, then its kept addresses resume for their recorded mailboxes and it can rejoin the relay without changing the relay generation; until then a subject whose primary is on it cannot be provisioned. Relay updates need fresh DNS only for newly added domains (retained ones prove when they can; at least one must), so one lapsed domain no longer blocks a credential rotation. Queued mail whose sending domain was retired or removed from the relay is quarantined instead of retried forever, and a deactivated owner's queued mail is quarantined even while DNS is down. A primary address on an unconfigured domain is recorded as a provisioning failure instead of being retried.

- Server → Mail domain manages the verified domain set: each domain shows whether it is established, awaiting verification (with its TXT record and expiry), lapsed or retired, the founding domain is marked, and domains can be added, verified, re-challenged, retired and re-added with the usual account or KySignOn confirmation. Retiring asks first and names what refuses it, and is unavailable while the relay sends for the domain; adding a domain already in service is refused instead of silently replacing its challenge. Server refusals and migration errors are shown as returned and the screen re-reads status; only a request that never got an answer locks the screen until reload. Retiring under a restore hold now returns the hold remediation instead of a generic refusal. The relay section picks its sending domains and saves a domains-only change without re-entering relay credentials. Against an older server the screen keeps the single-domain view.

- Support several verified native mail domains under one KyIdentity issuer. New admin routes list, add, verify and retire domains (`/api/admin/mail-domains`); the existing `/api/admin/mail-domain` routes keep serving the first configured domain. Each domain keeps its own DNS proof: re-verifying or a lapsed proof on one domain never blocks allocation, receiving or sending on another. A primary address may sit on any configured domain. Retiring a domain is refused while an address on it is active or a queued/retryable outbox job sends from it; its address records are kept. The relay accepts a `domains` set: adding or removing a domain alone keeps the relay generation, removal is refused while queued/retryable jobs use it, and removed domains stay valid for backup validation of older jobs. The generated Maddy profile lists every configured domain; regenerate it and restart the receiver after a domain change. A version-1 or phase-1 binary refuses a set with several domains. Retirement is also refused while accepted (pending) incoming mail is bound to the domain, while the relay still sends for it (remove it from the relay first) or under a restore hold. A retired domain can be re-added: it starts with a new challenge and carries no mail until verified again, then its kept addresses resume for their recorded mailboxes and it can rejoin the relay without changing the relay generation; until then a subject whose primary is on it cannot be provisioned. Relay updates need fresh DNS only for newly added domains (retained ones prove when they can; at least one must), so one lapsed domain no longer blocks a credential rotation. Queued mail whose sending domain was retired or removed from the relay is quarantined instead of retried forever, and a deactivated owner's queued mail is quarantined even while DNS is down. A primary address on an unconfigured domain is recorded as a provisioning failure instead of being retried.

- Separate administrators from native mailboxes: with native mail enabled, a KyIdentity subject holding `kypost.admin` now gets an ordinary mailbox-less KyPost account (no address reserved, no storage prepared), and native mail access, sending and receiving refuse administrator subjects. Administrators who already held a mailbox at migration keep it through `legacyMixedUse` until the mixed-use migration ships; demotion clears that flag for good. Promoting an everyday user refuses their mailbox from the next request and keeps all mail; demotion restores it. Outgoing mail queued before a promotion is quarantined and does not resume after demotion, and a mailbox reservation not yet published when its owner was promoted stays orphaned (kept, never delivered to). Demoting an administrator never turns their account into a mailbox. Refused requests answer 403 with `administratorIdentity: true` and "administrator identities have no mailbox; use your everyday identity", never 401, so clients keep their session and device pairing. With native mail enabled, administrators also cannot save or test personal IMAP settings; IMAP-only deployments are unchanged, and IMAP settings saved earlier are kept until the mixed-use migration.

- Move native mail storage to the addressing v2 formats (domain set `native-domains.json`, ledger version 2 with mailboxes and per-address generations, relay version 2 with a domain set) and convert existing deployments at container start with `kypost-server migrate-native`, run as the runtime user. Migration is crash-safe and idempotent, keeps the relay generation, queued jobs, receiving bindings and restore holds, and fails closed: half-migrated state and version-1 binaries refuse native mail while IMAP keeps working. Backups accept both formats. Configuring a mail domain now initializes an empty native ledger, so restoring a deployment with a configured domain but no native accounts is held like a relay-only restore, with no release path yet. Still one domain and one mailbox per user. Roll back by restoring the pre-upgrade backup.

- Add controlled one-message Cloudflare Email Worker/R2 capture and private manual HTTPS pickup, with frozen native ownership, exact bytes and durable replay receipts. Require local content scanning without invented SMTP peer provenance; retain provider bytes on refusal and after import. No continuous receiving, automatic provider cleanup or client contract changes.

- Add optional pinned local Rspamd sidecar and bounded pre-commit native SMTP filtering; defer scanner failures, preserve accepted replay and mailbox bytes, and keep existing IMAP/client behavior.

- MIME-encode non-ASCII subjects in generated messages and stored copies for SMTP relays that require seven-bit headers; preserve header sanitization and client-encrypted PGP/MIME.

- Retain bounded native SMTP rejection codes and outbox correlation IDs in shared API/daemon logs without provider replies, credentials or correspondence; preserve delivery, retry and client-response semantics.

- Restore private data-root permissions during startup under restricted Kubernetes capabilities without requiring CAP_FOWNER, and remove that capability from Compose; preserve retained mail and descendant modes.

- Add opt-in Android emulator native-mail qualification using real pinned HTTPS registration, Keystore pairing preferences, Room/native references, bodies, labels, attachments and ordinary TLS relay sending/Sent. Preserve production interfaces and keep PGP enrollment, physical devices and provider delivery as separate gates.
- Fence pre-restore browser/device ID tokens for historically qualified published native accounts with durable directory issuance cutoffs, including accepted future clock skew. Preserve higher floors, legacy authority and the native hold.

- Join actual SMTP receiving and daemon import to the production HTTPS native mail API, checking both frozen owners, exact binary attachments and cross-owner reference refusal without IMAP. This adds qualification only.
- Bound verified ID-token issuance timestamps in the shared browser/native verifier: require positive iat and at most 30 seconds of future clock skew before access grants. Preserve browser expiration and native five-minute token age; no native restore release is enabled.
- Qualify mailbox/receiving backup refusal when committed WAL data exceeds the per-file cap although the main database fits. Check explicit failure, no local capsule or leftover scratch, and retained original rows; backup limits remain unchanged.
- Qualify internal held-native repair planning and an atomic native-only access batch with intent-before-write ordering. Reverify current receipt/storage authority, preserve legacy credentials and PGP data, and retain the hold. Persist native-only repair intent/provenance barriers before account writes and qualify explicit before/after completion, interrupted/repeated repairs and newer-authority invalidation. Expose separately confirmed `POST /api/admin/native-recovery/repair`, retaining the original administrator/session proof through account publication and revoking native sessions, device/push, pairing and CardDAV credentials before completion. Cleanup failure stays held and incomplete. No hold release is supplied.

- Deny held native password/derived, MFA, SSO/step-up, session/admin and QR key-exchange authority independently of native feature flags. Preserve recovery material/nonces and legacy recovery administration; no hold release is introduced.

- Add protected, challenge-bound KyIdentity recovery-evidence storage and durable revision barriers for unpublished reservations in held native restores. Preserve ordinary published-account offboarding/demotion and permit ownership-validated held device/subscriber revocation without mail access. Preserve account state and the hold; fence local admin authority cycles and support bounded large exact-action uploads. Evidence import alone grants no account repair or hold release; separately confirmed held repair is described above.

- Align the contribution template with human-only attestations: record personal verification and trust-boundary explanations separately from agent checks and CI/review evidence.

- Give each offline native restore/quarantine attempt a fresh hold epoch, including retries and failed validation. Preserve holds on entropy/write failure; no restore activation or hold release is introduced.

- Qualify refusal of signed recovery evidence by ordinary directory sync without account, revision or hold mutation; document the available KyIdentity exporter and pending challenge/repair boundary.

- Web reader shows a "Copy code" card with a never-share warning for a verification code in INBOX mail under 15 minutes old; never on `$Phishing` mail. It makes no claim about the sender and no network request. Detects 5-8 digit and evenly grouped codes (`123 456`) near a code word, and 4-digit, alphanumeric and mixed-case codes (`K7P2QX`, `G-123456`, `a7Bx9k`) only after "code is"/"code:"/"is your … code", by bounded pattern matching, never the classifier.

- Revoke restored native CardDAV app-password hashes and deny held native accounts on both cached and fresh CardDAV authentication; preserve legacy/original credentials and contact data.

- Fence native HTTP reads/actions and new notification references with the persisted mailbox reference generation. Reject stale/foreign/bare numeric IDs before mail access, retain internal numeric/encryption identities and legacy IMAP behavior, and qualify actual snapshot ID reuse plus browser body-cache separation. Native restore remains held.

- Revoke restored native device/browser push registrations and outstanding pairing-token authority before publication, atomically per exact-source account. Preserve mail, receiving receipts and wrapped key material; legacy IMAP registrations remain unchanged. Native recovery stays held and will require fresh device pairing/enrollment without disabling MFA.

- Quarantine restored queued/retryable native outgoing deliveries before offline restore publication, preserving encrypted intent, claims and accepted/Sent evidence. Retain the whole-stack restore hold and refuse publication on quarantine failure; no automatic resubmission or hold release.

- Join native signed-directory allocation, real device pairing/registration, durable receiving-store import, device mailbox/attachment/keyword reads and actual TLS ordinary/PGP sending in one regression fixture without IMAP. Physical clients, app/Maddy admission and live provider delivery remain separate checks.

- Add a repeatable host-side controlled-domain setup wizard using the existing protected domain/relay UI and supervised receiver profile. Confirm public configuration/rebuild/init/start changes, protect dotenv writes and retain explicit external delivery/backup checks; no provider secrets or DNS automation.

- Qualify supervised receiving shutdown during incomplete SMTP DATA: retain staged recipient reservations without publishing partial payload, preserve previously accepted mail and import it after restart. The check uses the actual image and pinned engine; stalled storage/helpers and final-ack races remain separate qualification work.

- Add an optional supervised Linux x86_64 direct-receiving Compose profile with explicit SMTP publication, operator-supplied read-only pinned engine/TLS mounts and fresh startup admission. Qualify real-image automatic receiver restart, durable import and idle shutdown; public MX and full turnkey deployment remain gated.

- Protect container config, private-key and state roots with owner-only permissions at build and before bootstrap on every start, including mounted volumes. Preserve existing mail bytes and descendant modes; qualify the real entrypoint in isolated Docker.

- Allow 36 minutes for container shutdown so Supervisor can finish separate API and daemon backup drains sequentially. Add cumulative-budget and real-Supervisor regression checks; individual backup deadlines remain bounded to 16 minutes.

- Generate a controlled TLS-only Maddy receiving profile from verified domain and existing storage, with certificate/path validation and bounded message/transaction policy. Receiver installation and public MX remain explicit qualification steps; the optional supervised profile is described above.

- Refuse new local receiving growth at physical database/WAL and filesystem free-space reserves, with transaction-scoped checks and headroom-gated near-budget checkpoint recovery. Preserve exact receipt retries and accepted-mail import through restricted unchanged-route refresh. These admission estimates do not replace operator volume quotas or enable public reception.

- Add an admin-confirmed saved-relay TLS/AUTH check without submitting mail. Bind it to the displayed generation and fresh domain/issuer/restore authority before and after connection; bound cancellation, suppress provider replies and expose a transient result in Server → Mail domain. Delivery and receiving remain separate qualification steps.

- Add Server → Mail domain setup for issuer-bound TXT proof and operator-owned implicit-TLS relay settings. Reuse account confirmation, derived credentials, CSRF and identical KySignOn step-up replay; clear secrets and stop submissions after account changes/unmount. Require confirmation before challenge rotation and distinguish saved capability from tested delivery or public receiving readiness.

- Add opt-in native primary-address compose/client-PGP relay sending, current identity/domain/device admission, local revocation epochs, durable intent before SMTP, owner-scoped outbox status and joined recovery workers. Preserve confirmed-send and PGP custody/enrollment contracts. Qualify actual local TLS, signed encrypted recipient/Sent verification, uncertainty and independent Sent/follow-on recovery. Native pickup/alias/system sending and live provider readiness remain pending.

- Add encrypted native outbox intent in mailbox.db, owner/namespace/job-bound keys, transactional delivery claims, bounded definite-4xx retries, uncertain/crashed-claim retention and independent quota-reserved Sent receipts. Sealed snapshot checks preserve claims/Sent and refuse missing keys, orphan/partial schemas and quota corruption. Qualify with actual local TLS SMTP and killed submitters; native primary runtime integration is described above; pickup/alias/system paths remain gated. Shared encrypted reads now reject malformed nonce lengths without panicking.

- Add admin-only encrypted operator-owned domain relay settings with fresh issuer-bound DNS proof, account confirmation, redacted reads and generation rotation. Qualify strict TLS/AUTH transport with safe provider errors; sealed backups validate relay ciphertext, its dedicated key and historical domain binding, including relay-only restore quarantine. This does not enable native sending or prove provider readiness.

- Distinguish lost or invalid final SMTP DATA acknowledgments from definite rejection. Preserve encrypted pickup messages when their notification may already have reached the relay; report uncertainty without echoing untrusted relay response text and require provider evidence before retrying. Partial blind-copy and pickup-link warnings describe unconfirmed delivery and warn against duplicate retries, including alongside Sent-copy warnings. Native domain relay/outbox sending remains unavailable.

- Add separate `KYPOST_NATIVE_RECEIVING=true` qualification opt-in: trusted-local RCPT/DATA commands durably bind and accept mail, then the daemon commits frozen-owner mailbox receipts before acknowledgment without IMAP. Existing-only storage, live directory/users fences, pipe deadlines, generation quarantine and partial-delivery recovery refuse unsafe fallback. No bundled public SMTP receiver or native outgoing relay is enabled; public-MX deployment and restore-hold release remain gated.

- Add explicit `KYPOST_NATIVE_MAIL=true` provisioning and native mailbox runtime in API and daemon, preserving linked IMAP accounts. Retained signed directory events prepare accounts before publication and retry pending work; current issuer/activity/role/storage checks guard cached reads and every mail operation. Native IMAP assignment and legacy SMTP/pickup/probe paths refuse access. Reception requires the separate qualification opt-in described above; domain relay/outbox sending remains unavailable and native restores remain quarantined.

- Guard native account state access in API/maintenance and daemon paths before cache returns. Validate acknowledged owner/source/storage and refuse restore holds; open native state as existing-only with source validation before migration, preserving borrowed handles and inactive-account revocation. Runtime provisioning, mailbox selection and mail transport remain disabled.

- Validate native backup ownership across accounts, reservations, namespaces and receiving bindings. Restore privately before publishing an absent native target with a persistent hold; retain failed staging and refuse allocation while held. No automatic release or native runtime activation is enabled; current-authority repair, stale-ID fencing, capacity and power-loss qualification remain gates.

- Snapshot internal mailbox.db and ingress.db during sealed backups instead of copying main files without committed WAL rows. Restore drills check their SQLite integrity; external IMAP mail stays excluded. Whole-stack ownership/freshness reconciliation and mail-sized backup capacity remain gates before native activation.

- Add admin mail-domain DNS challenge/verification with account step-up and an issuer-bound claim. Add disabled native account allocation that prepares/acknowledges storage before users.json publication, retains issuer/source ownership and refuses legacy adoption or native relinking. Bound lock contention and proof lifetime with cancellable contexts. Reception, runtime source selection and provisioning workers remain disabled.

- Add internal revision-fenced native mailbox reconciliation with durable account/address reservations and pending/applied/failed preparation status. Retain ownership through offboarding and failed retries; fail closed on a missing ledger or acknowledged storage. Runtime provisioning and reception remain disabled.

- Retain verified directory resource fields with the access/revision fence for future provisioning repair. Add internal atomic native account preparation with source-bound state, no-replace publication, read-only retry validation and killed-process checks. Existing accounts are never adopted or recreated; production provisioning, source selection and receiver readiness remain disabled. Reuse pinned x/sys v0.48.0 without adding a dependency.

- Document the turnkey domain mail stack implementation plan: durable reception, KyPost-owned mailboxes, KyIdentity provisioning and operator-owned outgoing relay. No runtime behavior changes.
- Add test-only SQLite/MIME API feasibility and pinned receiving-gateway process-crash checks, plus the mail-operation caller audit. Receiver adoption and production storage gates remain open; no native mailbox or mail transport is enabled.
- Add the internal receiving holding store: immutable recipient bindings, atomic MIME/receipt acceptance, bounded admission, quarantined ownership conflicts and fenced importer claims. Pinned Maddy integration checks cover restart/lost-ack recovery and full-spool refusal. Production reception and KyIdentity/mailbox integration remain disabled.
- Add internal permanent mailbox storage and its receiving-buffer importer: exact raw bytes, immutable owners, transactional import receipts, folders, flags, labels, stable IDs and durable changes. SQLite quota counters coordinate writers; crash/retry checks preserve holding bytes until every recipient commits. The complete internal mail Client supports bounded MIME reads, attachments, search, atomic labels/actions, folders and drafts/Sent; authenticated API checks cover owner isolation and signed/ciphertext PGP reads. Malformed MIME fails one message explicitly while retaining raw bytes. Native incoming encryption reuses the ciphertext-only journal/key reservation and atomically commits replacement/tombstone/receipt; namespace and exact-byte guards recover lost acknowledgements without duplicate mail. Immutable account/cache source guards refuse native adoption of legacy state, mode switching and changed database namespaces. Native HTTP returns fresh full snapshots with numeric IDs and cursor 0; durable scoped deltas, verified provisioning and production deployment remain pending.

- **On-device embedding sorter in front of the LLM, learning from your corrections.** A small static embedding model (potion-base-8M, ~30 MB, pinned and baked into the image) labels mail in well under a millisecond; only mail it is less than `EMBED_MIN_CONFIDENCE` (default 0.6) sure about goes to the LLM, so confident mail no longer spends the classification rate limit. Out of the box it scores 85% on `backend/cmd/modeleval`'s corpus (`-embed-model`), and it resisted all 8 injection probes. Changing a message's label in the reader, or in another IMAP client (noticed when the inbox syncs), teaches it, per account; a label you add can be given an optional description in Settings → Email Labels so it is recognised before any corrections. It stores vectors and a hash, never message text, in each account's `state.db`, and the LLM's own answers are not used as training data. `CLASSIFIER_ENGINE=llm` restores LLM-only operation.
- Add per-user opt-in incoming public-key encryption after classification, with verified IMAP replacement, targeted deletion, resumable ciphertext-only recovery, pending-key protection and durable cache-body suppression. Security → Encryption requires backup acknowledgment and fresh password/KySignOn confirmation; existing users remain opted out. Feasibility checks precede classification; permanent size/rule rejections have bounded retries and explicit plaintext-retention failure decisions.

- Shorten the default classification prompt for limited models, preserving the four labels, purpose-based tie-breaks and untrusted-email handling.

- **Pre-sort before the classifier.** Mail from a contact you added is labelled `Primary` without an LLM call and without spending the classification rate limit; mail carrying `List-Id`, `List-Unsubscribe`, `Precedence: bulk/list/junk` or `Auto-Submitted` can no longer be labelled `Primary`. Decisions record the reason. Accounts with a custom label set that has no `Primary` are unchanged. Contacts added automatically (Autocrypt, key discovery) never count as known senders, and now keep their "added automatically" mark when a phone or CardDAV client re-saves them.
- **Filter rules can test any header** (`header :is ["X-Spam-Flag"] "YES"` in Sieve, a "header" field in the builder), so an upstream spam filter's verdict can drive a rule. Every copy of a repeated header is checked.

- **Send-as verification works again.** The probe went out from the account address while the checker demanded a DKIM-signed From equal to the alias, so no alias could ever verify and the UI said no action was needed. The probe is now sent as the alias to the alias through the user's own outgoing server, so an upstream that refuses that From refuses the alias too. The user types the mailed code back in Settings → Mail (`POST /api/mail/send-as/{id}/confirm`, five wrong codes fail the record, 30-minute window). Because the code travels through the user's own SMTP server, it authorizes the From address only; WKD publication and key User IDs still require the DKIM loop-back proof, which the daemon keeps checking for and records as `verifiedBy: dkim`. The alias list no longer returns the verification code.
- **Notifications are always queued for app pull**, not only in pull mode, so a device that stops hearing from the relay can poll `GET /api/notifications/native/pull` and catch up without an admin changing the delivery mode. Each queued notification records the devices it was addressed to and is served only to them, so a push-MFA challenge aimed at approver devices never reaches a device excluded from approval. The queue keeps its 100-entry cap.

- Match the Single Sign On login button to the theme's primary button colors and label it "login with Single Sign On".
- Web appearance now defaults to Busnes light/dark according to the OS; existing theme choices remain available.
- Refresh the paired-device key before opening enrollment, preventing a new Android enrollment code from being compared against the browser's previously loaded key.

- Fix device PGP enrollment after the envelope v3 rollout: browser uploads now include the required identity fingerprint for both v2 and v3, preventing rejection before Android and other paired devices can retrieve their sealed keys.

- **Browser v3 device enrollment.** On a converted account the Security page seals the complete keyring as v3 for a paired device, refuses a device whose app has not claimed v3 before asking for a code, and names the enrollment key and generation on the upload. A device confirmed at an older generation shows as holding retired material with an "Enroll again" action, since the server refuses its sends.
- **Device-envelope v3 delivery contract.** A `device:` slot delivery is bound to the device's published enrollment key, the snapshot revision and the keyring's material generation; the server parses the envelope framing, delivers v3 to a converted account and v2 to a legacy one, and refuses a version the device did not advertise. `GET /api/pgp/device/envelope` returns the version, fingerprint and generation beside the envelope. `POST /api/pgp/device/enrollment-state` takes a generation-aware acknowledgement, refuses a bare or stale one on a converted account, and the owner's device listing shows what each device holds. `POST /api/mail/send-pgp` refuses a stale `materialGeneration` on a converted account and a paired device not enrolled at the current generation. The browser now names the enrollment key and generation when it delivers, and the generation when it sends.

- Only a session signed in through KySignOn itself takes the KySignOn step-up; a session from Authentik or Keycloak keeps the password gate for the Security page and backup actions instead of being refused forever.

- **Action-bound KySignOn re-authentication.** A session signed in through KySignOn now confirms the Security page and backup actions with a fresh KySignOn sign-in in a popup instead of a password it does not have: `403 sso_step_up_required` carries a challenge bound to the exact request, `POST /api/auth/oidc/step-up` starts the round trip, and the request is repeated with `X-Kypost-Step-Up`. The grant is spent once, by that session, for that request. PGP identity operations keep the password.

- **KySignOn app roles.** A KySignOn ID token's `roles` claim is now the only source of admin for its subject: `kypost.admin` grants it, the legacy `role` claim and generic admin groups are ignored when `roles` is present, and the session records the result as a ceiling, so an SSO session acts as admin only when both the account and the token say so. Authentik and Keycloak admin-group mapping is unchanged for tokens without `roles`.

- **Directory sync is now KySignOn's versioned desired state.** `POST /api/sync/webhook` accepts a SCIM User signed with `syncauth` and carrying a `W/"n"` revision; stale, reordered and replayed deliveries are refused (422) by a fence kept in `sso-lifecycle.json`, so it survives restarts. Disabling or deleting a user revokes access and keeps their data; a rehire restores the same account (one with a local password re-authorizes its SSO link afterwards). The last active admin can be neither removed nor demoted. Admin comes only from the `kypost.admin` app role. A sign-in is refused for a subject the directory disabled, or with a token issued before its access last changed. The old `{jti,iat,event,user}` envelope, `X-Sync-Signature`, the bearer-secret form and the `requireFreshEvents` setting are removed.

- OpenID Connect back-channel logout: `POST /api/auth/oidc/backchannel-logout` ends the SSO session the provider names, with durable replay refusal and a fence against a login still in flight. SSO sessions now remember the provider's `sid`. Requires an `https` issuer; verification is `ky-primitives/oidcverify` v0.7.0.

- Devices can publish supported enrollment envelope versions with their sealing key. Preserve claims across token refreshes, default legacy publications to v2, and expose them in device listings. Browser setup refuses incompatible versions; v3 delivery and conversion remain gated.

- Prepare device-envelope v3 with complete-ring validation, device-code verification and a separate authenticated crypto domain. Pin v2/v3 bytes with shared WebCrypto/Go vectors; native delivery and conversion remain gated.

- Save verified complete-keyring recovery copies after saved-secret acknowledgement, with version/revision guards and exact slot confirmation. Preserve password/public material and retain the file/secret on uncertain responses.
- Restore matching complete-keyring backups with explicit version/revision guards and exact stored-ciphertext confirmation, including lost responses. Preserve all history, public metadata and recovery slots; older-backup merging and conversion remain gated.
- Support complete-keyring password changes for already-converted records with explicit version/revision guards. Preserve recovery copies (including absent slots), public identity and material generation; conversion remains gated.
- Add complete-ring recovery-v2 offline export and read-only drills. Preserve original private packets and revocation certificates, enforce sealed size limits, and compare every member/generation plus active public packets against fresh snapshots. Reject legacy backups for converted records; conversion and lifecycle restore/uploads remain gated.

- Prepare atomic whole-keyring storage for password/recovery envelopes, retained fingerprint inventories and credentials. Guard converted records against legacy writers (including plaintext forced password completion) and stale admin resets; expose keyring metadata in authenticated snapshots. No conversion or lifecycle HTTP writer is enabled.

- Bind browser PGP and password writes to the snapshot used for preparation. Preserve prepared recovery revisions across tab switches and upload failures; refuse missing revision support. Add a narrow password-change snapshot so forced resets remain possible while preserving old sealed PGP material for recovery; show that consequence even when the form is reopened. Device enrollment treats fingerprint casing consistently after recovery.

- Add persisted PGP revisions and optional atomic write preconditions to identity, envelope and password APIs. Stale same-key updates are rejected without changing credentials or ciphertext; admin resets advance the revision. Existing clients remain compatible; multi-key transactions follow separately.

- Prepare browser historical-key decryption with a bounded, validated keyring reader for mail, drafts and autosaves. Conversion remains unavailable; existing single-key signing, enrollment, recovery creation and rewrap paths refuse keyring plaintext until lifecycle write support is ready.

- Document the proposed PGP multi-key lifecycle and its native-client upgrade gates, with a runnable crypto feasibility check. No lifecycle behavior ships in this design change.

- PGP recovery now stores a sealed server copy as well as requesting a tested offline download. Restore the current identity without the file using its recovery secret; test a copy with a non-destructive drill and keep its date in this browser. Password changes warn when no server copy is confirmed, and identity deletion warns that it removes server recovery too. Recovery writes refuse a concurrently replaced identity.

- Signing defaults on when an unlocked browser key is available, including after unlocking while composing. Turning Sign off shows `Unsigned` and lasts for that message. Successfully decrypted mail without a signature shows `encrypted but unsigned`; encryption alone never means the sender was verified. Detached MIME signatures inside encryption are detected as signed but unchecked, including nested multipart wrappers.

- Sign without Encrypt now sends a real signed message (RFC 3156 `multipart/signed`) from a browser-encrypted account, to every recipient, without needing their keys. It used to route into the encrypted path and refuse recipients with no key. The Sent copy of any browser-encrypted send is now shape-checked as ciphertext on the server before it is stored, not just flagged by the client.
- An encrypted message no longer makes the whole mailbox load through the live IMAP path. The mail cache records that the server holds no plaintext for such a row and counts it as warm, so mailboxes with encrypted mail take the cache-first path again. Native clients see the same rows as before.
- Drafts saved from a browser-encrypted account are now encrypted to the user's own key before they reach IMAP, and reopen with their recipients, subject, body and attachments. The compose autosave snapshot is sealed to the same key. The reader shows the real Subject of a decrypted message rather than the outer placeholder. The server refuses a plaintext draft from a browser-encrypted account (409 `clientSideNeeded`), and the browser saves nothing until it knows the account's key custody.
- Browser-encrypted mail now carries attachments: they are encrypted inside the message with the body and the Sent copy, and decrypted attachments are offered as downloads and inline images without leaving the browser. Previously an encrypted send silently dropped them. Browser decryption is capped at 25 MiB decompressed, and `/api/mail/send-pgp` accepts up to 64 MiB per request so the per-recipient copies fit.
- Admins can publish default mail server settings (Server > Default Mail Server) and assign or lock a specific user's mailbox and contacts-sync credentials (Manage Users > Mailbox). Locked settings stay visible to the user, read-only. New routes: `GET|PUT /api/mail-defaults`, `GET|PUT|DELETE /api/users/{id}/imap-config`, `GET|PUT|DELETE /api/users/{id}/carddav-client`.
- Allow sealed backups when the optional TUNING_FILE override inside a collected root is absent, as in the default container layout; all external overrides and missing required keys remain refused.
- Compact the Backup page with an at-a-glance status row and expandable setup, schedule and history. Explain why actions require a password and show stored failure reasons in activity history.

### Added

- KyRecovery/local sealed backups with key pinning, admin scheduling, downloads, restore drills and an offline custodian-share restore CLI, using ky-primitives v0.5.1.
- Optional explicit LAN DNS compose override for private KyRecovery deployments.

### Changed

- Preserve declared security/operational log fields and classifier failure context without logging upstream response content; enforce field coverage against production call sites.
- Restrict the extended shutdown drain to active backups, retaining the 20-second grace for ordinary HTTP requests.

- Application and classifier logs use shared JSON stderr; supervisor owns rotation. `KY_LOG_LEVEL` controls verbosity. Raw classifier output is no longer logged.
- Container shutdown allows active backup deposits up to 16 minutes to finish.

- The backend now uses its public GitHub module path so other modules can import it.
- Passwords are hashed with Argon2id (ky-primitives). Existing scrypt hashes keep working and are upgraded on the next successful login. CardDAV app passwords and legacy device secrets verify the same way. This is the ky-primitives suite-wide RFC 9106 profile (64 MiB, 3 passes, 4 lanes); per-guess cost is lower than the previous scrypt setting in both wall time and memory, a deliberate choice so every product in the suite shares one password policy under one memory budget.
- TOTP and recovery codes come from ky-primitives. Recovery codes are stored as keyed HMAC-SHA256 digests under a key in the private volume (`$SECRET_DIR/totp-secret.key`, the same key TOTP secrets are sealed with), so regenerating them no longer costs ten scrypt derivations and a backup of the config volume alone still contains no usable second factor. Codes issued before this release still redeem.
- Removed the stale v1 copy of the KyRecovery pairing spec; the contract lives in kyrecovery-server.

## 0.3.0 — 2026-08-25

### Added

- **Displayed inbox messages now open faster.** Once inbox metadata has
  rendered, the web UI preloads bodies for the current 20-message page and
  reuses those requests when a message is opened. The initial list still omits
  bodies, preserving its small, fast payload, and failed preloads remain
  retryable.

- **The pairing QR now publishes a certificate pin.** The pairing request is
  the one call that carries the pairing token, the push endpoint and the WebPush
  keys, and until now the app sent all of it inside a trust-on-first-use window —
  the certificate was only trusted *after* the secrets were already disclosed. On
  a network with a locally trusted CA (enterprise MDM, a user-installed root, a
  hostile captive portal) an interceptor read the token, registered its own device
  against the relay first, and handed back credentials it controlled. The
  `kypost://native-pair` link now carries `pin=`, the base64 SHA-256 of the
  serving certificate's leaf SubjectPublicKeyInfo, and the app pins the
  registration handshake to that one key before sending anything. Read live from
  the certificate in use at link-generation time, so renewals need no action.
  Reverse-proxy deployments are covered without configuration: the server reads
  the pin from a verified handshake with `SERVER_BASE_URL`, which is the
  certificate the device is actually handed — for the `cloudflared` setup the
  docs describe there is no certificate on disk to read at all, and the probe
  leaves over anycast to the same edge rather than depending on the router
  hairpinning traffic back to itself. Falls back to this process's own
  certificate when it terminates TLS. The chain is verified, so a deployment
  behind a private CA gets no pin and should set `TLS_CERT_FILE` instead; every
  other failure leaves `pin` absent, which keeps today's trust-on-first-use
  behaviour rather than breaking pairing.

  Pinning behind a terminating proxy pins you to that proxy. With Cloudflare in
  front, this closes the hostile-local-network hole — it does not make the
  tunnel end-to-end, and Cloudflare still terminates.

- **Client-protected PGP identities can be backed up and restored.** Security
  now downloads a browser-encrypted recovery file and displays its separate
  secret once. Restoring decrypts locally, verifies the existing fingerprint,
  and uploads only a new account-password-wrapped envelope; the server never
  receives the recovery secret or plaintext private key.

- **Server-held PGP keys can be backed up too.** The recovery backup was wired
  only to client-protected identities, because it is built from the key held in
  the browser and a server-custody account never has one there. That left the
  one group who most needs a file without a way to make it: migrating to
  end-to-end puts the key beyond the server's reach, and from that moment an
  admin password reset destroys it and every message ever encrypted to it. The
  legacy path now offers the same download, through the existing one-time key
  export it already uses to migrate, producing the identical browser-encrypted
  file and one-time secret. A bare `.asc` download is deliberately still not
  offered — an unprotected private key in a downloads folder is what the format
  exists to avoid.

- **The inbox has an encryption column.** Every encrypted message now carries a
  padlock in its own column, in both the inbox and search results, whether or
  not it opened — previously the marking lived inside the Subject cell and
  appeared only when a message could not be read without a further step, so a
  message the server decrypted successfully looked exactly like one that
  arrived in the clear. A failed decrypt keeps the padlock and tints it rather
  than switching to a second symbol, so the column reads as "padlock or
  nothing" at a glance.

- **CI runs the scripts that install and update a deployment.** `scripts/` is
  the supported install and update path and nothing in CI touched it — the
  updater's own self-check existed and was never run. A `ci-scripts` job now
  runs it, the new model-installer self-check, shell syntax across `scripts/`,
  and `docker compose config`, and it is part of the release gate. The container
  smoke test additionally asserts the daemon is reporting its own health, so
  "healthy" cannot mean "the API answered".

### Security

- **No URL that carries a credential is built from the request any more.**
  `externalBaseURL` read `X-Forwarded-Host` from a trusted proxy and otherwise
  fell back to the request's `Host` header, and four things were built from it:
  the native pairing package (`serverBaseUrl`, `registerEndpoint`,
  `pullEndpoint`), the pull endpoint returned after a device registers, the
  desktop pairing `registerEndpoint`, and the OIDC `redirect_uri`. Every one of
  those names an address a secret is then sent to — a 90-second pairing token, a
  device secret, an authorization code — so a deployment reachable through a
  second hostname produced a pairing package aiming the token at that hostname.
  The helper is deleted rather than guarded, so nothing can reach for it again;
  `pairingBaseURL` and `ssoRedirectURI` read `SERVER_BASE_URL` and nothing else.

  **`SERVER_BASE_URL` is now required for mobile pairing, desktop pairing and
  Single Sign-On.** Unset, the pairing panel shows "set SERVER_BASE_URL" and no
  token is minted, `POST /api/notifications/desktop/pair` answers 503 without
  spending one of its five codes per hour, and SSO refuses with the message it
  already had. This is a deliberate breaking change for deployments that never
  set it: there is no safe way to guess where a credential should be sent, and
  the previous guess was whatever hostname the caller happened to arrive on.
  Pickup links and PGP QR key-exchange URLs are unaffected — they never read the
  request, and still fall back to `http://localhost:5866` with a logged warning.

- **A caught value can no longer take out the handler that caught it.** Eight
  `catch` blocks across the two relay Workers and their shared logic formatted
  the error as `String((err as Error).message ?? err)`. The cast is a lie the
  compiler accepts: a `catch` binding is `unknown`, `throw null` is legal
  JavaScript, and reading `.message` off it throws a TypeError out of the
  handler. Every one of those blocks is a fail-closed path — the 429 for a rate
  limiter whose binding threw, the 502 for a provider that never answered — so
  the refusal became the router's generic 500 and the log line described the
  logging bug instead of the outage. One total `errorMessage(unknown)` helper
  now does it everywhere, held by `push-relay-shared/error-message.test.mts`.

- **Every per-user JSON store now fails closed on a read it could not perform.**
  `contacts`, `groups`, `sendas`, `rules` and `mailcache` kept a warm in-memory
  copy of a file the api and daemon processes both write, and their readers
  discarded the re-read error — so once the file became unreadable or corrupt,
  a process answered from that copy indefinitely and indistinguishably from a
  healthy read. Concretely: a cached "signature verified" badge outlived the
  contact key the user removed to retire it; a recipient with a pinned key was
  reported as keyless, which is the bucket the send path offers the plaintext
  pickup fallback for; the contact-photo sweep read "nothing is referenced" and
  deleted every photo on disk; an unreadable `groups.json` erased a contact's
  group memberships on the next save. Readers now return the error and each
  caller fails closed — refusing the operation, discarding the derived trust, or
  answering with the empty set where empty is the safe answer.
- **Compose autosave no longer writes draft plaintext to `localStorage`.** The
  buffer being saved is the plaintext of a message the user may be about to
  PGP-encrypt, and `localStorage` kept it on disk until something deleted it.
  It now lives in `sessionStorage`, which still survives a reload, a crash
  restore and a reopened tab, and dies with the tab otherwise; the startup sweep
  additionally deletes any draft an earlier version left in `localStorage`,
  regardless of age.
- **`POST /api/contacts/carddav-client/sync` no longer answers `200` after
  failing to save its own state.** The write carrying the discovered
  address-book path, timestamp and counters was best-effort, so a failed
  persist left the next sync repeating discovery and re-importing while the UI
  reported success.
- **SSO ID tokens are now cryptographically verified.** The OIDC callback
  base64-decoded the ID token payload and trusted it — no signature, issuer,
  audience, expiry, algorithm or nonce was ever checked — so anything that could
  answer the configured token endpoint could mint `{"role":"admin"}` and be
  granted an administrator session. Verification is now delegated to
  `github.com/coreos/go-oidc` and requires a signature from the issuer's
  published JWKS using an asymmetric algorithm, an exact issuer, an audience
  containing the client ID, unexpired `exp`, `nbf` not in the future, `at_hash`
  matching the access token where the provider publishes one, a non-empty `sub`,
  and a `nonce` bound to the browser that started the login. The hand-written
  token parser is gone. **Anyone running SSO should treat previously
  auto-provisioned accounts as unverified and review them.**

- **An SSO identity can no longer seize an account by claiming its username.**
  When no linked subject matched, the callback fell back to looking the account
  up by `preferred_username` and silently linked whatever it found — so any
  directory user who could set that claim to `admin` inherited the local
  administrator and signed in with its role. Username collision is not proof of
  ownership even when the provider genuinely signs it. An existing account is
  now claimed only through a stored subject, or by linking from a session
  already authenticated as that account; one subject can never be linked to two
  accounts.

- **The SSO state cookie no longer names the account to link.** It carried the
  target user id, and the browser supplies it, so anyone who knew a victim's
  user id could set that cookie, complete the flow with their own identity
  provider account, and bind it to the victim's account. Link mode is now only a
  marker; the account is resolved from the caller's authenticated session at
  callback time.

- **OIDC discovery is held to a transport policy.** The issuer had no scheme
  requirement, no issuer-equality check, no response-size ceiling and no redirect
  restriction, so a typo or a hostile discovery document could send the OAuth
  client secret and authorization code to a cleartext or unrelated endpoint.
  https is now required of the issuer and of every endpoint discovery names,
  discovery must agree about its own issuer, redirects are refused, and bodies
  are bounded. Cleartext `http://` stays available for loopback, and for a LAN
  provider with no TLS via a new `allowInsecureIssuer` setting that an operator
  must turn on deliberately. Invalid settings are refused when saved rather than
  at the next sign-in.

- **Directory replication no longer reports failed writes as successes.**
  `POST /api/sync/webhook` discarded every persistence and credential-revocation
  error and answered `200 {"ok":true}`, so a deactivation that never reached
  disk was recorded as delivered, the identity provider stopped retrying, and a
  removed user stayed authenticated. Errors now reach the sender: a 5xx means
  retry, a 4xx means an operator must intervene, and unknown event types are
  rejected instead of silently acknowledged. Removal also runs unconditionally,
  so a retry after a half-applied deletion still performs the revocation the
  first attempt skipped.

- **Replication events can carry replay protection.** An HMAC proves who wrote
  an event, never when, so a captured `user.updated{role:"admin"}` stayed valid
  indefinitely and re-promoted a user the directory had since demoted. Events may
  now carry `jti` and `iat` (the RFC 8417 Security Event Token fields OIDC
  back-channel logout uses); those inside the freshness window are applied once
  and replays are refused. They are optional until an operator enables
  `requireFreshEvents`, so a sender that does not send them yet keeps working.

- **A short OIDC subject no longer panics the callback.** Provisioning sliced
  `sub[:8]`, and `sub` is whatever the provider chose — `42` is legal — so such a
  provider broke sign-in entirely. Derived usernames now come from a hash of the
  subject and are always valid.

- **`:main` images are no longer published before CI has passed.** The publish
  workflow started from the same push as the test workflow and raced it, so a
  commit that failed authentication, migration, race or frontend tests could
  still become the public `:main` image — with an attestation proving exactly
  which broken source produced it. Publishing is now triggered by the test
  workflow completing, and still refuses to move `:main` unless every required
  job completed successfully for that exact commit **and** that commit is still
  the tip of `main` — re-running an old test run would otherwise walk the
  published image backwards behind a green tick and a valid attestation.

- **The SSO `redirect_uri` no longer comes from the `Host` header.** It was
  built from `r.Host`, which behind a reverse proxy is the internal name — so
  the value never matched what was registered at the identity provider and SSO
  could not work there at all. It now follows the configured server base URL,
  falling back to the same trusted-proxy-aware helper the rest of the server
  uses, which also stops a request header influencing where an authorization
  code is delivered.

- **A rejected sync webhook no longer reads the settings file.** The unauthorized
  path loaded `sso.json` from disk on every request, so an unauthenticated
  caller could drive disk reads. The pairing secret is now checked first and
  alone.

- **A recipient whose PGP key changed no longer gets a plaintext pickup link.**
  The resolver already refused to switch keys when a contact's pinned
  fingerprint stopped matching what discovery served, but the send path threw
  that verdict away and treated the recipient as having no key at all — which is
  precisely the case the "Secure link if no key" fallback covers, by storing the
  message in the clear on this server for seven days. A broken pin is the one
  signal that the key published for an address may have been substituted, so it
  is now refused outright and the checkbox cannot override it. The compose error
  says so, and no longer offers the fallback.

- **Filter rules with an unusable regular expression are rejected when saved.**
  An invalid pattern used to be accepted and only fail while running, where
  "did not match" combined with *Not* inverted into "matched every message" — so
  one mistyped character could quietly sweep an inbox into Trash. A pattern that
  expands to an unreasonable size is refused for the same reason. Existing rules
  are unaffected unless they were already broken.

- **Address books have a size limit, and deleted contacts are now reclaimed.**
  Tombstones from deleted contacts were kept forever because nothing ever ran
  the collector, so a sync client that repeatedly added and removed entries grew
  the file without bound. Deletions past the retention window are now swept
  hourly, and an account is capped at 50,000 live contacts — far above any real
  address book, and a backstop rather than a limit anyone will meet.


- **The poll daemon refuses plaintext mailbox credentials.** The API's reader
  already rejected an unencrypted IMAP config and named the remedy, but the
  daemon's kept a copy of the fallback that had been removed from the shared
  helper — so a legacy config produced a daemon happily polling mail from a
  password sitting in cleartext on disk while the settings page reported the
  file unreadable. Re-save your IMAP/SMTP settings once if you see the new
  error; nothing else is needed.

- **A weak `PAIRING_SECRET` is refused instead of used.** The server generates
  a strong one by default, but an operator-supplied value was taken verbatim
  however short — and one guessable string signs pickup links, device pairing
  tokens and PGP QR key exchange alike. Values under 32 bytes are now
  rejected, with the reason and the remedy logged, and those three features stay
  disabled rather than being signed with something forgeable. Only deployments
  that set the variable by hand are affected.

- **The Security page re-authenticates before it draws.** It lists key
  fingerprints and paired devices and hands out a backup of the private key, and
  a session cookie only ever proved that somebody signed in on this browser
  once. Opening it now asks for the account password and, on an account with
  two-factor auth, a TOTP or recovery code — verified by the server
  (`POST /api/auth/step-up`), on the same throttles and the same per-account
  replay guard as signing in. The confirmation lasts five minutes, is dropped at
  logout, and does not survive a reload. It is a gate on the screen, not a new
  authorisation boundary: every operation behind it still re-verifies for
  itself, so nothing changes for a caller who skips the page.

- **Replacing or deleting a PGP identity requires the account password.** A
  session cookie was enough, and unlike everything else a session authorises,
  the damage outlives it: a replaced public key is published through WKD and
  Autocrypt, so every future correspondent encrypts to it, and a deleted
  identity cannot be recovered at all. Generate, import, client-key upload,
  rewrap and delete now take the same credential the legacy-key export already
  did. First-time setup is not gated — there is no key to redirect yet.

### Changed

- **KyPost is now released under the MIT License, replacing AGPL-3.0.** This is
  a relaxation, not a restriction: everything permitted before is still
  permitted, and the copyleft obligation is gone. If you run a modified KyPost
  as a network service you are no longer required to offer your users its
  source. Downstream consumers who avoided the project because of AGPL's
  network clause no longer have that reason to.

  Contributions are accepted under the same terms — see the Licence section of
  `CONTRIBUTING.md`. `LICENSE.txt` is the authoritative text, and the licence
  link in the sidebar footer now displays it.

- **Device pairing moved into Security, which is now tabbed.** Pairing, the
  paired-device list, and the per-device "can approve sign-ins" switch were
  spread across two nav sections and rendered the same hardware three times from
  two endpoints. Security is now Sign-in / Devices / Mail, and a device is one
  row carrying its identity and what it may do. The `Pairing` nav entry is gone;
  `/notifications` redirects to the new Notifications tab and is kept
  indefinitely, because a service worker cached in a browser or installed PWA can
  still send a notification tap there long after the deploy.

  Two consequences worth knowing. Pairing now sits behind the same step-up prompt
  as the rest of Security — which is what that prompt's own wording always
  claimed to cover ("your key fingerprints and paired devices"). And the pairing
  code is no longer minted just because a page opened: `GET
  /api/notifications/pairing` hands out a live 90-second pairing token as a side
  effect of reading, and the old page called it on load and every ninety seconds
  after, so any forgotten tab was a self-refreshing pairing credential. It is now
  fetched only while the "Pair a new device" panel is open. `GET
  /api/notifications/native/devices` gained a `deliveryMode` field so the Relay
  Push / App Pull toggle can read that setting without minting anything.

  "Encrypted mail on your devices" stays a separate card rather than becoming a
  column: it applies only to a client-protected account, so it would be blank for
  everyone else, and it owns the enrollment ceremony whose security rests on
  refetching when the identity changes.

- **Notification settings moved to Configuration → Notifications.** They were
  sharing a page with device pairing under a nav entry labelled "Pairing" that
  routed to `/notifications` and rendered a heading reading "Notifications and
  Pairing" — three names for two unrelated things. Delivery mode, IMAP keywords,
  content previews, the test notification and this-device unsubscribe are now a
  tab on Configuration, visible to every user because they are per-account
  preferences rather than system config. `/notifications` keeps only pairing for
  now. Configuration's active tab also moved into the URL (`?tab=`), so a link
  can open one and a reload no longer drops you back on the first tab; an
  unrecognised value, or an admin-only tab requested by a non-admin, falls back
  to that user's default rather than rendering a tab strip with no panel. The
  test notification and any push carrying no explicit url now land on the new
  tab.

- **Encrypted mail no longer goes to the classifier.** A PGP-encrypted message
  has no readable body — the poller never decrypts, and the payload is detected
  precisely *because* no MIME part rendered — so every one of them spent an
  Ollama call on an empty body, handed the model the sender and (for
  third-party PGP/MIME without protected headers) the real subject, and was
  then retired unlabeled into Uncategorized. Such a message is now tagged with
  the account's default label and skips the model. It is deliberately not given
  an `Encrypted` keyword: IMAP keywords are stored on the mail server in the
  clear, so that would hand whoever runs that server an index of which messages
  are worth attacking while looking like a security feature — and the published
  contract says keywords are a sorting hint, never a security boundary.

### Fixed

- **A server that cannot open its log file no longer starts anyway.** The
  rotating writer's initial `open` was discarded, so `logging.New` reported
  success whatever happened — and because `slog` drops write errors, the process
  then ran with no durable log and nothing to indicate it. An `app.log` that is
  unwritable or has become a directory (a bad mount, a permission change on the
  volume) meant the security and incident record was simply absent when someone
  went looking for it. The error is now propagated and startup refuses. The
  classifier's three diagnostic logs stay non-fatal but now report the failure
  instead of swallowing it.

- **A notification whose stored payload will not decode is no longer reported as
  delivered.** `PullNotificationsAfterStrict` discarded the JSON error and
  returned the notification with its routing metadata missing; the handler
  answered 200 and the device advanced its cursor past a notification it never
  received, permanently. The read now fails, and the handler already fails closed
  with 503 on it.

- **An expired session settles the caller's promise instead of hanging it.**
  `client.ts` answered a 401 by reloading the page and returning a promise that
  by design never settled, so no caller acted on a dead session — but no caller's
  `finally` ran either, leaving loading flags set and cleanup undone whenever the
  reload was blocked, deferred, or stubbed. It now reloads and throws
  `SessionExpiredError`.

- **Three UI failures are visible rather than erased.** The contact group list,
  the admin log file list, and the Single Sign-On configuration each had a
  `catch(() => {})`, so a failed load rendered identically to "there is nothing
  here" — an administrator opening Logs during the incident they were
  investigating saw no files, and an account with a linked SSO identity lost the
  unlink control entirely. Each now reports the failure.

- **The lock-order check can no longer pass because a mutex was never
  registered.** `TestLockOrderIsRespected` ignores any mutex missing from
  `lockRank`, which meant the suite went green precisely when the step it exists
  to enforce was skipped — `pinProbeMu` had shipped unranked.
  `TestEveryServerMutexIsRanked` now reads the `Server` struct and fails on any
  unranked mutex.

- **Relay tests are discovered, not listed.** CI named eight test files by hand,
  so a new one ran only if whoever added it also edited the workflow, and
  `npm test` failed outright in both Worker packages. `scripts/test-relays.sh` is
  now the single command CI and both packages use.

- **Encrypted Sent copies no longer lose their BCC recipients.** The copy was
  built by encrypting the delivered message, which omits `Bcc` on purpose so no
  recipient can see who else received it — and `SaveSent` ignores the draft's
  recipient fields entirely once it has raw bytes to append. So an encrypted
  send recorded no blind recipients while a plaintext one recorded them
  normally. The copy is now built from its own source that carries them.

- **The attachment listing looks inside real encrypted mail, not just test
  mail.** It required a message to have exactly one MIME part before treating it
  as an envelope, which no real encrypted message satisfies: the MIME parser
  files the ciphertext under both attachments and inlines, so it arrives listed
  twice. The check now asks whether every part is one an envelope could carry.

- **An ordinary message carrying an encrypted file is no longer mistaken for an
  encrypted message.** Detection accepted any bodyless message with an armored
  attachment, so `document.pgp` sent alongside a spreadsheet skipped
  classification, suppressed its own notification preview and showed a padlock.
  Every part must now be one a PGP/MIME envelope could contain.

- **Downloading an encrypted file returns that file.** The download endpoint
  decrypted anything whose bytes began with a PGP armor header, with no check
  that the message was an envelope, and then re-indexed into the decrypted
  contents — so clicking `archive.pgp` in a message that also had other
  attachments returned something from inside it, or a 404, for a file the
  listing said was there. Both endpoints now apply the same test.

- **An encrypted send never stores a readable Sent copy.** When the copy could
  not be encrypted, it was saved in plain text instead — and the argument for
  that ("this server holds the account's key anyway, so a readable copy reveals
  nothing new") missed where the copy goes: it is APPENDed to the account's IMAP
  host, which holds no key at all and now holds the body and real subject of a
  message the sender encrypted. Nothing is saved in that case now, the send
  reports `sentSaved:false`, and the reason comes back as a warning. The
  client-custody path has always refused to save cleartext here; the two paths
  agree again. The sender with no key of their own — previously the one case
  that produced plaintext with no warning anywhere — is included.

- **Whole-message encryption is decided by the message's root Content-Type.**
  Detection inferred it from the shape of a message's MIME parts, because the
  IMAP library does not parse root headers, and a bodyless message carrying one
  armored attachment is indistinguishable from a real PGP/MIME envelope that
  way. A sender could build one deliberately and be rewarded with a padlock and
  a message the classifier skips. The part shapes are now only a candidate
  filter; the real `multipart/encrypted; protocol="application/pgp-encrypted"`
  header is fetched for the few that pass it and decides.

- **The daemon's health reaches the health endpoint.** Under supervisord the
  poller runs as its own process with its own in-memory health, and
  `/api/health` served the API process's copy — so `classifierFailing` and
  `nativePushFailing` were permanently false, because nothing in the API
  process classifies mail or sends a push. A container whose poller had been
  dead for a week answered "healthy" and the health page rendered "Working".
  The daemon now publishes its subsystem health and a heartbeat to shared
  state; the API merges it and treats a heartbeat that stopped as unhealthy.

- **Shutdown actually cancels the work it waits for.** Poll ticks and per-message
  processing ran under `context.Background()` with 8- and 4-minute timeouts, so
  `Stop()` returned immediately, the tick loop exited, and the IMAP, SMTP and
  state writes underneath carried on — while shutdown blocked on exactly those.
  Against Docker's 10-second default grace period, routine updates reached
  SIGKILL mid-write. Both contexts now derive from the poller's own, no new
  message is admitted after cancellation, and Compose and supervisord are given
  grace periods that match.

- **One poll tick cannot exhaust the container's memory.** Every unread message
  past the checkpoint was fetched in a single call that buffers and MIME-decodes
  each one, with a 25 MiB per-message cap and no cap on the count — so the peak
  memory of a tick was a function of how much unread mail happened to be
  waiting, and the poller's rate limit could not help because it applies during
  processing, after the fetch. Fetching is now paged with a byte budget, and
  what it does not reach stays above the checkpoint for the next tick.

- **The model installer no longer gives up.** It exited after five failed pull
  attempts, which under `autorestart=false` was terminal — the program went
  EXITED rather than FATAL, so nothing restarted it and a container that met a
  few minutes of registry trouble at boot ran forever without the model it
  classifies with. It now retries on a capped interval until the model is
  installed, so a cause fixed later is picked up without anyone restarting
  anything.

- **An attachment-only encrypted message no longer tells you to unlock a key.**
  The inbox padlock read "unlock your PGP key to read" whenever an encrypted
  message had no text body — including one the server had already decrypted
  whose plaintext was attachments only, where there is nothing to unlock. The
  locked state now requires client-protected custody, which is the only kind
  that has a vault.

- **An encrypted message's sender and subject no longer reach FCM or APNs.**
  Native push travels to the relay Worker and on to Google or Apple in
  cleartext at every hop, and a third-party PGP/MIME message that does not use
  protected headers carries its real subject in the clear — so turning on
  Content Preview was shipping the subject of end-to-end encrypted mail through
  two third parties, invisibly. Both are now withheld for encrypted messages
  whatever that setting says. Web push is unchanged: RFC 8291 encrypts those
  payloads to the browser's own subscription keys, so Content Preview remains
  the user's call there.

- **The Sent folder now shows that an encrypted message was encrypted.** A
  server-custody encrypted send delivered ciphertext but appended its Sent copy
  as cleartext, rebuilt from the request. The web reader derives its "PGP:
  encrypted" badge by sniffing the stored message, so an encrypted send and a
  plain one rendered identically in Sent — with no indicator on either — and the
  plaintext and real subject of every encrypted message sat on the IMAP store
  regardless. The copy is now encrypted to the sender's own key, matching what
  the client-custody path has done since run-4. A sender who has no key of their
  own (encrypting to recipients never required one) keeps the previous plaintext
  copy: there is nothing to encrypt it to.

- **Attachments on encrypted mail are readable again.** The attachment endpoints
  served a message's outer MIME parts, which for PGP/MIME is only the armored
  payload — so an encrypted message with a file attached listed `encrypted.asc`,
  and downloading it produced armor instead of the file. Both endpoints now look
  inside the ciphertext when the server holds a key that opens it. Unencrypted
  mail is unaffected and still costs a single fetch; a client-protected account
  still receives the untouched ciphertext, since the server cannot read it.

- **Transient failures no longer discard mail-processing work.** A keyword
  write, rule action, or state write that failed after its own retries used to
  mark the message processed and advance the poll checkpoint past it: the
  message was classified correctly, the label was never applied, and no later
  tick would look at it again. These failures are now marked retryable, leave
  the message unprocessed, and hold the checkpoint below it so the next tick
  retries. Errors that are not explicitly marked retryable still retire the
  message, so an unrecognised repeating failure cannot stall a mailbox.

- **A failed rule action is no longer recorded as applied.** An IMAP error
  during "archive and stop" recorded an `applied` decision and permanently
  retired the message, with the failure visible only as extra text in the
  decision's detail. The message is now deferred and recorded as `failed`.

- **Deferrals are bounded.** Holding the checkpoint for a failure that never
  clears would re-fetch a growing batch every tick forever. Attempts are counted
  per message (`deferrals` table in `state.db`); after 120 consecutive deferrals
  — roughly three hours at the default 90-second interval — the message is
  retired with a decision recording that it was given up on.

- **A rule stops at its first failed action.** Remaining actions used to run
  anyway. Since the retry the failure triggers is an unread-inbox search, an
  action that archived or read the message after an earlier one failed put it
  out of reach of the retry it had just been promised: the remaining work was
  lost silently while the audit row recorded a failure. Rule actions also stop
  at a cancelled context instead of running the rest of the list against a
  connection that is going away.

- **Rule actions are validated and bounded at every write.** Creating or
  updating a rule — by form, by API, or by Sieve script — now checks the action
  list, which nothing did before: at most 20 actions, only action types the
  engine can execute, bounded rule names and values, and at most one action that
  changes a message's visibility (read, move, archive, spam, delete), which must
  come last. An unknown action type used to be stored happily and then fail on
  every matching message for three hours before the message was retired
  unlabelled. Rules already saved are checked the same way when loaded; one that
  cannot be executed is skipped with a log line instead of failing per message.

- **A deferral counter that cannot be written no longer discards the message.**
  If the state database failed while recording a retry attempt, the poller
  retired the message and advanced the checkpoint past it — turning a momentary
  database contention into lost work. The message is now kept for retry and the
  poll tick is reported as failed so the database problem is visible.

- **A prerelease can no longer take over `:stable`.** GitHub's `published`
  release event fires for prereleases too, and while the promotion step
  excluded prereleases from the versions it compared against, it added the
  release being published back unconditionally — so the one release whose
  prerelease flag was never checked was the current one. Publishing a
  prerelease tagged with a plain `v<major>.<minor>.<patch>` (the flag is a
  checkbox, independent of the tag) compared as newest and moved `:stable` onto
  it, upgrading every install that follows that tag to code nobody released.
  The immutable version tag is still published, so testers can pull it by name.

- **A malformed release tag now fails the release instead of skipping it.** The
  version check was a job-level `if:`, which does not refuse anything — it
  skips the job, and a skipped job is green. A release tagged `1.0.0` or
  `release-1.0.0` produced no image and no failure. Tag validation now runs as
  the first step, before the checkout that consumes the tag.

- **Contact import accepts the upload the browser actually sends.** The
  frontend posts `multipart/form-data`; the handler read the raw request body
  into the vCard decoder. That did not fail loudly — the decoder reported
  "no BEGIN field found" for the MIME boundary and part headers and then
  decoded every card correctly — so every successful import in the UI reported
  "Imported N contacts. (2 errors)". Multipart uploads are now parsed properly,
  raw vCard bodies are still accepted, and an over-limit upload is refused with
  413 rather than silently truncated mid-card and reported as a partial
  success.

- **The relay no longer logs Google's OAuth response body.** A failed
  service-account token exchange interpolated the whole upstream body into an
  error string that the worker logged as `send.error`. It now reports the
  status and the RFC 6749 error enum — which is the actual diagnostic, since
  `invalid_client` means `FCM_CLIENT_EMAIL` is wrong and `invalid_grant` means
  `FCM_PRIVATE_KEY` is wrong or the clock has drifted — validated against a
  narrow pattern so the field cannot carry free text. The success-path
  `JSON.parse` is guarded for the same reason: V8 quotes the first ten
  characters of its input back in the `SyntaxError` message, and on a 200 that
  input is the access token. Pinned by
  `worker/src/fcm-oauth-redaction.test.mts`.

### Added

- `GET /api/status` reports `deferredMessages` and `oldestDeferredUtc`:
  how much mail is waiting to be retried and how long the oldest has waited.
  `checkpointHeldSinceUtc` says the poller is holding position; these say how
  much is behind that hold.

- Full-tick regression tests (`backend/internal/processor/poller_tick_test.go`)
  drive `tickUser` end to end against a scripted mailbox and assert the
  processed set, the poll checkpoint, and the audit log together. The defects
  above were each invisible to the existing per-piece tests.

- A documented, restore-tested backup procedure (README, "Backup and Restore").

### Changed

- `release-image.yml` publishes the immutable version tag, attests it, verifies
  the attestation, and only then promotes that exact digest to `:stable`.
  Previously both tags were pushed by the same build step, so a failed
  attestation left `:stable` already moved onto an unattested image. The
  workflow also refuses to move `:stable` backward and serializes concurrent
  release runs.

### Documentation

- Corrected the persistent-state paths. The README told operators that mailbox
  state lived in `state.json` and `decisions.json`; it has lived in `state.db`
  since the SQLite migration.

- Recorded the one exception to the relay's "no upstream response bodies in
  logs" rule. `send.fcm_failed` and `send.apns_failed` carry the provider's
  reason clipped to 200 characters, deliberately: since `isStaleResponse`
  stopped retiring devices on the 400s that name a token, that log line is the
  only way an operator learns `FCM_PROJECT_ID` or `APNS_TOPIC` is wrong rather
  than the phones being dead. The rule and the code had disagreed silently;
  `push-relay-shared/AGENTS.md` now states the exception and its limits.

## Upgrade and rollback

| From | To | Path | Rollback |
|---|---|---|---|
| 0.1.x | 0.3.0 | `./scripts/update-host.sh`, or `docker compose pull && docker compose up -d` | Automatic on failed health check; otherwise pin the previous digest |
| Locally built | published images (one-time) | `git pull --ff-only && docker compose pull && docker compose up -d` | `KYPOST_VERSION=<older>`, or rebuild at the previous commit |
| Locally built | a newer local build | `git pull --ff-only && docker compose up --build -d` | Rebuild at the previous commit |
| Any | older release | Set `KYPOST_VERSION=<older>` in `.env`, then `docker compose up -d` | — |

Notes:

- **Schema changes are additive and applied on open.** Every statement in
  `backend/internal/state/schema.go` is `IF NOT EXISTS`, so starting 0.3.0
  against a 0.1.x `state.db` adds the `deferrals` table and changes nothing
  else. An older server started against a newer `state.db` ignores tables it
  does not know about.

- **`scripts/update-host.sh` rolls back by itself.** It records the running
  image's digest before updating, verifies the new image's attestation, and
  restores the recorded digest if the post-update health check fails. An install
  running a locally built image has no published digest, so the updater refuses
  it rather than guessing a rollback target. **Every install predating 0.3.0 is
  in that state**, because 0.3.0 is the first release to publish an image at
  all — see "Moving a locally built install onto published images" in the
  README for the one-time migration.

- **Rolling back the container does not roll back the data.** Restore a backup
  (README, "Backup and Restore") if a downgrade needs the old state too.

## 0.1.0

Initial development release.
