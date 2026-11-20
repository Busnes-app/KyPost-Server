# Native mail in a browser: plus and masked addresses, 2026-11-19

These are checks 3 and 4 from the 2026-11-19 run-through, which until now had
Go-test coverage only. They were run with the
[native mail harness](../DEVELOPMENT.md#native-mail-harness).

## Conditions

- KyPost from this branch (base `origin/main` `28d91bc2`), frontend built on
  Node 26.13.1.
- KyIdentity `origin/master` `a86964c`, kypost-demo-IMAP `31cf283` and Maddy
  0.9.5 (the pinned hash).
- Everything ran inside one loopback-only namespace on a Fedora 45 x86_64
  workstation. No host interface was used, and neither were ports 5391 or 5392.
- Playwright 1.64.1 Chromium, 1440×900, light theme. Real sign-in: the local
  bootstrap administrator for admin steps, and `owner` through KyIdentity's
  OIDC sign-in for user steps.
- Real TLS everywhere: a throwaway CA, trusted by the servers and by
  Chromium's NSS database.
- The incoming "outside sender" is `curl` over SMTP with STARTTLS, to
  `mx.harness.test:25`.
- Replies leave through the saved domain relay to the fixture on 465, and are
  read back over the fixture's IMAPS.
- `evidence.json` holds the SMTP replies and the relayed headers.
- All 10 driver assertions passed on the final clean run (`down`, then `up`
  through `check4`).

## Check 3: plus addressing

| Step | Result | Evidence |
| --- | --- | --- |
| Mail to `owner+news@harness.test` is accepted and arrives | pass: RCPT `250`, then it shows in `owner`'s inbox | `03-1-plus-arrived.png` |
| Reply From is the plus address | pass: compose preselects `owner+news@harness.test`; the relay received `From: owner+news@harness.test` | `03-2-plus-reply-from.png`, `evidence.json` `check3.relayedHeaders` |
| The reply is filed in Sent | pass | `03-3-plus-reply-in-sent.png` |

## Check 4: masked addresses

| Step | Result | Evidence |
| --- | --- | --- |
| Admin ticks `harness.test` under Mail addresses → Masked addresses | pass: "Saved. Users can make masked addresses on these domains." | `04-1-admin-masked-domain-enabled.png` |
| User creates one (note "example-shop") | pass: `calm.raven.3458@harness.test`, Active | `04-2-masked-created.png` |
| Mail to it arrives | pass: RCPT `250` | `04-3-masked-arrived.png` |
| Reply From is the masked address | pass: compose preselects it; relay received `From: calm.raven.3458@harness.test` | `04-4-masked-reply-from.png`, `evidence.json` |
| Paused address refuses mail | pass: `550 5.1.1 Recipient unavailable`, the same reply as a never-existing address | `04-5-masked-paused.png`, `evidence.json` `paused`/`unknown` |
| Resume restores delivery | pass: RCPT `250`, and the mail arrives | `04-6-masked-resumed-arrived.png` |
| Deleted address refuses mail and is listed as deleted | pass: `550 5.1.1`, same as unknown | `04-7-masked-deleted.png` |

Setup captures: `01-admin-domain-established.png` (TXT proof verified through
dnsmasq) and `02-admin-relay-checked.png` ("TLS and SMTP authentication
passed for the saved relay").

## Observed, not fixed

- Settings → Mail → Masked & plus addresses: the "Accept mail sent to plus
  addresses" checkbox renders on its own line, centred, above its label
  (`04-5-masked-paused.png`).
- Full-page captures of long admin pages show the sidebar pinned at the
  bottom. That comes from Playwright's full-page capture of a sticky sidebar,
  not from the page.

## Not exercised

- Dark theme and phone width.
- PGP, with the key policy set to Optional.
- Disposable (expiring) masked addresses, the per-day limit, and turning plus
  addressing off.
- Cloudflare receiving, Rspamd, and delivery past the relay. The fixture is a
  black hole, so its acceptance is not recipient delivery or DMARC alignment.
