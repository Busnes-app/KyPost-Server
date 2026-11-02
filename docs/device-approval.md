# Device approval: client contract

Status: shipped in the server for 0.6.0 (#516). The Android, iOS/Mac and Linux apps
and the KyPost Demo build against this file. Change it whenever the server's
behaviour changes.

## Why

A `kypost://native-pair` pairing token is a bearer credential. If someone scans the
QR code with the system camera, opens the "Pair this device" link in a phone
browser, or leaks a screenshot, the link can reach any app that claims the
`kypost://` scheme. Before 0.6.0, whoever redeemed the token first got a device
credential with full mail access.

Since 0.6.0, a device that redeems a web-minted pairing token is created
**pending**. It gets no mail, contacts, push, App Pull or PGP delivery until the
signed-in user approves it on the web page that showed the code. There is no
server setting, env var or switch that skips approval.

## Which pairings need approval

| How the app got the token | Approval |
|---|---|
| QR code or deep link from Security → Devices (`POST /api/notifications/pairing`) | **Required** |
| KyIdentity sign-on (`POST /api/auth/native/signon`) | Not required |
| Review pairing (`POST /api/notifications/review-pairing`) | Not required |

Sign-on and review pairing return the token in the HTTP response to the app
that just proved the account credential: a device-bound KyIdentity ID token, or
the account's derived password credential. The token never travels as a link or
QR code. The server marks these tokens with a signed `direct` claim, which is the
only thing that registers a device at once. Clients must parse that response's
`deepLink` in-process. They must never hand it to the OS (`startActivity`,
`openURL`, `xdg-open`), because that would expose it the same way a scanned QR code is exposed.

A token minted before the upgrade has no `direct` claim, so it needs approval too.

## States

```
register ──202──▶ pending ──approve──▶ approved (device is paired)
                     │
                     ├──deny──────────▶ deleted at once
                     └──5 min─────────▶ deleted (expired)
```

- **pending**: held in server memory only. The `deviceId` and `deviceSecret` are
  valid for the status endpoint and nothing else. Every other device-authenticated
  route answers 401 `invalid device credentials`. A server restart deletes every
  pending device.
- **approved**: the device is written to the account and behaves like any paired
  device. It is not a sign-in approver until the owner opts in, as before.
- **deleted**: denied, expired, revoked by a password change or admin reset, or
  never existed. The status endpoint answers 410 in every one of these cases.

Times:

- The pairing token expires 90 s after it is minted. That is unchanged.
- A pending device expires **5 minutes** after it registers (`expiresAt`).
- An approved outcome stays readable through the pending entry until that same
  `expiresAt`. After that, the status endpoint answers from the paired-device list.

## Device side

### 1. Register

`POST /api/notifications/native/register`: the request is unchanged. One new optional
field:

| Field | Type | Notes |
|---|---|---|
| `osVersion` | string | Shown in the approval prompt, e.g. `"Android 16"`, `"iOS 19.1"`, `"Fedora 44"`. At most 256 bytes; longer values are cut. Display only. |

Send `deviceName` as the device model (`Build.MODEL`, the Mac or iPhone model, or the hostname).
The prompt shows it.

**202 Accepted: pending.** Every pairing from a QR code or web link ends here.

```json
{
  "ok": false,
  "synced": false,
  "status": "pending",
  "error": "approve this device on the web page that showed the pairing code",
  "deviceId": "3f1c…",
  "deviceSecret": "…",
  "approvalCode": "048213",
  "expiresAt": "2026-11-01T12:05:00Z",
  "statusEndpoint": "https://mail.example.com/api/notifications/native/pairing-status",
  "pollIntervalSeconds": 3
}
```

- Store `deviceId` and `deviceSecret` exactly as for a successful pairing, but mark
  the pairing as **not yet usable**. Do not start mail sync, App Pull or push
  handling.
- `deviceId` is the id you sent, or a server-minted UUID if you sent none.
- Show the user: "Approve this device on the web page that showed the code. Code: **048 213**".
  Show the `approvalCode` large. The web prompt shows the same code. That is
  how the user tells their own device apart from another app that redeemed the same link.
- Ignore `statusEndpoint` unless it is same-origin with the paired server. Fall back
  to `srv + /api/notifications/native/pairing-status`, using the same rule as `pullEndpoint`.

**200 OK: paired at once.** This happens only for sign-on and review-pairing
tokens. The body is the same as before 0.6.0, plus `"status": "approved"`:
`ok`, `synced`, `deviceId`, `deviceSecret`, `devices`, `deliveryMode`,
`pullEndpoint`, `transport`.

Errors (unchanged unless marked new):

| Code | Meaning | Client action |
|---|---|---|
| 400 | Malformed request, bad token or field | Show the message |
| 401 | Pairing token invalid or expired | Ask for a new code |
| 409 `pairing token already used` | **Someone else already redeemed this link** | Say so plainly: "This code was already used. If you did not use it, deny the request on the web page." Do not treat it as a transient error. |
| 409 `a pairing request for this device is already waiting for approval` (new) | Same `deviceId` already pending | Keep polling the existing request, or wait for it to expire |
| 409 `too many devices are waiting for approval…` (new) | 5 pending devices on the account | Ask the user to approve or deny them on the web |
| 409 other | Device id taken or device limit reached | Show the message |
| 503 | Pairing not configured | Show the message |

### 2. Poll for the outcome

`GET /api/notifications/native/pairing-status` with the headers

```
X-Kypost-Device-Id: <deviceId>
X-Kypost-Device-Secret: <deviceSecret>
```

Poll every `pollIntervalSeconds` (3 s), only while the pending screen is showing,
and stop at `expiresAt` or on any final answer. Responses:

| Code | Body | Meaning / action |
|---|---|---|
| 202 | `{"status":"pending","approvalCode","expiresAt","pollIntervalSeconds"}` | Keep polling |
| 200 | `{"ok":true,"synced":true,"status":"approved","deviceId","deliveryMode","pullEndpoint","transport"}` | Paired. **Use the `deviceId` from this response.** It differs from the pending id when the server merged the registration into an existing row with the same push token. Then start sync exactly as after a pre-0.6.0 200 from register. |
| 410 | `{"status":"rejected","error":"this pairing was denied or has expired; scan a new pairing code"}` | Final. Delete the stored id and secret and return to the pairing screen with that message. |
| 429 / 503 | `Retry-After` header | Back off as on every device route |

Denied, expired and unknown credentials are deliberately the same 410. After
`expiresAt`, an approved device still gets 200 from this endpoint, because it falls back to the
paired-device list. A device that was offline during approval can therefore
still find out it was approved.

A client may also skip polling and simply retry its first real device call. A
pending device gets 401 on every other route, and an approved one succeeds.
Polling is the supported way to show progress.

### Old clients against a 0.6.0 server

A client that predates this contract treats the 202 as a failed registration
(`ok:false`, `synced:false`, not HTTP 200). Android shows "Failed to register
device (202)". It never gets access without approval. If an old client keeps the
secret anyway, that secret works only after the user approves the device.

## Web side (session API)

These are the endpoints the KyPost web page uses, and the KyPost Demo helper uses
them the same way. All of them need a signed-in session: the `kypost_session` cookie from
`POST /api/auth/login`, and for POST the `X-CSRF-Token` header from
`GET /api/auth/csrf`. They act only on the signed-in user's own pending devices.
No step-up is needed, because minting the pairing code already required one.

### List

`GET /api/notifications/native/pending` → 200

```json
{
  "pending": [
    {
      "deviceId": "3f1c…",
      "deviceName": "Pixel 9",
      "platform": "android",
      "osVersion": "Android 16",
      "appVersion": "KyPost for Android v0.6.0",
      "userAgent": "okhttp/5.1.0",
      "requestIp": "203.0.113.7",
      "approvalCode": "048213",
      "requestedAt": "2026-11-01T12:00:00Z",
      "expiresAt": "2026-11-01T12:05:00Z"
    }
  ]
}
```

Only devices still awaiting a decision appear here; approved and expired ones do not.
`requestIp` is the client address as the server resolves it, honouring
`TRUSTED_PROXY_CIDRS`. Every string field is client-supplied except `requestIp`, `approvalCode`
and the timestamps, so render them as text, never as HTML.

### Approve

`POST /api/notifications/native/pending/{deviceId}/approve` (empty body) → 200
`{"ok":true,"deviceId":"<id the device is stored under>","devices":<count>}`

| Code | Meaning |
|---|---|
| 404 | No such pending device for this user: unknown, expired, already decided, or **another user's**. All of these give the same answer. |
| 409 | Device id taken by another account, or the account is at the paired-device limit. The pending entry is deleted. |
| 410 | The account's credentials were revoked, or it was deactivated, after the device registered. The pending entry is deleted. |
| 401 / 403 | No session, or a missing or wrong CSRF token |

### Deny

`POST /api/notifications/native/pending/{deviceId}/deny` (empty body) → 200 `{"ok":true}`.
The pending device is deleted at once, and its next status poll gets 410. 404 as for approve.

### Web UI

While a pairing code is open, and for 5 minutes after one was shown, Security → Devices →
"Pair a new device" polls the list every 3 s. It shows each request as
"*deviceName* wants to pair", with OS, app version, request IP and approval code,
and **Approve** / **Deny** buttons.

## KyPost Demo helper

The demo's helper signs in as a demo persona with `POST /api/auth/login`, fetches
`GET /api/auth/csrf`, then calls list and approve as above. The KyPost server has
no demo-specific code, and its approval cannot be turned off.
