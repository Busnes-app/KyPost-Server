**Repo:** Busnes-app/KyPost-Server
**Worktree:** /home/yoshi/git/busnes.app/kypost-server (branch feat/laya-evaluation)

# Rspamd spam-filtering sidecar handoff

Prepared 2026-09-30 by OpenAI Codex (GPT-6), Usagi / sundial.
Durable copy: `docs/RSPAMD_SIDECAR_HANDOFF.md`.
Shared copy: https://myslop.urlxl.us/f/kypost-rspamd-sidecar

## Request and status

The user reports three real-world problems: sorting is less correct than the tests suggest; obvious phishing/spam survives upstream filtering; sorting is slow for one user and may not serve ten users. They asked whether a sidecar could help, then requested this handoff. This turn creates the handoff only: no scanner, deployment, configuration, or filtering behavior has been implemented or tested. Implementation and production rollout are future work; production/security/config approval gates still apply.

Recommended first scope: Rspamd + Redis as local sidecars, a small Go HTTP adapter, shadow scoring, then explicitly enabled reversible Junk moves after real-mail validation. ClamAV is a later optional addition. This does not replace the ordinary category classifier or claim complete phishing protection.

The checkout already contains unrelated tracked/untracked changes to README, CHANGELOG, backend AGENTS files, poller, redaction, classifier adapters, model evaluation, and CalDAV documentation. Do not reset, stage wholesale, or incorporate these into a sidecar commit. Re-check `git status` and use an isolated checkout for implementation if necessary. Branch association does not imply that the Laya experiment is the implementation base; select and verify the intended base before coding. No sidecar PR exists.

## Verified constraints and reusable code

- `backend/internal/processor/poller.go`: `tickUser` fetches unread INBOX mail, warms the cache, checks processed state, flags app impersonation, harvests Autocrypt, then applies `allowByRate` before `handleMessage`. The rate gate breaks the message loop. Security screening inserted only in `handleMessage`, or immediately ahead of that break, will leave the rest of the batch unscanned when classification is throttled. Separate bounded scan admission from LLM admission and preserve cancellation/checkpoint semantics.
- `handleMessage` already executes `rules.Evaluate` / `rules.ApplyOutcome`; a successful `stop` rule bypasses inference. Reuse this engine for deterministic sorting, rather than introducing a second routing engine. Moving its admission boundary is a related improvement to evaluate separately.
- `backend/internal/adapters/imap/client.go`: `Client.FetchRawMessage(ctx, mailbox, uid)` supplies bounded raw RFC 5322 bytes. `ApplyInboxAction(..., "spam", ...)` resolves the account's special-use Junk mailbox. This action currently moves the message; do not assume it also sets `$Junk`.
- `backend/internal/api/server_inbox.go`: `handleInboxActions` is the implementation behind `POST /api/inbox/actions`; the previously suggested `server_inbox_actions.go` path does not exist.
- `backend/internal/mailmsg/limits.go`: shared 25 MiB inbound limit and explicit oversized errors already exist. Reuse bounds; no truncated scan may be recorded as a complete scan.
- `backend/internal/processor/phish_scan.go`: the shipped `$Phishing` flag is a narrow app-impersonation verdict, with an existing DKIM verification exception. Keep generic spam scores separate from that contract.
- `backend/internal/config/config.go`: code defaults are 90-second polling, 10 messages/minute, 20/hour per user. These are not verified active deployment settings. Verify actual settings before attributing all delay to inference; do not silently raise limits.
- `backend/internal/adapters/classifier/http_client.go`: temperature is already zero, default inference concurrency is one, and concurrency is configurable. A mailbox's processing loop remains sequential.
- `docs/LAYA_SIX_LABEL_EVALUATION.md`: Ollama warm p50/p95 10,245/11,503 ms; English Laya 1,023/1,151 ms; multilingual 335/410 ms, on a shared host. Laya failed the quality gate (about 40–41% accuracy). These synthetic results do not validate a production spam filter, calibrated confidence, or a replacement model.

## Proposed deployment and flow

Use an optional Compose overlay/profile following existing deployment conventions. Pin Rspamd and Redis tags plus manifest digests; verify amd64/arm64 support at implementation time. Persist required data and learning state. Keep scan/controller/Redis ports unpublished, isolate access from the externally facing proxy, remove default controller credentials, set resource limits and healthchecks, and grant only required filesystem/network access. Rspamd needs DNS/network access for enabled reputation checks; private service access does not mean an egress-disabled network. Check DNSBL resolver eligibility and error handling.

```text
Unread IMAP message → bounded Rspamd scan
  shadow mode: record result, leave existing behavior intact
  enforcement mode:
    high-confidence spam → reversible Junk move, skip LLM
    suspicious → advisory result, retain visibility
    no spam verdict → existing user rules → LLM when needed
    unavailable/inaccessible → explicit unscanned status
```

Submit original readable raw MIME via HTTP `/checkv2`, not the redacted/truncated classifier body. Bound request time, scan concurrency, response size, and parsed result fields. Use standard Go HTTP/JSON support unless an installed helper already covers the need. Scanner actions are recommendations: map them into KyPost's post-delivery policy; SMTP reject/greylist actions cannot be executed against an already delivered message.

Implement disabled/shadow/enforce behavior without silently enabling enforcement. Proposed outage policy: retain mail, record unscanned status and degraded scanner health, and allow ordinary inbox access/sorting to continue. Keep a bounded retry path independent of the ordinary processed marker so a transient scanner outage does not permanently retire screening. Confirm the final policy in review before deployment.

For Junk moves, preserve existing mailbox resolution, retry/error handling, cache invalidation, unread state, and processed/checkpoint rules. A move may succeed before the audit write fails: the source UID is then gone. Trace existing terminal-action recovery and explicitly handle ambiguous successes; do not report a durable verdict for an action that failed. Avoid repeated scans and duplicate learning on ordinary polls/retries. Key stored results using the repository's mailbox/UIDVALIDITY identity conventions, not a globally unique assumption about a numeric UID or sender-supplied Message-ID.

## Trust, privacy, and learning

- IMAP polling happens after delivery. Another client or the warmed cache may expose mail before filtering. The unread-INBOX/checkpoint scope also misses mail read or moved elsewhere first. Document this coverage; expand ingestion scope only as a separate deliberate design change. Pre-delivery protection requires integration at the receiving MTA/provider.
- Do not trust arbitrary `Authentication-Results`, `Received-SPF`, or `Received` headers. Identify trusted receiver boundaries before using upstream authentication/connection metadata. Missing SMTP envelope/IP evidence is unknown; never substitute the sidecar caller's IP or a sender-controlled hop. A DKIM signature header is not a verified signature. DMARC policy and authentication result are different; authentication alone does not establish harmless content.
- PGP-encrypted content cannot be scanned by this sidecar without changing the custody contract. Record content as unscanned; do not decrypt server-side or infer that it is safe.
- Store user-facing score/reason/status in the owning user's data, with a defined retention policy. Instance-wide logs use the shared JSON logger and operational identifiers, never message bodies, subjects, addresses, or revealing matched-symbol options. Audit scanner logs, controller history, Redis data, backup needs, and retention too.
- Reputation queries can disclose domains from mail. Document enabled network lookups and related privacy trade-offs in operator-facing documentation. Do not send full mail or URL tokens to external services by default.
- Feedback must come from authenticated explicit Spam/Not spam actions. Train from the correct message bytes, acquired before a move invalidates its source UID. Prevent duplicate retry learning and isolate user statistics; verify Rspamd's per-user key/configuration behavior rather than assuming a username header alone isolates training. Generic folder moves are not automatically spam training.
- Never delete mail or strip originals automatically. Suspicion is advisory, distinct from the existing deterministic `$Phishing` contract. Unknown scan results, DNSBL errors, and scanner outages must never become clean verdicts.

## Implementation sequence and acceptance

1. Re-read the root and all applicable child AGENTS files, CONTRIBUTING, and existing mailcache/terminal-action tests. Verify deployment configuration and intended branch/base. Confirm supported Rspamd protocol, image version/digests, Redis persistence, and per-user learning configuration.
2. Add private Rspamd + Redis deployment and a bounded adapter with scanner health reporting. Integrate shadow scans independent of LLM quotas. Document scan states and outage/retry behavior before enabling enforcement.
3. Validate with private, human-adjudicated real-mail examples. Do not commit personal correspondence or post it to myslop. Measure spam precision/recall, false positives, scan coverage, timing, and pending age; synthetic tests exercise plumbing only. Choose enforcement thresholds from evidence, not a guessed score or LLM confidence.
4. Add reversible high-confidence Junk routing and explicit correction learning. Roll out as an operator-selected mode after review/approval; no automatic mode promotion from elapsed time.
5. Load-test one and ten accounts, including bursts, slow DNS, scanner/Redis outages, and one noisy mailbox. Report arrival rate, scan/classification throughput, p50/p95 processing delay, oldest pending age, memory/CPU, and LLM calls avoided. No millisecond target or bypass percentage is established yet.

Required checks: adapter unit tests for malformed/oversized/non-finite/missing results, timeout and non-2xx responses; integration against real pinned Rspamd/Redis with readable ham/spam test fixtures; regressions for scans beyond the LLM quota, failed/ambiguous Junk moves, cache consistency, encrypted/oversized mail, cancellation, duplicate retries/learning, and cross-user isolation. Verify persistence across restarts and no externally exposed service ports. Run existing relevant Go checks and CI. Obtain the required security reviewer-agent pass; surviving findings belong in the PR description.

Update owning AGENTS contracts, README Features/Environment Variables/Project Structure and applicable API highlights, CHANGELOG, security/privacy guidance, and client contracts whenever new exposed status/actions affect them. Define restore/export coverage for new learning state; do not imply existing sealed backups include new volumes. Disable integration/remove the optional overlay to roll back while preserving originals and learning data; previous Junk moves remain and require explicit restoration.

Out of first scope: ClamAV/archive inspection, automatic sender-domain routing, replacing Ollama with Laya, rebuilding phishing detection, bundling an MTA, or changing classification limits without evidence. Dedicated sorting accuracy/fair scheduling work remains useful; a spam sidecar only avoids LLM work for messages it diverts.

## Sources and closeout

- [Official image and volume requirements](https://docs.rspamd.com/downloads/)
- [HTTP scanning protocol](https://docs.rspamd.com/developers/protocol/)
- [Redis and learning requirements](https://docs.rspamd.com/getting-started/)
- [Rspamd features](https://docs.rspamd.com/about/features/)
- [Authentication-Results trust boundary](https://www.rfc-editor.org/rfc/rfc8601.html)
- [Current DMARC specification](https://www.rfc-editor.org/rfc/rfc9989.html)

DOX pass: this document is root-owned under the existing `docs/` boundary. AGENTS files, README and shipped client contracts remain unchanged because this handoff describes proposed work and changes no runtime contract. CHANGELOG records the documentation artifact only. No runtime tests are warranted for this documentation-only turn; validate the file and exact myslop mirror before closeout.
