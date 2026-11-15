# KyDrive files

Contract between KyPost and KyDrive: wave 1 of the suite integrations
(section C of `ky-redesign/integrations/suite-integrations-contracts.md`) and
wave 2's picker and fetch (sections I and I2) and guest links (section K) of
`suite-integrations-wave2.md`, copied here on 2026-11-14; this file is now
the durable copy. KyPost sends
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
| `kypost.drive.fetch` | `POST /api/suite/kypost/fetch` |

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

`{"sub":"…","issuer":"…"}` → 200 `{"enabled":true,"free_bytes":123,"max_file_bytes":123,"guest_links":true,"guest_max_days":30}`.
`guest_links` and `guest_max_days` are wave 2 (K); an older KyDrive omits them.

### Guest links (K)

The link request gains an optional `"audience":"guest"` (absent means
`"organisation"`).

- Allowed only when KyDrive's administrator guest-link policy is on, the
  user's second KyDrive switch "Let KyPost create guest links for people
  outside the organisation" is on (default off, shown only while the policy is
  on), and `days` is 1 to the policy's `max_days`, never 0. Otherwise 409
  `guest_links_off` or 400 `bad_body`.
- The link is a KyDrive guest link (`https://drive.example/g/#<token>`) with
  the same rights as one made in KyDrive (view and download, no upload),
  listed with the file's other links so the owner can revoke it, and audited
  as made through KyPost.
- An old KyDrive ignores `audience` and would make an organisation link, so
  KyPost sends `audience` only when the status carried `guest_links:true`.

### Fetch (I2)

`{"sub","issuer","file_id","max_bytes"}` → 200 with the raw bytes,
`Content-Type: application/octet-stream`, `Content-Disposition` carrying the
name. The file must be one the user may read, else 404 `not_found`. Larger
than `max_bytes` (required; KyPost sends its attachment limit, 18 MiB): 413
`too_large`. The per-user switch applies (409 `sharing_off`).

## Picker (I)

`https://<kydrive-origin>/pick?origin=<caller origin>&mode=link|copy`, opened
as a popup on a user click.

- KyDrive serves it only to a signed-in everyday user, and only when `origin`
  is exactly the `https://` origin of an app in that user's suite app list.
  Otherwise it shows a plain refusal and sends nothing.
- The user picks files they may share and confirms; only that click sends.
- `mode=link` (default): KyDrive makes an organisation link per file (role
  reader, contract C's expiry choice), then
  `window.opener.postMessage({type:"kydrive.picked", files:[{name,size,url,expires}]}, origin)`
  and closes. Never `*`.
- `mode=copy`, only when `origin` is the paired KyPost: `files:[{name,size,file_id}]`,
  no link made; KyPost fetches the bytes (I2).
- The picker keeps `frame-ancestors 'none'`.
- Callers listen for exactly `kydrive.picked` from exactly the KyDrive origin
  and treat every field as untrusted text.

## KyPost's routes

Session (or paired device) routes, CSRF as for mail routes, per-user rate
limit (burst 30, one every two seconds; 429 `rate_limited` with
`Retry-After`), `Cache-Control: no-store`. Refusals are `{"error","code"}`.
Dedicated administrator identities (native mode: an administrator without a
legacy mixed-use mailbox) get the administrator 403. 409 `kydrive_off`
means: hide the KyDrive options.

| Route | Body | Answer |
|---|---|---|
| `GET /api/kydrive/status` | | 200 `{available, freeBytes?, maxFileBytes?, appUrl?, origin?, guestLinks?, guestMaxDays?}` |
| `POST /api/kydrive/upload-tickets` | `{name, size}` | 201 `{uploadUrl, expires, maxChunk}` |
| `POST /api/kydrive/links` | `{fileId, days?, audience?}` (default 30; `audience` `"organisation"` or `"guest"`) | 201 `{url, expires}` |
| `POST /api/kydrive/copies` | `{fileId}` (from the picker) | 200 `{name, mimeType, size, dataBase64}` |
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
- `guestLinks`/`guestMaxDays` appear only when KyDrive's status carried
  `guest_links:true` with `guest_max_days` ≥ 1 (capped at 365). A guest
  `links` request is refused here, before KyDrive is asked: 409
  `guest_links_off` when the status does not offer them, 400 when `days` is
  not 1 to `guestMaxDays`, 403 `step_up_required` unless this session passed
  `POST /api/auth/step-up` in the last 10 minutes (`Session.SteppedUpAt`; a
  paired device has no session and never qualifies). Only then is
  `audience:"guest"` sent, and the answer must be a `/g/#…` link on the paired
  origin (an `/l/` answer, from a KyDrive that ignored `audience`, is 502).
  Organisation links never carry `audience`. KyDrive's `guest_links_off`
  passes through and drops the cached status. Each guest link is logged with
  the user and file IDs.
- `copies` signs `kypost.drive.fetch` with `max_bytes` set to the attachment
  limit (two-minute budget) and answers the file as a compose attachment.
  KyDrive's `too_large` is 413 `too_large`; an answer that is not
  `application/octet-stream` or is over the limit is 502. The name comes from
  KyDrive's `Content-Disposition` (header-sanitized, `validDriveName`, else
  `attachment`), the type from its extension.
- `recipients` resolves the addresses with `suitedir.Resolve` (KyIdentity
  `GET /api/suite/people?sub=&email=…`, 100 per call) under the session's
  subject and answers the ones that are not people in the organisation. 503
  `unavailable` when KyIdentity cannot answer.

The Content-Security-Policy's `connect-src` names the paired KyDrive origin
only while it is paired, so the browser can PUT to the ticket. The picker
needs no CSP source (a popup and `postMessage` are not CSP-governed), but
`Cross-Origin-Opener-Policy: same-origin` would sever its `window.opener`, so
while paired KyPost sends `same-origin-allow-popups` instead. Being opened by
another origin still severs; every link in a message (`<a>`, SVG `<a>`, image-map
`<area>`) opens with `noopener`, whatever `target` or `rel` the sender wrote.

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
- Guest links: when `guestLinks` is on and every KyDrive link in the body is
  a file uploaded in this window, the outside-recipient warning also offers
  **Send a guest link instead**, with an expiry choice (1, 7, 14, 30, 60, 90,
  180, 365 days up to `guestMaxDays`, and `guestMaxDays` itself; default 7)
  and a line saying anyone with the link can view and download the file
  until it expires. Choosing it makes one guest link per file at that moment,
  points each body link at it and replaces its note with `(size, KyDrive
  guest link, expires <date>)`, then sends. A `step_up_required` answer holds
  the send behind a "Confirm it is you" form (password and two-factor code,
  or KySignOn for a KySignOn session); confirming makes the links and sends.
  A compose reset during the step-up or between links makes no further link
  and changes nothing in the next draft. The recipients, subject and body
  are fixed when the user chooses guest links: if any of them changes before
  the links are made (during the step-up or while KyDrive answers), nothing
  is rewritten or sent and the user is told to check the message and send
  again; the send that follows uses the compose as it is then. Cancel (on
  the warning or the step-up form) stops a guest-link send in flight: a
  confirmation that succeeds afterwards makes no link and sends nothing. Links already made stay in KyDrive,
  where the owner can revoke them.
- Compose: **Attach from KyDrive** (a ky-ui menu) offers **Share a link**
  (`mode=link`; each link is inserted as above, and counts for the
  outside-recipient check) and **Attach a copy** (`mode=copy`; each file is
  fetched through `copies` and attached, a file that would take the
  attachments over the limit is refused naming it). The listener takes only a
  `kydrive.picked` message whose `origin` is the paired KyDrive and whose
  `source` is the popup; names lose control characters and must be 1 to 255
  characters, sizes are non-negative integers, links must be `/l/#…` on the
  paired origin, IDs printable ASCII ≤ 128; anything else is dropped, at most
  50 files. Closing the picker changes nothing. A compose reset while the
  picker or a fetch is open drops its result.
- Reading: each attachment has **Save to KyDrive**; success shows the folder
  and name it got and an **Open KyDrive** link to the app's suite URL.
