# KyMessages chat

Server-to-server contract between KyPost and the KyMessages console, section H
of the suite integrations wave 2 (`ky-redesign/integrations/suite-integrations-wave2.md`,
copied here on 2026-11-14; this file is now the durable copy). A "Chat" action
beside a message's sender and on a contact opens a direct chat with that
person in KyMessages' Element. Change both sides together.

## Deviation from the wave 2 text

The contract says the answer's URL must be on "the paired KyMessages origin's
Element host", and the Chat action needs "KyMessages in the user's suite app
list". The pairing only gives KyPost the console's URL, and Element runs on its
own host. So KyPost reads both from the user's suite app list, entry
`kymessage-chat`, the one KyQuickStart and KyIdentity list for everyday
users. **Chat shows only when that entry is listed with an `https://` URL. The
console's `url` must be on that entry's origin.** The console-only entry
`kymessage` (shown to administrators) does not turn Chat on.

## Pairing

Pair `kymessage`↔`kypost`, exactly as KyCalendar's
([KYCALENDAR.md](KYCALENDAR.md#pairing)): same URL rules, refresh, backoff,
outage and unpair rules; secret in memory only. Implemented by
`backend/internal/suitepair` (KyIdentity contract G adds the pair).

| Variable | Meaning |
|---|---|
| `KYPOST_KYMESSAGE_URL` | The KyMessages console's `https://host[:port]`, no path, query or credentials. |
| `KYPOST_KYMESSAGE_SECRET_FILE` | File holding the shared secret, at least 32 bytes after trimming. |
| `KYPOST_KYMESSAGE_ALLOW_PRIVATE` | `true` lets KyPost reach the console at a private address. Default `false`. |

Both unset: KyPost asks `GET {issuer}/api/suite/peer?client=kymessage` with
its own SSO client credentials. The API process pairs; the daemon does not
use KyMessages. `GET /api/admin/kymessage` (administrators) reports
`{source, connected, url, reason}`, never the secret.

## Signing

| Event type | Route on the KyMessages console |
|---|---|
| `kypost.chat.open` | `POST /api/suite/chat` |

Signed with `ky-primitives/syncauth` v1 under the pair secret, with a fresh
event ID (`kypost-chat-<uuid>`; KyPost does not retry, the user does). The
console accepts `kypost.chat.open` only under KyPost's pair secret
(`kycalendar.chat.open` is KyCalendar's, under its own). It checks the event
type, then verifies with a replay guard: a verified replay is 200
`{"outcome":"duplicate"}`, anything else 401 `{"error","code":"bad_signature"}`,
which makes KyPost re-resolve the pairing (at most once a minute).

```json
{"sub":"<caller>","issuer":"https://id.example","target_sub":"<person to chat with>"}
```

200 `{"url":"https://chat.example/#/user/@bob:example"}`: Element's user view,
which offers "Message" and reuses an existing direct chat. The console creates
nothing.

| Status | Code | When |
|---|---|---|
| 404 | `kypost_off` | Not paired. |
| 404 | `no_user` | Caller has no active KyMessages account. |
| 404 | `no_target` | Target has no active KyMessages account, is the caller, or is an administrator identity. |
| 400 | `bad_body` | |
| 429 | `rate_limited` | |

## KyPost's routes

Session (or paired device) routes, CSRF as for mail routes, per-user rate
limit shared by both (burst 30, one a second; 429 `rate_limited` with
`Retry-After`), `Cache-Control: no-store`. Refusals are `{"error","code"}`.
Dedicated administrator identities get the administrator 403. 409
`kymessage_off` means: hide Chat. It answers when KyMessages is unpaired, the
account has no live SSO link to the configured issuer, the account is
inactive, or `kymessage-chat` is not in the user's suite app list.

| Route | Body | Answer |
|---|---|---|
| `POST /api/kymessage/people` | `{addresses}` (1 to 100; any that is not a bare address is skipped) | 200 `{inside}` |
| `POST /api/kymessage/chat` | `{address}` (one bare address) | 200 `{url}` |
| `POST /api/kymessage/status` | `{addresses}` (1 to 50; non-bare ones skipped; more is 400 `too_many`) | 200 `{people:[{address,state,until?}]}` |

- `people` resolves the addresses with `suitedir.Resolve` under the session's
  subject and answers, lowercased, the ones that are people in the
  organisation other than the caller. 503 `unavailable` when KyIdentity
  cannot answer.
- `chat` resolves the address the same way; the target subject is never taken
  from the client. Outside the organisation, or the caller: 404 `no_target`
  without asking the console. Then it posts `kypost.chat.open` and maps the
  answer:

| Console answer | Client sees |
|---|---|
| 200 with a `url` passing the check below | 200 `{url}` |
| 404 `no_target` | 404 `no_target`, "This person isn't on KyMessages yet." |
| 404 `no_user` | 404 `no_user` |
| 429 `rate_limited` | 429 `rate_limited`, `Retry-After: 30` |
| anything else, a bad signature, or unreachable | 502 `kymessage_unavailable` |

- The `url` must be at most 2048 bytes with no spaces or controls, `https`,
  no credentials, query or path beyond `/`, on the `kymessage-chat` origin,
  with a fragment `/user/@…`. Anything else is 502 and never reaches the
  browser.
- Logs carry user and event IDs and status codes only: no address, subject,
  name or URL.
- Chat says who an address belongs to in the organisation, not who sent the
  message: a forged From still opens a chat with the address's real owner.

## Status

Colleagues' coarse status from KyMessages (owner request 2026-11-15; KyMessages
`docs/PRESENCE.md` owns the contract). `status` resolves the addresses with
`suitedir.Resolve` like `people`, then posts `kypost.presence.lookup` to
`POST /api/suite/presence/lookup` with `{"sub","issuer","subs"}`: subjects only, never an
address. Answers are cached per caller and subject for 60 seconds (`statusCache`): KyMessages decides
per caller, so one caller's answer never serves another. States are `available`, `busy`,
`in_a_meeting`, `away`, `out_of_office` and `unknown` (anything else reads unknown); `until`
comes only with `in_a_meeting` and `out_of_office`. Only organisation people are answered.
KyMessages unreachable or refusing: 502 `kymessage_unavailable`. The recipient autocomplete
shows a dot and the words ("In a meeting until 15:00") under each organisation suggestion;
colour never carries the meaning alone.

**Out-of-office.** Every save of the out-of-office reply (the web, an agent through MCP)
also posts `kypost.presence.ooo` to KyMessages' `POST /api/suite/presence/out-of-office`
(`syncOOOMessages`, `api/kymessage_ooo.go`) while KyMessages is paired and the account is
active with a live SSO link: `{"sub","issuer","revision","away":true,"start","end"}` (RFC 3339
UTC) while the reply is on with both dates, else `{"sub","issuer","revision","away":false}`.
Never the reply text. The user's "Show my status to colleagues" switch lives in KyMessages,
which drops the state when it is off. Best effort: a failure is logged (user ID and result
only) and the next save sends it again; it never blocks the save.

## Web client

- `api/kymessage.ts` `chatOrigin(suiteApps)` is the `https:` origin of the
  `kymessage-chat` entry, else `""`, which hides Chat with no request.
- `components/ChatButton.tsx` renders **Chat** only after `people` placed the
  address in the organisation. Lookups made in one tick share one request (100
  addresses each); an answer is kept five minutes, a failure is not kept.
- Shown beside the reader's Sender, and in a contact's row menu (its first
  address).
- A click opens a blank tab at once (no popup blocker), cuts its `opener`,
  calls `chat`, checks the URL is `https` on the Element origin again, and
  points the tab at it. A refusal closes the tab and shows the message beside
  the button (contacts: in the page status).
