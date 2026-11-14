# Suite integrations, wave 1 (KyPost's sections)

KyPost's copy of sections E and F of the suite wire contracts
(`ky-redesign/integrations/suite-integrations-contracts.md`, owner decisions
2026-11-14). This copy is the durable one for KyPost; change it together with
KyCalendar. The mail and calendar contract itself is [KYCALENDAR.md](KYCALENDAR.md);
native clients answer invitations as [NATIVE-RSVP.md](NATIVE-RSVP.md) says
once it exists.

Status in KyPost: F first bullet built (native send, KYCALENDAR.md "Send").
F second bullet and E: in progress on their own pull requests.

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
