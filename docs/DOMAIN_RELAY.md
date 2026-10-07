# Operator-owned domain relay configuration

Native primary-address sending uses the configured operator-owned relay in
explicit native mode. Saving credentials makes no provider connection;
`sendingEnabled` reports configured native runtime capability, not DNS/provider
readiness or recipient delivery. Busnes supplies no relay account or service.
Server → Mail domain guides domain proof and relay configuration; provider
delivery qualification and full deployment setup remain pending. A protected saved-profile check verifies TLS and SMTP authentication without sending mail. See the
[outbox runtime contract](NATIVE_OUTBOX.md) for supported sends and remaining gates.

## Configure through the admin API

For browser setup, open **Admin → Server → Mail domain** after pairing
KyIdentity under SSO. Create the challenge, publish the exact TXT name/value,
then verify. Domain and issuer stay fixed; replacing the challenge requires
confirmation and clears verification until DNS is updated. Enter your provider's
implicit-TLS host, port and SMTP credentials separately from your admin account
password. KySignOn sessions confirm each change there. The screen clears account
and relay credentials after each action and never reads relay secrets back.
Unreadable/mismatched status or an uncertain write disables changes until reload;
reload status before considering another write.

Saving a relay makes no provider connection and sends no test message. Updating
an existing profile rotates its generation, invalidating unclaimed deliveries
queued under the old profile; the screen warns and asks for confirmation.
Inspect owner-scoped outbox status and provider evidence before resubmission.
There is no supported uncertainty reconciliation/retry button yet.

### Check the saved relay without sending mail

After saving, choose **Check saved relay** and confirm the action. The check
contacts the displayed saved endpoint using its stored credential; unsaved form
edits are ignored. The provider can record this login attempt. No MAIL, RCPT or
DATA commands are sent, and no outbox or Sent record is created.

`POST /api/admin/mail-relay/test` accepts `{expectedGeneration,password}` (or
`authSecret`; KySignOn uses exact-request step-up). Use the generation returned
by the relay GET. Success returns `{generation,host,port,tls:true,
authenticated:true,deliveryTested:false}` with `Cache-Control:no-store`.
Missing/invalid generation returns 400; changed/missing profile, failed DNS,
changed issuer/challenge or restore hold returns 409. TLS/AUTH/QUIT failure
returns 503 without provider response text. Checks are limited to one per
API process per 30 seconds (the cooldown resets on restart) (429 with Retry-After). Each network check has one
15-second deadline; the authority/check phase after account confirmation is bounded to 30 seconds.

Fresh issuer-bound DNS and current saved-profile/restore checks run before the
connection and again after success. Configuration locks are released during
network I/O. A rotation during the check refuses a positive result. Authority may change after a successful response. Results
are transient evidence for that generation, not persisted readiness. Success
does not prove authorized From addresses, provider production access, rate
limits, recipient acceptance, DKIM/SPF/DMARC, inbox placement or receiving.
The check is available before native mode activation so configuration can be
qualified first. It grants no account or sending authority.

### Controlled domain test

1. Use a dedicated test domain and operator-owned relay account. Preserve prior
   DNS/settings and keep production MX unchanged. Configure the current issuer
   and signed directory integration, then complete TXT proof in the screen.
2. Enable `KYPOST_NATIVE_MAIL=true` explicitly for the test deployment. Assign a
   new active identity with an explicit primary address in the proved domain.
   Existing IMAP accounts cannot be adopted; confirm new native mailbox access.
3. Configure the provider's sender/domain verification and DNS records, then
   save its relay profile and run Check saved relay. For AWS SES use the region's SMTP host and **465**
   with SES SMTP credentials, not AWS access keys. For Cloudflare use
   `smtp.mx.cloudflare.net:465`, username `api_token` and an Email Sending: Edit
   token for an onboarded sending domain. Confirm access and restrictions in
   [SES SMTP setup](https://docs.aws.amazon.com/ses/latest/dg/send-email-smtp.html)
   and [Cloudflare SMTP setup](https://developers.cloudflare.com/email-service/api/send-emails/smtp/).
   Native sending supports implicit TLS only. Keep provider credentials private.
4. Configure sealed backups under Server → Backup and run a restore drill.
   Review [native restore limits](RESTORE.md#offline-restore-and-native-quarantine):
   restored native mail remains held, with no supported hold release yet.
5. Sign in as the new identity and compose a small message to a recipient you
   control. Record its outbox ID, authenticated owner-scoped
   `GET /api/mail/outbox/{id}` result, Sent copy, provider logs and recipient
   receipt including authentication headers/inbox placement. Repeat with PGP
   and a controlled Bcc recipient. A successful submit proves acceptance only.
   An uncertain result requires evidence, never a blind duplicate send.
6. For direct receiving without IMAP, enable `KYPOST_NATIVE_RECEIVING=true` only
   in the controlled qualification deployment and follow
   [receiving gateway qualification](RECEIVING_GATEWAY_ASSESSMENT.md). The admin
   screen does not install/expose a public receiver, set MX, or certify TLS,
   capacity, abuse protection or accepted-mail recovery. Those remain separate
   activation gates before a public domain cutover.

Rollback the relay check by reverting its commit; it changes no persistent schema. The saved relay and existing delivery path remain available.
Do not delete domain/relay files or switch a native account to IMAP to roll back
mail storage. Preserve accepted mail, queued jobs and matching relay keys.

Pair KyIdentity and establish the mail-domain TXT proof first; see
[NATIVE_PROVISIONING.md](NATIVE_PROVISIONING.md). Use an authenticated admin
session, CSRF protection and current account confirmation. KySignOn requires
request-bound step-up. `PUT /api/admin/mail-relay` accepts:

```json
{
  "host": "smtp.provider.example",
  "port": 465,
  "smtpUsername": "operator-relay-login",
  "smtpPassword": "operator-relay-secret",
  "password": "current-admin-account-password"
}
```

The values above are placeholders. Use `authSecret` instead of `password` for
accounts using derived authentication. These confirmation fields authenticate
the admin; `smtpUsername` and `smtpPassword` authenticate the relay. Supply
credentials privately through the protected API, never in chat or shell argv.
Each update supplies the complete relay profile and rotates its generation.

### Relay domain set

The relay sends for a set of domains. An optional `"domains": ["a.example",
"b.example"]` field selects it; omitted, an update keeps the saved set, and a
first profile uses the founding domain, so the single-domain request above is a
one-domain set. Every domain in the resulting set must be configured, not retired
and freshly proven by DNS on each update; one lapsed domain therefore blocks
relay updates until it is proven or removed. A body carrying only `domains`
(plus the confirmation field) changes the set alone: the saved credentials stay
and the `generation` is kept, so jobs queued on retained domains stay valid.
Removing a domain is refused (409) while a `queued` or `retryable` outbox job
sends from it (`submitting` and `uncertain` jobs are never reclaimed and do not
block); a removed domain moves to `retiredDomains`. A job whose domain was removed
afterwards is never submitted. `From` must be on a domain in `domains`;
backup validation checks historical jobs against `domains ∪ retiredDomains`,
and both lists against the domain set's configured and retired domains.

The host is a lowercase ASCII dotted DNS name or IPv4 literal; URLs, IPv6
literals and hosts with an appended port are refused. Port zero or omission selects 465. Other valid ports still use
implicit TLS; STARTTLS and plaintext are unsupported by this native profile.
TLS verifies the host against the system trust store and requires TLS 1.2 or
newer. SMTP AUTH is mandatory. `ALLOW_INSECURE_SMTP` cannot weaken this profile.
The provider login is independent of mailbox From addresses; its credentials
alone never authorize sending as a user.

Each update rechecks the current TXT record and issuer/domain claim before
writing under the domain fence. Changed authority, unavailable DNS or a restore
hold refuses the update. It cannot adopt another domain or issuer's relay file.
`GET /api/admin/mail-relay` returns configuration status, `domain` (the founding
domain when the relay sends for it), `domains`, `retiredDomains`, issuer, host,
port and generation with `Cache-Control:no-store`. Both methods omit the relay
username and password; unreadable existing ciphertext or key produces an
explicit error rather than an unconfigured fallback.

## Storage, recovery and activation

`$CONFIG_DIR/native-relay.json` is encrypted with the dedicated
`$SECRET_DIR/native-relay.key`; both are created owner-only. Its sealed JSON is
version 2, holding a sorted domain set (`domains`, at least one, and
`retiredDomains`, disjoint from it). Startup migration reseals a version-1 file
with the same key and the same `generation`, so queued and retrying deliveries stay
valid; the runtime refuses version 1, and backups accept both. See
[storage format migration](NATIVE_PROVISIONING.md#storage-format-migration). There are no relay
path overrides or environment credentials. The dedicated key lets KyPost protect
one domain-wide provider credential without borrowing per-user IMAP keys. The
admin routes are needed to configure that credential without per-account setup;
they make only the existing domain-verification DNS request. Native sending
reaches the explicitly configured SMTP endpoint. A host or backup compromise
that exposes both ciphertext and key exposes the domain-wide relay credential;
this is server-held encryption, not end-to-end secret custody. Preserve the matching key: updates
refuse to regenerate a missing key for existing ciphertext. Restoring or
rotating provider credentials requires deliberate recovery, not deleting files
to silence an error.

Sealed backups include both files, decrypt the collected relay bytes and check
their historical domain-set/issuer binding. Drills enforce the additive version-1
recipe field `relay:domain-credentials-and-authority`. A relay-only restore also
persists the native restore hold. Old backups without relay configuration remain
compatible. Historical backup evidence grants no current sending authority;
[restore reconciliation](RESTORE.md#offline-restore-and-native-quarantine)
remains required, with no supported hold release yet.

The [internal outbox](NATIVE_OUTBOX.md) now stores encrypted intent with keys
derived from the retained relay master key, freezes generations and separates
Sent receipts from delivery attempts. Current account/header/envelope admission and recovery workers are integrated
for primary-address compose and client PGP; pickup/alias/system routes and
uncertainty reconciliation tooling remain pending. Restored jobs must never
replay automatically. The internal transport preserves acceptance error types
while rendering safe errors; callers must not log unwrapped provider responses.
Provider acceptance does not establish recipient delivery or inbox placement.
Live AWS/Cloudflare testing needs the operator's account, authorized sender and
controlled recipient; see the [test matrix](TURNKEY_MAIL_STACK_PLAN.md#operator-owned-relay-test-matrix).

## Runnable qualification

From `backend/`:

```sh
GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailmsg ./internal/api ./internal/backup -run 'TestDomainRelay|TestNativeMailRelay|TestNativeMailDomains|TestNativeRetiredRelayDomain' -count=1 -timeout=20m
```

These tests use encrypted files, sealed capsules, actual authenticated HTTP
routes and a loopback TLS SMTP server. They cover secret rotation/loss, admin and
step-up gates, DNS/restore races, mandatory AUTH, untrusted certificates,
plaintext refusal, exact MIME and hidden envelope recipients, echoed credential
redaction and lost final acknowledgments. They submit no live provider mail.
