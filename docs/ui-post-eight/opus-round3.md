# KyPost UI Council round3: Opus independent review

Reviewer: Claude Opus 5.5 (`claude-opus-5-5`). Rubric `ky-suite-v2` pinned at ky-ui `639e5882` (five objects verified). Product `2ff40cc2`, evidence `546cc595`, worktree head `917f3f6e`.

## Result

- Overall quality, all 32 stage04 originals: **252.4 / 32 = 631/80 = 7.8875 → 7.9** (half-up 0.1).
- Separate Ky brand fit mean: 8.3406 (2669/320).
- Satisfied: **no**. Genuinely satisfied: **no**. My frozen Opus overall is 252.4/32 = 7.8875, 7.9 half-up, below 8.0. The round3 fixes are real and visible (reader phone 2x2 + full sender, compose picker face and aligned phone margins, contacts phone rows, all six selection actions visible), but the set still shows small repeated slips: 16px page-header and editor add-button labels beside 14px shell, footer and menu labels; split name disclosures in the contact editor; 13px native checkboxes with no mixed select-all state; uneven security-check status on login; an 8px title inset on the phone editor and 16 vs 20px page insets on phone. None is broken; I do not ask for scope beyond these small fixes. Parent decides progression.
- Frozen 2026-10-27T18:51:16+00:00 before opening any prior/peer material; vectors sha256 `e26dc76cebc73ba7a8f31220417dd16ff09a81dd38dd0bfce8c5078e76a418af`.

## Inventory

136 files: 4 brand-reference, 32 historical-r2-scored (paired drift only), 32 historical-stage00-baseline, 32 scored-stage04-original, 32 superseded-stage02, 4 supplemental-settled-login-before (unscored). Scored 32, all unique by sha256; 240 same-size pairs recomputed, 0 near-duplicates (min changed fraction 0.0663). Hashes, bytes and dimensions match inventory-v3, manifest-04-after and brand-references; duplicate-audit-v3 reproduced exactly.

## Cohorts (coverage, not a ranking)

| Cohort | n | Sum | Exact | Half-up |
|---|---|---|---|---|
| auth | 4 | 32.8 | 41/5 = 8.2 | 8.2 |
| compose | 4 | 32.4 | 81/10 = 8.1 | 8.1 |
| contact-editor | 4 | 30.0 | 15/2 = 7.5 | 7.5 |
| contacts-list | 4 | 30.4 | 38/5 = 7.6 | 7.6 |
| mail-list | 8 | 62.0 | 31/4 = 7.75 | 7.8 |
| mail-read | 4 | 33.2 | 83/10 = 8.3 | 8.3 |
| settings | 4 | 31.6 | 79/10 = 7.9 | 7.9 |

## Files

| File | H | T | D | C | A | Co | Total | ky_fit (N/A) |
|---|---|---|---|---|---|---|---|---|
| inbox-desktop-light.png | 8 | 8 | 7 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| inbox-selection-desktop-light.png | 8 | 8 | 8 | 8 | 7 | 8 | 7.9 | 8.20 (-) |
| reader-desktop-light.png | 9 | 8 | 7 | 9 | 8 | 8 | 8.2 | 8.40 (-) |
| contacts-desktop-light.png | 8 | 7 | 8 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| contact-editor-desktop-light.png | 8 | 7 | 7 | 8 | 8 | 7 | 7.5 | 8.20 (-) |
| appearance-desktop-light.png | 8 | 8 | 8 | 8 | 8 | 7 | 7.9 | 8.40 (-) |
| compose-desktop-light.png | 8 | 8 | 8 | 9 | 8 | 8 | 8.2 | 8.40 (-) |
| login-desktop-light.png | 9 | 8 | 7 | 9 | 8 | 8 | 8.2 | 8.50 (shell) |
| inbox-desktop-dark.png | 8 | 8 | 7 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| inbox-selection-desktop-dark.png | 8 | 8 | 8 | 8 | 7 | 8 | 7.9 | 8.20 (-) |
| reader-desktop-dark.png | 9 | 8 | 7 | 9 | 8 | 8 | 8.2 | 8.40 (-) |
| contacts-desktop-dark.png | 8 | 7 | 8 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| contact-editor-desktop-dark.png | 8 | 7 | 7 | 8 | 8 | 7 | 7.5 | 8.20 (-) |
| appearance-desktop-dark.png | 8 | 8 | 8 | 8 | 8 | 7 | 7.9 | 8.40 (-) |
| compose-desktop-dark.png | 8 | 8 | 8 | 9 | 8 | 8 | 8.2 | 8.40 (-) |
| login-desktop-dark.png | 9 | 8 | 7 | 9 | 8 | 8 | 8.2 | 8.50 (shell) |
| inbox-mobile-light.png | 8 | 8 | 7 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| inbox-selection-mobile-light.png | 8 | 8 | 7 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| reader-mobile-light.png | 9 | 8 | 8 | 9 | 8 | 8 | 8.4 | 8.50 (shell) |
| contacts-mobile-light.png | 8 | 7 | 7 | 8 | 7 | 8 | 7.5 | 8.20 (-) |
| contact-editor-mobile-light.png | 8 | 7 | 7 | 8 | 8 | 7 | 7.5 | 8.25 (shell) |
| appearance-mobile-light.png | 8 | 8 | 8 | 8 | 8 | 7 | 7.9 | 8.60 (-) |
| compose-mobile-light.png | 8 | 8 | 7 | 9 | 8 | 8 | 8.0 | 8.50 (shell) |
| login-mobile-light.png | 9 | 8 | 7 | 9 | 8 | 8 | 8.2 | 8.50 (shell) |
| inbox-mobile-dark.png | 8 | 8 | 7 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| inbox-selection-mobile-dark.png | 8 | 8 | 7 | 8 | 7 | 8 | 7.7 | 8.20 (-) |
| reader-mobile-dark.png | 9 | 8 | 8 | 9 | 8 | 8 | 8.4 | 8.50 (shell) |
| contacts-mobile-dark.png | 8 | 7 | 7 | 8 | 7 | 8 | 7.5 | 8.20 (-) |
| contact-editor-mobile-dark.png | 8 | 7 | 7 | 8 | 8 | 7 | 7.5 | 8.25 (shell) |
| appearance-mobile-dark.png | 8 | 8 | 8 | 8 | 8 | 7 | 7.9 | 8.60 (-) |
| compose-mobile-dark.png | 8 | 8 | 7 | 9 | 8 | 8 | 8.0 | 8.50 (shell) |
| login-mobile-dark.png | 9 | 8 | 7 | 9 | 8 | 8 | 8.2 | 8.50 (shell) |

Per-dimension observations, caveats, artifacts and confidence for every file are in `opus-round3.json`.

## Findings and smallest fixes

- **Mixed button label sizes** (contacts, contact-editor (8 files)): contacts header 16px, editor add buttons 16px, shell/footer/menu 14px. Fix: Use the 14px control label token for New Contact, More actions and '+ Add email/phone'.
- **Split name disclosures** (contact-editor (4)): 'Organization and name details' sits between Emails and Phones; 'More name fields' after Phones. Fix: Merge into one disclosure or place both directly under Full Name.
- **No mixed select-all state** (inbox-selection (4)): 2 of 4 selected shows an empty header checkbox (live: indeterminate=false). Fix: Set input.indeterminate when 0 < selected < total.
- **13px checkboxes** (inbox, selection, contacts (12)): native checkboxes in inbox/contacts tables, also on phone. Fix: Give the cell a 40px label hit area (keep native box).
- **Phone editor title inset** (contact-editor mobile (2)): title x=24 while fields x=16. Fix: Use the same 16px inline padding for the phone dialog header.
- **Phone page inset mismatch** (appearance mobile (2) vs inbox, selection and contacts mobile): Appearance 16px vs Inbox/Contacts 20px. Fix: Pick one phone page padding token.
- **Login status alignment** (login (4)): left 'Security check' label over a centred status in a tall gap. Fix: Left-align the status under its label and drop the extra gap.
- **Desktop reader dialog height** (reader desktop (2)): fixed near-full dialog with two-line body. Fix: Let the dialog size to content up to max height.
- **Contacts duplicate count** (contacts (4)): '4 contacts' in header and 'Address Book 4'. Fix: Keep one count.

## Drift against my round2 (opened after freeze)

Sum 244.2 → 252.4, mean drift +0.25625. Nine files are byte-identical to stage03 (login ×4, appearance ×4, inbox desktop light), with mean drift 0.1333; that is reviewer variance, not improvement. My post-freeze attribution puts 2.50 of the weighted sum on reviewer variance; without it the overall would be 249.90/32 = 7.809375 → 7.8. Either way it is below 8.0.

| File | Prior | Now | Δ | Cause |
|---|---|---|---|---|
| inbox-desktop-light.png | 7.7 | 7.7 | 0.0 | unchanged/near-identical: Light byte-identical, dark differs in 0.001% of pixels in the selected Inbox nav row; scores identical. |
| inbox-selection-desktop-light.png | 7.8 | 7.9 | 0.1 | pixels changed (0.06%) + reviewer variance: Selected-count baseline now aligns with actions (density +1, product); affordance -1 because I now count the select-all box showing no mixed state (variance on unchanged pixels). |
| reader-desktop-light.png | 7.5 | 8.2 | 0.7 | pixels changed + reviewer variance: More actions now matches sibling button type (coherence +1, +0.15); hierarchy, color and affordance +1 each (+0.50) are re-judgment of regions whose pixels did not change (More actions still has no menu chevron), so +0.50 of +0.65 exact is reviewer variance. |
| contacts-desktop-light.png | 7.7 | 7.7 | 0.0 | pixels changed + reviewer variance: Right-aligned Actions header and row geometry changes (density +1, product); typography -1 for the 16px page buttons versus 14px New Email, which was already present (variance). |
| contact-editor-desktop-light.png | 7.5 | 7.5 | 0.0 | pixels changed + reviewer variance: Native matching disclosures and compact save-first guidance (affordance +1, product); typography -1 for the 16px add buttons that round2 already noted but scored 8 (variance). |
| appearance-desktop-light.png | 8.0 | 7.9 | -0.1 | reviewer variance (byte-identical to stage03): Identical bytes; coherence 8->7 because I now count the icon-less Settings sub-nav against the icon-bearing Mailboxes sub-nav. |
| compose-desktop-light.png | 7.7 | 8.2 | 0.5 | pixels changed + reviewer variance: Only the Normal picker changed (0.05% of pixels): shared face and baseline lift typography and coherence +1 each (+0.30); color +1 (+0.15) is reviewer variance on identical pixels. |
| login-desktop-light.png | 7.8 | 8.2 | 0.4 | reviewer variance (byte-identical to stage03): Identical bytes; hierarchy 8->9 and color 8->9 are my re-judgment that one-primary sign-in with semantic green status meets the 9 anchor for those two dimensions. Not a product change. |
| inbox-desktop-dark.png | 7.7 | 7.7 | 0.0 | unchanged/near-identical: Light byte-identical, dark differs in 0.001% of pixels in the selected Inbox nav row; scores identical. |
| inbox-selection-desktop-dark.png | 7.8 | 7.9 | 0.1 | pixels changed (0.06%) + reviewer variance: Selected-count baseline now aligns with actions (density +1, product); affordance -1 because I now count the select-all box showing no mixed state (variance on unchanged pixels). |
| reader-desktop-dark.png | 7.5 | 8.2 | 0.7 | pixels changed + reviewer variance: More actions now matches sibling button type (coherence +1, +0.15); hierarchy, color and affordance +1 each (+0.50) are re-judgment of regions whose pixels did not change (More actions still has no menu chevron), so +0.50 of +0.65 exact is reviewer variance. |
| contacts-desktop-dark.png | 7.7 | 7.7 | 0.0 | pixels changed + reviewer variance: Right-aligned Actions header and row geometry changes (density +1, product); typography -1 for the 16px page buttons versus 14px New Email, which was already present (variance). |
| contact-editor-desktop-dark.png | 7.5 | 7.5 | 0.0 | pixels changed + reviewer variance: Native matching disclosures and compact save-first guidance (affordance +1, product); typography -1 for the 16px add buttons that round2 already noted but scored 8 (variance). |
| appearance-desktop-dark.png | 8.0 | 7.9 | -0.1 | reviewer variance (byte-identical to stage03): Identical bytes; coherence 8->7 because I now count the icon-less Settings sub-nav against the icon-bearing Mailboxes sub-nav. |
| compose-desktop-dark.png | 7.7 | 8.2 | 0.5 | pixels changed + reviewer variance: Only the Normal picker changed (0.05% of pixels): shared face and baseline lift typography and coherence +1 each (+0.30); color +1 (+0.15) is reviewer variance on identical pixels. |
| login-desktop-dark.png | 7.8 | 8.2 | 0.4 | reviewer variance (byte-identical to stage03): Identical bytes; hierarchy 8->9 and color 8->9 are my re-judgment that one-primary sign-in with semantic green status meets the 9 anchor for those two dimensions. Not a product change. |
| inbox-mobile-light.png | 7.7 | 7.7 | 0.0 | pixels changed (7% in search/list region): Scores identical to round2. |
| inbox-selection-mobile-light.png | 7.7 | 7.7 | 0.0 | pixels changed: All six actions are now visible, wrapping 4+2; my dimension scores equal round2, so the product fix offsets a ragged 4+2 wrap that I still count. |
| reader-mobile-light.png | 7.2 | 8.4 | 1.2 | pixels changed + reviewer variance: Balanced 2x2 actions replace the lonely More actions row (density +2, +0.40), the full sender value no longer breaks mid-domain (typography +1, +0.15) and button type matches (coherence +1, +0.15); hierarchy, color and affordance +1 each (+0.50) are reviewer variance on unchanged elements. |
| contacts-mobile-light.png | 7.3 | 7.5 | 0.2 | pixels changed: Full-width identity rows with actions beneath remove the squeezed, uneven rows (density 6->7); tall 137px rows and the clipped placeholder keep it at 7. |
| contact-editor-mobile-light.png | 7.5 | 7.5 | 0.0 | pixels changed + reviewer variance: Same as desktop: disclosure unification is product (+0.15), add-button type re-judged (-0.15). |
| appearance-mobile-light.png | 8.0 | 7.9 | -0.1 | reviewer variance (byte-identical to stage03): Identical bytes; coherence 8->7 for the mixed icon/no-icon tab strip. |
| compose-mobile-light.png | 7.2 | 8.0 | 0.8 | pixels changed + reviewer variance: Shared picker face (typography +1), aligned toolbar/editor margins removing the stepped inset (density +1) and coherence +1 are product pixels (+0.50); color and affordance +1 each (+0.30) are reviewer variance, Trash still sits beside Contacts. |
| login-mobile-light.png | 7.8 | 8.2 | 0.4 | reviewer variance (byte-identical to stage03): Identical bytes; same hierarchy and color re-judgment as desktop login. Not a product change. |
| inbox-mobile-dark.png | 7.7 | 7.7 | 0.0 | pixels changed (7% in search/list region): Scores identical to round2. |
| inbox-selection-mobile-dark.png | 7.7 | 7.7 | 0.0 | pixels changed: All six actions are now visible, wrapping 4+2; my dimension scores equal round2, so the product fix offsets a ragged 4+2 wrap that I still count. |
| reader-mobile-dark.png | 7.2 | 8.4 | 1.2 | pixels changed + reviewer variance: Balanced 2x2 actions replace the lonely More actions row (density +2, +0.40), the full sender value no longer breaks mid-domain (typography +1, +0.15) and button type matches (coherence +1, +0.15); hierarchy, color and affordance +1 each (+0.50) are reviewer variance on unchanged elements. |
| contacts-mobile-dark.png | 7.3 | 7.5 | 0.2 | pixels changed: Full-width identity rows with actions beneath remove the squeezed, uneven rows (density 6->7); tall 137px rows and the clipped placeholder keep it at 7. |
| contact-editor-mobile-dark.png | 7.5 | 7.5 | 0.0 | pixels changed + reviewer variance: Same as desktop: disclosure unification is product (+0.15), add-button type re-judged (-0.15). |
| appearance-mobile-dark.png | 8.0 | 7.9 | -0.1 | reviewer variance (byte-identical to stage03): Identical bytes; coherence 8->7 for the mixed icon/no-icon tab strip. |
| compose-mobile-dark.png | 7.2 | 8.0 | 0.8 | pixels changed + reviewer variance: Shared picker face (typography +1), aligned toolbar/editor margins removing the stepped inset (density +1) and coherence +1 are product pixels (+0.50); color and affordance +1 each (+0.30) are reviewer variance, Trash still sits beside Contacts. |
| login-mobile-dark.png | 7.8 | 8.2 | 0.4 | reviewer variance (byte-identical to stage03): Identical bytes; same hierarchy and color re-judgment as desktop login. Not a product change. |

## Gates

- `keyboard`: untested. Partial live check (Chrome 154 DPR2, own incognito tab, localhost:18461): Escape closed the Contacts More actions menu with focus on More actions; Escape closed the Add Contact dialog with focus on New Contact. Full task paths not traversed.
- `focus`: untested. No focused-state captures; rings not inspected.
- `contrast`: untested. No computed-style contrast measured this round; parent checks not re-run by me.
- `states`: untested. No empty, loading or error captures.
- `responsive`: untested. Manifest overflow 0 for all 32 at 390/1280; live select-all and row checkboxes measure 13x13 CSS px, so target usability is unproven.

Screenshots alone cannot pass a gate. Parent-reported checks (1235 tests/111 files, typecheck, lint, runtime audit, build, 4 real-browser cases) were not re-run by this reviewer.

## Live supplement (after freeze, not scored)

Chrome 154.0.0.0, DPR2, 1280x800 (differs from scored Chromium 153 DPR1 1280x900). real login as the synthetic user (credentials not recorded); checked and unchecked two inbox rows (client selection only); opened/closed Contacts More actions; opened Add Contact and closed it with Escape unsubmitted. No message opened, nothing sent/saved/deleted/configured. The app's own inbox load requested message bodies 1-4 (as on any load); I did not inspect or reset read flags.

- Select-all with 2 of 4 rows selected: checked=false, indeterminate=false (confirms no mixed state)
- Row/select-all checkboxes 13x13 CSS px
- New Contact 16px/600 and More actions 16px/400 vs sidebar New Email 14px/600 (confirms mixed button type)
- Add Contact: '+ Add email' 16px/400 vs Cancel 14px/400 and Add Contact 14px/600; dialog title 17px; all seven section toggles are native <summary>
- Contacts utility menu items are quiet 14px/400 rows at 40px in a shadowed overlay; consistent with the quiet item treatment the parent describes (reader menu not opened this round)
- Escape closes menu and dialog with focus returned to the trigger

No replacement scoring; Chrome 154/DPR2 conditions; console 404 on /api/pgp/identity seen on Contacts load (fixture has no PGP identity; not judged).

## Limits

Other roles/admin, empty/loading/error states, long lists, HTML mail, attachments, menus beyond those listed, assistive tech, full keyboard paths, hosted CI/security. No competitor, ranking or whole-product claim. Exposure disclosed: authority/inventory/duplicate/manifest-04/provenance/brand/progression-rule read before freeze; manifest-03-after and opus-round2 only after freeze. progression-rule.json states the round2 Opus aggregate (1221/160); I read it before freezing and disclose that exposure. No peer report, COUNCIL.md, workbook, CSV, audits, gallery, historical quality image or prior per-file score was opened before the freeze. Gemini unused; MySlop not posted (known 401).
