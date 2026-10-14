# API Highlights

- Public `GET /api/installer/network` returns only client IP, proxy trust and the effective security scheme for edge verification. It uses the product’s configured-origin security fallback; see [installer setup](INSTALLER_SETUP.md).

- Admin backup routes: `GET /api/admin/backup/status`; `POST` to `run`, `export-capsule`, `drill`, `pair-remote`, `pin-key`; `DELETE pairing`; `PUT schedule` under `/api/admin/backup/`. Mutations require the current account credential and CSRF protection, or, for a session signed in through KySignOn, a fresh KySignOn confirmation of that exact action.

Auth:

- `POST /api/auth/login`
- `GET /api/auth/login-params` — the per-account salt and work factor a client
  needs to derive its auth secret, so the password is never transmitted. Public,
  and deliberately answers identically for a username that does not exist.
- `GET /api/auth/captcha-config`
- `GET /api/auth/pow-challenge` (`CAPTCHA_PROVIDER=pow` only)
- `GET /api/auth/csrf`
- `GET /api/auth/me`
- `POST /api/auth/logout`
- `GET|POST /api/auth/password` (read a private password-change snapshot, then atomically commit credential and optional PGP rewrap; forced resets preserve the previous sealed key for recovery)
- `POST /api/auth/step-up` (re-confirms the password, and a second factor when one is enrolled, before the Security page renders; a session signed in through KySignOn confirms with KySignOn instead, see below)

Single Sign-On (OpenID Connect):

- `GET /api/auth/sso-config` — public `{enabled, issuerUrl, clientId}` that gates the sign-in button and native sign-on. Never returns the client secret.
- `POST /api/auth/native/signon` — body `{idToken}`: a KyIdentity device-grant ID token, refused unless its `origin` claim matches `SERVER_BASE_URL`. Requires SSO plus `PAIRING_SECRET` and `SERVER_BASE_URL`, otherwise 503. Answers the same single-use 90-second pairing deep link as review-pairing.
- `GET /api/auth/oidc/login` (alias `/auth/sso/login`) — starts the authorization-code flow for signing in.
- `POST /api/settings/sso/link` — links the provider identity to the *caller's own* account. Requires the account password (and the two-factor code, when one is enrolled) re-entered now, because a linked identity is a way to sign in.
- `GET /api/auth/oidc/callback` (alias `/auth/sso/callback`) — verifies the ID token, including a positive issuance timestamp no more than 30 seconds ahead of the server, then signs in, auto-provisions, or links by `sub`. Keep issuer/server clocks synchronized. The session remembers the provider's `sid`, so the provider can end it.
- `POST /api/auth/oidc/step-up`, `GET|DELETE /api/auth/oidc/step-up/{id}` — action-bound re-authentication for a session signed in through KySignOn. A sensitive request (the Security page gate, backup actions) answers `403 {"error":"sso_step_up_required","challenge":…}`; the browser opens a KySignOn sign-in popup for that one action (`prompt=login`), and repeats the request with `X-Kypost-Step-Up: <challenge>` once it is verified. The grant is spent once, for that request only.
- `POST /api/auth/oidc/backchannel-logout` — OpenID Connect back-channel logout receiver. Register it at the provider (KySignOn: the client's *back-channel logout URI*). The `logout_token` is verified against the issuer's JWKS, admitted once durably, and ends the session it names, or every session of the subject when it names none. Needs an `https` issuer.
- `POST /api/settings/sso/unlink`
- `GET|PUT /api/admin/sso` (admin only. The provider configuration. The client secret is write-only: responses carry `clientSecretSet`, and a blank secret on `PUT` keeps the stored one only for the same issuer and client ID. `PUT` needs the step-up below.)
- `POST /api/sync/webhook` — KySignOn directory push. Pair KyPost in KySignOn as a webhook system with this URL and either the pairing secret or the SSO client secret as the sync secret. Each event is a signed, versioned SCIM User; stale or reordered deliveries are refused, a disabled or deleted user keeps their mailbox and loses access, and a rehire brings the same account back. Supported SCIM fields are retained durably alongside the revision fence for later repair; acknowledgment does not mean a domain mailbox or receiver route is ready.

Multi-factor authentication:

- `GET /api/mfa/status`
- `POST /api/mfa/totp/setup`
- `POST /api/mfa/totp/confirm`
- `POST /api/mfa/totp/disable`
- `POST /api/mfa/recovery-codes/regenerate`
- `PUT /api/mfa/push/enabled`
- `POST /api/auth/mfa/totp` and `POST /api/auth/mfa/recovery-code` (login-time verification)
- `POST /api/auth/mfa/push/poll`, `POST /api/auth/mfa/push/finish`, and `POST /api/mfa/push/respond` (push-approval sign-in)

User management (admin only). Every non-GET route needs a step-up bound to the exact request: the acting admin's credential as `adminPassword`/`adminAuthSecret` in the body, or a KySignOn confirmation for a KySignOn session.

- `GET|POST /api/users`
- `PUT /api/users/{id}` (change role)
- `POST /api/users/{id}/reset-password`
- `POST /api/users/{id}/deactivate`
- `POST /api/users/{id}/reactivate`
- `POST /api/users/{id}/clear-mfa`
- `GET|PUT|DELETE /api/users/{id}/imap-config` (assign a user's IMAP/SMTP credentials; `managed: true` locks the user's own route)
- `GET|PUT|DELETE /api/users/{id}/carddav-client` (same for the outbound CardDAV client)

Runtime:

- `GET /api/status`
- `GET /api/health`
- `GET /healthz` (public `ky.health/1` status for KyPulse: 200 `ok` or 503 `down`; one cached `service` check covers the API and daemon without exposing mailbox, version, or failure details. `/api/health` keeps its existing detailed response.)
- `POST /api/admin/mail/poll-now` (admin only. Starts an immediate poll.)
- `GET /api/setup` (reports whether the initial admin setup completed)
- `GET /api/server/version` (the running version and whether a newer release exists)
- `GET /api/ollama/version` (the classifier runtime's version)
- `GET /pickup/{id}?t=<token>` (single-use mobile pickup link), plus `POST /pickup/{id}/open` and `POST /pickup/{id}/blob` for a client-sealed pickup

Config and data:

- Scheduled send: `POST /api/mail/send` and `POST /api/mail/send-pgp` take an optional `sendAt` (RFC 3339, one minute to 366 days ahead) and answer `202 {"scheduled":true,"id","sendAt"}` instead of sending. The message is built and frozen then, dated `sendAt`. Refused when scheduling: calendar replies, recipients without a key (no pickup link is ever scheduled) and mail without a To. A client-custody account must send its encrypted draft (`pgpDraft` on `/api/mail/send`, `draftCopy` on send-pgp, whose every delivery `Date` must be within a minute of `sendAt`); an encrypted `/api/mail/send` without one returns to Drafts as its self-encrypted Sent copy, and is refused without a key of your own. At most 50 pending and 256 MiB of pending data per user (409); a single message over 128 MiB is refused (413). If the password, two-factor, role, sign-in, PGP keys or keyring generation, sending device, send-as alias or IMAP account changed by then, the message is not sent and returns to Drafts with a reason. A definite SMTP refusal retries every 5 minutes until an hour after `sendAt`, then returns to Drafts; an unconfirmed acceptance becomes `uncertain` and is never retried. Native mailboxes enter the outbox only when due (status `queued`; `GET /api/mail/outbox/{id}` with the same id).
- `GET /api/scheduled` — the selected mailbox's scheduled mail: `{scheduled:[{id,status,reason,sendAt,subject,to}]}`, status `scheduled|sending|returning|returned|sent|queued|uncertain|failed`. Subject is the encrypted-placeholder for browser-encrypted mail. Final rows are kept 30 days.
- `POST /api/scheduled/{id}/cancel` — returns a `scheduled` message to Drafts (200, or 202 with `warning` when the draft save is retried later); 409 once sending has started; 404 for another user's or mailbox's id.
- `GET /api/mail/outbox/{id}` — session/device-authenticated native delivery state for the acting owner, with attempts/retry timing and Sent status; no MIME or credentials. Native send replies add `outboxId`; success still requires confirmed primary SMTP acceptance. Inspect uncertain jobs before resubmitting. See [outbox contract](NATIVE_OUTBOX.md).
- `GET|PUT /api/admin/mail-relay` — admin-only encrypted domain relay settings. PUT accepts `{host,port,smtpUsername,smtpPassword,domains?,password}` (or `authSecret` for account confirmation), requires CSRF/request-bound step-up and fresh issuer-bound DNS proof of every newly added relay domain and of at least one domain overall, and defaults port to 465. Omitted `domains` keeps the saved set (the founding domain for a new relay); a body with only `domains` changes the set and keeps the generation; removing a domain with queued or retryable jobs is refused and moves it to `retiredDomains`. Responses redact both relay credentials and report configured native capability in `sendingEnabled` (not provider readiness); this native profile requires verified implicit TLS and AUTH. See [relay contract](DOMAIN_RELAY.md).
- `POST /api/admin/mail-relay/test` — admin-confirmed no-mail check of the saved `expectedGeneration`, using the same strict TLS/AUTH policy as delivery. Fresh domain/issuer and restore gates run before and after connection; one check per instance per 30 seconds. No MAIL/RCPT/DATA or outbox writes; success reports `deliveryTested:false` and proves neither From authorization nor inbox placement. See [check contract](DOMAIN_RELAY.md#check-the-saved-relay-without-sending-mail).

- `GET|PUT /api/config` (GET omits `redaction.patterns` for non-admins; PUT is admin only)
- `GET /api/labels`
- `GET|PUT /api/labels/preferences` (the caller's own label list, auto-apply preference and optional label descriptions for the embedding sorter. `PUT` replaces the whole block.)
- `GET /api/decisions` (the caller's own decisions)
- `GET|PUT /api/tuning` (the caller's own tuning prompt)
- `GET /api/mail-defaults` (any user) / `PUT /api/mail-defaults` (admin): instance-wide host and port defaults, no credentials

IMAP and inbox:

- `GET|POST|DELETE /api/imap/config`, returns 403 while `managed`
- `POST /api/imap/test`
- `GET /api/inbox?limit=500&mailbox=<name>`. Add `bodies=0` to get the list without message bodies — 13.3 MiB against 3.1 KiB for a 500-message window, since the rows render no body. The web UI then preloads the current 20-message page one body at a time from `GET /api/mail/body`, so the list renders first and opening a displayed message normally needs no wait. See [docs/INBOX_PAYLOAD_HANDOFF.md](INBOX_PAYLOAD_HANDOFF.md). Add `before=<messageId>` for the next page of older mail (metadata only, with `hasMore`/`nextBefore`); see [docs/PLATFORM_BASELINE.md](PLATFORM_BASELINE.md) §7.
- `POST /api/admin/native-recovery/challenge` and `/api/admin/native-recovery/evidence` — administrator CSRF and exact-action confirmation bind one signed KyIdentity export to a usable restore epoch and complete retained subjects. Import preserves the hold and account state while installing durable revision barriers only for unpublished native reservations; published accounts retain ordinary offboarding/demotion. It grants no recovery readiness. See [protected recovery procedure](NATIVE_RESTORE_AUTHORITY.md#protected-consumer-procedure).
- `GET /api/admin/native-recovery/status` — administrator, read-only: each native restore hold release precondition with its reasons, the `KYPOST_NATIVE_RESTORE_RELEASE` flag, Cloudflare fence state and ordered next steps; never evidence contents. `kypost-server restore status`, run as the `STATE_DIR` owner, prints the same. See [reading release status](RESTORE.md#reading-release-status).
- `POST /api/admin/native-recovery/repair` — separately confirmed administrator repair using the current stored signed evidence. Reconciles existing native activity/roles, persists replay barriers before account writes, and revokes sessions, device/push, pairing and CardDAV credentials before recording completion. Legacy accounts and retained mail/PGP data are preserved. Partial failures keep the restore held and require fresh evidence; success also preserves the hold. See [protected recovery procedure](NATIVE_RESTORE_AUTHORITY.md#protected-consumer-procedure).
- `POST /api/admin/native-recovery/release` — off unless `KYPOST_NATIVE_RESTORE_RELEASE=true`. An unlinked local administrator on a password session confirms the credential; the server rechecks every release precondition under its fences and a fresh DNS proof, and on the bundled-receiver profile requires `"confirm": "original-host-decommissioned"`. It deactivates non-native accounts the evidence shows inactive, demotes non-native administrators it shows without the administrator role, writes release floors and ID-token fences, then renames the hold to `native-restore-released.json`. A repeat answers `alreadyReleased`. See [releasing the hold](RESTORE.md#releasing-the-hold).
- `GET|PUT /api/admin/mail-domain` and `POST /api/admin/mail-domain/verify` — admin-only mail-domain diagnostics/setup. Writes require CSRF and the current account credential (KySignOn uses request-bound step-up). Configure `{domain,password}` or `{domain,authSecret}` after KyIdentity pairing, publish the returned exact TXT record, then verify with the same credential fields. These routes serve the founding domain. DNS proof binds one immutable domain/issuer, initial verification expires after 24 hours, and configuring again rotates the challenge. Once established, the same TXT record is rechecked automatically without daily DNS changes. System DNS is not DNSSEC; every allocation rechecks it. Explicit KYPOST_NATIVE_MAIL=true enables provisioning and local mailbox access after verified directory assignment; verification alone enables no reception or sending. See [setup and failure responses](NATIVE_PROVISIONING.md).
- `GET|POST /api/admin/mail-domains`, `POST /api/admin/mail-domains/{domain}/verify` and `DELETE /api/admin/mail-domains/{domain}` — admin-only domain set: list per-domain status (`configured`, `retired`, `recordName`, `recordValue`, `established`, `expiresAt`, `verifiedUntil`), add a domain or rotate its challenge with `{domain}`, verify one domain, and retire one (409 while an address on it is active, a queued/retryable outbox job sends from it, accepted (pending) incoming mail is bound to it or the relay still sends for it; address records are kept; re-adding a retired domain configures it afresh, and its addresses resume for their recorded mailboxes once it verifies again). Same CSRF, credential and step-up rules as the single-domain routes; each proof fences only its own domain. See [several domains](NATIVE_PROVISIONING.md#several-domains).
- `GET|POST /api/admin/mail-addresses`, `DELETE /api/admin/mail-addresses/{address}` and `POST /api/admin/mail-addresses/{address}/reassign` — admin-only native aliases: list mailboxes and their addresses (`address`, `kind`, `state`, `generation`; `?user=<id>` filters) with each mailbox's `usedBytes`/`quotaBytes` and a `storage` summary of quotas against the drive (`overcommitted` above 80%), add an alias with `{mailbox,address}` to an everyday identity's mailbox on a configured domain, release one (kept as `reserved`, never deleted) and reassign a reserved one with `{mailbox}`. Each change of owner or state moves the address to a new generation, so mail or outbox jobs frozen against an older one are quarantined, never delivered; ordinary KyIdentity edits change no generation. 400 malformed, 404 unknown, 409 taken/reserved/primary/a KyIdentity primary/administrator owner/unconfigured domain/restore hold; a committed change whose receiving-route update is still pending answers 200 with a `warning`. Same CSRF, credential and step-up rules as the domain routes. Users send from an active alias by naming it in `from`. See [addresses](NATIVE_PROVISIONING.md#addresses-aliases-and-generations).
- `GET|POST /api/admin/mailboxes`, `POST /api/admin/mailboxes/{id}/disable` and `POST /api/admin/mailboxes/{id}/enable` — admin-only extra native mailboxes: list mailboxes with `kind` and `state` (`?user=<id>` filters), create one for an everyday identity with `{user,address}` (its primary address on an established domain, unique and not any KyIdentity primary; stored under `$STATE_DIR/mailboxes/`), and disable or re-enable it (mail is kept; its addresses stop routing and sending; jobs still queued in its outbox are quarantined). Listings carry `prepared`. Creation is 409 while the owner has incoming encryption on, and enabling incoming encryption is 409 while the user has an extra mailbox: it covers the primary only. Same gates and error mapping as the alias routes. See [extra mailboxes](NATIVE_PROVISIONING.md#extra-mailboxes).
- `GET /api/admin/receiving/cloudflare` — admin-only continuous Cloudflare receiving status: `running`, `fenced`, `error`, `uninitialized` or `not-started`, last published revision, last publish and pickup times, oldest waiting capture and its one-hour warning, waiting and ledger counts. Never addresses, envelopes or credentials.
- `GET /api/admin/receiving/quarantine[?after=<sequence>]`, `POST /api/admin/receiving/quarantine/{gateway}/{id}/release` and `POST /api/admin/receiving/quarantine/{gateway}/{id}/discard` — admin-only quarantined native deliveries: list up to 100 envelopes (gateway, ID, received time, envelope sender, size, and per recipient the address, frozen mailbox, owning user and generation; never body, subject or headers), release one to the mailboxes it was frozen to (refused with 409 and the reason when a frozen mailbox was deleted or disabled, changed owner or is no longer admitted; never to an address's current owner), or discard it (bytes removed, a tombstone blocks redelivery; `partially_released` when an interrupted release may already have reached some mailboxes). Release refusals say whether the cause is durable (discard remains) or transient (resync and retry). An `unresolved` delivery (continuous Cloudflare mail captured under a routing table this server does not know, typically after a restore) lists its address's owner today (`currentMailbox`, `currentUser`) and is released only with `{"toCurrentOwner": true, "currentMailbox": "<as listed>"}` to that owner (refused if the address moved since it was listed), not proven to be the original, while the address is active; that action is audited separately. POSTs need CSRF and the account credential or KySignOn step-up; every action is audited without content. See [quarantine release](NATIVE_PROVISIONING.md#quarantine-release).
- `GET|POST /api/admin/receiving/blocks` and `DELETE /api/admin/receiving/blocks/{id}` — admin-only sender blocks for both receiving profiles: list blocks in force (`id`, `kind`, `value`, `until` in Unix ms or null, `source` `manual` or `automatic`, `level`, `createdAt`, `actor`, `reason`) plus automatic-block `evidence` status (`damaged`, `resetAt`, `domainBlocksFrom`, `goodFull`, `automaticFull`), block an `address` or `domain` with `{kind,value,until?,reason?}` (reason `spam`, `phishing`, `abuse` or `other`; any sender either profile accepts, A-Z lowercased and non-ASCII compared exactly, A-labels for internationalized domains; domains match exactly, not subdomains; your own mail domains and addresses on them are refused with 409, and adding a mail domain a block matches is refused until you unblock it; at most 5000), or unblock one by its listed `id` (so proxy logs never record a blocked address; this also stops automatic re-blocking of it for 30 days; if that cannot be recorded the block is still removed and the answer carries a `warning`). Maddy answers blocked senders `550 5.7.1` at RCPT; the Cloudflare Worker rejects them from the signed table, republished within seconds. Already accepted mail is unaffected and the null sender is never blocked. POST and DELETE need CSRF and the account credential or KySignOn step-up; audited with the block ID, never the address. See [sender blocks](NATIVE_PROVISIONING.md#sender-blocks).
- `GET /api/mailboxes` — the caller's accessible native mailboxes (primary first) and their active addresses, with `usedBytes` (absent when unreadable) and `quotaBytes`; disabled ones are omitted. Mail routes accept an optional `X-KyPost-Mailbox` header naming one of them (absent means the primary, so older clients are unchanged); unknown, foreign and disabled mailboxes all answer the same 404. Per-user routes ignore the header, and native notifications carry the mailbox ID. See [mailbox selection](PLATFORM_BASELINE.md#native-mailbox-selection-additive).
- `GET /api/export/folders`, `POST /api/export` and `GET /api/export/{token}` — self-service export of the caller's own native mailbox or one folder as mboxrd or a zip of EML files, exact stored bytes (encrypted mail stays encrypted). The POST needs a browser session, CSRF and account or KySignOn confirmation and returns a single-use download link valid for five minutes, bound to that session, with the message count; the GET streams it as an attachment, or redirects back to Security → Export Mail with a reason. mbox needs HTTP/1.1 to the server. External IMAP accounts get 409. See [mail export](NATIVE_PROVISIONING.md#mail-export)
- `POST /api/import`, `POST /api/import/{token}`, `GET /api/import` and `POST /api/import/cancel` — self-service import of mbox, EML or a zip of EML files into the caller's own native mailbox. The first POST needs a browser session, CSRF and account or KySignOn confirmation, creates the target folder (default `Imported`) and returns a single-use upload link valid for five minutes in that session; the upload streams the raw file (at most the mailbox's storage quota) to a temp file and starts a background job answering 202. `GET` reports `{state, imported, duplicates, skipped, bytes, error}` and the size limits. One import per user and two per server; 409 for external IMAP accounts and while incoming encryption is on. See [mail import](NATIVE_PROVISIONING.md#mail-import)
- `POST /api/import/imap`, `POST /api/import/imap/{token}/folders` and `POST /api/import/imap/{token}/start` — self-service import from another mail account over IMAP into the caller's own native mailbox. The first POST takes `{mailbox, host, port, security, username}` plus the account or KySignOn confirmation (never the provider password) and mints a 10-minute grant for that session; `.../folders` takes `{password}`, signs in (TLS on 993 or STARTTLS on 143, certificate always verified; private, loopback, link-local and CGNAT addresses refused) and lists the folders; `.../start` takes `{folders, target}` (default `Imported/<host>`) and answers 202. Read-only on the provider (EXAMINE, BODY.PEEK); the password is held in memory only until the job signs in. Failed sign-ins are limited per user (six an hour, then 429), at most four listings connect at once (503), and a job's connection reads at most twice the mailbox's storage. Status and cancel are `GET /api/import` and `POST /api/import/cancel`. See [import from another mail account](NATIVE_PROVISIONING.md#import-from-another-mail-account)
- Internal native provisioning records durable reservations and pending/applied/failed preparation status from verified directory state. Admin domain challenge/verification routes are available; KYPOST_NATIVE_MAIL=true enables new-account allocation, retained-work retries and API/daemon local mailbox selection. Direct receiving commands/import and TLS-only profile generation additionally require KYPOST_NATIVE_RECEIVING=true; mandatory STARTTLS refuses plaintext senders and no bundled public receiver starts. See [controlled receiver setup](RECEIVING_SETUP.md). Native sending (primary or an owned active alias) additionally requires the configured relay; pickup/system sending remains pending and restore-hold release is off by default (`KYPOST_NATIVE_RESTORE_RELEASE`). See [provisioning contract](NATIVE_PROVISIONING.md).
- Mail state/configuration failures return an explicit error instead of an empty inbox. Internal native-mailbox qualification rejects source mismatches with 409; switching remains disabled, and native inbox responses are full snapshots (`delta:false`, `cursor:0`). External IMAP remains the default; native runtime is opt-in.
- `POST /api/inbox/actions` (`delete`, `archive`, `spam`, `read`, `unread`, `move`, `label`, `unlabel`, `snooze`). `snooze` takes `wakeAt` (RFC3339, in the future, at most 366 days ahead; 400 otherwise), moves each message to `Snoozed`, and records it by Message-ID; a message without exactly one valid Message-ID fails individually. At the wake time the daemon moves it back to INBOX as unread. Unsnooze is a `move` to INBOX
- `GET /api/snoozes` — `{snoozes:[{messageId, wakeAt}]}` for messages in the selected mailbox's `Snoozed` folder
- Inbox rows carry `wokenAt` (RFC3339) for 30 days after a snoozed message returns to INBOX. Clients sort by `wokenAt` in place of `atUtc` so the message sits at the top; the displayed date stays `atUtc`. Cursor clients receive a gained or lost `wokenAt` as an `updated` row. A native mailbox also lists a woken message that is older than the requested window
- `GET|POST|PUT|DELETE /api/inbox/folders`. `GET` answers `{parent, folders:[{path, deletable, special?}]}`; `special` is `sent`, `drafts`, `trash`, `junk` or `archive`, present only when the IMAP server marks that exact path with the RFC 6154 attribute. Such a folder is `deletable: false`; `PUT` and `DELETE` refuse it with 400
- `GET /api/mail/search`

Mail:

- `POST /api/mail/send`. Optional `attachments: [{name, mimeType, dataBase64}]`, 25 MB in total. Optional `encrypt` and `sign`. If `encrypt` is true and a recipient has no usable key, the call fails with 409. To allow the pickup-link fallback instead, set `allowPickupFallback`. See [Where your PGP private key lives](../README.md#where-your-pgp-private-key-lives). Optional `replyToMessageId` (and `replyToMailbox`) makes it a threaded reply: the server reads the original and sets `In-Reply-To`/`References` itself. Optional `calendarReply: {ics}` sends a calendar RSVP (iTIP REPLY) alongside the body.
- `POST /api/mail/draft` (the same optional `attachments` shape)
- `GET /api/mail/body?mailbox=&messageId=` (one message's body and its `bodyMode`, for clients that list with `bodies=0`; 422 if an adapter reports malformed MIME, preserving original mail)
- `GET /api/mail/attachments?mailbox=&messageId=` (lists the attachment metadata of a message; a calendar invite sent as a message body part appears as `invite.ics` with `calendarMethod`)
- `GET /api/mail/attachment?mailbox=&messageId=&index=` (downloads one attachment)
- `GET|POST /api/mail/send-as`, `POST /api/mail/send-as/{id}/confirm` and `DELETE /api/mail/send-as/{id}` (alias addresses. A new alias is unusable until the user confirms the mailed code or a DKIM-signed copy from its own domain reaches the inbox; only the DKIM proof makes it publishable. The list never returns the code.)

Filter Rules (the caller's own rules):

- `GET|POST /api/rules`
- `PUT|DELETE /api/rules/{id}`
- `POST /api/rules/reorder`
- `GET|PUT /api/rules/{id}/sieve` (view and edit the raw Sieve script)
- `POST /api/rules/run` (runs the rules on demand)

PGP:

- PGP snapshots expose `pgpRevision`; identity, envelope and password writes accept optional `expectedRevision` and reject stale updates with 409. Clients must use the revision from the snapshot that produced their ciphertext. The browser requires revision support for these writes; older clients may still omit it on unconverted accounts. Versioned keyring snapshots and internal atomic storage are prepared; converted records reject legacy single-key writes. Multi-key conversion is not enabled. See [the revision contract](E2E_PGP.md#pgp-revision-preconditions).

- `POST /api/pgp/identity/generate` and `POST /api/pgp/identity/import`
- `GET|DELETE /api/pgp/identity`
- `POST /api/pgp/identity/client` (store a client-protected identity — the server never sees the private key)
- `GET /api/pgp/identity/wrapped` and `POST /api/pgp/identity/rewrap` (fetch and repair the account-password-wrapped envelope; optional `expectedFingerprint` refuses a changed identity with 409)
- `GET|PUT|DELETE /api/pgp/identity/envelope/{slot}` (session-only wrapped recovery/device copies; PUT and DELETE require the current account credential. PUT accepts `expectedFingerprint`; GET includes the identity fingerprint and public key from the same snapshot.)
- `POST /api/pgp/identity/export-legacy` (one-time export of a server-held key, so it can be migrated or backed up)
- `GET|PUT|DELETE /api/pgp/identity/envelope/{slot}` (per-slot key envelopes)
- `GET /api/pgp/bootstrap` (everything the browser needs to unlock a client-protected identity in one call)
- `GET /api/pgp/keyserver/lookup` (queries keys.openpgp.org)
- `POST /api/pgp/recipients/check` (key status for a set of recipients before you send)
- `POST /api/pgp/recipients/resolve` (resolves the key actually used for each recipient)
- `GET /api/pgp/qr/token` and `GET /api/pgp/qr/key` (public key exchange through a QR code)
- `POST /api/pgp/pickup` (creates a sealed pickup for a recipient with no usable key, so a client-protected sender can still use the secure-link fallback)
- `GET /api/mail/pgp-payload` and `POST /api/mail/send-pgp` (fetch a ciphertext for local decryption; submit a locally encrypted message. On a converted account the send carries `materialGeneration` and is refused with 409 when it is stale, and a paired device must be enrolled at that generation)

PGP key discovery and device enrollment:

- `GET|PUT /api/pgp/incoming` — caller-only incoming-encryption preference; PUT requires fresh account confirmation and enabling requires backup acknowledgment and current `expectedRevision`.
- `GET|PUT /api/pgp/discovery/settings`
- `GET /api/pgp/discovery/suppressions` and `DELETE /api/pgp/discovery/suppressions/{email}`
- `POST /api/pgp/discovery/suppress-contact`
- `GET /api/pgp/device/envelope` (a paired device fetches the envelope sealed to its own secure-element key), `POST /api/pgp/device/enrollment-key` (a device publishes its public sealing key and optional `envelopeVersions` under its pairing credential), `POST /api/pgp/device/enrollment-state` (a device reports whether it can actually read the identity and, on a converted account, which envelope version, material generation and fingerprint it holds; a stale or bare report is refused with 409). A browser delivers a sealing with `PUT /api/pgp/identity/envelope/device:<id>`, bound to the device's published key, the snapshot revision and the material generation; v3 for a converted account, v2 for a legacy one

Web Key Directory (admin only, plus the public serving path):

- `GET|POST /api/pgp/wkd/domains` and `DELETE /api/pgp/wkd/domains/{domain}`
- `POST /api/pgp/wkd/domains/{domain}/verify`
- `GET /.well-known/openpgpkey/...` (public. Serves verified users' keys — see [docs/WKD_Publishing.md](WKD_Publishing.md).)

Contacts:

- `GET|POST /api/contacts`
- `GET|PUT|DELETE /api/contacts/{id}`
- `POST /api/contacts/dedupe`
- `GET /api/contacts/search`
- `POST /api/contacts/bulk-delete`
- `GET /api/contacts/export` and `POST /api/contacts/import`
- `GET|POST|DELETE /api/contacts/dav-password` (app-specific CardDAV password)
- `GET|POST|DELETE /api/contacts/carddav-client/config` and `POST /api/contacts/carddav-client/sync` (sync from an external CardDAV server), returns 403 while `managed`
- `POST|GET|DELETE /api/contacts/{id}/photo`
- `POST /api/contacts/{id}/self`
- `GET|POST /api/contacts/sync` (mobile two-way sync. A pairing token authenticates the call.)

Groups:

- `GET|POST /api/groups`
- `PUT|DELETE /api/groups/{id}`

CardDAV server (address book sync for phones and desktop apps. A per-user DAV password authenticates the call.):

- `/.well-known/carddav`
- `/dav/...`

Notifications (all scoped to the signed-in user):

- `GET|PUT /api/notifications/preferences`
- `GET /api/notifications/vapid-public-key`
- `POST|DELETE /api/notifications/subscriptions`
- `POST /api/notifications/test`
- `GET /api/notifications/pairing`
- `POST /api/notifications/native/register` and `POST /api/notifications/native/deregister`
- `GET|DELETE /api/notifications/native/devices`
- `PUT /api/notifications/native/mode` (relay push vs. app pull)
- `PUT /api/notifications/native/devices/{deviceId}/mfa` (allow a device to approve sign-ins)
- `GET /api/notifications/native/pull` (every notification is queued here in both modes, addressed to the devices it was sent to, so a device that stops hearing from the relay can poll and catch up)
- `POST /api/notifications/native/unpair`

Logs (admin only):

- `GET /api/logs?file=<name>.log&lines=<n>`
- `GET /api/logs/list`
