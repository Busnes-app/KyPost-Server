# KyPost UI Council — round1 baseline, Claude Opus 5.5

Reviewer `claude-opus-5-5`. Rubric `ky-suite-v2` from ky-ui `639e5882`, with weights .20/.15/.20/.15/.15/.15. Each total is rounded half-up to 0.1, and cohort means are exact over the rounded totals. Product source is `7a83df30`, and the evidence commit is `5503da35`. Machine record: `opus-round1.json`.

I scored blind: no peer, historical, Council or spreadsheet score was read. Quality scores were frozen (scratch file sha256 `a3d766c1…`) before I opened the brand references or did any live inspection, and nothing changed afterwards.

## Inputs verified

- **Authority files:** all five match `authority.json` byte for byte (`git show 639e5882:<path> | sha256sum`). The vendored `tokens.css` matches. `navigation.css` matches its own `VERSION` record `cc6dbaa0…`, as the authority note anticipates.
- **Originals:** the 32 files on disk, `inventory-v1.json` and `manifest-00-before.json` have the same membership. All 32 match on sha256, bytes and dimensions, and there are 32 unique hashes.
- **Duplicates:** I recomputed all 240 same-size pairs (120 desktop + 120 phone). All match `duplicate-audit-v1.json` exactly, and there are 0 near duplicates. The closest pair differs in 6.75% of pixels.
- **Brand references:** the four KyIdentity `d7f2a4b6` references match their hashes. I opened them for brand context only; they are not quality inputs.
- **Capture:** Chromium 153.0.8010.12 / Playwright 1.63.0, DPR1, normal service workers, OS light/dark with no saved theme, en-US, America/New_York. Everyday user alex with four synthetic messages, synthetic contacts and the fixture's own TLS demo. Compose and the contact editor are open but not submitted. No Ollama.

## Scores

I viewed every original individually at native resolution. Paired light and dark files scored the same except login-mobile-dark, which is the most faded.

| Surface | Desktop L / D | Phone L / D | Cohort |
|---|---|---|---|
| Inbox | 7.4 / 7.4 | 6.7 / 6.7 | mail-list |
| Inbox, 2 selected | 7.5 / 7.5 | 6.7 / 6.7 | mail-list |
| Reader | 6.5 / 6.5 | 6.2 / 6.2 | mail-read |
| Compose | 6.9 / 6.9 | 6.3 / 6.3 | compose |
| Contacts | 7.4 / 7.4 | 6.6 / 6.6 | contacts-list |
| Contact editor | 7.6 / 7.6 | 7.4 / 7.4 | contact-editor |
| Appearance | 7.7 / 7.7 | 7.4 / 7.4 | settings |
| Login | 5.1 / 5.1 | 5.1 / 5.0 | auth |

Dimension scores (H/T/D/C/A/Co) and one observation per dimension for each file are in the JSON.

### Cohorts

This is a KyPost-only coverage summary, not a ranking. There are no competitor captures, so there is no Ky gap.

| Cohort | n | Sum | Exact mean | Fraction |
|---|---|---|---|---|
| mail-list | 8 | 56.6 | 7.075 | 283/40 |
| mail-read | 4 | 25.4 | 6.35 | 127/20 |
| compose | 4 | 26.4 | 6.6 | 33/5 |
| contacts-list | 4 | 28.0 | 7.0 | 7/1 |
| contact-editor | 4 | 30.0 | 7.5 | 15/2 |
| settings | 4 | 30.2 | 7.55 | 151/20 |
| auth | 4 | 20.3 | 5.075 | 203/40 |
| **Covered set** | **32** | **216.9** | **6.778125** | **2169/320** (half-up 6.8) |

### Ky brand fit (separate, never added to quality)

The five dimensions carry equal weight. `shell` is N/A wherever a modal backdrop or full-screen sheet hides the shell, and on the standalone login page. `suite_cohesion` is scored against the KyIdentity references.

| Surface | Fit |
|---|---|
| Appearance desktop / phone | 8.2 / 8.0 |
| Inbox, selection and contacts, desktop | 7.6 |
| Contact editor | 7.5 (shell N/A) |
| Inbox, selection and contacts, phone | 7.2 |
| Reader / compose desktop / compose phone | 6.75 / 6.75 / 6.5 (shell N/A) |
| Login | 4.75 (shell N/A) |

The file mean is 2237/320 = 6.99.

The main brand gap: KyPost puts page content on `--ky-panel` (#ffffff / #1f2b2e), while KyIdentity puts it on `--ky-bg` (cream / #182326). The account region is a text Logout row plus a copyright line, where KyIdentity has a role and an icon button. The Appearance cards match KyIdentity card for card.

## Material findings and smallest fixes

1. **Login breaks the suite contract, and is also captured mid-animation.**
   - **Capture artifact.** The driver takes the screenshot as soon as "Security check verified." appears, while the 420 ms `auth-card-in` fade is still running. The pixels confirm it: the title renders #909492, not ink-strong. Live Chrome 154 shows opacity 1 once the fade finishes.
   - **Real defects, independent of timing:**
     - Labels are uppercase, letter-spaced IBM Plex Mono at 10.88px, at **2.71:1** contrast.
     - Input borders are 22% `--line` at **1.57:1**.
     - The success line uses `--accent`, under a stale comment claiming there is no success token.
   - **Fix:**
     - `.auth-label`: Space Grotesk 13px/600, no transform or letter-spacing, color `var(--ink)`.
     - `.auth-input`: border `var(--ky-field-border)`.
     - `.auth-pow-done`: color `var(--ky-success)`.
     - Drop the gradient blob.
     - In `capture.mjs`, wait until `document.getAnimations()` have finished before the login shot.
2. **Reader.**
   - The plain-text body is set in monospace inside a bordered, tinted box that is mostly empty. On phone the hard line breaks reflow raggedly.
   - "Mark as Read" is offered when Status already reads `read`: `ReadPage.tsx:1777` has no unread variant, though the API already has an `unread` action.
   - The Keywords input sits above the message.
   - **Fix:**
     - Render text/plain in the body font with `white-space: pre-wrap`, no box and no fixed height.
     - Switch the label and action to "Mark as unread" when the message is read.
     - Move Keywords below the body, or collapse it.
3. **Compose phone.**
   - The editor's bottom edge is cut off by the crypto band.
   - The editor toolbar wraps its clear-format icon onto a second line.
   - The actions wrap into two rows.
   - Labels mix "TO:/CC:/BCC:" with "Subject:".
   - Three tinted bands stripe the dialog on both viewports.
   - **Fix:** let the editor shrink above a sticky action row, keep the toolbar on one line (scroll or overflow), use sentence-case labels, and drop the tint from the notice and crypto bands.
4. **Phone mail list.**
   - The checkbox gutter is about 60px.
   - The sender address wraps mid-domain.
   - The bulk toolbar is icon-only, with no selection count.
   - **Fix:** show the display name only on phone, narrow the checkbox column, and add an "n selected" label. Keep labels, or add `title`/`aria` text and a More menu.
5. **Contacts.**
   - Delete is plain ink, not destructive.
   - An orphan chevron floats mid-row.
   - The "Actions" header is misaligned with the actions it labels.
   - On phone, five header buttons wrap into three rows.
   - **Fix:** style Delete with `--ky-danger` text, right-align the Actions header, and fold Merge/Export/Import into one "More" menu.

### Optional polish

- Desktop Appearance: the sidebar footer runs off the 900px canvas.
- Phone gutters are inconsistent: 10px on the reader, 16px on Appearance, 20px on inbox and contacts.
- Navigation labels differ by viewport: phone shows "Sent" and Junk, desktop shows "Sent Items" and no Junk row.
- Unread rows bold the subject, sender and date all at once.
- The native 13px checkboxes and the native select.
- The contact editor's enabled-looking "Choose File" next to "save first".

## Gates

| Gate | Result | Evidence |
|---|---|---|
| keyboard | untested | none run |
| focus | untested | no focus captures |
| contrast | **fail** | live `/login`, Chrome 154 DPR2, OS light: label 2.71:1, field border 1.57:1. Other surfaces and themes unmeasured. |
| states | untested | no empty, loading or error captures |
| responsive | untested | manifest shows overflow 0 at 390/1280; touch-target usability not measured |

## Scope

- **Verified:** the items under Inputs verified; one live read-only check of `/login` (computed styles, no sign-in) in my own incognito tab, then closed. Chrome 154 / DPR2 differs from the scored Chromium 153 / DPR1, and the live check did not change any score.
- **Unverified:** administrator, provisioning and server sections; native mail; other roles; named themes; HTML mail; validation, error and empty states; keyboard; screen readers; contrast beyond login; competitive standing.
- I claim no whole-product acceptance, no competitive result and no full required suite; the parent owns CI.
- **Drift:** this is the first review in the sequence, so there is none.
- I did not post to MySlop (the credential is known to return 401).

## Verdict

**Not satisfied** (genuinely_satisfied: false). My raw covered-set quality mean is **6.778125 (216.9/32)**, below 8.0. The gap is concentrated in reader, compose, the phone list and login, and the fixes above are small and local. The combined Claude/Sol progression decision belongs to the parent; my score is not a veto.
