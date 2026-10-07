# Cloudflare receiving pilot

This is a controlled **one-message** test, not continuous domain reception.
Cloudflare Email Routing delivers the exact-recipient message to an operator-owned
Email Worker. It awaits private R2 storage before returning success. KyPost then
manually fetches it over authenticated HTTPS, scans it locally and imports it into
the existing native mailbox. No public KyPost intake endpoint, IMAP server or
inbound port 25 is needed. AWS SES outgoing settings are independent.

## Trust and limitations

- Cloudflare and the operator's Cloudflare administrators can read ordinary raw
  mail and its envelope in R2. PGP ciphertext remains ciphertext; Rspamd cannot
  inspect its encrypted body. KyPost encryption does not erase provider copies.
- Only one exact recipient and a 15-minute capture window are configured. Any
  sender can fill that slot. A full slot rejects additional mail; use only a
  dedicated test domain. Do not cut over a working domain to this pilot.
- Conditional R2 storage keeps the first message immutable. An ambiguous put may
  retain an object even if the Worker reports refusal. This pilot claims no
  Cloudflare retry guarantee or exactly-once upstream delivery. Inspect retained
  storage before attempting a new test.
- A frozen route binds issuer, subject, mailbox, source and the recipient's
  address generation (carried in the route's `revision` field; it changes only on
  reassign, disable, re-enable or release, never on ordinary directory edits). The
  recipient may be the primary or an active alias. The local original route file
  must equal the stored route. Pickup rechecks current DNS/domain and
  identity/access/restore authority inside the existing admission fences. An
  expired capture window does not discard a message captured in time. Changed
  ownership or generation refuses delivery; preserve R2 bytes for explicit
  reconciliation.
- A dedicated random 256-bit bearer secret protects read-only pickup. It is
  independent of administrator sessions, pairing keys and push relay credentials.
  Bucket public access and lifecycle deletion must remain disabled.
- Cloudflare's [Email Worker interface](https://developers.cloudflare.com/email-service/api/route-emails/email-handler/)
  supplies raw mail and SMTP envelope, but no original peer IP/HELO. KyPost
  supplies fixed Rspamd settings disabling SPF, DMARC and ARC groups; content
  checks and DKIM remain. MIME Authentication-Results/Received headers grant
  neither delivery authority nor trusted sender provenance.
- Scanning occurs **after Cloudflare capture**. Spam refusal or scanner failure
  leaves the R2 object retained; it cannot retroactively reject Cloudflare's SMTP
  acceptance. There is no automatic provider deletion, Junk move or quarantine
  cleanup. Exact local replay uses the durable receipt without rescanning.
- Local native/receiving flags and an existing spool are required. Preserve
  established IMAP/client wire contracts; no client changes accompany this pilot.

## Prepare locally

Run as the normal KyPost runtime account, using its existing configuration/state.
Require `KYPOST_NATIVE_MAIL=true`, `KYPOST_NATIVE_RECEIVING=true` and
`KYPOST_RECEIVING_RSPAMD=true`, with the qualified loopback Rspamd sidecar running.
Use existing native provisioning and DNS proof; never initialize an existing
spool or change a mailbox source for this test.

```bash
umask 077
kypost-server receiving cloudflare route mail-test@example.test > route.json
openssl rand -hex 32 > pickup-token.txt
```

`route.json` is an expectation, not a signing credential. Keep the original file
private and unchanged for pickup. Regenerate it immediately before a capture
window; if setup takes longer than 15 minutes, open a new window deliberately.
Token and route files must be regular files without symlinks, owner-only, and
readable by the runtime account. Keep secrets out of command arguments and logs.

## Operator-owned Cloudflare setup

Obtain separate approval for the exact domain/route/DNS and new cloud resources.
Record the existing MX/SPF and Email Routing configuration for rollback before
changing anything. These steps are manual configuration instructions, not an
automatic installer.

1. Create one dedicated private R2 bucket, with public access and lifecycle
   deletion disabled. Use a new empty bucket; retain an occupied bucket after
   any failed or ambiguous test.
2. Copy `receiving-worker/wrangler.toml.example` to a private configuration beside
   `worker.mjs`. Set only the operator-owned account, dedicated Worker name and
   bucket. Keep observability disabled. Deploy with the repository's existing
   pinned Wrangler version; install `PICKUP_TOKEN` and `ROUTE` through protected
   secret-file input, never checked-in variables or pasted chat.
3. Inspect Cloudflare's required Email Routing DNS changes. Enable only the
   dedicated test domain, preserving the KyPost TXT proof, SES DKIM and existing
   sending policy. Configure one exact recipient to this Worker; leave catch-all
   disabled. Do not forward to an unrelated inbox or another Worker.
4. Verify public authoritative DNS and the Worker deployment/binding/secret
   names. An authenticated empty-slot GET returns 404; unauthenticated GET
   returns 401. Treat any other result as a failed preflight.

Cloudflare's [R2 conditional put contract](https://developers.cloudflare.com/r2/api/workers/workers-api-reference/#conditional-operations)
supports `If-None-Match: *` and returns null when its condition fails. The Worker
persists raw bytes and the envelope together in that conditional write.

## Run the controlled test

1. Keep KyPost pickup stopped. Open a fresh route window and install that exact
   route in the Worker. Send one controlled message to the configured address.
2. Verify one R2 object exists under `pilot-message`, with the capture UUID,
   byte count and digest. Do not print raw mail or credentials in diagnostics.
3. Run the manual pickup using the normal runtime account:

   ```bash
   kypost-server receiving cloudflare pickup \
     https://DEDICATED-WORKER.OPERATOR.workers.dev pickup-token.txt route.json
   ```

   Only verified HTTPS `workers.dev` origins without redirects, credentials,
   ports or query parameters are accepted. No system HTTP proxy is used.
4. Verify one permanent mailbox receipt, exact raw digest and successful body
   display in KyPost. Repeat pickup and prove the same message remains a single
   delivery. The provider object must still exist after both pickups.
5. Close the capture window by removing/disabling the exact Email Routing rule.
   Restore the previously recorded MX/SPF/Email Routing settings unless continued
   test routing was explicitly approved. Preserve the R2 object and old route
   file until the operator approves specific cleanup. The Worker has no delete
   endpoint. Retained objects are outside KyPost's sealed backup contract.

Rollback never deletes native mail, local receipts or provider objects. Stopping
pickup/removing the exact rule leaves existing accepted local obligations and
the previously qualified Maddy path intact. Revoke the dedicated pickup secret
after qualification if no further pickup is needed.

## Runnable checks

```bash
node --test receiving-worker/worker.test.mjs
bash scripts/test-relays.sh
# Requires the existing worker/ npm dependencies:
node receiving-worker/runtime-check.mjs
python3 scripts/check-rspamd.py
cd backend
GOTOOLCHAIN=go1.26.6 go test -race ./internal/app \
  -run 'TestCloudflare|TestNativeReceiving|TestReceivingRspamd' -count=1
```

These use disposable test state. They verify raw bytes, bounded authenticated
pickup, frozen authority, replay, namespace isolation and scanner refusal.
The additional workerd/R2 emulator check proves atomic conditional capture,
occupied-slot refusal and exact retained pickup using the pinned local runtime.
These checks do not prove the live provider's behavior; the controlled live test
must qualify it before relying on this profile.
