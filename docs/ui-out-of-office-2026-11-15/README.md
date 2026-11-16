# Out of office: KyPost screens (2026-11-15)

Before and after captures for the out-of-office reply (#628) and its KyCalendar tie (#632).
Playwright Chromium, 1440×900 and 390×844, Busnes Light and Dark, America/New_York, page clock
2026-10-28. Each run used a disposable KyPost server, the loopback kypost-demo-IMAP fixture
(`8458159`), and a created user signed in for real. Push relays were unset. Drivers are in `drivers/`
(they import `ky-redesign/captures/drivers/lib.mjs`).

## KyCalendar boxes (`kypost-ooo-cal-*`, #632)

- Before: the frontend from main at `626994b8` (#628 merged). After: the branch frontend, unchanged
  from `c3d35378` onwards, built on Node 26.13.0. Both runs used one server binary built from the branch.
- A scratch server cannot be paired with KyCalendar for an SSO-linked user. So
  `kypost-ooo-calendar.mjs` routes `/api/mail/out-of-office` through Playwright and changes one
  field of the real server's answer, `calendarAvailable`, to `true`. The before frontend ignores the
  field. Everything else is real: sign-in, the saved reply and the period.
- `settings`: Settings → Mail → Out of office with a dated period. After adds **Show me as away in
  KyCalendar (needs a start and an end)** and **Answer new invitations Tentative while I'm away**,
  both ticked and saved.

## Out-of-office reply (`kypost-ooo-before-*`, `kypost-ooo-after-*`, #628)

Captured by the first run of worker f-ooo between 14:58 and 15:00 EST with `kypost-ooo.mjs`, before
it stopped. Before is main's frontend before #628; after is the `feat/out-of-office` frontend. That
run did not record the exact commits.

- `1-settings`: Settings → Mail. Before has no Out of office tab. After shows the tab, with the reply
  on, an end date, and outside replies set to contacts only.
- `2-inbox`: after shows the "Out of office is on" bar with Turn off.
- `3-admin`: Admin → Server. After shows the switch "Allow out-of-office replies to outside senders",
  turned on.

Document width stayed within the viewport in every capture (overflow 0).
