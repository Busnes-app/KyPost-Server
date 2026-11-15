# Suite integrations (KyPost's sections)

KyPost's copy of sections E and F of the suite wire contracts
(`ky-redesign/integrations/suite-integrations-contracts.md`, owner decisions
2026-11-14) and of wave 2's section J (`suite-integrations-wave2.md`). This copy is the durable one for KyPost; change it together with
KyCalendar. The mail and calendar contract itself is [KYCALENDAR.md](KYCALENDAR.md);
native clients answer invitations as [NATIVE-RSVP.md](NATIVE-RSVP.md) says
once it exists.

Status in KyPost: F first bullet built (native send, KYCALENDAR.md "Send");
F second bullet built (web invitation card: updates, cancellations, Join);
E built (`frontend/src/lib/meetingLink.ts`); J built (`frontend/src/lib/composeLink.ts`).

## J. "Email this file": the compose deep link, KyDrive → KyPost

`https://<kypost-origin>/compose?to=<addresses, ≤100>&subject=<≤255 bytes>&body=<≤2000 bytes>`

- Opens a new draft prefilled; nothing is sent or saved until the user acts.
  Values are untrusted text, `body` is plain text (links become links),
  anything invalid is dropped silently. Survives the sign-in redirect.
- KyDrive's file menu gains "Email a link" when KyPost is in the user's suite
  app list: it creates an organisation link the usual way, then opens the
  deep link with the file name as subject and the link in the body.

KyPost's reading of it:

- `to`: comma-separated; more than 100 entries drops the whole field. Each
  entry must be a plausible bare address (≤ 254 characters, no
  `<>"'(),;:\[]`); bad ones and case-insensitive duplicates are dropped.
- `subject`: dropped when over 255 UTF-8 bytes or holding any control
  character (so no header line breaks).
- `body`: CRLF/CR become LF; dropped when over 2000 bytes or holding a control
  character other than LF and tab. Rendered as escaped paragraphs; `http(s)`
  URLs without credentials become links, nothing else is markup.
- Sign-in: `/compose` stores the parsed link in `sessionStorage`
  (`kypost.compose-link.v1`, this tab only) and goes to `/read` or `/login`.
  The first signed-in page (after any forced password change) takes it once,
  re-parses it, and opens it unless it is over 10 minutes old. Password and
  single sign-on sign-in both return to the same tab, so both keep it.
- It opens through the same path as a reply (`openDraftInCompose`, intent
  `new`, so the "new message" signature applies) and never replaces a compose
  window already open.

## E. "New meeting" deep link: KyPost → KyCalendar

`https://<kycalendar-origin>/new?title=<≤255 bytes>&attendees=<comma-separated addresses,
≤100>&body=<≤2000 bytes>`

- Opens the event editor prefilled. Nothing is stored until the user saves. All values are
  untrusted text: rendered as text, attendees validated as addresses, anything invalid
  dropped silently.
- Survives the sign-in redirect when the app has a return-to mechanism.
- KyPost shows "New meeting" (compose toolbar and contacts) and "Reply with meeting"
  (reading a message: title from the subject, attendees from From/To/Cc minus the user)
  only when KyCalendar is in the user's suite app list. The origin is the origin of that
  app's `url`.

## F. Mail + calendar completion

- KyPost-hosted (native) mailboxes send invitations: remove `native_unsupported` by
  sending through the native outbound path, with the same checks, quota and Sent copy.
- The web RSVP card handles updates and cancellations: a newer REQUEST for an answered
  event shows what changed and asks again; a CANCEL shows "Cancelled" and, when KyCalendar
  is on, is already applied there by the inbound forward.
- Native KyPost clients (Android, Mac, Linux) answer through `POST /api/calendar/rsvp`
  first and fall back to the mailed reply on `kycalendar_off` / `no_event`, exactly as the
  web client does (KyPost `docs/KYCALENDAR.md` "Respond").
- KyCalendar event editor: a free/busy strip for attendees in the organisation, from the
  existing `GET /api/freebusy`.
