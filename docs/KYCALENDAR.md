# KyCalendar invitation mail and tasks

Server-to-server contract between KyPost and KyCalendar. KyCalendar sends
invitations, cancellations and replies through the user's own KyPost mailbox,
and KyPost hands KyCalendar the invitations that arrive there. KyCalendar's
half is already built; change both sides together.

## Pairing

KyPost pairs through KyIdentity on its own, or by environment.

**Environment.** Set, these win entirely and KyIdentity is never asked:

| Variable | Meaning |
|---|---|
| `KYPOST_KYCALENDAR_URL` | KyCalendar's `https://host[:port]`, no path, query or credentials. |
| `KYPOST_KYCALENDAR_SECRET_FILE` | File holding the shared secret: at least 32 bytes after trimming whitespace. Never an environment value. |
| `KYPOST_KYCALENDAR_ALLOW_PRIVATE` | `true` lets KyPost reach KyCalendar at a private LAN, CGNAT or reserved address. Default `false`. Applies to both ways of pairing. |

One set without the other, a URL that is not `https://host`, a loopback
URL, or a missing or short secret refuses startup.

**KyIdentity.** With both unset and KyPost's SSO enabled with a client
secret (Server → Single Sign-On, or the installer), each KyPost process asks
KyIdentity's suite directory, `GET {issuer}/api/suite/peer?client=kycalendar`
with KyPost's own client credentials (KyIdentity's `docs/SUITE-DIRECTORY.md`,
client `ky-primitives/suitedir`), for KyCalendar's URL and the 32-byte
secret KyIdentity also gives KyCalendar. The answer must pass the URL rules
above, and its host must not resolve to an address the private rule
refuses. The secret is held in memory only, never written or logged.

It asks at startup, every 15 minutes, within 15 seconds of an SSO settings
change, and at most once a minute after a bad signature either way (KyCalendar
answered `bad_signature`, or a request to KyPost failed verification), which
is how a rotated secret is picked up without a restart. A failure leaves
KyCalendar unpaired and is retried after 30 seconds, doubling to 15 minutes.
One exception: when KyIdentity is unreachable or erroring, a pairing the same
SSO settings produced is kept, so a KyIdentity outage alone does not unpair a
working KyCalendar. `no_peer`, refused client credentials, a bad answer or
changed settings unpair at once. SSO off, or no pairing either way, is off.

Off: `POST /api/calendar/itip/send` answers 404 `kycalendar_off` and nothing
is forwarded. Outbound calls are HTTPS only, never follow a redirect, and
never reach loopback, unspecified, link-local or multicast addresses; the
host is resolved once and the checked address dialled.

`GET /api/admin/kycalendar` (administrators) reports `{source, connected,
url, reason}`: `source` is `kyidentity`, `env` or empty; never the secret.
The web admin shows it under Server → KyCalendar.

Each user has one switch under Settings → Mail → KyCalendar ("Let KyCalendar
send invitations from this mailbox"). It covers both directions. A user who
has not chosen follows the pairing: on when KyIdentity paired KyCalendar, off
when the environment did. A stored choice, `kyCalendar: true` or `false` in
their settings, always wins. Earlier versions wrote `kyCalendar: false` for
every user who saved any setting; it cannot be told from a deliberate opt-out,
so it is kept as off and such a user turns it on once. Only
accounts with a live SSO link (subject and issuer) take part.

## Signing

Every request is signed with `ky-primitives/syncauth` (scheme v1) under the
shared secret; headers via `Headers.Apply` / `FromRequest`.

| Event type | Direction | Route |
|---|---|---|
| `kycalendar.itip.send` | KyCalendar → KyPost | `POST /api/calendar/itip/send` |
| `kypost.itip.inbound` | KyPost → KyCalendar | `POST {KYPOST_KYCALENDAR_URL}/api/itip/inbound` |
| `kypost.itip.respond` | KyPost → KyCalendar | `POST {KYPOST_KYCALENDAR_URL}/api/itip/respond` |
| `kypost.task.flag` | KyPost → KyCalendar | `POST {KYPOST_KYCALENDAR_URL}/api/tasks/flag` |
| `kycalendar.task.state` | KyCalendar → KyPost | `POST /api/calendar/tasks/state` |

A receiver checks the event type equals its route's, then `syncauth.Verify`
with a replay guard (`DefaultWindow`, 100000 IDs). A verified replay is
answered 200 `{"outcome":"duplicate"}` without acting again; any other
failure is 401 `{"error":"Bad signature","code":"bad_signature"}`. Event IDs
are stable across the sender's retries of one message.

KyPost's send route forgets an event ID when it refused or failed without
sending, so KyCalendar's retry of that ID is processed again. It keeps the ID
when the SMTP server's acceptance was uncertain: the message may have gone
out. The guard is in memory; a restart forgets it, which the 5-minute
signature window bounds.

Every body names the user by `"sub"` (KyIdentity subject) and `"issuer"`.

## Send: KyCalendar → KyPost

```json
{"sub":"…","issuer":"https://id.example","from":"alice@example.com",
 "recipients":["bob@example.net"],"method":"REQUEST","ics":"BEGIN:VCALENDAR…"}
```

Checks run in this order. Every refusal is `{"error","code"}`; codes are
`[a-z0-9_]`, at most 64 characters.

| Status | Code | When |
|---|---|---|
| 404 | `kycalendar_off` | KyPost is not paired. |
| 401 | `bad_signature` | Wrong event type or signature. |
| 200 | `duplicate` (as `outcome`) | Verified replay; nothing sent. |
| 400 | `bad_body` | Malformed JSON, a missing field, a recipient that is not a bare address, more than 500 recipients, or `method` not `REQUEST`, `CANCEL` or `REPLY`. |
| 404 | `no_user` | No active account for `(issuer, sub)`, or its SSO link is revoked, or (native mail on) a dedicated administrator identity. |
| 409 | `sending_off` | The user has not turned KyCalendar on. |
| 409 | `no_mailbox` | No outgoing mail server configured. Native mailbox: native mail off, no domain relay, or the relay does not send for `from`'s domain. |
| 409 | `bad_from` | `from` is neither the account address nor a verified send-as alias. Native mailbox: `from` is not an active address of one of the user's own mailboxes. |
| 422 | `bad_ics` | Not exactly one VCALENDAR whose single METHOD equals `method`, with VEVENTs sharing one UID and one mailto: ORGANIZER; over 1 MiB; a parameter value over 1 KiB; control characters. |
| 409 | `bad_recipient` | REQUEST/CANCEL: `from` is not the ORGANIZER, or a recipient is not an ATTENDEE. REPLY: `from` is not an ATTENDEE, or a recipient is not the ORGANIZER. Case-insensitive. |
| 409 | `no_invitation` | REPLY only: this mailbox has no recorded invitation with that UID from that ORGANIZER (provenance, below). |
| 429 | `rate_limited` | The user's sending quota; `Retry-After` set. Charged per recipient, keyed by the event ID. |
| 422 | `too_many_recipients` | More recipients than the user's daily quota allows at all. |
| 503 | `unavailable` | Storage unreadable; retry. |
| 503 | `smtp_failed` | The user's SMTP server (native: the domain relay) did not accept; retry. |
| 202 | `sent` (as `outcome`) | Delivered and a copy saved to Sent. |

The message is a fixed template: From the user's address (with the display
name of a send-as alias, when `from` is one), To the recipients, Subject the
first VEVENT's SUMMARY without control characters and cut to 200 bytes
(else `Invitation`, `Cancelled` or `Reply`), a one-line plain-text body, and
the object as a `text/calendar; method=<METHOD>; charset=UTF-8` alternative.
No caller-supplied header reaches the message. It goes out through the
user's own SMTP account and is charged to their quota like any send.

A KyPost-hosted (native) mailbox sends the same message through the native
outbox (`docs/NATIVE_OUTBOX.md`) under the same fresh admission as compose:
from the mailbox owning `from` (the primary or an extra mailbox of the same
user), relay or direct mode, with the outbox filing the Sent copy. The quota
charge is the same, keyed by the event ID. The replay guard forgets the event
ID only when the outbox refused before storing the job, or every delivery
ended `failed` with no further attempt; otherwise (accepted, uncertain, or
left for the outbox to retry) the ID is kept, because a second job would send
a second copy. Direct mode answers `sent` when the first recipient accepted;
the outbox workers finish the rest.

ATTENDEE and ORGANIZER are read from VEVENTs only; an ATTENDEE inside a
VALARM is not a recipient.

## Inbound: KyPost → KyCalendar

For each new message in a polled mailbox (IMAP, or a native primary or
extra mailbox: the poller runs one path for both) of a user with KyCalendar on, the
poller looks for a `text/calendar` part whose METHOD is `REQUEST`, `REPLY`
or `CANCEL` and that passes the `bad_ics` rules above. It forwards it only
when a DKIM signature whose `d=` exactly equals the From address's domain
verifies and its signed headers include From (occurring once); the address
forwarded is the signed From's. Not aligned: nothing happens and the mail
stays ordinary, with its RSVP card.

```json
{"sub":"…","issuer":"…","from":"alice@example.com","dkim_aligned":true,"ics":"BEGIN:VCALENDAR…"}
```

The event ID is `kypost-inbound-` plus a hash of the account, mailbox and
message ID, so a message seen again is the same event. KyCalendar answers
200 `{"outcome":"applied"|"pending"|"ignored"|"duplicate"}`, 422
`unverified_sender`/`bad_itip`, 404 `no_user`, 429 `rate_limited`, 401
`bad_signature`. A 429, 5xx or network failure is retried on the user's
next poll ticks, at most three attempts in all, then dropped; retries live
in memory, so a restart drops them. KyCalendar's errors never fail the
message, and the logs carry IDs only.

### Provenance

For an aligned `METHOD:REQUEST` whose ORGANIZER equals the signed From
address, KyPost records `(UID, organizer)` in the user's
`$STATE_DIR/users/<id>/kycalendar-invitations.json` (newest 2000) before
forwarding. A REPLY through the send route needs a matching record, so a
user can answer only invitations their mailbox really received from that
organizer, and cannot aim replies at an arbitrary address. An invitation
signed by someone other than its organizer is forwarded but not recorded.

## Respond: KyPost → KyCalendar

KyPost's clients answer an invitation with `POST /api/calendar/rsvp`
(session or paired device, CSRF as for mail routes; `X-KyPost-Mailbox`
selects the mailbox):

```json
{"messageId":"…","mailbox":"INBOX","index":1,"partstat":"ACCEPTED","recurrenceId":"20261020T100000Z"}
```

`index` is the attachment index from `GET /api/mail/attachments` (the
`invite.ics` entry). KyPost reads that part from the message itself; a
client never supplies the ics. It must be a `text/calendar` METHOD:REQUEST
object passing the `bad_ics` rules. KyPost then posts:

```json
{"sub":"…","issuer":"…","uid":"…","recurrence_id":"…","partstat":"ACCEPTED","ics":"<the invitation as mailed>"}
```

with a fresh event ID (KyPost does not retry it; the user does).

| KyCalendar answer | Client sees |
|---|---|
| 200 `answered` | 200 `{"outcome":"answered"}` |
| 404 `no_event` | 404 `no_event` |
| 404 `no_user` | 409 `kycalendar_off` (the user has no KyCalendar account) |
| 422 `not_invited` | 422 `not_invited` |
| 400 `bad_partstat` | 400 `bad_partstat` |
| 409 `conflict` | 409 `conflict` (reload and answer again) |
| 507 `quota` | 507 `quota` |
| anything else, or unreachable | 502 `kycalendar_unavailable` |

The route's own refusals: 400 `bad_request`/`bad_partstat` (partstat must be
`ACCEPTED`, `TENTATIVE` or `DECLINED`), 404 `not_found`, 422
`not_invitation`, 409 `kycalendar_off`. On `kycalendar_off` — KyPost not
paired, the user's switch off, no SSO link, or no KyCalendar account — the client sends the RSVP
the old way: `calendarReply` on `POST /api/mail/send`. The web client does
the same, after the same confirmation, on 404 `no_event`: an invitation
KyCalendar never received (unaligned, encrypted, or older than pairing) can
still be answered.
Native clients follow [NATIVE-RSVP.md](NATIVE-RSVP.md).

## Out of office: KyPost → KyCalendar

Section O of the suite contracts, added 2026-11-15 under this pair (no new pair). KyPost's
out-of-office setting is the single source of truth (KyPost `docs/OUT_OF_OFFICE.md`).
KyCalendar stores what KyPost last sent and never turns out-of-office on itself, so the two
cannot loop.

`POST {KyCalendar}/api/kypost/out-of-office`, event type `kypost.ooo.set`, signed with
`ky-primitives/syncauth` v1 under the pair secret. The body is the user's whole state, every
time:

```json
{"sub":"…","issuer":"https://id.example","revision":1794760000123,
 "away":true,"start":"2026-12-01T05:00:00Z","end":"2026-12-05T05:00:00Z",
 "all_day":true,"time_zone":"America/New_York",
 "tentative":true,"comment_local":"…","comment_remote":"…"}
```

- `revision`: KyPost's save time in milliseconds, which only grows. KyCalendar keeps the
  newest revision it has seen. An older one is answered `{"outcome":"stale"}` and changes
  nothing. The same revision again is applied again, so a retry after a failure works.
- `away` false, sent when out-of-office is off or the user unticks "Show me as away", removes
  the event and stops automatic answers. The other fields are then ignored.
- With `away` true:
  - `start` and `end` (RFC 3339) are required. `end` is after `start`, and the period is at
    most 366 days.
  - `all_day` needs an IANA `time_zone` in which both instants are midnight; the event then
    uses DATE values.
  - The comments are plain text, at most 4000 characters each, with no control characters
    except newline and tab.

**The event.** One per user, UID `kypost-ooo-<hash of the user>`, in the default calendar
(or wherever the user moved it among their own calendars; a calendar shared to them is never searched or written):

- `SUMMARY:Out of office`, `TRANSP:OPAQUE`, `X-MICROSOFT-CDO-BUSYSTATUS:OOF` and
  `X-KYCALENDAR-OUT-OF-OFFICE:TRUE`. Free/busy shows it as busy.
- A new period moves it; `away:false` deletes it. It has no attendees and produces no iTIP.

**Automatic Tentative.** With `tentative` true, while `start <= now < end`, KyCalendar answers
each new invitation `PARTSTAT=TENTATIVE`, never anything else. The REPLY carries `COMMENT`:

- `comment_local` when the organizer is a KyCalendar user;
- `comment_remote` otherwise. KyPost sends it only when its remote reply is on for everyone
  and allowed by its administrator; otherwise it is empty and the answer has no comment.

This covers external invitations (`kypost.itip.inbound` answers `applied` instead of
`pending`) and internal ones (the outbox delivery). Updates to an invitation already in the
calendar are not answered again. Such an answer is the user's standing consent, so the copy
is written the way `kypost.itip.respond` writes it.

| Status | Answer | When |
|---|---|---|
| 404 | (plain) | KyPost unlinked, or scheduling off |
| 401 | `bad_signature` | Wrong signature or event type |
| 200 | `{"outcome":"duplicate"}` | A verified replay |
| 400 | `bad_body` | Malformed JSON, or a rule above broken |
| 404 | `no_user` | No active everyday account for `(sub, issuer)`; administrator identities included |
| 429 | `rate_limited` | 60 per user per hour, its own budget (not invitations') |
| 507 | `quota` | The calendar is full |
| 500 | `error` | Retry under the same event ID; KyCalendar forgets it |
| 200 | `{"outcome":"applied"\|"stale"}` | Done, or an older revision |

**KyPost's side.**
- KyPost sends the whole state on every save (the web, an agent, the inbox Turn off) while
  the pair is up, the user's KyCalendar switch is on and the account has a live SSO link. It sends `away:true` only when out-of-office is on with both dates and
  "Show me as away in KyCalendar" is ticked.
- A failure is reported to the user and resent on their next save. KyPost never retries in
  the background.
- When the administrator turns outside replies off, KyPost resends every calendar-tied state at
  once, without `comment_remote`.
- KyPost removes from any REPLY it mails for KyCalendar each `COMMENT` that is an outside-reply
  text KyCalendar may still hold. The only exception is while the reply is on and active, both
  calendar boxes are ticked, and the remote reply goes to everyone with the administrator's
  permission.
- KyPost records each remote comment, with the revision of the newest sync carrying it, before
  sending. An `applied` answer for revision R marks superseded only the texts last carried
  before R, so answers may arrive in any order. A superseded text stays refused for 25 hours.
  A REPLY KyCalendar built earlier may still be retried in that time, and a text changed,
  cleared or turned off while KyCalendar was unreachable is still refused.
- **KyCalendar's half of that bound:** its scheduling outbox abandons an unsent message 24 hours
  after building it (`outboxGiveUp`, `internal/davbackend/scheduling.go`). Raising that limit
  means raising KyPost's 25-hour retention (`config.OOORemoteRetention`) in the same change.
- Logs and audit carry IDs only, never the period or the texts.

KyPost's half: `api/out_of_office.go` `syncOOOCalendar`, run after every
`PUT /api/mail/out-of-office` (its answer carries `calendarSync`) and every agent
`set_out_of_office`. Tests: `TestOutOfOfficeSyncsKyCalendar`, `TestOutOfOfficeRemoteCommentRevoked`, `TestOutOfOfficeRemoteCommentChangedWhileKyCalendarDown`, `TestOutOfOfficeRemoteCommentReorderedAnswers`, `TestOutOfOfficeRemoteCommentReplyOff`, `TestStripRemoteOOOComment`, config `TestOOORemoteTextsSurviveReorderedAnswers`.

## Tasks from flagged mail

Added 2026-11-16 under this pair (KyCalendar design
`docs/superpowers/specs/2026-11-15-kypost-flag-to-task-design.md`). Flagging a message in KyPost
makes a task in the user's KyCalendar Inbox list; finishing the task clears the flag. Neither
side calls the other back for a change the other made.

**KyPost → KyCalendar, `kypost.task.flag`.** Sent once per message by `POST /api/inbox/actions`
`flag` or `unflag` (web, list or reader), with a fresh event ID and a 5-second limit, while
the pair is up, the user's KyCalendar switch is on, the account has a live SSO link and
`SERVER_BASE_URL` is set:

```json
{"sub":"…","issuer":"…","ref":"<22 base64url characters>","subject":"Café plans",
 "url":"https://mail.example.com/read?flag=<ref>","due":"2026-11-20","flagged":true}
```

- `ref` is an opaque random reference KyPost records per message in the user's
  `$STATE_DIR/users/<id>/kycalendar-flags.json` (newest 2000): mailbox selector, folder, message
  reference, due date, and whether the flag was cleared. Flagging the same message again reuses
  it, so KyCalendar updates one task.
- `subject` is the message's Subject header, decoded, on one line, without control characters,
  cut to 255 bytes. Never the body, the sender, the Message-ID or the attachments.
- `url` is on `SERVER_BASE_URL`'s host. KyCalendar refuses a link on any other host than the one it
  pairs with (400 `bad_body`), so `SERVER_BASE_URL` must name the KyPost host KyIdentity lists.
- `due` is the user's local date (the web's choices: Today, Tomorrow, This week = Friday,
  Next week = Monday); absent for No date and on unflag.
- An unflag of a message KyCalendar never heard of sends nothing (`none`).
- KyCalendar answers 200 `{outcome: created|updated|completed|unchanged}` or a refusal
  (`tasks_off` when the user turned it off there, `no_user`, `bad_body`, 429). Anything but 200
  is `failed` to the client; the flag stays set in KyPost and the web says the task was not made.
  KyPost does not retry.

**KyCalendar → KyPost, `kycalendar.task.state`.** KyCalendar sends `{sub, issuer, ref, done}`
when such a task is completed, reopened or deleted on its web or over MCP (not from a CalDAV
app). The route verifies like `itip/send` (event type, signature, replay guard; 404
`kycalendar_off` unpaired). Then:

| Status | Answer | When |
|---|---|---|
| 400 | `bad_body` | Malformed JSON, or `sub`, `issuer`, `ref` or `done` missing |
| 404 | `no_user` | No active account for `(issuer, sub)`, a revoked SSO link, or (native mail) an administrator |
| 409 | `sending_off` | The user's KyCalendar switch is off |
| 503 | `unavailable` | Storage or the mailbox unreadable; the event ID is forgotten, so a retry works |
| 200 | `{"outcome":"gone"}` | Unknown `ref`, a message reference that no longer resolves, or an extra mailbox the user lost |
| 200 | `{"outcome":"unchanged"}` | The flag is already in that state |
| 200 | `{"outcome":"cleared"\|"flagged"}` | `done` true clears `\Flagged`, false sets it again |

**The link.** `/read?flag=<ref>` asks `GET /api/mail/flags/{ref}` (the signed-in user's own
records only), switches to the message's mailbox and opens it, or shows "This message is no
longer available." when the reference is unknown or the message is gone. `GET /api/mail/flags`
gives the web its Flagged marks.

KyPost's half: `api/flag_tasks.go`, `kycalendar/flags.go`. Tests: `TestFlagMakesKyCalendarTask`,
`TestTaskStateOffWhenUnpaired`, mailbox `TestNativeClientFlagAction`, web
`ReadPage.flag.test.tsx` and `App.mailboxes.test.tsx` ("flagged mail task link").
