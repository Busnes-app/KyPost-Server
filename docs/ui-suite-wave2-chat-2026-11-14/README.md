# Suite integrations wave 2: Chat from mail (2026-11-14)

Before and after captures for #613 (contract H, KyPost side). Setup:
- Playwright Chromium, 1440×900 and 390×844, OS-following Busnes Light and Dark.
- America/New_York, page clock 2026-10-28.
- One disposable KyPost server built from the branch, with the loopback kypost-demo-IMAP fixture (`180c651`, unchanged), signed in for real as a created user and as the bootstrap administrator.
- Before images serve the frontend from main (`ad1f0655`); after images serve the branch build.
- Built on Node 26.13.0.
- Driver: `drivers/chat-driver.mjs`.

There is no KyIdentity or KyMessages in this scratch setup. These are answered in the browser by Playwright routes:
- `GET /api/suite/apps`: KyPost, plus `kymessage-chat` at `https://chat.kypost-demo.local/`.
- `POST /api/kymessage/people`: addresses at `@kypost-demo.local` count as inside the organisation.
- `POST /api/kymessage/chat`: a fixed Element user-view URL, or `no_target`.

`window.open` was recorded, not followed. The server-side routes are covered by `backend/internal/api/kymessage_handlers_test.go`, not by these captures.

| Capture | Before | After |
|---|---|---|
| `1-reader` | No Chat. | **Chat** under the sender. Clicking it opened a blank tab, then `https://chat.kypost-demo.local/#/user/@dana:kypost-demo.local`. |
| `2-reader-not-on-kymessages` | No Chat. | The console answered `no_target`: "This person isn't on KyMessages yet." beside the button, and the blank tab was closed. |
| `3-contacts` | Row menu: Edit, Delete. | Row menu: Edit, **Chat**, Delete. |
| `4-admin` | `?tab=kymessages` falls back to Application. | Server → KyMessages: "Not connected: KyPost does not sign in through KyIdentity." |

Every capture had 0 px sideways scroll.

Not exercised:
- a real KyIdentity pair (w2-id)
- a real console `/api/suite/chat` (w2-msg-chat)
- Element itself
- palettes other than Busnes
