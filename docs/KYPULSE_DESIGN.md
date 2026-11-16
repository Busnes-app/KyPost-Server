# KyPulse alert mail: KyPost side

Date: 2026-11-15. Owner-approved feature (2026-11-15); design by worker f-pulse.
Wire contract: `docs/KYPULSE.md` (byte-identical with KyPulse `docs/KYPOST.md`). The
KyPulse side's design is KyPulse `docs/superpowers/specs/2026-11-15-kypost-email-alerts-design.md`.

## Threat model

- **KyPost must not become a relay for KyPulse.** KyPost owns every rule: the member's
  switch (off by default), the recipient list, link confirmation for extra recipients, a
  per-mailbox limit of 30 alerts an hour, and a fixed plain-text template. A compromised
  KyPulse can at most mail a confirmed recipient of a member who opted in, under a
  `[KyPulse]` subject, at that rate.
- **Forged or replayed requests.** `syncauth` v1 under the KyIdentity pair secret, event
  type checked, replay guard; the secret is held in memory only. A durable claim per event
  ID in the shared send ledger stops a replay that another pod or a restart would not
  remember from sending twice.
- **Confirmation links.** The owner's Sent copy withholds the link, so the owner cannot confirm for the recipient.
  Limit: a member who sends through their own external SMTP server controls that relay and
  could read the link there. KyPost has no sender independent of the member's mailbox (system
  sending is not built), and such a member can already mail that address from the same account
  at the same quota, so self-confirming grants nothing new beyond the `[KyPulse]` template. Opening a link only shows a page; the POST behind its button
  confirms, so mail scanners confirm nothing. Links expire after 7 days, die once used, and
  are revoked when the address is removed.
- **Header injection.** Only the subject text crosses into a header, after the fixed
  prefix and a control-character check. No HTML, attachments, Reply-To or other headers are
  accepted; an unknown body field is `bad_body`.
- **Identity.** The member is resolved by KyIdentity `(issuer, sub)`; revoked SSO links,
  inactive accounts and dedicated administrator identities are `no_user`.
- **Privacy.** Logs and audits carry IDs and codes only, never an address, subject or body.

## Data

| Where | What |
|---|---|
| Per-user settings (`kyPulse`) | switch; up to 10 extra recipients with confirmed flag, live link nonce, sent time |
| Global store send admission | per-mailbox alert count (`kypulse-rate:`) and a one-day claim per event ID (`kypulse-claim:`), shared across pods |
| Normal outbox and Sent | each alert, like any send, charged to the sending quota |

## Routes

| Method | Path | Auth |
|---|---|---|
| POST | `/api/suite/kypulse/alert` | syncauth signature under the kypost–kypulse pair |
| GET, PUT | `/api/settings/kypulse` | signed-in member (own settings) |
| POST, DELETE | `/api/settings/kypulse/recipients` | signed-in member |
| POST | `/api/settings/kypulse/recipients/resend` | signed-in member |
| GET, POST | `/kypulse/confirm` | the link token |

## Defaults and choices

- Off by default per member; pairing only through KyIdentity, no environment pairing.
- The member's own addresses are always allowed and never need confirmation.
- All-or-nothing recipients: one refused recipient refuses the alert.
- No new store: settings live with the member's other settings and the rate counter in the
  global store, so the multi-pod work carries them without Postgres-specific code.

## Not built

- No HTML, attachments, Reply-To, per-alert sender choice or KyPost→KyPulse callbacks.
- No administrator-wide switch: each member decides for their own mailbox.
