# Operator-owned domain relay configuration

Native primary-address sending uses the configured operator-owned relay in
explicit native mode. Saving credentials makes no provider connection;
`sendingEnabled` reports configured native runtime capability, not DNS/provider
readiness or recipient delivery. Busnes supplies no relay account or service.
Provider readiness probing and guided setup remain pending. See the
[outbox runtime contract](NATIVE_OUTBOX.md) for supported sends and remaining gates.

## Configure through the admin API

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
`GET /api/admin/mail-relay` returns configuration status, domain, issuer, host,
port and generation with `Cache-Control:no-store`. Both methods omit the relay
username and password; unreadable existing ciphertext or key produces an
explicit error rather than an unconfigured fallback.

## Storage, recovery and activation

`$CONFIG_DIR/native-relay.json` is encrypted with the dedicated
`$SECRET_DIR/native-relay.key`; both are created owner-only. There are no relay
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
their historical domain/issuer binding. Drills enforce the additive version-1
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
GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailmsg ./internal/api ./internal/backup -run 'TestDomainRelay|TestNativeMailRelay' -count=1 -timeout=20m
```

These tests use encrypted files, sealed capsules, actual authenticated HTTP
routes and a loopback TLS SMTP server. They cover secret rotation/loss, admin and
step-up gates, DNS/restore races, mandatory AUTH, untrusted certificates,
plaintext refusal, exact MIME and hidden envelope recipients, echoed credential
redaction and lost final acknowledgments. They submit no live provider mail.
