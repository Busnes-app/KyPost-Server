# Receiving gateway qualification

2026-10-03 proof, with 2026-10-04 opt-in runtime follow-up for [the turnkey plan](TURNKEY_MAIL_STACK_PLAN.md). No public receiver, production DNS or new dependency is installed.

**Maddy 0.9.5 passes the original queue proof, synchronous holding-store integration and actual opt-in KyPost receiving-command/daemon-import test.** Supported command checks bind recipients and commit mail before SMTP acknowledgment, avoiding the stock queue's expiry. It remains the selected candidate, without a bundled public listener. Verified runtime ownership/commit fences now have runnable checks; deployment gates remain open. Postfix remains the fallback; it has not been executed in this assessment.

## Candidate comparison

| Candidate | Useful existing functionality | Costs and gaps |
| --- | --- | --- |
| Maddy | Standalone release binary; configurable recipient routing; filesystem queue; forwarding over SMTP or LMTP. Tested below without installing a service. | Default queue retries terminate; stock forwarding lacks a first-class pickup receipt/ownership API. GPL licensing requires a packaging review. |
| Postfix | Established queue manager and SMTP/LMTP delivery with per-recipient completion/retry tracking. | More service/package configuration; also needs an ingestion boundary and ownership binding. A finite queue lifetime remains something to configure and test. No runnable qualification yet. |

Maddy's [release](https://github.com/foxcpp/maddy/releases/tag/v0.9.5) was published 2026-05-23. Its [routing](https://maddy.email/reference/smtp-pipeline/), [queue](https://maddy.email/reference/targets/queue/) and [forwarding](https://maddy.email/reference/targets/smtp/) are documented upstream. Postfix's [architecture](https://www.postfix.org/OVERVIEW.html) and [delivery client](https://www.postfix.org/lmtp.8.html) describe its handoff boundary; its [announcements](https://www.postfix.org/announcements.html) list stable 3.11.7 on 2026-09-07. These are maintenance signals, not security audits or support guarantees.

Maddy includes [GPLv3 license text](https://github.com/foxcpp/maddy/blob/v0.9.5/COPYING), with GPLv3-or-later notices in [queue source](https://github.com/foxcpp/maddy/blob/v0.9.5/internal/target/queue/queue.go). Postfix has [dual IPL/EPL licensing](https://www.postfix.org/announcements/postfix-3.3.0.html). This proof runs a separate upstream executable; no upstream source is copied into KyPost. Distribution obligations must be checked before shipping either component.

## Pinned executable and runnable check

Source tag `v0.9.5` resolves to commit `58e8a11423e140ad37063ce8dfc446de4c1591ed` ([upstream commit](https://github.com/foxcpp/maddy/commit/58e8a11423e140ad37063ce8dfc446de4c1591ed)). The qualification uses the upstream **x86_64 Linux musl** artifact:

- Archive SHA-256: `665cb3ffc43dcad0b904bb2792f31cd1025e484b0bacaf4ece06e2bd0091d5e2`.
- Extracted binary SHA-256: `6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db`.
- Hashes were measured from the downloaded release. The check refuses other binary bytes. The upstream asset signature was not independently verified here; production packaging needs provenance verification as well as a content pin.

Run from the repository root on x86_64 Linux with Python 3 and `tar`/`zstd`:

```bash
gateway_check_dir=$(mktemp -d)
curl -fL https://github.com/foxcpp/maddy/releases/download/v0.9.5/maddy-0.9.5-x86_64-linux-musl.tar.zst -o "$gateway_check_dir/maddy.tar.zst"
echo '665cb3ffc43dcad0b904bb2792f31cd1025e484b0bacaf4ece06e2bd0091d5e2  '"$gateway_check_dir/maddy.tar.zst" | sha256sum -c -
tar --zstd -xf "$gateway_check_dir/maddy.tar.zst" -C "$gateway_check_dir"
python3 scripts/check-receiving-gateway.py "$gateway_check_dir/maddy-0.9.5-x86_64-linux-musl/maddy"
rm -rf "$gateway_check_dir"
```

The [check](../scripts/check-receiving-gateway.py) uses only Python's standard library, ephemeral loopback high ports and a temporary spool. It cleans its processes and spool even on failed assertions. Docker was available but unnecessary. No root privilege, global installation, public port or provider account is needed.

## Observed result

The executable check passed on 2026-10-03 with two pickup attempts:

1. Unknown local recipients and recipients outside the configured addresses were rejected before mail acceptance. An oversized message was refused with a configured 1 MiB maximum.
2. SMTP returned acceptance while the delivery sink withheld its final acknowledgment. The actual queue had header/body/metadata files, the original envelope sender, both visible and hidden recipients, and a generated spool ID.
3. `SIGKILL` terminated the receiver before pickup acknowledgment. The accepted queue item survived and replayed after restart with its original spool ID.
4. Both attempts delivered the original RFC5322 message as an exact byte suffix, including a dot-stuffed body line and a base64 binary attachment. Maddy prepended a `Received` trace header; full delivered bytes therefore differ from submitted bytes. No Bcc header or hidden recipient appeared in recipient-visible MIME.
5. After the sink returned final `250`, Maddy removed queue metadata and body. The test sink is intentionally ephemeral: this verifies the gateway acknowledgment boundary, **not a KyPost mailbox commit**. The production importer must commit mail and receipts before acknowledging.

This proves process-restart recovery and at-least-once delivery for the tested boundary. It does not prove machine power-loss durability, disk-full recovery, every crash point, long outages, concurrent consumers, signed-MIME verification, or production abuse protection.

## Gates before production selection

- **Hold-until-import policy:** stock queue gives up on permanent delivery errors or exhausted attempts and removes the item; DSNs are absent without a bounce pipeline. Never ship the proof configuration. Define durable quarantine/expiry policy and test importer downtime beyond the retry budget. Source `dispatch`, `emitDSN`, and `removeFromDisk` in the [pinned queue implementation](https://github.com/foxcpp/maddy/blob/58e8a11423e140ad37063ce8dfc446de4c1591ed/internal/target/queue/queue.go) own this behavior. A large retry count alone is not a retention contract.
- **Authenticated delivery identity:** spool IDs are observable in private metadata, but stock SMTP forwarding does not expose a first-class pickup API with authenticated delivery ID, payload digest, exclusive claim and acknowledgment. Do not use sender-written Message-ID as an idempotency key. Prove a scoped bridge or a trusted trace-token contract before release; do not let an importer directly edit a live gateway queue.
- **Recipient ownership:** the spool records addresses, not KyIdentity issuer/subject or routing generation. Resolving a queued alias against today's directory can deliver yesterday's mail to a new owner. Binding at acceptance, signed/revisioned routing snapshots, staleness refusal and reassignment tests remain mandatory. Static test recipient rules demonstrate refusal only.
- **Durability and capacity:** source syncs payload and metadata files and renames metadata; this process-kill proof does not establish parent-directory durability after power failure. Exercise persistence on the intended filesystem, interrupted updates, quota exhaustion and recoverable errors. Add capacity refusal and alerts; message-size limits alone cannot bound total spool growth.
- **Secure handoff and abuse:** the proof uses unauthenticated plaintext loopback delivery and two static recipients. Production needs an authenticated/scoped local channel, recipient reconciliation, TLS setup, rate/concurrency limits, spam policy and trusted provenance for authentication verdicts. The [endpoint reference](https://maddy.email/reference/endpoints/smtp/) describes available limits; configuration and end-to-end tests still need building.

These stock-queue gaps prompted the synchronous boundary below. Neither receiver eliminates the need for inbound SMTP reachability; operators without reachable port 25 need a separately qualified hosted-reception profile.

## Synchronous receiving boundary

The reusable [ingress store](../backend/internal/ingress/store.go) now implements a separate SQLite holding buffer. The actual [Maddy integration test](../backend/internal/ingress/maddy_test.go) invokes a private, test-only subprocess through Maddy's [supported command checks](https://maddy.email/reference/checks/command/): `run_on rcpt` freezes the route; `run_on body` commits bounded raw MIME before the helper succeeds. The test uses a dummy final target **only after the successful commit check**. The [controlled configuration generator](RECEIVING_SETUP.md) now emits the tested TLS-only bridge; no compose file installs or starts it; copying a dummy-target configuration without its mandatory durable check would discard mail.

The local OS account and owner-only directory are the trust boundary. Gateway identity is fixed by trusted helper configuration, and delivery identity comes from Maddy's `{msg_id}` argument, not message headers. There is no new network pickup endpoint or bearer-token scheme. The ingress package's route setter accepts already-verified issuer/subject/mailbox decisions; it does not verify directory provenance itself.

Important pinned-source detail: `check.command` accumulates attempted recipients, including refused ones, in `{rcpts}`. This configuration makes the durable RCPT binding check the sole recipient authority; only successful binding rows define delivery. The test helper enforces its fixed served domain before binding, including when an otherwise valid route exists outside that domain. Unknown recipients and external relay attempts create no extra bindings. Production must use the verified configured domain and ensure no later recipient check can reject an address after its binding succeeded.

Run after obtaining the pinned executable above:

```bash
cd backend
MADDY_PROOF_BINARY=/absolute/path/to/pinned/maddy GOTOOLCHAIN=go1.26.9 go test -race ./internal/ingress -count=1 -v
```

With the executable unset, the gateway test explicitly skips; store unit/concurrency checks still run. The gateway check validates the binary hash before executing it. Its temporary listener and processes are isolated from live mail.

Verified on 2026-10-03:

- Two envelope recipients, including Bcc, retain their frozen issuer/subject/mailbox/generation. Reassignment between RCPT and DATA retains the old binding; pickup quarantines it without transferring the payload. New transactions bind the new owner with a different receiver ID.
- Forged Message-ID and ownership headers do not supply receipt identity or owner. Original MIME, including folded headers and dot-stuffed content, remains an exact suffix after receiver trace headers.
- Accepted mail survives receiver SIGKILL. A second experiment closes the parent database, kills the holding-writer subprocess with its database open after commit but before helper success, and reopens SQLite afresh. The committed receipt/payload survives; exact receipt replay does not duplicate it.
- The real SMTP boundary returns temporary `451` when the logical payload quota fills and retains earlier pending/quarantined mail. Unit checks reject oversized MIME and stale/disabled routes. Accepted and staged records have no automatic age expiry.
- Competing independent connections admit only one claim. Concurrent subprocesses at the record ceiling admit only one receiver. Expired or replaced claim tokens cannot acknowledge; successful acknowledgment archives the delivery into a tombstone (sender, digest, recipients) outside the record limit, allowing lost local acknowledgment and exact receipt replay without re-delivery.

Remaining public deployment gates: safe explicit fenced cleanup of abandoned reservations; tombstone pruning and capacity recovery; hard filesystem/database/WAL quotas and representative reserve qualification; intended-volume power-loss/backup/restore drills; stalled-helper/final-ack shutdown extremes and remaining rate-limit qualification; TLS, spam/rate policy and packaging provenance/licensing. Physical/free-space admission estimates now refuse new growth inside writer transactions and allow unchanged-route accepted-mail recovery; pinned-reader tests prove refusal and checkpoint recovery. These estimates do not replace hard shared-volume quotas. Logical quotas do not bound SQLite/WAL physical bytes, and removing a live BLOB does not erase old pages/backups. An upstream retry after a lost SMTP acknowledgment normally receives a **new** transaction ID and may create a second receipt; inbound SMTP is not exactly-once across those transactions.

Permanent mailbox receipts and the all-recipient-before-ack bridge have [storage/recovery checks](TURNKEY_MAIL_PHASE1.md#permanent-storage-and-import-follow-up). The [direct receiving runtime](NATIVE_PROVISIONING.md#direct-receiving-runtime-qualification-profile) adds separate explicit flags, existing-only store admission, current directory/local-user fences, inherited-pipe deadlines and conservative signed-generation quarantine. `TestNativeReceivingMaddyRuntime` runs this production command through the same pinned Maddy executable and actual daemon importer using test-only DNS. Run `MADDY_PROOF_BINARY=/absolute/path/to/pinned/maddy GOTOOLCHAIN=go1.26.9 go test -race ./internal/app ./internal/ingress -run '^TestNativeReceiving|^TestMaddyHoldingBoundary|^TestMailboxImporter' -count=1 -timeout=5m` from backend. This does not install the tested SMTP configuration or authorize public MX.

## Generated TLS qualification profile

`TestNativeReceivingMaddyRuntime` now consumes production `receiving config` output, changing only the test binary dispatch prefix. It checks mandatory STARTTLS with no plaintext bindings, verified client TLS, unknown/external recipient refusal, two-recipient durable acceptance, receiver SIGKILL/restart before native daemon import, and physical-pressure refusal. A third active transaction from the same IP is temporarily refused; `defer_sender_reject false` makes TLS and limits run at MAIL rather than deferring initialization to RCPT. Per-IP burst exhaustion also returns a temporary refusal. Refill timing and global limits remain to qualify independently. No public SMTP port or MX is changed. See [setup and limitations](RECEIVING_SETUP.md).

The optional launcher checks the same pinned executable and preserves previous config on failed domain preflight. With `RECEIVING_PROOF_IMAGE=<locally-built-image>` added to the command above, the supervised variant runs the image’s actual Supervisor, automatically restarts a killed receiver before import, preserves committed two-owner mail and verifies idle container shutdown. It uses only synthetic local state, test-only loopback DNS/TLS and host networking for that fixture. The engine remains operator-supplied; public reachability, active-DATA shutdown extremes and intended-volume failures are not proved by this check.

The `supervised_partial` runtime case additionally stops the actual image Supervisor with TLS DATA open and partial bytes flushed. It requires a clean receiver exit, refusal of incomplete SMTP DATA, a retained staged binding with no digest/payload, and unchanged previously accepted mail that imports after another start. The 55-second test deadline bounds the real 45-second receiver stop plus container teardown; this does not qualify stalled-volume/helper shutdown or every acceptance/acknowledgment race.
