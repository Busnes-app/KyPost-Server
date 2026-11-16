# Out-of-office reply

Design written 2026-11-15 (the brief's `docs/superpowers/specs/` path is gitignored in this repo, so the design lives here and becomes the contract as it ships).

Owner request, 2026-11-15: an Outlook-style automatic reply, with separate replies for
people inside the organisation and for everyone else, optionally shown in KyCalendar.
This doc covers deliverable 1 (KyPost server and web) and deliverable 2 (the KyCalendar
tie). Native clients are out of scope.

## Threat model

An automatic reply is mail the user's account sends without the user looking. So it can
be abused in these ways:

1. **Backscatter.** A spammer forges a victim's address as the sender, and the reply goes
   to the victim. The reply also confirms that the mailbox is live.
2. **Loops and storms.** Two responders answer each other, or one answers a mailing list
   or a bounce.
3. **Information leak.** The text tells an outsider that the user is away and until when.
   The remote reply is the outsider-facing part.
4. **Reflection.** Someone triggers replies that carry text they control. They cannot:
   the reply body is the user's own text, and the original body is never quoted.
5. **Agent misuse.** An MCP agent with a token turns on a reply to strangers.
6. **Duplicate sends** when a message is processed more than once (retries, deferrals,
   a second pod).

## Mitigations

- **RFC 3834.** No reply is sent when any of these hold:
  - `Auto-Submitted` is present and is not `no`.
  - `Precedence` is `bulk`, `list`, `junk` or `auto_reply`.
  - Any `List-*` header is present.
  - `X-Auto-Response-Suppress`, `X-Autoreply` or `X-Autorespond` is present.
  - `Return-Path` is `<>`.
  - The sender's local part is `mailer-daemon`, `postmaster`, `noreply`, `no-reply`
    or `donotreply`, or contains `noreply` or `no-reply`.
  - The sender is one of the user's own addresses.
  - None of the user's own addresses is in To or Cc.
  - There is no single From address, or no well-formed `Message-ID`.

  Every reply carries `Auto-Submitted: auto-replied`, `In-Reply-To`, `References`, and
  the subject `Automatic reply: <original>`. The original subject goes through
  `mailmsg.SanitizeHeaderValue` and is cut to 200 characters. The original body is never
  quoted.
- **Junk.** No reply when any of these hold:
  - One of the user's filter rules would move the message to Junk or delete it.
  - `X-Spam-Flag: YES`, or `X-Spam` / `X-Spam-Status` starts with `yes`. These are an
    upstream provider's verdict. A sender can forge the header, but forging it only
    suppresses a reply, so it is safe to trust.

  Native mail that rspamd rejects never reaches INBOX. Mail it tags as "add header" is
  accepted unmarked, which the DMARC rule below covers.
- **DMARC.** KyPost records no per-message DMARC verdict: IMAP accounts have none, and
  native mail does not keep rspamd's. So "failed DMARC" is enforced the strict way: KyPost
  replies only when a DKIM signature whose `d=` equals the From domain verifies and covers
  From. This is the same check (`imapadapter.DKIMAlignedFrom`) that KyCalendar invitation
  forwarding uses, and the reply goes to that authenticated address. Unsigned or
  misaligned mail gets no reply. This is stricter than "not failed": an SPF-only DMARC
  pass gets no reply. That is chosen because it is the one check that proves the sender
  address is real, which is what stops backscatter. Major providers DKIM-sign everything.
  Owner question 1.
- **Rate.** Each sender gets one reply per 4 days, or one per out-of-office period if the
  period is shorter. A new period starts whenever the user turns the reply on or changes
  its start or end. Every reply is also charged to the user's normal send quota (one
  recipient); a refused charge means no reply.
- **Once per message.** Before sending, the reply claims the original's `Message-ID` in
  the user's state store. The claim and the sender check are one transaction, so a retry,
  a deferral or a second worker cannot send twice. If sending then fails, the claim
  stands: missing one reply is better than risking two.
- **Remote is opt-in twice.** An administrator switch (Admin → Application, "Allow
  out-of-office replies to outside senders") is off by default. While it is off, the
  remote reply is never sent and the web hides it. The user's remote setting itself
  defaults to `off`.
- **Agents.** MCP read tokens may read the setting. Changing it needs `mcp:organize`.
  An agent may change on/off, start, end and the local text. It may never change the
  remote mode or text, and may not turn the reply on while the remote reply is `on` or
  `contacts`, because that would switch on a reply to strangers. It can always turn the
  reply off.
- **Logs** carry the user ID, the mailbox UID and a reason code only. They never carry an
  address, subject, name or the reply text.

## Data

**Settings**, in the user's existing settings file (`config.UserSettings.OutOfOffice`,
served only by its own route, like signatures):

| Field | Type | Default | Rule |
|---|---|---|---|
| `enabled` | bool | false | |
| `start`, `end` | RFC 3339 instant or empty | empty | `end` after `start`; either may be empty. The browser converts the user's local date and time. |
| `timeZone` | IANA name | empty | Checked with `time.LoadLocation`, at most 64 bytes. The browser's zone; used for display and for KyCalendar all-day events. |
| `localMessage` | text | empty | Plain text, at most 4000 characters, no control characters except newline and tab, no bidi controls. |
| `remote` | `off`, `on` or `contacts` | `off` | `contacts` replies only to senders in the user's own contacts (key-discovery contacts excluded). |
| `remoteMessage` | text | empty | As `localMessage`. |
| `period` | server-set token | | Renewed when the reply is turned on or `start`/`end` change. Never accepted from a client. |

Turning the reply on with an empty text for a reply that would be sent is refused.

**State**, in the primary mailbox's `state.Store` (SQLite or Postgres):

- `ooo_messages(message_key PK, at)`: one row per original `Message-ID`, hashed with the
  mailbox. Rows older than 30 days are pruned.
- `ooo_senders(sender PK, period, at)`: the last reply to each address. Rows older than
  4 days are pruned. The table is capped at 20000 rows. A full table sends nothing more
  until rows age out.

## Who counts as local

The sender's DKIM-aligned From is local when either holds:

- its domain is an established native mail domain (`sso.NativeDomainStore`), and the
  polled mailbox is native. Retired and unproven domains do not count: a retired domain
  may now belong to anyone; or
- KyIdentity's suite directory resolves it to a person (`suitedir.Resolve`) under the
  user's linked subject.

Everyone else is remote. A failed directory lookup counts as remote. The remote reply is
the more guarded one, and usually off.

## Where it runs

In `processor.processBatch`, for each new INBOX message, beside the KyCalendar invitation
forwarder. That one loop serves both external IMAP accounts and native mailboxes. It runs
only for the user's primary mailbox, never an extra (shared) mailbox, and only while the
reply is on and inside its period. Only then does it fetch the raw message.

**Sending:**

- **Native mailbox:** the reply is queued in the native outbox (`sso.NativeOutbound.Queue`)
  with an ID derived from the original. The outbox delivers it, retries it and files it
  in Sent.
- **External account:** the reply goes through the account's SMTP server, and a copy is
  appended to Sent, as the poller's existing oversize notice does.

## Routes

| Route | Auth | Notes |
|---|---|---|
| `GET /api/mail/out-of-office` | session or paired device (`withMailAuth`) | `{settings…, active, remoteAllowed}`. `no-store`. |
| `PUT /api/mail/out-of-office` | session + CSRF (`withAuth`) | Whole document, 32 KiB body limit, unknown fields refused. Answers the stored document. |
| MCP `get_out_of_office` | any agent token | |
| MCP `set_out_of_office` | `mcp:organize` | Rules above. |

The administrator switch is a field of the existing server settings document
(`outOfOfficeRemote`). It adds no route.

## Web

- **Settings → Mail → Out of office:**
  - On/off, start and end (`datetime-local`), and the local message.
  - The remote reply as a select (off / everyone / my contacts only) with its message.
    Shown only when the administrator allows it.
  - The time zone in use is shown.
- **Inbox:** while a reply is on, a bar reads "Out of office is on", with the period
  when there is one, and a "Turn off" button that saves `enabled:false`.

## KyCalendar tie

Built under the existing pair; the wire contract is in [KYCALENDAR.md](KYCALENDAR.md) ("Out of
office").

- Two more settings, shown only when KyCalendar takes this user's period: `calendarAway` ("Show
  me as away in KyCalendar") and `calendarTentative` ("Answer new invitations Tentative while
  I'm away", which needs `calendarAway`). KyCalendar takes the period when it is paired, the
  user's KyCalendar switch is on, and the account has a live SSO link. `GET` reports this as
  `calendarAvailable`.
- **Every save sends the whole state** (`kypost.ooo.set`): `away` is true only while the reply
  is on, both dates are set and `calendarAway` is ticked. Saves come from the web, the inbox
  Turn off and the MCP agent. The PUT answer's `calendarSync` is `synced` or `failed`, and the
  web says so. The next save sends again; nothing retries in the background.
- KyCalendar keeps one integration-owned `TRANSP:OPAQUE` out-of-office event and, when asked,
  answers new invitations TENTATIVE with `comment_local`, or with `comment_remote`. KyPost
  sends `comment_remote` only while its remote reply goes to everyone and the administrator
  allows it. KyCalendar never turns out-of-office on.
- Turning the administrator switch off resends every calendar-tied state without the remote
  comment, and a REPLY KyCalendar asks KyPost to mail loses any `COMMENT` that is an outside
  reply KyCalendar may still hold, unless the reply is on and active with both calendar boxes
  ticked and the remote reply goes to everyone with the administrator's permission
  (`stripRemoteOOOComment`). Those texts are the server-only `calendarRemoteTexts`, each with the
  revision of the newest sync that carried it. They are added before each send, and an `applied`
  answer for revision R forgets only those last carried before R. At most 32 are kept; past
  that, a sync with a new text is refused, not sent.
- An agent's save keeps the calendar settings as they are.

## Not built

- Replies from extra (shared) mailboxes.
- HTML replies, per-sender exceptions, and forwarding while away.
- A per-message DMARC record for native mail.
- Rescheduling replies that the outbox gives up on.
- Native clients (Android, Linux, Mac). They read `GET /api/mail/out-of-office` once
  their follow-ups land.
- Sieve `vacation` import.

## Owner questions (defaults in force)

1. Reply only to DKIM-aligned senders. The default is yes. The alternative would reply to
   unauthenticated mail and invite backscatter.
2. The remote reply is behind an administrator switch that defaults to off. The default
   is yes, per the rule for outsider-facing features.
3. Primary mailbox only. The default is yes; shared mailboxes have no personal "away".
