# KyCalendar invitation mail

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
| 404 | `no_user` | No active account for `(issuer, sub)`, or its SSO link is revoked. |
| 409 | `sending_off` | The user has not turned KyCalendar on. |
| 409 | `native_unsupported` | The account is a KyPost-hosted (native) mailbox; sending through its relay is not built yet. |
| 409 | `no_mailbox` | No outgoing mail server configured. |
| 409 | `bad_from` | `from` is neither the account address nor a verified send-as alias. |
| 422 | `bad_ics` | Not exactly one VCALENDAR whose single METHOD equals `method`, with VEVENTs sharing one UID and one mailto: ORGANIZER; over 1 MiB; a parameter value over 1 KiB; control characters. |
| 409 | `bad_recipient` | REQUEST/CANCEL: `from` is not the ORGANIZER, or a recipient is not an ATTENDEE. REPLY: `from` is not an ATTENDEE, or a recipient is not the ORGANIZER. Case-insensitive. |
| 409 | `no_invitation` | REPLY only: this mailbox has no recorded invitation with that UID from that ORGANIZER (provenance, below). |
| 429 | `rate_limited` | The user's sending quota; `Retry-After` set. Charged per recipient, keyed by the event ID. |
| 422 | `too_many_recipients` | More recipients than the user's daily quota allows at all. |
| 503 | `unavailable` | Storage unreadable; retry. |
| 503 | `smtp_failed` | The user's SMTP server did not accept; retry. |
| 202 | `sent` (as `outcome`) | Delivered and a copy saved to Sent. |

The message is a fixed template: From the user's address (with the display
name of a send-as alias, when `from` is one), To the recipients, Subject the
first VEVENT's SUMMARY without control characters and cut to 200 bytes
(else `Invitation`, `Cancelled` or `Reply`), a one-line plain-text body, and
the object as a `text/calendar; method=<METHOD>; charset=UTF-8` alternative.
No caller-supplied header reaches the message. It goes out through the
user's own SMTP account and is charged to their quota like any send.

ATTENDEE and ORGANIZER are read from VEVENTs only; an ATTENDEE inside a
VALARM is not a recipient.

## Inbound: KyPost → KyCalendar

For each new message in a polled mailbox of a user with KyCalendar on, the
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
| 422 `not_invited` | 422 `not_invited` |
| 400 `bad_partstat` | 400 `bad_partstat` |
| 409 `conflict` | 409 `conflict` (reload and answer again) |
| 507 `quota` | 507 `quota` |
| anything else, or unreachable | 502 `kycalendar_unavailable` |

The route's own refusals: 400 `bad_request`/`bad_partstat` (partstat must be
`ACCEPTED`, `TENTATIVE` or `DECLINED`), 404 `not_found`, 422
`not_invitation`, 409 `kycalendar_off`. On `kycalendar_off` — KyPost not
paired, the user's switch off, or no SSO link — the client sends the RSVP
the old way: `calendarReply` on `POST /api/mail/send`.
