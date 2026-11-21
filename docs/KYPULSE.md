# KyPulse alert mail through KyPost

Server-to-server contract between KyPulse and KyPost: KyPulse asks KyPost to send one
plain-text alert from the mailbox of a member who allowed it, to recipients that member
listed. This file is byte-identical in KyPost (`docs/KYPULSE.md`) and KyPulse
(`docs/KYPOST.md`); change both sides together.

KyPost is the resource owner: every rule about who may receive mail, how much and from
which mailbox lives on the KyPost side. KyPulse only names a member and the recipients.

## Pairing

KyIdentity pairs `kypost` and `kypulse` (KyIdentity `docs/SUITE-DIRECTORY.md`). Each side
asks `GET {issuer}/api/suite/peer?client=<other>` with its own SSO client credentials
(`ky-primitives/suitedir`) and holds the 32-byte secret in memory only, never written or
logged. Each asks at startup, every 15 minutes, within 15 seconds of an SSO settings change,
and at most once a minute after a bad signature; a failure leaves the pair off and is
retried after 30 seconds, doubling to 15 minutes. A KyIdentity outage keeps a pairing the
same SSO settings produced; `no_peer`, refused client credentials, a bad answer or changed
settings unpair at once.

There is no environment pairing: the member is named by KyIdentity subject and issuer, so
the channel needs KyIdentity sign-in on both sides. KyPulse calls KyPost; KyPost never calls
KyPulse.

KyPulse's outbound call is HTTPS only, never follows a redirect, and never reaches
loopback, unspecified, link-local, multicast or metadata addresses (`internal/egress`); a
private LAN address is allowed, as for every other KyPulse egress.

## Signing

| Event type | Direction | Route |
|---|---|---|
| `kypulse.alert.send` | KyPulse → KyPost | `POST /api/suite/kypulse/alert` |

Signed with `ky-primitives/syncauth` v1 under the pair secret (`Headers.Apply` /
`FromRequest`). KyPost checks the event type equals the route's, then `syncauth.Verify`
with a replay guard (`DefaultWindow`, 100000 IDs). A verified replay is answered 200
`{"outcome":"duplicate"}` without sending again; any other failure is 401
`{"error":"Bad signature","code":"bad_signature"}`. The event ID is stable across
KyPulse's retries of one alert: `kypulse-alert-<KyPulse event id>`, or
`kypulse-test-<random>` for a test. KyPost forgets an event ID when it refused or failed
without storing a message, so a retry of that ID is processed again; it keeps the ID once a
message is stored for sending.

## Send: KyPulse → KyPost

```json
{"sub":"…","issuer":"https://id.example","recipients":["ops@example.com"],
 "subject":"KyPost is down","text":"KyPulse: KyPost is down (refused) at 2026-11-17T09:00:00Z\nhttps://pulse.example/#/apps/tgt_1"}
```

The body is exactly these five fields; any other field (`html`, `attachments`,
`reply_to`, `headers`, `from`, …) is `bad_body`. The whole request is at most 16 KiB.

| Field | Rule |
|---|---|
| `sub`, `issuer` | The mailbox owner's KyIdentity subject and issuer. |
| `recipients` | 1 to 11 bare addresses, no duplicates (case-insensitive). |
| `subject` | 1 to 150 bytes of UTF-8, no control characters. |
| `text` | 1 to 8 KiB of UTF-8, no control characters other than `\n`. |

Checks run in this order. Every refusal is `{"error","code"}`; codes are `[a-z0-9_]`, at
most 64 characters, and KyPulse shows the code.

| Status | Code | When |
|---|---|---|
| 404 | `kypulse_off` | KyPost is not paired with KyPulse. |
| 401 | `bad_signature` | Wrong event type or signature. |
| 200 | `duplicate` (as `outcome`) | Verified replay; nothing sent. |
| 400 | `bad_body` | Malformed JSON, an unknown or missing field, or a field outside its rule. |
| 404 | `no_user` | No active account for `(issuer, sub)`, its SSO link is revoked, or a dedicated administrator identity. |
| 409 | `alerts_off` | The owner has not turned on "Let KyPulse send alerts from my mailbox". |
| 409 | `recipient_not_allowed` | A recipient is not on the owner's list. |
| 409 | `recipient_unconfirmed` | A recipient is on the list but has not confirmed. |
| 409 | `no_mailbox` | No way to send from the owner's mailbox (no outgoing server, native mail off). |
| 429 | `rate_limited` | Over the per-mailbox limit (default 30 alerts per hour); `Retry-After` set. |
| 503 | `unavailable` | Storage unreadable; retry. |
| 503 | `smtp_failed` | The owner's SMTP server did not accept; retry. |
| 202 | `sent` (as `outcome`) | Stored for sending; a copy goes to Sent. |

A refused recipient refuses the whole alert: KyPost sends to all listed recipients or none.

## The owner's switch and list (KyPost)

KyPost Settings → Mail → KyPulse: "Let KyPulse send alerts from my mailbox", off by
default, stored with the owner's other settings. While on, it holds the allowed recipients:

- the owner's own addresses (account address, verified send-as aliases and active
  addresses of their KyPost-hosted mailboxes), always allowed and never confirmed;
- up to 10 more addresses. Adding one mails it a confirmation link from the owner's
  mailbox (the same path and template rules as an alert, subject `[KyPulse] Confirm alert
  mail`). The address receives no alert until someone opens the link and presses Confirm:
  opening the link only shows the page, and the POST behind the button confirms, so a mail
  scanner fetching the link confirms nothing. A link expires after 7 days and stops working
  once used; resending replaces it, and removing the address revokes it.

Turning the switch off keeps the list but refuses every alert with `alerts_off`.

## The message (KyPost)

A fixed template, nothing from KyPulse reaches a header except the subject text:

- From: the owner's account address (a KyPost-hosted mailbox: its primary address).
- To: the recipients.
- Subject: `[KyPulse] ` + `subject`.
- `Auto-Submitted: auto-generated`, `X-Ky-Source: kypulse`.
- One `text/plain; charset=UTF-8` part: `text`. No HTML part, no attachment, no Reply-To.

It goes through the owner's normal outbox (SMTP account or the native outbox), appears in
Sent, and is charged to the owner's sending quota like any send. The per-mailbox rate limit
is separate and counts alerts KyPost accepted for sending; confirmation mails count toward
the owner's sending quota only.

## KyPulse side

Settings → Server → Email through KyPost (administrators): the mailbox owner, chosen from
KyPulse accounts linked to KyIdentity, and 1 to 11 recipients. Hidden, with a note saying
why, while KyPulse does not sign in through KyIdentity or is not paired. Every alert the
webhook gets is also sent here; the last outcome (`sent` or the refusal code) is shown
beside the channel, never an address or the message. Network errors, 429 and 5xx are
retried over the webhook's backoff; any other refusal is final.

**Same host.** If KyPost runs on the same host as the app that failed, it may fail with
it, and the alert never leaves. The webhook to an off-host receiver stays the recommended
path; this channel is a second one.

## Audit and logging

Both sides audit success and refusal with IDs and codes only. Neither logs or audits an
address, a subject, a message body, a name, a token or the secret.

## Multi-tenant mode

Only when `KYPOST_TENANCY=multi`, which startup still refuses (tenancy design, build plan PR 3).
The alert body must carry the top-level `org_id` of the account its `(sub, issuer)` names; a
missing, malformed or other org answers `404 no_user`. KyPulse has no tenancy spec and sends
none yet, so a multi install refuses every alert. A single-tenant install still refuses
`org_id` as an unknown field (`400 bad_body`). Test `TestTenancyInboundPeersNeedTheAccountOrg`.
