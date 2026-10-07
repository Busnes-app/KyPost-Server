# Native outbox storage and qualification

Native sending from the primary or an owned alias, of the primary or an extra mailbox, is available with `KYPOST_NATIVE_MAIL=true` and
an admin-configured operator-owned relay. Ordinary compose, public-key encrypted
compose and client-prepared PGP use the same durable admission/claim boundary.
API and daemon processes recover due definite-refusal attempts and accepted Sent
obligations; SQLite claims arbitrate competing processes. External IMAP behavior
is unchanged. Native pickup notifications, applicable system/probe mail, provider readiness UI and restore-hold release remain pending.
The local TLS/PGP checks below are qualification, not live AWS/Cloudflare delivery.

## Current admission and client responses

Before queueing or claiming, perform fresh issuer-bound DNS verification of the
`From` domain, then hold domain, settings, directory and users fences in that
order. Admit the current signed subject, activity, role, primary address,
nonrevoked link and acknowledged existing-only mailbox/source. A forced password
change refuses sends. Relay credentials authenticate the operator; `From` is the
primary or any `active` ledger address of the sending mailbox on a relay domain
(the API answers 403 for any other before queueing).
Legacy IMAP credentials and verified send-as rows grant no native authority.

Each mailbox has its own outbox. `NativeOutbound` methods take the sending
mailbox ID (a primary's is its user ID): the owner is admitted, an extra mailbox
must be `active` and owned by the same subject, and the job, its claims and its
Sent copy live in that mailbox's `mailbox.db`. The API sends from the mailbox
`X-KyPost-Mailbox` selects and reads outbox status there; recovery workers walk
every mailbox of each published native user. Device fences always read the
owner's primary `state.db` under the owner's own source. A disabled mailbox
queues nothing new; its accepted Sent obligations still finish.

Freeze directory revision, `From` address generation (`FromGeneration`), relay
generation, local send-authority epoch, PGP revision/fingerprint and applicable
material generation/device-credential witness into encrypted intent. On queue and
every claim or retry the `From` address must still be `active`, owned by the
sending mailbox and at that generation; release, reassignment, disable and
re-enable change the generation without changing the directory revision, so
such a job ends `ErrNativeOutboundStale` (quarantined) and is never submitted.
Jobs queued before per-address generations carry 0 and pass only from the
primary, still fenced on the directory revision. Local deactivation/reactivation, credential/MFA/role/link
changes advance the epoch even within one second. Re-pairing the same device ID
cannot inherit jobs made with its former secret. Converted client-PGP sends also
require current confirmed device enrollment. Expiry bounds device/mailbox lock
waits and is checked before commit. Device SQLite precedes mailbox SQLite;
release every authority lock before contacting SMTP. A committed claim may finish
after later revocation; its real outcome and historical Sent obligation remain
owed to its frozen owner, even when that user becomes inactive.

Successful HTTP send responses keep `ok:true` meaning confirmed primary SMTP
acceptance, with existing `sentSaved`/`warning` and additive `outboxId`. Merely
queued, refused or uncertain primary attempts return 503 with `ok:false` and an
outbox ID; the ID may be absent from storage if initial admission failed. Inspect
before resubmitting: a new request is a new intent, not a deduplicated retry.
`GET /api/mail/outbox/{id}` uses existing session/device mail authentication and
returns only the acting owner's delivery sequence/state/attempt/next-attempt and
Sent filing status. It exposes no MIME, recipients, credentials or device witness;
unknown/unreadable jobs return an explicit 503. There is no retry/reconcile route.

Recovery discovers at most 50 due jobs per owner per pass, with four concurrent
owners and a 30-second cadence. Follow-ons wait for accepted primary evidence;
blocked/uncertain primary groups cannot starve unrelated work discovery. Definite
authority changes quarantine unclaimed work; transient DNS/storage failures retain
it. Sent failure cannot suppress an eligible delivery or authorize SMTP again.
Cancellation stops fresh work; shutdown joins connection/TLS (45 seconds),
SMTP after connection (another 45 seconds), and local finalization (30 seconds), in addition to ordinary HTTP/backup draining.
These bounds assume healthy local filesystems. Foreground HTTP sends may exceed
the ordinary 20-second drain; process exit preserves their durable claims for
reconciliation rather than authorizing automatic resend. No new dependency or setting exists.

## Persistent contract

An immutable job contains its envelope From, frozen relay generation, independent
delivery groups with exact normalized wire MIME and recipients, a separately
prepared Sent copy, and frozen sender-authority/device/material-generation metadata. The
caller must complete MIME/PGP/sender validation before admission. The storage
boundary rejects sender/header mismatches, recipient duplication, Bcc/resent
leaks and noncanonical SMTP bytes; it never rewrites a stored signature body.
Body/key material remains opaque. Each message obeys the mailbox's message cap;
a job allows at most 100 recipients/delivery groups and 64 MiB of wire/Sent bytes.

Server-composed mail gets one `Date` and one `Message-ID: <random@from-domain>`
(130 random bits) when it is built, before admission, so the stored wire MIME,
every retry and the Sent copy carry the same values. Client-supplied MIME
(`/api/mail/send-pgp`) is stored as sent; its client owns those headers. Some
relays replace them: AWS SES overrides both
([header fields](https://docs.aws.amazon.com/ses/latest/dg/header-fields.html)),
so the delivered Message-ID differs from the Sent copy's and replies to
SES-relayed mail do not thread against it. That is a provider limitation.

Use a server-generated UUID job ID. Exact enqueue replay preserves existing
states; a different intent under that ID is refused. One transaction commits all
deliveries and reserves both bytes and a record for Sent, within the same quota
as incoming mail and retained tombstones. Mailbox counters include queued
ciphertext, delivery records and Sent reservations, including reconstruction.
No automatic mail/job eviction occurs. Retained completed jobs still consume
quota; representative capacity and explicit retention remain operating gates.

Wire MIME, envelope addresses and Sent intent are encrypted together using
AES-GCM. A domain-separated HKDF key derives from the retained
`$SECRET_DIR/native-relay.key`, immutable mailbox owner/namespace and job ID.
No extra key file or environment setting is added. Copying ciphertext between
owners, recreated databases or job IDs fails decryption. Preserve the relay
master key; credentials can rotate without replacing it. Missing or corrupt
keys/ciphertext fail explicitly. Shared encrypted readers refuse malformed
nonce lengths without panicking.

Signed-only client MIME remains plaintext on the SMTP wire under its existing
contract; queue encryption uses a server-held key and is not end-to-end
confidentiality. A host/backup compromise exposing both database and master key
can decrypt queued intent. Preserve client key custody and the independently
encrypted PGP Sent copy; never invent a plaintext Sent fallback.

## Delivery and Sent states

`queued` or due `retryable` work may receive one durable `submitting` claim with
a unique attempt token. The caller must hold current domain/directory/users and
applicable alias/device/key authority through that claim, then release every
authority lock before network I/O. Relay generation must still match. Discovery
and storage consistency do not authorize a send.

Complete only that claim. A successful acceptance or accepted-then-QUIT failure
becomes `accepted`. A definite 4xx can become `retryable` with exponential
30-second–8-minute waits and at most six total attempts; the sixth refusal is
terminal. Definite 5xx becomes `failed`. Lost final evidence and other ambiguous
failures become `uncertain`. Provider error text is never stored in queue state. API and daemon log a failed
relay attempt with its outbox `correlation_id`, zero-based delivery sequence in
`slot`, bounded `reason` (`smtp_400`–`smtp_599` or `transport_error`) and
`result` (`not_confirmed`, `uncertain` or `accepted_teardown_failed`). No provider
response text, addresses, MIME or credentials are logged. This event precedes
local finalization: inspect durable owner-scoped status separately. It identifies
no SMTP command stage and does not prove recipient delivery. Old failures cannot
be reconstructed from this new logging.

A killed submitter remains `submitting`; no lease expiry reclaims it. Bounded,
oldest-first work discovery excludes both `submitting` and `uncertain`. These
require provider evidence or a deliberate retry decision before future tooling
can act. Unclaimed work can become `quarantined` after authority/config changes;
in-flight mail cannot be recalled. There is no automatic uncertain retry or
current operator reconciliation API.

Sent filing needs accepted delivery evidence and consumes its reserved capacity
in one transaction with the exact message and immutable filing receipt. Retry
returns the same numeric ID even after move/deletion. Missing Sent folders or
filing failure preserve the accepted job; retry filing never submits SMTP again.
Discovery finds accepted Sent obligations independently of delivery retries.
No existing client response is changed to mean merely queued.

## Backup and restore

The existing SQLite snapshots carry job ciphertext, claims, statuses and Sent
receipts together with committed WAL rows. Collection/drills decrypt the actual
snapshot, validate historical owner/domain bindings, bounded job/state shape,
Sent digests, foreign keys and quota accounting. Partial outbox schemas and orphan
claims are refused. Old databases with neither queue table remain compatible.
A nonempty queue requires version-1 recipe evidence
`outbox:encrypted-jobs-claims-and-sent` plus the relay config and matching key.

Snapshots preserve historical generations and interrupted claims. This evidence
must never enable replay. After historical validation and persistence of the
whole-stack hold, offline restore changes queued/retryable deliveries to
`quarantined` and clears their retry timers before publishing recovered data.
Frozen ciphertext, claims, attempts, accepted/Sent and submitting/uncertain
states remain intact. This prevents old queue entries from becoming sendable
if live authority is restored later; no automatic unquarantine exists.
A failed per-mailbox update leaves private staging unpublished and held;
repeating restore quarantine is safe. Native restores retain the whole-stack hold;
fresh identity/domain/provider and stale-ID reconciliation is still required,
with no supported release. Per-store consistency, backup size limits, power-loss
qualification and current authority remain separate [restore gates](RESTORE.md).

## Runnable checks

From `backend/`:

```sh
GOTOOLCHAIN=go1.26.6 go test -race ./internal/mailbox ./internal/backup ./internal/cryptutil -run 'TestNativeOutbox|TestOpenRefusesMalformedNonce' -count=1 -timeout=5m
```

Add the runtime check:

```sh
GOTOOLCHAIN=go1.26.6 go test -race ./internal/api ./internal/sso ./internal/mailbox ./internal/app -run '^TestNativeOutbound|TestNativeOutboxDiscovery' -count=1 -timeout=5m
```

Runtime checks exercise authenticated native HTTP over actual trusted loopback
TLS, AUTH/From separation, hidden BCC, signed encrypted recipient and independent
Sent decryption/signature verification, lost ACK retention, authority revocations,
settings/device contention, Sent-only failure with follow-on delivery, and actual active-worker SMTP cancellation/finalization without claiming the
next job. They use synthetic accounts/messages and a process-specific CA,
without weakening production TLS or contacting a provider.

The `device-plain` and `device-pgp` cases additionally join signed-directory
native allocation to the real pairing/register handlers, close/reopen a private
holding store, import exact MIME into that same owner's mailbox, and read body,
binary attachment and keyword changes through device credentials before sending.
No IMAP configuration or mail-client injection is used. They reject wrong secrets
and offboarded device reads. Ordinary Bcc isolation, client-prepared PGP recipient
and independent Sent verification reuse the actual TLS relay fixture. The incoming
bridge uses trusted-local ingress APIs; app/Maddy authority and SMTP admission
have separate checks. Test DNS, simulated device HTTP and loopback relay evidence
do not establish physical Android/Linux/iOS behavior or live provider delivery.

The storage checks cover encrypted signed-only-shaped wire intent, owner/namespace/key
isolation, exact replay conflicts, competing SQLite claims, a real killed
submitter, lost acknowledgments, bounded definite-refusal retry, independent Sent
recovery, shared quota, malformed/corrupt snapshots and sealed restore. Actual
loopback TLS tests preserve exact bytes and hidden envelope recipients; these
fixtures do not prove live PGP signature verification or provider delivery.

Next integration must route pickup notifications and applicable system/probe mail through current admission and this durable boundary. Preserve existing external IMAP behavior,
all PGP/device gates and the send-success meaning. Live tests need an operator
account, authorized sender and controlled recipient; use the
[provider matrix](TURNKEY_MAIL_STACK_PLAN.md#operator-owned-relay-test-matrix).
