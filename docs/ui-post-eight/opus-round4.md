# KyPost UI Council round4: Opus independent review

Reviewer: Claude Opus 5.5 (`claude-opus-5-5`). Rubric `ky-suite-v2` pinned at ky-ui `639e5882` (five objects verified). Product `558761af`, evidence `f53e76ce`, worktree head `c5efc047`.

## Result

- Overall quality, all 32 stage06 originals: **255.4 / 32 = 1277/160 = 7.98125 → 8.0** (half-up 0.1).
- Separate Ky brand fit mean: 8.35625 (1337/160).
- Satisfied: **yes**. Genuinely satisfied: **yes**. My frozen Opus overall is 255.4/32 = 7.98125, 8.0 half-up. Every round3 objection is visibly fixed in the stage06 pixels and the ones I could exercise held live: 14px/40px contact controls, adjacent name disclosures, native indeterminate select-all, 16px boxes in 40px labels, compact left-aligned PoW status, 20px phone Appearance gutter with two columns. No screen is broken; the weakest is the phone contacts list (7.5) for its stacked, misaligned rows. Remaining items are small density slips (phone contact rows, editor photo note, phone toolbar wrap) plus optional taste; I do not ask for another round for them. My reviewer variance this round is net negative, so the result is not lifted by re-judging unchanged pixels. Satisfied at the owner's 8.0 bar; not a 9/10 claim. Parent decides progression.
- Frozen 2026-10-27T19:15:17.296138+00:00 before opening any prior or peer material; vectors sha256 `594f1aaf00c920633a3439e7a9df619db01b93ff8ada5ed96680796f078efe87`.

## Inventory

200 files: 4 brand only; no quality score, 32 round2 scoring input, 32 scored round1, 32 scored round3, 32 scored round4, 32 superseded initial after; unscored, 4 unscored settled login comparison, 32 unscored superseded initial round4. Independent script: all 200 artifacts.json entries re-hashed from disk (0 mismatches) and equal the on-disk set (196 screenshots + 4 brand references); inventory-v4 and manifest-06-after membership == the 32 "scored round4" artifacts with matching sha256/bytes/width/height; brand-references.json hashes match; all 240 same-size pairs recomputed and equal duplicate-audit-v4 (changed_fraction and max_channel_delta). Scored 32, all unique by sha256; 0 near-duplicates (min changed fraction 0.0654). Stage06 shares bytes with earlier-stage copies (appearance desktop, phone compose/reader, login); each is scored once here as the current original.

## Cohorts (coverage, not a ranking)

| Cohort | n | Sum | Exact | Half-up |
|---|---|---|---|---|
| auth | 4 | 33.6 | 42/5 = 8.4 | 8.4 |
| compose | 4 | 31.6 | 79/10 = 7.9 | 7.9 |
| contact-editor | 4 | 32.0 | 8/1 = 8 | 8.0 |
| contacts-list | 4 | 30.8 | 77/10 = 7.7 | 7.7 |
| mail-list | 8 | 64.0 | 8/1 = 8 | 8.0 |
| mail-read | 4 | 31.4 | 157/20 = 7.85 | 7.9 |
| settings | 4 | 32.0 | 8/1 = 8 | 8.0 |
| **overall** | 32 | 255.4 | 1277/160 = 7.98125 | **8.0** |

## Files

| File | H | T | D | C | A | Co | Total | ky_fit (N/A) |
|---|---|---|---|---|---|---|---|---|
| appearance-desktop-dark.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 43/5 (-) |
| appearance-desktop-light.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 43/5 (-) |
| appearance-mobile-dark.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 42/5 (-) |
| appearance-mobile-light.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 42/5 (-) |
| compose-desktop-dark.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 42/5 (-) |
| compose-desktop-light.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 42/5 (-) |
| compose-mobile-dark.png | 8 | 8 | 7 | 8 | 8 | 8 | 7.8 | 17/2 (shell) |
| compose-mobile-light.png | 8 | 8 | 7 | 8 | 8 | 8 | 7.8 | 17/2 (shell) |
| contact-editor-desktop-dark.png | 8 | 8 | 7 | 9 | 8 | 8 | 8.0 | 42/5 (-) |
| contact-editor-desktop-light.png | 8 | 8 | 7 | 9 | 8 | 8 | 8.0 | 42/5 (-) |
| contact-editor-mobile-dark.png | 8 | 8 | 7 | 9 | 8 | 8 | 8.0 | 17/2 (shell) |
| contact-editor-mobile-light.png | 8 | 8 | 7 | 9 | 8 | 8 | 8.0 | 17/2 (shell) |
| contacts-desktop-dark.png | 8 | 8 | 8 | 8 | 7 | 8 | 7.9 | 42/5 (-) |
| contacts-desktop-light.png | 8 | 8 | 8 | 8 | 7 | 8 | 7.9 | 42/5 (-) |
| contacts-mobile-dark.png | 8 | 8 | 6 | 8 | 7 | 8 | 7.5 | 41/5 (-) |
| contacts-mobile-light.png | 8 | 8 | 6 | 8 | 7 | 8 | 7.5 | 41/5 (-) |
| inbox-desktop-dark.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 41/5 (-) |
| inbox-desktop-light.png | 8 | 8 | 8 | 8 | 8 | 8 | 8.0 | 41/5 (-) |
| inbox-mobile-dark.png | 8 | 8 | 7 | 8 | 8 | 8 | 7.8 | 8/1 (-) |
| inbox-mobile-light.png | 8 | 8 | 7 | 8 | 8 | 8 | 7.8 | 8/1 (-) |
| inbox-selection-desktop-dark.png | 8 | 8 | 8 | 8 | 9 | 8 | 8.2 | 41/5 (-) |
| inbox-selection-desktop-light.png | 8 | 8 | 8 | 8 | 9 | 8 | 8.2 | 41/5 (-) |
| inbox-selection-mobile-dark.png | 8 | 8 | 7 | 8 | 9 | 8 | 8.0 | 8/1 (-) |
| inbox-selection-mobile-light.png | 8 | 8 | 7 | 8 | 9 | 8 | 8.0 | 8/1 (-) |
| login-desktop-dark.png | 9 | 8 | 8 | 9 | 8 | 8 | 8.4 | 17/2 (shell) |
| login-desktop-light.png | 9 | 8 | 8 | 9 | 8 | 8 | 8.4 | 17/2 (shell) |
| login-mobile-dark.png | 9 | 8 | 8 | 9 | 8 | 8 | 8.4 | 17/2 (shell) |
| login-mobile-light.png | 9 | 8 | 8 | 9 | 8 | 8 | 8.4 | 17/2 (shell) |
| reader-desktop-dark.png | 8 | 8 | 7 | 8 | 8 | 8 | 7.8 | 42/5 (-) |
| reader-desktop-light.png | 8 | 8 | 7 | 8 | 8 | 8 | 7.8 | 42/5 (-) |
| reader-mobile-dark.png | 8 | 7 | 8 | 8 | 8 | 8 | 7.9 | 17/2 (shell) |
| reader-mobile-light.png | 8 | 7 | 8 | 8 | 8 | 8 | 7.9 | 17/2 (shell) |

Per-dimension observations, artifacts, confidence and caveats for every file are in `opus-round4.json`.

## Findings and smallest fixes

- **Phone contact rows stack and misalign** (contacts mobile (2); material pictured defect): Each contact is ~137px (name, org, email, Edit/Delete), so 3.5 fit; email x=72, Edit x=85, name x=116; search placeholder cut at "organizat". Fix: Indent email and Edit/Delete to the name column (x=116) and shorten the placeholder to "Search contacts".
- **Orphaned photo note and mixed dividers in contact editor** (contact-editor (4); material pictured defect): "Save the contact first to add a photo." sits between Emails and Phones; Emails/Phones are unruled groups among ruled disclosure rows. Fix: Move the photo note to the form top (or footer) and give Emails and Phones the same top divider as the disclosures.
- **Phone bulk toolbar wraps 4+2** (inbox-selection mobile (2); material pictured defect): Read and Print sit alone on a second line, pushing search ~100px down. Fix: Keep one row: let the action row scroll horizontally like the nav strip, or move Read/Print into an overflow.
- **Phone nav labels differ from desktop rows** (phone shell (6 captures; brand shell, not quality); optional): Strip shows Drafts/Junk/Sent/Trash stand-ins (App.tsx:599); desktop shows Sent Items and no Junk row. Contract: same order and labels below the breakpoint. Fix: Label the stand-ins from the account's real folder names, or confirm Junk is intentionally phone-only. Label-only; preserve routing.
- **Duplicate contact count** (contacts (4); optional): "4 contacts" in header and "Address Book 4". Fix: Keep one count.
- **Detached row chevron** (contacts desktop (2); optional): Chevron at x≈640 floats between Name and Contact Info; live DOM shows it is the trailing icon of the name button that opens the contact. Fix: Drop the chevron (the name already reads as the link) or move it to the row end.
- **Reader dialog height** (reader desktop (2); optional): Near-viewport dialog for a three-line message. Fix: Size the dialog to content up to a max height.
- **Optional taste** (various; optional): Unread bold across subject, sender and date; full angle-bracket addresses in the desktop sender column; Sign in button 46px vs 40px fields; phone compose Trash beside Contacts; plain-text hard wraps on phone reader. Fix: None required; consider bolding only the subject/sender for unread and showing display names only.

## Drift against my round3 (opened after freeze)

Sum 252.4 → 255.4 (3.0), mean drift 0.09375. Unrounded weighted attribution: product pixels 4.80, my reviewer variance -1.50. Variance appears on six byte-identical files (appearance desktop ×2, compose phone ×2, reader phone ×2) and on unchanged regions elsewhere: reader desktop and compose desktop differ only in the dimmed inbox behind the scrim. Capture changes are not counted as design improvement. |Δ| ≥ 0.5: contact-editor-desktop-dark.png, contact-editor-desktop-light.png, contact-editor-mobile-dark.png, contact-editor-mobile-light.png, reader-mobile-dark.png, reader-mobile-light.png.

| File | Pixels | Prior | Now | Δ | Cause | Explanation |
|---|---|---|---|---|---|---|
| appearance-desktop-dark.png | identical | 7.9 | 8.0 | 0.1 | reviewer variance | coherence +1 reviewer variance: Round3 docked coherence for the icon-less Settings sub-nav/strip item; that element is pixel-unchanged and I did not count it this time. |
| appearance-desktop-light.png | identical | 7.9 | 8.0 | 0.1 | reviewer variance | coherence +1 reviewer variance: Round3 docked coherence for the icon-less Settings sub-nav/strip item; that element is pixel-unchanged and I did not count it this time. |
| appearance-mobile-dark.png | 5.920% | 7.9 | 8.0 | 0.1 | reviewer variance | coherence +1 reviewer variance: Round3 docked coherence for the icon-less Settings sub-nav/strip item; that element is pixel-unchanged and I did not count it this time. Pixels changed (phone gutter 16 -> 20px, two columns kept), but that aligns phone pages with each other rather than changing anything inside this screen I score differently. |
| appearance-mobile-light.png | 5.960% | 7.9 | 8.0 | 0.1 | reviewer variance | coherence +1 reviewer variance: Round3 docked coherence for the icon-less Settings sub-nav/strip item; that element is pixel-unchanged and I did not count it this time. Pixels changed (phone gutter 16 -> 20px, two columns kept), but that aligns phone pages with each other rather than changing anything inside this screen I score differently. |
| compose-desktop-dark.png | 0.308% | 8.2 | 8.0 | -0.2 | reviewer variance | color -1 reviewer variance: Dialog pixels unchanged (desktop diffs lie only in the dimmed inbox behind the scrim at x1120-1247; phone byte-identical); I judged color at 8 rather than 9 on the same Send/Trash/Unsigned treatment. |
| compose-desktop-light.png | 0.305% | 8.2 | 8.0 | -0.2 | reviewer variance | color -1 reviewer variance: Dialog pixels unchanged (desktop diffs lie only in the dimmed inbox behind the scrim at x1120-1247; phone byte-identical); I judged color at 8 rather than 9 on the same Send/Trash/Unsigned treatment. |
| compose-mobile-dark.png | identical | 8.0 | 7.8 | -0.2 | reviewer variance | color -1 reviewer variance: Dialog pixels unchanged (desktop diffs lie only in the dimmed inbox behind the scrim at x1120-1247; phone byte-identical); I judged color at 8 rather than 9 on the same Send/Trash/Unsigned treatment. |
| compose-mobile-light.png | identical | 8.0 | 7.8 | -0.2 | reviewer variance | color -1 reviewer variance: Dialog pixels unchanged (desktop diffs lie only in the dimmed inbox behind the scrim at x1120-1247; phone byte-identical); I judged color at 8 rather than 9 on the same Send/Trash/Unsigned treatment. |
| contact-editor-desktop-dark.png | 2.480% | 7.5 | 8.0 | 0.5 | product pixels + reviewer variance | typography +1 product: add-email/add-phone labels 16px -> 14px (phone title also 16px and aligned to the 16px field inset) color +1 reviewer variance: Stage04 already had a single orange Add Contact on a neutral sheet; color 8 -> 9 is my re-judgment, not a pixel change. coherence +1 product: both name disclosures now adjacent under Full Name instead of split around Emails/Phones |
| contact-editor-desktop-light.png | 2.480% | 7.5 | 8.0 | 0.5 | product pixels + reviewer variance | typography +1 product: add-email/add-phone labels 16px -> 14px (phone title also 16px and aligned to the 16px field inset) color +1 reviewer variance: Stage04 already had a single orange Add Contact on a neutral sheet; color 8 -> 9 is my re-judgment, not a pixel change. coherence +1 product: both name disclosures now adjacent under Full Name instead of split around Emails/Phones |
| contact-editor-mobile-dark.png | 6.079% | 7.5 | 8.0 | 0.5 | product pixels + reviewer variance | typography +1 product: add-email/add-phone labels 16px -> 14px (phone title also 16px and aligned to the 16px field inset) color +1 reviewer variance: Stage04 already had a single orange Add Contact on a neutral sheet; color 8 -> 9 is my re-judgment, not a pixel change. coherence +1 product: both name disclosures now adjacent under Full Name instead of split around Emails/Phones |
| contact-editor-mobile-light.png | 6.082% | 7.5 | 8.0 | 0.5 | product pixels + reviewer variance | typography +1 product: add-email/add-phone labels 16px -> 14px (phone title also 16px and aligned to the 16px field inset) color +1 reviewer variance: Stage04 already had a single orange Add Contact on a neutral sheet; color 8 -> 9 is my re-judgment, not a pixel change. coherence +1 product: both name disclosures now adjacent under Full Name instead of split around Emails/Phones |
| contacts-desktop-dark.png | 2.054% | 7.7 | 7.9 | 0.2 | product pixels | typography +1 product: New Contact / More actions labels 16px -> 14px, matching shell and row actions |
| contacts-desktop-light.png | 2.024% | 7.7 | 7.9 | 0.2 | product pixels | typography +1 product: New Contact / More actions labels 16px -> 14px, matching shell and row actions |
| contacts-mobile-dark.png | 2.221% | 7.5 | 7.5 | 0.0 | product pixels + reviewer variance | typography +1 product: New Contact / More actions labels 16px -> 14px, matching shell and row actions density_layout -1 reviewer variance: Phone contact rows are pixel-identical to stage04 (137px stacked rows, three left edges); I weighed them more harshly (7 -> 6). |
| contacts-mobile-light.png | 2.171% | 7.5 | 7.5 | 0.0 | product pixels + reviewer variance | typography +1 product: New Contact / More actions labels 16px -> 14px, matching shell and row actions density_layout -1 reviewer variance: Phone contact rows are pixel-identical to stage04 (137px stacked rows, three left edges); I weighed them more harshly (7 -> 6). |
| inbox-desktop-dark.png | 2.722% | 7.7 | 8.0 | 0.3 | product pixels | density_layout +1 product: row checkboxes now vertically centred in each row (stage04 top-aligned 13px boxes) visible_affordances +1 product: 13px boxes -> 16px native boxes in 40px labels |
| inbox-desktop-light.png | 2.652% | 7.7 | 8.0 | 0.3 | product pixels | density_layout +1 product: row checkboxes now vertically centred in each row (stage04 top-aligned 13px boxes) visible_affordances +1 product: 13px boxes -> 16px native boxes in 40px labels |
| inbox-mobile-dark.png | 6.136% | 7.7 | 7.8 | 0.1 | product pixels | visible_affordances +1 product: 13px boxes -> 16px native boxes in 40px labels |
| inbox-mobile-light.png | 5.925% | 7.7 | 7.8 | 0.1 | product pixels | visible_affordances +1 product: 13px boxes -> 16px native boxes in 40px labels |
| inbox-selection-desktop-dark.png | 3.411% | 7.9 | 8.2 | 0.3 | product pixels | visible_affordances +2 product: select-all now shows native indeterminate for 2 of 4 (stage04 empty) and boxes 13px -> 16px |
| inbox-selection-desktop-light.png | 3.377% | 7.9 | 8.2 | 0.3 | product pixels | visible_affordances +2 product: select-all now shows native indeterminate for 2 of 4 (stage04 empty) and boxes 13px -> 16px |
| inbox-selection-mobile-dark.png | 7.000% | 7.7 | 8.0 | 0.3 | product pixels | visible_affordances +2 product: select-all now shows native indeterminate for 2 of 4 (stage04 empty) and boxes 13px -> 16px |
| inbox-selection-mobile-light.png | 6.958% | 7.7 | 8.0 | 0.3 | product pixels | visible_affordances +2 product: select-all now shows native indeterminate for 2 of 4 (stage04 empty) and boxes 13px -> 16px |
| login-desktop-dark.png | 8.254% | 8.2 | 8.4 | 0.2 | product pixels | density_layout +1 product: verification status now compact and left-aligned under its label (stage04 centred in a tall gap) |
| login-desktop-light.png | 8.248% | 8.2 | 8.4 | 0.2 | product pixels | density_layout +1 product: verification status now compact and left-aligned under its label (stage04 centred in a tall gap) |
| login-mobile-dark.png | 12.649% | 8.2 | 8.4 | 0.2 | product pixels | density_layout +1 product: verification status now compact and left-aligned under its label (stage04 centred in a tall gap) |
| login-mobile-light.png | 12.647% | 8.2 | 8.4 | 0.2 | product pixels | density_layout +1 product: verification status now compact and left-aligned under its label (stage04 centred in a tall gap) |
| reader-desktop-dark.png | 0.007% | 8.2 | 7.8 | -0.4 | reviewer variance | hierarchy -1 reviewer variance: Reader dialog/sheet pixels unchanged (desktop diff 0.007% in the inbox behind the scrim; phone byte-identical); I judged hierarchy at the 8 anchor rather than 9. color -1 reviewer variance: Same unchanged pixels; color judged 8 rather than 9. |
| reader-desktop-light.png | 0.007% | 8.2 | 7.8 | -0.4 | reviewer variance | hierarchy -1 reviewer variance: Reader dialog/sheet pixels unchanged (desktop diff 0.007% in the inbox behind the scrim; phone byte-identical); I judged hierarchy at the 8 anchor rather than 9. color -1 reviewer variance: Same unchanged pixels; color judged 8 rather than 9. |
| reader-mobile-dark.png | identical | 8.4 | 7.9 | -0.5 | reviewer variance | hierarchy -1 reviewer variance: Reader dialog/sheet pixels unchanged (desktop diff 0.007% in the inbox behind the scrim; phone byte-identical); I judged hierarchy at the 8 anchor rather than 9. typography -1 reviewer variance: Phone only: unchanged pixels; I now count the hard-wrapped "so the / client's" body break as a typography slip. color -1 reviewer variance: Same unchanged pixels; color judged 8 rather than 9. |
| reader-mobile-light.png | identical | 8.4 | 7.9 | -0.5 | reviewer variance | hierarchy -1 reviewer variance: Reader dialog/sheet pixels unchanged (desktop diff 0.007% in the inbox behind the scrim; phone byte-identical); I judged hierarchy at the 8 anchor rather than 9. typography -1 reviewer variance: Phone only: unchanged pixels; I now count the hard-wrapped "so the / client's" body break as a typography slip. color -1 reviewer variance: Same unchanged pixels; color judged 8 rather than 9. |

## Gates

- `keyboard`: untested. Partial live (Chrome 154 DPR2, own incognito tab): Escape closed Add Contact unsubmitted and focus returned to New Contact. Full task paths not traversed.
- `focus`: untested. No focused-state captures; rings not inspected.
- `contrast`: untested. No computed-style contrast measured by me.
- `states`: untested. No empty, loading or error captures.
- `responsive`: untested. Manifest overflow 0 for all 32 at 390/1280; live desktop row/select-all checkboxes 16x16 inside 42x40 labels, contact controls 40px. Phone targets not measured live.

Screenshots alone cannot pass a gate. Parent-reported checks (1235 tests/111 files, typecheck, lint, runtime audit, build on Node 26.12.1, 4 real-browser cases) were not re-run by me; hosted CI/security on the follow-up PR remain pending.

## Live supplement (after freeze, not scored)

Chrome 154.0.0.0, DPR2, 1280x800 (differs from scored Chromium 153 DPR1 1280x900). Real sign-in with real PoW as the synthetic user (credentials not recorded); clicked two row-checkbox label corners (outside the 16px box) to select, then unselected them; opened Contacts; opened Add Contact and closed it with Escape unsubmitted. No message opened, nothing sent/saved/deleted/configured; read flags not touched by me beyond the app's own inbox load.

- Login: verification status left-aligned (text-align start, x=451 = field x); fields 40px, Sign in 46px.
- Inbox: 5 table checkboxes are 16x16 inside 42x40 labels with descriptive names ("Select email …").
- Label-corner clicks selected rows 2-3; select-all then reported checked=false, indeterminate=true, "2 selected"; after unselecting both, indeterminate=false.
- Contacts: New Contact 14px/600, More actions 14px/400, New Email 14px/600, Edit/Delete 14px/400, all 40px tall.
- Add Contact: title 17px/600; + Add email / + Add phone / Cancel 14px/400 at 40px; field order Full Name, Organization and name details, More name fields, then Emails.
- Escape closed the dialog; focus returned to New Contact.

## Limits and exposure

Other roles/admin, empty/loading/error states, long lists, HTML mail, attachments, menus, phone live behaviour, assistive tech, full keyboard paths, contrast measurement, hosted CI/security. No competitor, ranking or whole-product claim. Before freeze I opened only: /home/yoshi/git/busnes.app/AGENTS.md; AGENTS.md; frontend/AGENTS.md; docs/ui-post-eight/AGENTS.md; authority.json + the five pinned ky-ui objects; inventory-v4.json; duplicate-audit-v4.json; manifest-06-after.json; capture-provenance-r4.json; artifacts.json (file/sha256/role only); brand-references.json + its four images; the 32 stage06 originals, each individually at native resolution; frontend/src/App.tsx lines 560-640; capture.mjs hashed only, not read. After freeze: opus-round3.md; opus-round3.json; stage04 originals viewed: contact-editor-desktop-light, contacts-mobile-light, login-desktop-light, appearance-mobile-light, inbox-selection-desktop-light; all 32 stage04/stage06 pairs diffed numerically. Never opened: progression-rule.json, HANDOFF.md, verification.json, delivery.json, round*-audit.json, COUNCIL.md, CSV/XLSX, README.md, galleries, PR description/comments, opus-round1/2, any sol-* report, stage00/01/02/03/05 images. After freeze, `git status` listed untracked peer files docs/ui-post-eight/sol-round4.json and sol-round4.md by name only; not opened. A post-freeze diff --stat named HANDOFF.md and delivery.json with line counts only; not opened. The task prompt itself described the round4 responses and the parent stage05 inspection. Gemini unused; MySlop not posted (known 401).
