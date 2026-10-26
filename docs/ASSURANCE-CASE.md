# Assurance case

[Product overview](../README.md) · [Security](../SECURITY.md) · [Architecture](ARCHITECTURE.md)

This is the argument that KyPost meets its security requirements: what it protects, from whom,
where the trust boundaries are, and why the design and the code hold at each one. The detailed
behaviour lives in [SECURITY.md](../SECURITY.md); this page ties it together and points at the
evidence. Where a claim names code, the path is relative to the repository root.

## What KyPost must protect

KyPost is a self-hosted mail client and, opt-in, a mail host. Its security requirements:

1. **Only the right person signs in as an account.** Passwords, second factors, sessions and
   agent tokens cannot be guessed, replayed or forged, and the server never holds a user's
   password.
2. **A user reaches only their own mail and settings**, and only an administrator reaches
   administration.
3. **Hostile mail stays inert.** Mail HTML, MIME, vCards and OpenPGP packets come from strangers
   and must not run script in the app, read the session or reach the server's network.
4. **End-to-end encrypted mail stays end to end.** A client-protected PGP private key is wrapped
   in the browser; the server, the classifier and AI agents never see the key or the plaintext.
5. **Secrets at rest stay secret** from anyone who copies the config volume without the private
   key volume.
6. **The server cannot be turned against the network it sits in** through hosts a user
   supplies.
7. **No secret or message content reaches a log.**

## Threat model

| Attacker | Can | Wants |
|---|---|---|
| Anonymous internet client | Send any request to the published port | Take over an account, guess passwords, exhaust resources |
| Anyone who can send mail to a user | Put arbitrary HTML, MIME, attachments, vCards and OpenPGP data in the inbox | Run script in the web app, steal the session, exfiltrate mail, steer an AI agent |
| Signed-in ordinary user | Use their own session and APIs, set their own IMAP/SMTP/CardDAV hosts | Reach admin functions, other users' mail, or internal services |
| AI agent holding a user's token | Call the MCP tools at `/mcp` | Send or delete mail, read encrypted mail |
| Network attacker on a path KyPost uses | Read and alter traffic to IMAP, SMTP and the browser | Steal credentials or downgrade TLS |
| Someone with a copy of the config volume or a backup capsule | Read the files | Recover IMAP passwords, TOTP secrets, PGP keys |
| Another server holding a push relay key | Call the relay's `/send` | Push to devices that belong to someone else |

Out of scope: an attacker who already runs code in the container or holds both the config and the
`/kypost/private` volume (they are the server), and a malicious administrator. See
[Known limitations](../SECURITY.md#known-limitations--trust-boundaries).

## Trust boundaries

1. **Browser and apps → KyPost.** KyPost serves plain HTTP unless `TLS_CERT_FILE` is set. The
   entrypoint refuses a non-loopback cleartext publish without `ALLOW_INSECURE_HTTP=true`
   (`scripts/entrypoint.sh`, proved by the `ci-docker` job). Forwarded headers are read only
   from peers in `TRUSTED_PROXY_CIDRS`, one named header, right-most hop
   (`clientIP` in `backend/internal/api/server_request_context.go`).
2. **Mail content → the web app.** Mail HTML is sanitised with DOMPurify and rendered in an
   iframe whose sandbox does not allow scripts (`FRAME_SANDBOX` in
   `frontend/src/pages/read/EmailBodyFrame.tsx`), under a CSP without `'unsafe-inline'` scripts.
3. **KyPost → user-supplied hosts** (IMAP and SMTP servers, CardDAV, WKD key lookups, browser and
   UnifiedPush endpoints). These dial through `backend/internal/netguard`, which resolves once,
   refuses the host if any address is loopback, private, link-local (which covers
   169.254.169.254) or otherwise reserved, then dials the checked address, unless the operator
   allow-lists the host. KyCalendar applies the same address check unless
   `KYPOST_KYCALENDAR_ALLOW_PRIVATE` is set. Two operator-configured endpoints that normally live
   on the private network are **not** address-checked: the Ollama classifier (which instead
   refuses cleartext to a public host, `backend/internal/adapters/classifier/base_url.go`) and
   the SSO issuer (`backend/internal/sso/sso.go`, no redirects, bounded responses).
4. **KyPost → its volumes.** Secrets are encrypted with AES-256-GCM under master keys that live
   in `/kypost/private`, separate from the data they protect.
5. **KyPost → AI agents.** `/mcp` accepts only a bearer agent token and refuses any request that
   carries an `Origin` header.
6. **Self-hosted servers → push relays.** The relays store only the SHA-256 of each API key, and
   the first key to deliver to a device token owns it; another key gets `403`
   (`worker/README.md#token-pinning`, `push-relay-shared/push-relay-common.ts`).
   New FCM service-account assertions require a literal RSA-2048 minimum
   (`worker/src/fcm.ts`), checked from the public modulus rather than workerd's
   byte-rounded metadata. The native runtime regression verifies undersized-key
   refusal before provider requests, strong signing and cached-token continuity
   (`worker/src/fcm-workerd-key-floor.test.mts`); hosted TLS policy is separate.

## Secure design principles applied

- **Least privilege.** The runtime user owns `/kypost` and nothing else; `/opt/kypost`, which
  holds the entrypoint and the frontend, stays root-owned ([AGENTS.md](../AGENTS.md)). The
  API requires every agent token to name its access level and the page offers read only first;
  no MCP tool sends or deletes mail. Administrator accounts get
  no agent.
- **Fail-safe defaults.** `KYPOST_BIND` has no default. Forwarded headers are ignored until a
  proxy is named. SMTP submission without STARTTLS is refused unless `ALLOW_INSECURE_SMTP=true`.
  Private addresses behind user-supplied hosts are refused unless allow-listed.
- **Complete mediation.** Authorization is checked on every request, so a deactivation or
  demotion takes effect on the next request, not at the next sign-in.
- **Economy of mechanism.** One container, the standard library for HTTP and crypto primitives,
  twenty direct Go modules, four of them `golang.org/x` (`backend/go.mod`).
- **Separation of privilege.** TOTP, recovery codes and push approval as second factors;
  step-up re-authentication for sensitive changes; KyPost refuses to remove the last active
  administrator.
- **Open design.** Only published algorithms: Argon2id, PBKDF2-SHA256, HKDF, AES-256-GCM and
  OpenPGP through ProtonMail's go-crypto. The code and this argument are public.
- **Psychological acceptability.** Every place where security yields to convenience is
  signposted where the user reads it ([CONTRIBUTING.md, the user contract](../CONTRIBUTING.md#the-user-contract)).

## Common weaknesses and how they are countered

| Weakness | Counter | Where |
|---|---|---|
| Password theft from the server | The browser sends only a derived half of the password (PBKDF2-SHA256, 600,000 iterations, then HKDF); the server stores that as Argon2id (64 MiB, 3 passes, 4 lanes, 16-byte random salt) | `frontend/src/lib/authSecret.ts`, `backend/internal/users/authsecret_derive.go`, `HashPassword` in `backend/internal/users/users.go` (via `ky-primitives/password`) |
| Credential stuffing and guessing | 3 failures lock an account for 15 minutes; a looser per-address lockout keyed on the IPv6 /64; an instance-wide login limit; proof-of-work CAPTCHA by default | `backend/internal/api/login_lockout.go`, `lockoutKeyForIP` |
| Session theft or fixation | A fresh 24-byte `crypto/rand` token per sign-in, in an `HttpOnly`, `SameSite=Lax` cookie, `Secure` on HTTPS; 24-hour idle expiry; removed on sign-out | `backend/internal/api/server_auth_session.go`, `randomToken` in `server.go` |
| CSRF | Double-submit token per session, compared in constant time on state-changing requests | `csrfCheckOK` in `backend/internal/api/server_auth_session.go` |
| Cross-site scripting through mail | DOMPurify, an iframe sandbox without `allow-scripts`, and a CSP with no `'unsafe-inline'` or `'unsafe-eval'` in `script-src`, `object-src 'none'`, `frame-ancestors 'none'` | `frontend/src/lib/emailHtml.ts`, `frontend/src/pages/read/EmailBodyFrame.tsx`, `backend/internal/api/security_headers.go` |
| SQL injection | `database/sql` with `?` parameters; the one formatted statement adds columns whose names are constants in the same file | `backend/internal/state/schema.go` |
| Command injection | The only subprocess is `restic`, started with an argument list, never a shell | `backend/internal/backup/bulk.go` |
| Server-side request forgery | User-supplied hosts use one guarded dialer, address checked after resolution and before connect; the operator's classifier and SSO issuer are trusted configuration | `backend/internal/netguard/netguard.go`, `backend/internal/api/ssrf_guard.go` |
| TLS downgrade on submission | STARTTLS required when not on implicit TLS; refused otherwise unless the operator opts out for the whole deployment | `requireSTARTTLS` in `backend/internal/mailmsg/smtp_send.go` |
| Secrets at rest | AES-256-GCM with a random nonce per encryption and 32-byte master keys; client-protected PGP keys wrapped in the browser with AES-GCM under a PBKDF2 key | `backend/internal/cryptutil/cryptutil.go`, `frontend/src/lib/keyVault.ts` |
| Undersized browser OpenPGP RSA keys | Literal 2048-bit minimum for new encryption, signing and signature trust; retained historical keys still decrypt old ciphertext. A weak recipient refuses the operation without omission or plaintext fallback | `RSA_POLICY` in `frontend/src/lib/pgpClient.ts`; attack-path and history controls in `frontend/src/lib/pgpClient.keyFloor.test.ts` |
| Weak or broken crypto | No MD5, DES or RC4; SHA-1 appears only where a protocol fixes it (WKD's local-part hash) and as a non-secret CardDAV path key; `golang.org/x/crypto/openpgp` is kept out of the build graph by a CI step | `backend/internal/api/pgp_wkd.go`, `carddav_client.go`, `ci-backend-other` |
| Prompt injection through mail | Agents cannot send, delete or move mail to Trash; encrypted mail reaches them as metadata only; the README warns before granting write access | `backend/internal/api/mcp*.go`, [README](../README.md#connect-an-ai-agent) |
| Secrets in logs | A field allowlist, a redaction pass (fuzzed in CI) and a source-harvesting test of every log key | [LOGGING.md](../LOGGING.md), `backend/internal/redaction`, `backend/internal/api/log_privacy_test.go` |
| Vulnerable dependencies | Dependabot for Go, npm, Actions and base images; `govulncheck` and `npm audit --omit=dev --audit-level=low` in CI | `.github/dependabot.yml`, `.github/workflows/ci.yml` |
| Supply-chain tampering | Actions pinned by commit and checked by CI; base images pinned by digest; release images attested and built only from a commit that passed CI; the server binary rebuilds to the same bytes | [VERIFYING-RELEASES.md](VERIFYING-RELEASES.md) |

## Residual risk

Known limitations are listed, not hidden: anyone with both the config and private volumes can
decrypt IMAP credentials and TOTP secrets, account metadata and mailbox state are stored
unencrypted, DOMPurify is a defence layer rather than a boundary, rate limits are process-local,
and native receiving is still in qualification. Each is in
[SECURITY.md](../SECURITY.md#known-limitations--trust-boundaries) with what to do about it.

## Evidence

- **Tests:** attack-path tests such as `backend/internal/api/*_security_fixes_test.go`,
  `security_headers_test.go`, `hardening_test.go` and `log_privacy_test.go`; CONTRIBUTING requires
  attack-path coverage for security changes. CI runs every backend package under the race
  detector on every push and pull request.
- **Static analysis:** CodeQL for Go, JavaScript/TypeScript and Actions on push, pull request and
  a weekly schedule; `go vet`, `golangci-lint` (staticcheck included), `deadcode` and
  `govulncheck`.
- **Dynamic analysis:** the race detector on the full suite, four Go fuzz targets run for 20
  seconds each in CI (filter rules, redaction, reply threading, IMAP header records), and
  container smoke tests that prove the cleartext bind guard and private-root ownership.
- **Review:** every change goes through a pull request with green CI and an adversarial review
  ([CONTRIBUTING.md](../CONTRIBUTING.md#every-pr-must-pass-hostile-ai-code-review)).
- **Releases:** attested images and a repeatable binary ([VERIFYING-RELEASES.md](VERIFYING-RELEASES.md)).

This case is updated when a trust boundary or a security requirement changes. A pull request that
moves one must update this page in the same change.
