# Fleet Appearance evidence — 2026-10-26

Staged screenshots for the suite Appearance picker (ky-ui 0.4.0 theme catalog, canonical source `4f83f34`). These are
scratch captures of the real application with synthetic data. They are implementation
evidence for the council's independent rating, not a rating: nothing here claims a score.

- `manifest.json`: base commit, OS, toolchain, browser, fixture, every stage's source patch,
  server binary and built-asset hashes, and one record per PNG (viewport, OS colour scheme,
  theme on screen, checked radio, radio count, horizontal overflow, `h1`, SHA-256).
- `captures.csv`: the same per-PNG records as a spreadsheet.
- `checks.csv`: the required checks run for this branch, with exit codes.
- `patches/`: the exact `frontend/src` source of each after stage, against the base commit
  (vendored ky-ui included). The server binary is unchanged; stages differ in frontend assets.
- `drivers/`: the scratch capture and build scripts, archived as text. They need the recorded
  scratch KyPost (`run.sh`), the loopback `kypost-demo-IMAP` fixture and Playwright.

## Conditions

Scratch KyPost on localhost:5696 with a copy of the council's synthetic data (everyday user
`alex`) and the loopback `kypost-demo-IMAP` fixture (`alice@kypost-demo.local`, TLS with the
fixture CA). Seeded mail flags are restored and drip mail deleted through the app's own API
before each inbox capture. Real proof-of-work sign-in, America/New_York. 1280×900 and 390×844,
OS light and dark, nothing saved (OS-following Busnes) unless the file name starts with a theme
chosen through the Appearance radios. No auth, CSRF or CSP bypass and no mocked responses.

## Stages

| Stage | Before | After | What changed |
| --- | --- | --- | --- |
| 01 | `00-before` | `01-after` | ky-ui 0.4.0; the 17 swatch buttons become 23 native radio cards (Match system plus 22 catalog themes, including Patina Ky (base), Cyber Dark, Nord Slate, Paper Clean, OLED Black); saved names kept. Phone layout overflowed by 640px. |
| 02 | `01-after` | `02-after` | `.panel` may shrink in its flex row: phone overflow 0. Inbox unchanged. |
| 03 | `02-after` | `03-after` | Space between the description and the picker (superseded by 04). |
| 04 | `03-after` | `04-after` | Owner correction: shared description, preview hairline and wrapping from ky-ui `4f83f34`; the product spacing rule is removed. KyPost had no shell theme select. |
| 05 | `04-after` | `05-after` | Appearance only: a bare `h1` so the title-to-description gap is the shared one, identical in KyPost, KyCalendar and KyMessages. |

The inbox is the main task view; it changes only in stage 01 (shared tokens) and is reused from
`02-after`. `ocean-*`, `sky-*` and `nord-*` show named themes chosen through the radios.

A void first `01-after` batch was served by the still-running before-build server and showed the
old picker; it was overwritten before review and is not evidence (`manifest.json` notes).

## Limitations

- Chromium only; KyPost has no product browser suite, so the picker is covered by vitest and these captures.
- Chromium gives each native radio an empty `style=""`; there are no inline declarations.
- At 390px KyPost's content box is 350px, 6px short of two 172px cards, so the phone grid is one column.
