# Native direct outbound delivery

Native mail may deliver straight to recipient MX servers using KyPost's existing
encrypted outbox and SMTP sender. Select it explicitly through the protected admin
API. The default remains the operator-owned relay; no deployment is changed on
upgrade and external IMAP accounts keep their existing SMTP configuration.
The native receiving stack is configured separately in [receiving setup](RECEIVING_SETUP.md).

## Configure

Pair KyIdentity, enable native mail and prove the sending domains first. Through
`PUT /api/admin/mail-relay`, submit an administrator-confirmed request:

```json
{"mode":"direct","host":"mail.sender.example","domains":["sender.example"],"authSecret":"<administrator derived credential>"}
```

`host` is the operator's public SMTP EHLO name. Use a bare lowercase ASCII dotted
DNS name, not an IP, URL or port. Omit `port`, `smtpUsername` and `smtpPassword`;
combining direct mode with relay credentials or a port is refused. The domains
have the same fresh issuer-bound proof and ownership gates as relay setup.
CSRF and action-bound KyIdentity step-up remain mandatory where applicable.
No endpoint is added and configuring a profile sends no mail.

GET returns `mode:"direct"`, `transport:"mx-starttls"`, `authRequired:false`,
`host`, `port:0` and its generation. The existing Mail domain browser form
configures relay profiles; direct mode is currently configured through this API.
The saved-relay login check explicitly refuses a direct profile: there is no
relay login to test, and configuration alone is not delivery readiness.
To return to a relay, submit the complete normal host/port/credential profile
with mode omitted (or empty). Either switch rotates the generation; unclaimed
old jobs are quarantined rather than silently adopting another transport.
Keep the key and queued evidence. Domain-set-only edits keep the generation.

## Destination and TLS policy

The sender resolves recipient MX records, tries lower preference numbers first,
and uses the recipient domain as implicit MX when no MX exists. A sole null MX
(`0 .`) permanently refuses delivery. Transient DNS, connection or TLS failures
use the existing bounded definite-refusal retry. Address/MX fallback happens
only before the envelope starts. No second endpoint is tried after DATA.

Every MX is resolved once and its checked IP is dialled directly on port 25.
Any private/reserved answer rejects that MX, including mixed public/private
answers. The account sandbox allowlist cannot relax direct-delivery egress.
There are at most 20 MXs and 16 addresses per MX, under one 45-second attempt
budget; connection/greeting/TLS setup is limited to five seconds per address.
Untrusted server/TLS input is capped at 1 MiB per connection, independently of
the deadline.

STARTTLS, a valid trusted certificate for the MX hostname and TLS 1.2+ are
required. Plaintext fallback is absent and `ALLOW_INSECURE_SMTP` does not weaken
this mode. DNS selection is not authenticated by DNSSEC, DANE or MTA-STS here;
those policies are not implemented. A verified MX certificate alone does not
prove the recipient domain authorized that MX. Qualify DNS and TLS policy for
the intended deployment before public mail use.

## Recipient outcomes and API semantics

Each recipient has a separate durable delivery claim. A refused, uncertain or
crashed attempt for one recipient cannot prevent unrelated recipients from
progressing, and accepted recipients are not automatically sent again. Exact
wire MIME, Date, Message-ID and PGP bytes are retained across retries; Bcc stays
in the envelope/private Sent copy and never enters the wire headers.

The existing `ok:true` send contract still means primary SMTP acceptance,
not merely queueing or inbox placement. Direct sends add
`deliveryMode:"per-recipient"`, `deliveriesConfirmed` and `deliveriesTotal`.
A primary failure returns HTTP 503 with `ok:false`, an outbox ID and a warning
that other recipients may already have accepted. A primary success can have a
warning for other pending/failed/uncertain recipients. Inspect owner-scoped
`GET /api/mail/outbox/{id}` before resubmitting: delivery sequences follow the
original delivery-group order, then each group's recipient order. A new send
request is a new intent, not a retry of only failed recipients.

A lost final DATA acknowledgment remains `uncertain`, never a definite failure
or automatic retry. Definite 4xx uses the existing six-attempt backoff; definite
5xx or retry exhaustion is terminal `failed`. Existing claim expiry/authority
rules and restore quarantine apply. Failure reporting is owner-scoped status
and the send response; this mode generates no SMTP bounces or backscatter.
There is no operator uncertain-retry/reconciliation API yet.

Sent filing occurs once after any confirmed acceptance; a Sent failure never
resubmits SMTP. A partial send's Sent copy retains the original complete intended
recipient set, while outbox states distinguish acceptance. Completed jobs retain
compact encrypted receipts under the existing rules. New independent-job markers
are committed in `outbox_independent` in the same transaction as encrypted intent.
Legacy queued jobs keep their primary-before-follow-on behavior.

Fan-out obeys existing 100-recipient/delivery, 64 MiB total wire-plus-Sent and
mailbox quotas. Each direct recipient currently counts a complete wire copy
against those bounds. Large messages with several recipients can therefore be
refused before SMTP; no limit is raised or quota bypassed. Per-recipient claims
also add SMTP connections and persistent records. Measure those costs under
realistic mail sizes before making capacity claims.

## Acceptance and operations

Before public activation, qualify public DNS/PTR, reachable outbound port 25,
SPF/DKIM/DMARC and recipient receipt/inbox placement. This mode does not add DKIM
signing or a DNS installer. Preserve native ownership/revocation, restore holds,
existing backups, sending quotas and inbound/background processing. Include
receiver, scanner, inference and storage budgets in workforce tests.

Tests use controlled recipients and injected package-test DNS/dial seams; no
production egress exception exists. Public Internet delivery, physical clients,
whole-day workforce load and 3,000/10,000-person support are separate gates.
See [native outbox](NATIVE_OUTBOX.md) for persistence/recovery and
[relay settings](DOMAIN_RELAY.md) for protected configuration and rollback.
