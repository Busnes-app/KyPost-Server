# Answering invitations from native clients

For the Android, Mac and Linux clients. This is exactly what the web client
does (`frontend/src/pages/read/invite.ts`, `InviteCard.tsx`). The server
contract is [KYCALENDAR.md](KYCALENDAR.md) "Respond"; the mail fallback is
[PLATFORM_BASELINE.md](PLATFORM_BASELINE.md) "Calendar invites" and "RSVP".

## When to offer Accept, Maybe and Decline

All of these must hold. Otherwise show the invitation without answer buttons.

- `GET /api/mail/attachments` lists a part with `mimeType` `text/calendar` and
  `calendarMethod` `REQUEST`. Keep its `index`.
- That part (1 byte to 256 KiB) parses with your bounded parser as one
  VCALENDAR whose `METHOD` is `REQUEST`, with a series-master VEVENT that has
  a UID, a `mailto:` ORGANIZER and a readable SEQUENCE.
- A `METHOD:CANCEL` part shows "Cancelled" and never offers answers.

## 1. Answer through KyCalendar first

```
POST /api/calendar/rsvp
X-Kypost-Device-Id: <device id>
X-Kypost-Device-Secret: <device secret>
X-KyPost-Mailbox: <mailbox id>        (omit for the primary)
Content-Type: application/json

{"messageId":"<exact messageId string>","mailbox":"<folder, \"\" = INBOX>","index":<attachment index>,"partstat":"ACCEPTED"}
```

- `partstat` is `ACCEPTED`, `TENTATIVE` or `DECLINED`.
- Always answer for the series master: send no `recurrenceId`.
- Device authentication carries no cookie, so no CSRF token is needed.
- Copy `messageId` exactly as listed, including native `n1:…` references.
- The server reads the invitation from the message itself. Never send ics.

| Answer | Client does |
|---|---|
| 200 `{"outcome":"answered"}` | Done. Send no mail. |
| 200 with any other body | Not confirmed: say so and let the user try again. Send no mail. |
| 409 `kycalendar_off` | Fall back to mail (2), with the confirmation. Also the answer when the user has no KyCalendar account. |
| 404 `no_event` | Fall back to mail (2), with the confirmation. KyCalendar never received this invitation (not DKIM-aligned, encrypted, or older than pairing). |
| 409 `conflict` | "The event changed since this invitation." Reload the attachment and answer again. |
| anything else (400 `bad_request`/`bad_partstat`, 404 `not_found`, 422 `not_invitation`/`not_invited`, 502 `kycalendar_unavailable`, 507 `quota`, network error) | Show the server's `error` text, or "KyCalendar could not be reached; try again." Re-enable the buttons. Send no mail. |

## 2. Mail fallback, after the user confirms

Before sending, ask the user to confirm, saying: "your answer goes to
<organizer> as an unencrypted, unsigned email". On `no_event`, start that
sentence with "KyCalendar has no event for this invitation"; on
`kycalendar_off`, with "KyCalendar is not in use for this mailbox".

```
POST /api/mail/send
{"to":"<organizer>","from":"<attendee>","subject":"Accepted: <title>",
 "body":"<attendee> has accepted this invitation.","mode":"plain",
 "encrypt":false,"sign":false,"calendarReply":{"ics":"<REPLY>"}}
```

- `attendee` is the first of the user's own addresses for this mailbox that
  the invitation lists as an ATTENDEE (case-insensitive), else the primary
  address. Without one, do not send.
- Subject verbs are `Accepted`, `Tentative` and `Declined`. The title is the
  SUMMARY with whitespace collapsed, or `Invitation` when there is none.
- Body verbs are `accepted`, `tentatively accepted` and `declined`.
- The REPLY has these CRLF lines, each folded at 75 octets without splitting
  a UTF-8 sequence:
  `BEGIN:VCALENDAR`, `VERSION:2.0`, `PRODID:-//KyPost//<client>//EN`,
  `METHOD:REPLY`, `BEGIN:VEVENT`, `UID:<exact UID text>`,
  `SEQUENCE:<the invitation's>`, `DTSTAMP:<now, UTC, YYYYMMDDTHHMMSSZ>`,
  `ORGANIZER:mailto:<organizer>`,
  `ATTENDEE;PARTSTAT=<partstat>:mailto:<attendee>`, `END:VEVENT`,
  `END:VCALENDAR`. No RECURRENCE-ID.

| Answer | Client does |
|---|---|
| 200 with `"calendarReply": true` | Sent: "Your answer was sent to <organizer>." |
| 200 without it | Delivered as ordinary mail: "their calendar may not update." |
| 4xx, or 503 | Nothing went out: show the reason and re-enable the buttons. |
| other 5xx, or network error | It may have been sent: "Check Sent before answering again." Keep the buttons off until the message is reopened. |

## Updates (optional)

The web client remembers each answered revision locally, as digests only. A
later REQUEST for the same UID, with a higher SEQUENCE or the same SEQUENCE
and a later DTSTAMP, then says which of time, place and title changed, and
asks again. Never store readable invitation text for this.
