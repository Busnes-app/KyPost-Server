# Suite integrations wave 1: KyPost screens (2026-11-14)

Before and after captures for #606 (invitation card) and #607 (New meeting / Reply
with meeting). Playwright Chromium, 1440×900 and 390×844, OS-following Busnes
Light and Dark, America/New_York, page clock 2026-10-28. Each run used a disposable
KyPost server (one binary, built from main at #606) and the loopback kypost-demo-IMAP
fixture, signed in for real as a created user. Before images serve the frontend
from main before #606 (`fe92d9e8`); after images serve the branch builds. All were
built on Node 26.13.0.

## Invitation card (`kypost-invite-*`)

The fixture adds three messages from the same organizer (`drivers/fixture-invites.patch`
against kypost-demo-IMAP `1130062`):

1. A REQUEST at SEQUENCE 0.
2. A REQUEST at SEQUENCE 1 that moves the time and room and adds an https `CONFERENCE`.
3. A CANCEL.

Each run first answers message 1 with Accept through the live fallback: KyCalendar is
unpaired, so the server answered `kycalendar_off`, the user confirmed, and
`/api/mail/send` sent the REPLY. Then:

- `1-updated` is message 2. After: "Join meeting (chat.kypost-demo.local)", and
  "Changed since you answered: time and place. You had accepted; answer again."
  The driver read the link back as `target=_blank rel="noopener noreferrer"`.
  Before: only "This updates an earlier invitation."
- `2-cancelled` is message 3. After: "Cancelled" with no buttons and no Join.
  Before: "This event was cancelled."

## Meeting links (`kypost-meeting-*`)

`GET /api/suite/apps` was answered in the browser by a Playwright route listing KyPost
and KyCalendar (`https://calendar.kypost-demo.local/`), because there is no
KyIdentity in this scratch setup. `window.open` was recorded, not followed.

- `1-reader`: the reader. After it adds Reply with meeting, which opened
  `/new?title=Welcome+to+the+KyPost+sandbox&attendees=dana%40kypost-demo.local`.
  The user's own address was excluded.
- `2-compose`: compose. After it adds New meeting, which opened
  `/new?title=Budget+review&attendees=bob%40…%2Cdana%40…`. Bcc is never included.
- `3-contacts`: a contact's row menu. After it adds New meeting
  (`/new?attendees=dana%40kypost-demo.local`).

Every capture had 0 px sideways scroll. The before runs found no meeting buttons.
Not exercised:

- a real KyIdentity
- a real KyCalendar `/new`
- a paired KyCalendar answering the card
- palettes other than Busnes
