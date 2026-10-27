# KyPost UI Council: round 2 (Claude Opus 5.5)

Independent primary review of the 32 stage03 confirmation originals. Rubric `ky-suite-v2`
from ky-ui `639e5882`, with all five authority hashes verified. Product source `50bbbff5`,
with no frontend diff to evidence head `b6852d8f`. Machine record: `opus-round2.json`.

**Raw covered-set quality mean: 7.63125** (244.2 / 32 = 1221/160; 7.6 half-up). Raw
satisfaction is **not satisfied**. I am not asking for another round. The parent's combined
Claude/Sol measure decides progression.

## Method

- **Inventory.** `inventory-v2.json` and `manifest-03-after.json` list the same 32 files. I
  re-read the SHA-256, byte count and dimensions of every file from disk, and all of them
  match. There are 32 distinct surface/device/theme keys. I recomputed all 240 same-size pair
  diffs with numpy and every one equals `duplicate-audit-v2.json`. The smallest changed
  fraction is 0.0658, so there are no exact or near duplicates.
- **Viewing.** I opened every original on its own at native resolution. I also cropped
  three regions to confirm details: the reader action row, the compose toolbar and the phone
  compose editor edges.
- **Arithmetic.** Totals are computed in Decimal and rounded half-up to 0.1. The float-str
  path gives the same result for all 32 files. Cohort and overall means are exact over the
  rounded totals.
- **Freeze.** I froze the scores at 2026-10-27T17:21:05Z (vectors sha256 `96a73c54…`). That
  was before I read `opus-round1`, before I looked at the settled stage01 login image and
  before the live check. No score changed after the freeze.
- **Brand.** I verified the four KyIdentity references against `KyIdentity-server@d7f2a4b6`
  and opened each one. I used them only as brand context.

## Scores

Columns are H Ty D C A Co: hierarchy, typography, density/layout, color, visible
affordances and coherence. Light and dark scored identically on every surface.

| Surface | Desktop light/dark | Total | Phone light/dark | Total |
|---|---|---|---|---|
| inbox | 8 8 7 8 7 8 | 7.7 | 8 8 7 8 7 8 | 7.7 |
| inbox-selection | 8 8 7 8 8 8 | 7.8 | 8 8 7 8 7 8 | 7.7 |
| reader | 8 8 7 8 7 7 | 7.5 | 8 7 6 8 7 7 | 7.2 |
| compose | 8 7 8 8 8 7 | 7.7 | 8 7 6 8 7 7 | 7.2 |
| contacts | 8 8 7 8 7 8 | 7.7 | 8 7 6 8 7 8 | 7.3 |
| contact-editor | 8 8 7 8 7 7 | 7.5 | 8 8 7 8 7 7 | 7.5 |
| appearance | 8 8 8 8 8 8 | 8.0 | 8 8 8 8 8 8 | 8.0 |
| login | 8 8 7 8 8 8 | 7.8 | 8 8 7 8 8 8 | 7.8 |

KyPost is the only product in the set, so this is a coverage summary, not a ranking. There
is no competitor and no Ky gap.

| Cohort | n | Sum | Exact mean | Half-up |
|---|---|---|---|---|
| settings | 4 | 32.0 | 8 | 8.0 |
| auth | 4 | 31.2 | 39/5 = 7.8 | 7.8 |
| mail-list | 8 | 61.8 | 309/40 = 7.725 | 7.7 |
| contacts-list | 4 | 30.0 | 15/2 = 7.5 | 7.5 |
| contact-editor | 4 | 30.0 | 15/2 = 7.5 | 7.5 |
| compose | 4 | 29.8 | 149/20 = 7.45 | 7.5 |
| mail-read | 4 | 29.4 | 147/20 = 7.35 | 7.4 |

**Brand fit** is kept separate from quality. The overall mean is 1333/160 = 8.33125. Shell is
marked N/A, and left out of the mean, for the 16 dialog, full-screen and pre-auth captures.
`suite_cohesion` is scored because KyIdentity gives a second Ky product. Measured surfaces
match the tokens: #f8f6f0, #ffffff and #f2efe7 in light; #182326 and #1f2b2e in dark.
Per-surface brand means:

| Surface | Brand mean |
|---|---|
| appearance | 8.6 |
| login | 8.5 |
| contacts | 8.4 |
| reader | 8.25 |
| compose | 8.25 |
| contact-editor | 8.25 |
| inbox | 8.2 |
| inbox-selection | 8.2 |

## Material findings and the smallest correct fix for each

1. **Compose toolbar.** Quill's `Normal` picker renders in a system sans, and its baseline
   sits about 8px below the glyphs.
   - Fix: give `.ql-picker` `font: inherit` and use `align-items: center` on the toolbar.
   - On the phone, the editor body is inset about 8px each side from its toolbar, which
     makes a stepped outline. Fix: use the same inline inset for `.ql-toolbar` and
     `.ql-container`.
2. **Reader More actions** uses a 16px label while Reply, Reply All and Forward use 14px.
   I confirmed this live.
   - Fix: use the shared button font size.
   - On the phone it wraps alone onto a second row. Fix: keep the action row on one line,
     or shorten the label to an icon plus "More".
   - The sender address breaks mid-domain. Fix: break before `<` or keep the address on
     one line.
3. **Contacts on the phone.** Per-row Edit and Delete squeeze the name column, so
   organisation names wrap and row heights vary from 59px to 100px.
   - Fix: move Edit and Delete under the email line, or into a row overflow below the
     breakpoint.
   - On desktop the "Actions" header sits about 110px left of the buttons it labels. Fix:
     right-align it.
   - The count appears twice ("4 contacts" and "Address Book 4"). Fix: drop one.
4. **Inbox selection.** "2 selected" sits about 12px above the action baseline.
   - Fix: `align-items: center` on the toolbar.
   - On the phone the action strip ends mid-word at "Snooze" with no cue that it scrolls.
     Fix: add an edge fade mask.
5. **Disclosure vocabulary.** The contact editor mixes a tiny native ▸ triangle with chevron
   rows, and its "+ Add" buttons render larger than the other button text. Live, the
   Contacts popover uses boxed item buttons while the reader popover uses plain icon rows.
   - Fix: pick one disclosure style and one popover item style.

## Optional polish

- Inbox:
  - There is a wide checkbox and padlock gutter (42px + 34px live).
  - The soft-accent tint on the Uncategorized chip is decorative.
  - "Updated Just Now" floats at the page foot.
- Login:
  - The "Security check" label is left-aligned but its verified status is centred.
  - The dark icon tile merges with the dark page.
- Appearance:
  - Unselected radios in dark render as grey filled discs.
  - The phone gutter is 16px, against 20px on other phone pages.

## Drift against my own round 1 (read after freeze)

The total delta is +27.3 over 32 files; the mean is +0.853125. Round 1 was 216.9 / 32 =
6.778125.

- **Appearance (unchanged pixels).** The four images are byte-identical to stage00. They
  moved +0.3 on desktop and +0.6 on the phone. That is reviewer variance, not product
  progress: in round 1 I weighted the clipped nav strip and the 16px gutter more heavily.
- **Login (+2.7 to +2.8).** Two causes are mixed here:
  - The round 1 originals were captured mid-fade.
  - The product changed: mono uppercase faint labels, faint borders, accent-coloured success
    and the glow/gradient are all gone.

  After the freeze I viewed the unscored settled stage01 desktop-light login. From that, I
  estimate about 1 point is the capture correction and the rest is product change. This
  split is not a score.
- **Reader (+1.0).** The monospace boxed body, the cramped metadata and the
  unlabelled or overloaded tool row are gone.
- **Phone inbox and selection (+1.0).** Rows no longer wrap mid-domain, and the bulk toolbar
  now has labels and a count.
- **Compose (+0.8 to +0.9).** The uppercase labels and tinted bands are gone, the toolbar is
  one row, and the phone editor is no longer clipped.
- **Phone contacts (+0.7).** One primary action is visible instead of a three-row button wrap.
- **Under 0.5.** Desktop inbox, selection and contacts moved +0.3, mostly from variance.
  Contact editor moved ±0.1.

## Gates

None of these pass, because a screenshot cannot pass a gate.

- **contrast: untested.** Live light login shows labels at 6.19:1, field border at 3.63:1
  against the card and 3.46:1 against the fill, success at 5.74:1 and the button at 5.33:1.
  The parent's `checks/r2-browser.json` covers both themes. Other surfaces are not measured.
  The round 1 login failure is resolved.
- **keyboard: untested.** Escape closes the reader popover and returns focus to More
  actions. Escape again closes the reader and returns focus to the row. That is a partial
  check only.
- **focus: untested.**
- **states: untested.**
- **responsive: untested.** Overflow is 0 at both widths, but there are 13px checkboxes and
  32px Edit/Delete buttons.

## Scope

**Verified:**
- The 32 stage03 originals: hashes, bytes and dimensions.
- All 240 pair diffs.
- The authority and brand reference hashes.
- The exact arithmetic.

**Live supplement:** my own incognito tab in Chrome 154 at DPR2, which differs from the
scored Chromium 153 at DPR1. I signed in as the synthetic user alex. I opened one message,
which marked it read; I did not reset the flag. I opened and closed two popovers. There were
no sends, edits or configuration changes, and I closed the tab.

**Not verified or not covered:**
- Admin, provisioning, native mail and other roles.
- Error, loading and empty states.
- HTML mail and attachments.
- Competitors.
- Full CI. The parent owns CI and PR #489.

I make no whole-product acceptance claim and no competitive claim.

**Unscored:**
- 32 stage00 historical files.
- 4 stage01 supplemental login files. I viewed only desktop light, after the freeze.
- 32 stage02 superseded files.
- 4 brand references.

I did not attempt a MySlop post; the board is known to return 401.
