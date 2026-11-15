# Mail links open with no opener while KyDrive is paired — 2026-11-15

`opener-driver.mjs.txt` (Playwright Chromium, fleet copy from `ky-redesign/KyCalendar/web`) starts a
disposable KyPost server paired to KyDrive by environment (`KYPOST_KYDRIVE_URL=https://drive.example.test`,
a stand-in that is never contacted), so `/read` is served with
`Cross-Origin-Opener-Policy: same-origin-allow-popups`. It signs in for real against the kypost-demo-IMAP
fixture and opens a message. The driver replaces that message's `/api/mail/body` with a hostile body and
turns on Show Remote Content so the image maps can be clicked. The real frontend, CSP and sandboxed mail
frame (`allow-same-origin allow-popups allow-popups-to-escape-sandbox`) are not modified. The driver
clicks each link and reads `window.opener !== null` in the page it opened (`https://evil.example.test`,
routed).

| Link in the message | origin/main | this branch |
| --- | --- | --- |
| `<area target="_blank" rel="opener">` | **opener** | none |
| `<area target="w1">` (named target) | **opener** | none |
| `<svg><a xlink:href target="_blank" rel="opener">` | **opener** | none |
| `<svg><a xlink:href target="w2">` | **opener** | none |
| `<area>` (no target or rel) | none | none |
| `<a target="_blank" rel="opener">` | none (already stamped) | none |
| `<svg><a href rel="opener">` | none (already stamped) | none |
| `<svg><a xlink:href target="_blank">` | none | none |

Logs: `run-before-origin-main.log` (expect `open`) and `run-after-branch.log` (expect `closed`).

Impact as measured in Chromium: the opened page held a live `WindowProxy` for the mail frame. That is enough
for cross-origin `postMessage` to the frame and the KyPost window, though the driver did not try it. Chromium refused it permission to navigate either
the mail frame or the KyPost tab (`SecurityError ... does not have permission to navigate the target
frame`). Navigation was not tested in Firefox or WebKit: the fleet's Playwright Firefox did not launch.

Usage: `node opener-driver.mjs <kypost-server binary> <frontend dist> <kypost-demo-IMAP dir> <open|closed>`.
No screen changes visually, so there are no screenshots.
