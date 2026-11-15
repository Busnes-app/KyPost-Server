# Suite integrations wave 2: KyPost KyDrive screens (2026-11-14)

Before and after captures for #615 (Attach from KyDrive, contract I/I2), #616
(compose deep link, J) and #617 (guest links, K). Playwright Chromium, 1440×900
and 390×844, Busnes Light and Dark, America/New_York, page clock 2026-10-28. Each
run used a disposable KyPost server built from the branch, the loopback
kypost-demo-IMAP fixture (`1130062`), and a created user signed in for real.
Before images serve the frontend from main before #615 (`ad1f0655`); after images
serve the branch builds (#615 at `92161a3f`; #616 before its kept-draft fix,
which changes nothing shown here; #617 at `8fc2be5c`). Built on Node 26.13.0. Drivers: `drivers/` (`w2-captures.mjs`, boot in
`boot.mjs`; they import wave 1's `ky-redesign/captures/drivers/lib.mjs`).

KyPost is paired with KyDrive by the environment (`https://drive.example.test`, a
throwaway secret; nothing listens there), so the real server sends the paired CSP
and COOP and no CSP bypass is used. KyDrive itself is not running:
`/api/kydrive/{status,upload-tickets,links,recipients,copies}`, the upload PUTs and
`https://drive.example.test/pick` are Playwright-routed stand-ins. KyPost's sign-in,
password change and step-up are real.

## Attach from KyDrive (`kypost-picker-*`, #615)

- `9-compose-actions`: before, no KyDrive action; after, **Attach from KyDrive**.
- `10-compose-drive-menu`: the ky-ui menu, opening upward: Share a link, Attach a copy.
- `11-compose-picked-links`: after the stand-in picker posted `kydrive.picked` to
  `window.opener`, two links inserted as wave 1 inserts them.
- `12-compose-picked-copy`: Attach a copy, fetched through `/api/kydrive/copies`, as a chip.

The server answered `Cross-Origin-Opener-Policy: same-origin-allow-popups`, and in
all four runs the picker page read `window.opener` as present (with `same-origin`
it is severed).

## Compose deep link (`kypost-deeplink-*`, #616)

The link was `/compose?to=dana@partner.example,not an address,ann@kypost-demo.local
&subject=Q3 board deck.pdf&body=Here is the file: https://drive.example.test/l/#fixture-deck\n<b>not bold</b>`.

- `13-deeplink`, signed in: before, an empty page (no route); after, the prefilled
  draft. The invalid address is dropped, the URL is a link, `<b>` is text.
- `14-deeplink-after-signin`: the same link opened signed out, through the real
  sign-in form, lands on the prefilled draft.

## Guest links (`kypost-guest-*`, #617)

The stand-in status offers `guestLinks` (max 30 days); the first guest-link request
answers `step_up_required`, as the real server does without a recent step-up;
`/api/mail/send` is held so the final body stays on screen.

- `15-outside`: the outside-recipient warning. Before: Send anyway / Cancel. After:
  also the guest-link explanation, the expiry choice and **Send a guest link instead**.
- `16-guest-stepup`: after choosing it, "Confirm it is you" with the password filled.
- `17-guest-sending`: after the real `POST /api/auth/step-up`, the body link points
  at the `/g/` guest link and reads "(19.0 MB, KyDrive guest link, expires 11/4/2026)".

No horizontal overflow in any capture.
