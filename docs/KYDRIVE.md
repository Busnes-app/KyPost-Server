# KyDrive files

Server-to-server contract between KyPost and KyDrive, wave 1 of the suite
integrations (section C of `ky-redesign/integrations/suite-integrations-contracts.md`,
copied here on 2026-11-14; this file is now the durable copy). KyPost sends
large files as KyDrive links and saves received attachments to KyDrive.
KyDrive is the resource owner. Change both sides together.

## Pairing

Pair `kydrive`↔`kypost`, exactly as KyCalendar's
([KYCALENDAR.md](KYCALENDAR.md#pairing)): same URL rules, refresh, backoff,
outage and unpair rules; secret in memory only.

| Variable | Meaning |
|---|---|
| `KYPOST_KYDRIVE_URL` | KyDrive's `https://host[:port]`, no path, query or credentials. |
| `KYPOST_KYDRIVE_SECRET_FILE` | File holding the shared secret, at least 32 bytes after trimming. |
| `KYPOST_KYDRIVE_ALLOW_PRIVATE` | `true` lets KyPost reach KyDrive at a private address. Default `false`. |

Both unset: KyPost asks `GET {issuer}/api/suite/peer?client=kydrive` with its
own SSO client credentials. The API process pairs; the daemon does not use
KyDrive. `GET /api/admin/kydrive` (administrators) reports
`{source, connected, url, reason}`, never the secret; the web admin shows it
under Server → KyDrive.

The per-user switch lives in KyDrive settings ("Let KyPost save and share my
files"): default on when KyIdentity paired the apps, off when the environment
did; a stored choice wins. Off: KyDrive answers 409 `sharing_off`.

## Signing

Every request is signed with `ky-primitives/syncauth` v1 under the pair
secret, with a fresh event ID (`kypost-drive-<uuid>`; KyPost does not retry,
the user does). KyDrive checks the event type equals the route's, then
verifies with a replay guard: a verified replay is 200 `{"outcome":"duplicate"}`,
anything else 401 `{"error","code":"bad_signature"}`, which makes KyPost
re-resolve the pairing (at most once a minute). Unpaired KyDrive answers 404
`kypost_off`. Bodies name the user by `"sub"` and `"issuer"`; KyDrive answers
404 `no_user` when no active everyday account is linked to them.

| Event type | Route on KyDrive |
|---|---|
| `kypost.drive.ticket` | `POST /api/suite/kypost/upload-tickets` |
| `kypost.drive.link` | `POST /api/suite/kypost/links` |
| `kypost.drive.status` | `POST /api/suite/kypost/status` |

Files go into the user's personal workspace, folder `Mail attachments`
(created on first use, an ordinary folder; a deleted one is recreated). A
name collision gets ` (2)`, ` (3)` before the extension.

### Upload ticket

```json
{"sub":"…","issuer":"…","name":"report.pdf","size":7340032}
```

201 `{"upload_url":"https://drive.example/api/suite/upload/<ticket>","expires":"2026-11-14T15:04:05Z","max_chunk":67108864}`.

- 256 random bits, stored hashed, single file, bound to that user, name and
  exact size, valid 15 minutes, at most 8 live per user.
- `PUT <upload_url>` with the raw bytes, `Content-Length` equal to `size`. No
  cookie, no CSRF, no signature: the ticket is the credential, so the URL is
  never logged. Larger than `max_chunk`: `Content-Range` chunks in order,
  same URL.
- That route answers CORS for exactly the paired KyPost origin (`PUT`,
  `OPTIONS`, headers `Content-Type`, `Content-Range`; no credentials).
- Final answer 201 `{"file_id":"…","name":"report (2).pdf","size":7340032}`.
- Refusals at issue: 413 `too_large`, 507 `quota`, 400 `bad_body` (name
  rules), 429 `too_many_tickets`.

### Link

`{"sub":"…","issuer":"…","file_id":"…","days":30}` → 201
`{"url":"https://drive.example/l/#<token>","expires":"…"}`. An organisation
link (signed-in members only), role reader. `days` is 1, 7, 30 or 0 for no
expiry. `file_id` must be a file the user owns in their personal workspace,
else 404 `not_found`.

### Status

`{"sub":"…","issuer":"…"}` → 200 `{"enabled":true,"free_bytes":123,"max_file_bytes":123}`.

## KyPost's routes

Session (or paired device) routes, CSRF as for mail routes, per-user rate
limit (burst 30, one every two seconds; 429 `rate_limited` with
`Retry-After`), `Cache-Control: no-store`. Refusals are `{"error","code"}`.
Dedicated administrator identities (native mode: an administrator without a
legacy mixed-use mailbox) get the administrator 403. 409 `kydrive_off`
means: hide the KyDrive options.

| Route | Body | Answer |
|---|---|---|
| `GET /api/kydrive/status` | | 200 `{available, freeBytes?, maxFileBytes?, appUrl?, origin?}` |
| `POST /api/kydrive/upload-tickets` | `{name, size}` | 201 `{uploadUrl, expires, maxChunk}` |
| `POST /api/kydrive/links` | `{fileId, days?}` (default 30) | 201 `{url, expires}` |
| `POST /api/kydrive/save-attachment` | `{messageId, mailbox?, index}` | 201 `{fileId, name, size, folder, appUrl}` |
| `POST /api/kydrive/recipients` | `{addresses}` (1 to 500 bare addresses) | 200 `{outside}` |

- `available` is true only when KyDrive is paired, the account has a live
  SSO link to the configured issuer, `kydrive` is in the user's suite app
  list, and KyDrive's status says enabled. Cached one minute per user; any
  failure reads as false. `origin` is the paired KyDrive whenever paired, so
  the client can find its links in a body even while `available` is false.
- KyDrive's `too_large`, `quota`, `bad_body`, `too_many_tickets`,
  `sharing_off`, `not_found` and `no_user` pass through with KyDrive's
  status; anything else, a network failure, or an answer whose URL is not on
  the paired origin (`/api/suite/upload/…` for tickets, `/l/#…` for links) is
  502 `kydrive_unavailable`. A `sharing_off` drops every cached status.
- The ticket URL goes to the signed-in browser in that one answer, never to
  a log. Logs carry user and event IDs and status codes only: no URL, file
  name, subject or address.
- Save to KyDrive reads the part server-side (the mail parser already holds
  it, within the 25 MiB inbound cap), asks for a ticket and PUTs that same
  copy in `max_chunk` pieces; no second buffer, ten-minute budget.
- `recipients` resolves the addresses with `suitedir.Resolve` (KyIdentity
  `GET /api/suite/people?sub=&email=…`, 100 per call) under the session's
  subject and answers the ones that are not people in the organisation. 503
  `unavailable` when KyIdentity cannot answer.

The Content-Security-Policy's `connect-src` names the paired KyDrive origin
only while it is paired, so the browser can PUT to the ticket.

## Web client

- Shown only when `available`.
- Compose: picking files over the 18 MiB attachment limit offers "Upload to
  KyDrive and send a link" instead of the error; every attachment chip has a
  **Link** action doing the same. Uploads show progress with Cancel; Send and
  Schedule wait for them. A failed or cancelled upload changes nothing in the
  draft (a chip stays attached). The link is inserted at the cursor as the
  file name linked to the URL, then `(size, KyDrive, link expires <date>)` as
  text. It is ordinary body HTML, so it survives draft save, autosave and
  reload.
- Before sending a body that carries links on the paired origin, compose
  calls `recipients`. Anyone outside is named, with "cannot open the KyDrive
  links", and the user chooses **Send anyway**, **Attach the files instead**
  (only when every linked file was uploaded in this window and all of them
  fit the attachment limit; their link lines are removed), or Cancel. A
  failed check holds the send the same way, naming every recipient.
- Reading: each attachment has **Save to KyDrive**; success shows the folder
  and name it got and an **Open KyDrive** link to the app's suite URL.
