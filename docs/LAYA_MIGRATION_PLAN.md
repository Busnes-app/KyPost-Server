# Replace Ollama with Laya for email categorization

**Repo:** Busnes-app/KyPost-Server
**Worktree:** /home/yoshi/git/busnes.app/kypost-server (branch feat/laya-evaluation)

Status: approved 2026-09-28; initial phase 1 amd64 comparison complete.
Both tested Laya checkpoints failed the quality gate (24/60 and 32/60 versus
Ollama 50/60). Production remains on Ollama; phases 2–6 are gated.
See the [evaluation report](LAYA_EVALUATION.md) for evidence and remaining gates.
The requested [six-label rerun](LAYA_SIX_LABEL_EVALUATION.md) also failed the gate:
Ollama 64/80, Laya English 33/80, multilingual 32/80. Spam and Unsure remain
experimental benchmark categories; production defaults are unchanged.
Prepared 2026-09-28 against KyPost commit `7ba03abae3583f409670d74a1470a1677cc13081`.
Request and prior experiments: [issue #233](https://github.com/Busnes-app/KyPost-Server/issues/233).
Starting article: [Laya overview](https://huggingface.co/blog/sora-2/laya-ai-model-how-it-works-run-it-locally-and-eval).

## Recommendation

The initial evaluation does not authorize a cutover. Improve and re-evaluate
categorization quality before proceeding with this conditional target design.

Replace Ollama with a pinned, CPU-first Laya sidecar after a reproducible evaluation and an opt-in rollout. Keep Go responsible for redaction, label policy, retries, IMAP writes, and audit state. Use the upstream Python HTTP server; defer ONNX, a Go inference binding, fine-tuning, automatic language routing, and confidence-based triage until measurements justify them.

The end state has one local classification provider, Laya. Retain Ollama temporarily as an explicitly selected rollback option, never an automatic fallback. Start evaluation with both English and multilingual checkpoints; prefer a single explicit multilingual checkpoint for the multilingual product if it passes quality and resource gates. Do not load both by default.

## Evidence and corrections to issue #233

- The issue reports Laya at 46/60 correct and approximately 554 ms average CPU latency, versus Ollama at 53/120 correct. These are useful leads, not a controlled comparison: the denominators differ, and the comments do not include a complete reproducible run manifest. Recover original scripts/results if available; otherwise rerun both on the same cases and current production preprocessing. No new performance claim is made here.
- The repository corpus has 60 hand-authored cases: 40 core, 12 traps, 8 injection attempts. The user adjudicated its four policy cases on 2026-09-28: both personal recruiter messages are Primary, the automated job alert is Promotions, and the trial-expiry account notice is Updates. Report injection resistance separately from correct categorization. See [evaluation notes](../backend/cmd/modeleval/README.md).
- Typed outputs constrain the answer space; they do not prevent hostile text from steering selection of a valid wrong category. Keep adversarial cases and privacy boundaries. Do not describe Laya as injection-immune.
- The runtime already includes Python through Debian `supervisor`; the main image is not literally Python-free. A sidecar still isolates PyTorch and its dependencies, preserves `CGO_ENABLED=0`, and avoids an in-process native inference dependency. See [Dockerfile](../Dockerfile).
- Four categories are defaults, not the whole product contract. Accounts have independent allowlists, keyword mappings, and tuning files. The migration must handle those accounts before changing the default.

## Verified Laya integration facts

Research inspected upstream commit `9d955671415fc19f069b9cc998928075c1f255ec` (package metadata version 0.3.21) and model bundle revision `55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851`. These are candidate evaluation pins, not approved deployment artifacts.

| Concern | Verified behavior and planning consequence |
|---|---|
| Runtime | Python >=3.10, Apache-2.0. Dependencies use open version ranges; pinning only `laya` is insufficient. Lock and hash the dependency closure. [Package metadata](https://github.com/NandhaKishorM/laya/blob/9d955671415fc19f069b9cc998928075c1f255ec/pyproject.toml) |
| HTTP | `laya-serve` provides `POST /v1/systemone` and `GET /health`. `/predict` is a playground endpoint. Preload completes before bind, but health does not execute inference. [Server](https://github.com/NandhaKishorM/laya/blob/9d955671415fc19f069b9cc998928075c1f255ec/laya/serve.py) |
| Response | Read `answers.category.choice` and `probabilities`. `confidence` is normalized entropy; `answer_confidence` is the maximum probability. Probabilities are rounded. Neither field establishes calibration on email. [Agent](https://github.com/NandhaKishorM/laya/blob/9d955671415fc19f069b9cc998928075c1f255ec/laya/agent.py) |
| Context | English defaults to 512 total / 192 head tokens; multilingual and typed-decisions default to 1024 / 256. Instructions and options share the total budget with the email. [Model configs](https://huggingface.co/convaiinnovations/laya/tree/55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851) |
| Truncation | Option descriptions initially cap at 48 tokens and share a further head budget. Dictionary state loses its tail when too long. `usage.options` exposes collapsed option representations, not a complete input-truncation report. [Sequence construction](https://github.com/NandhaKishorM/laya/blob/9d955671415fc19f069b9cc998928075c1f255ec/laya/common.py) |
| Routing | Default checkpoints are subfolders of one `convaiinnovations/laya` bundle. Unknown request model names can auto-route. `LAYA_MODELS` controls preload, not an allowed-model list. Send a validated explicit model. [Router](https://github.com/NandhaKishorM/laya/blob/9d955671415fc19f069b9cc998928075c1f255ec/laya/router.py) |
| Pins | `LAYA_REVISION` accepts an explicit commit; `LAYA_SHA256_DIGESTS` supplies artifact checksums. Prefetch tokenizer/config/encoder artifacts as well as weights. [Revision handling](https://github.com/NandhaKishorM/laya/blob/9d955671415fc19f069b9cc998928075c1f255ec/laya/revisions.py) |
| Hardware | Upstream has a CPU amd64/arm64 Docker recipe, but its base and dependency set are not fully pinned. Native architecture tests and KyPost-host benchmarks remain necessary. [Dockerfile](https://github.com/NandhaKishorM/laya/blob/9d955671415fc19f069b9cc998928075c1f255ec/Dockerfile) |

The server serializes inference, defaults to 16 admitted requests, and returns 503 with `Retry-After` when full. Authentication is optional upstream and binding defaults to all interfaces. Its HTTP endpoint does not forward SDK `min_confidence`; any future abstention policy belongs in Go. See the server source above.

## Existing flow that must survive

[Application setup](../backend/internal/app/app.go) constructs `*classifier.HTTPClient` for daemon/server modes. Docker runs them as separate processes; their admission counters are not shared. [The poller](../backend/internal/processor/poller.go) then:

1. Fetches bounded IMAP messages and applies user filter rules; a stop action bypasses classification.
2. Uses the existing fallback-label behavior when automatic labeling is disabled or the message is encrypted.
3. Redacts sender, subject, and body before truncating to 256, 512, and 2000 runes respectively.
4. Sends the account's allowlist and tuning to the classifier, then applies its keyword mappings.
5. Records per-user decisions and processed state, with retry behavior for classifier and IMAP failures, and sends existing notifications.

Preserve these steps, tenant isolation, unread handling, and backup/shutdown behavior. Never decrypt PGP mail for categorization. Preserve cancellation-aware admission, response size bounds, same-origin redirects, and content-free logs in [the adapter](../backend/internal/adapters/classifier/http_client.go). Current URL validation logs a policy failure but does not block startup; do not accidentally claim it already enforces rejection. Keep destination configuration operator-only.

## Implementation sequence and acceptance gates

### 1. Reproduce the comparison before selecting the checkpoint

Extend `backend/cmd/modeleval` with a small Laya request path, reusing the corpus and result reporting. Make production and evaluation use the same Laya request builder; compare against current production Ollama options and redaction, not an old prompt matrix aggregate. Add a preprocessing parity check so the harness cannot silently send raw sender/body data while production sends redacted data.

Run English and multilingual on native CPU amd64 and arm64. Save a manifest containing server/package/model/tokenizer revisions, dependency lock, corpus hash, rubric and token budgets, CPU/thread counts, memory limits, and cold/warm conditions. Record each case's label, error, latency, and probability distribution in local evaluation artifacts. Use public synthetic fixtures for reproducibility; representative private mail, if supplied, stays local and out of Git/logs.

Use a held-out, human-labeled sample beyond the 60 examples, split by thread/sender to avoid duplicates across tuning and final tests. Include non-English/code-switched mail, short messages, long bodies/subjects, HTML-derived text, Unicode, custom labels, overlapping names, and hostile instructions. Select token budgets on development cases, including category descriptions and tie-breaking criteria; do not paste all of `TUNING.md` into a 512-token request.

**Proposed cutover gates, fixed before the final run:**

- Paired overall accuracy and macro-F1 at least match Ollama; no regression in the number of correctly classified cases in any existing core/trap/injection bucket. Report per-class precision/recall and language slices with sample counts and uncertainty.
- Zero unknown labels, missing answers, or collapsed options on the supported test matrix. Zero tenant-mixing or redaction failures.
- Warm p95 classification latency at most half the measured Ollama p95 on the same host, with cold-start and queue latency reported separately.
- Peak total deployment memory lower than the equivalent Ollama deployment, with no OOM/restart under concurrent mailbox load. Set container limits from measured headroom, not the issue's suggested 3–4 GB.
- Offline startup, restart, and inference work with the complete pinned artifacts on both architectures.

Stop the default switch if a gate fails. Keep the opt-in implementation available for tuning; do not hide poor slices in an aggregate. Fine-tune only after the failure analysis shows that rubric/context changes are insufficient.

### 2. Package a local Laya service

Add a small `laya/` build boundary with its own `AGENTS.md`, pinned CPU image/dependency lock and model manifest. Build from upstream, not the issue's unverified `:latest` image. Bake the chosen checkpoint into the released image initially: this removes first-boot model downloads and makes rollback one image digest. Prefetch/repair tokenizer files during build and verify inference with read-only model artifacts and network disabled. Include license/notice material.

In Compose, put KyPost and Laya on a dedicated internal classification network; KyPost retains its normal mail/network access. Do not publish the inference port or attach Laya to the external proxy network. Run Laya non-root with bounded memory/CPU, no Docker socket, no unnecessary mounts, and a read-only root filesystem where verified. Model artifacts are readable, never writable by the mail-facing Go process. Use one explicit checkpoint and `LAYA_DEVICE=cpu`; do not rely on silent device fallback. Gate the existing supervisor Ollama and model-pull programs on the selected provider now, before canaries or memory comparisons; their final deletion can wait until phase 6.

Run a warm inference probe before reporting classifier readiness. Separate service liveness from model readiness and preserve KyPost's ability to serve mail when categorization is unavailable: no hard Compose dependency that prevents the API from starting. Keep `classifierFailing` visible and arrange recovery checks after a restart; readiness cannot remain cached forever after service loss. Preserve the post-warmup unread sweep's cancellation behavior.

**Gate:** clean build, startup with networking disabled after image acquisition, real inference, sidecar kill/restart recovery, and overload behavior on both architectures. No model or email data in service logs.

### 3. Add Laya to the existing classifier boundary

Keep `HTTPClient.Classify`, `Warmup`, `Stats`, and lifecycle wiring usable by current callers. Add a small provider branch and Laya transport in the existing package; no provider registry or extra inference framework. Proposed transition config is `CLASSIFIER_PROVIDER=ollama|laya`, `CLASSIFIER_BASE_URL`, and `CLASSIFIER_MODEL`. Default remains Ollama until gate 5. Validate explicit model names rather than allowing upstream auto-routing.

For Ollama mode, retain current `OLLAMA_*` behavior during the transition. For Laya mode, use only the canonical classifier variables: an old Compose-injected `OLLAMA_BASE_URL` must not win precedence. Emit a safe deprecation warning for ignored Ollama variables. Keep the Laya route fixed at `/v1/systemone`; do not inherit `OLLAMA_GENERATE_PATH`. Document external authenticated endpoint handling before supporting it; the default internal service needs no new credential workflow.

Send redacted `state: {sender, subject, body}` plus one `questions.category` choice question whose keys are exactly the account's current allowlist. Validate request label count/size and rubric limits before inference. Validate bounded responses, answer presence, exact label membership, probability keys/ranges, rounding-tolerant normalization, and collapsed-option metadata. Identify the checkpoint through `routing.model` and verify health's loaded revision against the manifest; the outer `model` value is a shared runtime identifier, not the checkpoint. Return the canonical selected label. Do not run free-text substring extraction on Laya output.

Keep `(string, error)` initially; collect probabilities in evaluation, without adding user-visible confidence or a new database schema. Use the existing poller retry loop for Laya transient failures, avoiding nested generation-repair retries. Respect cancellation and bounded `Retry-After`; do not retry schema/auth errors per message.

**Critical error change:** `NoAllowedLabelError` and errors containing `422`/`invalid input` currently can cause mail to be retired. A malformed Laya response, unsupported rubric, unavailable model, or sidecar schema mismatch is a classifier/configuration fault, not successfully processed mail. Add explicit error handling so these failures do not retire the message; pause the affected classification work with an actionable status and resume after repair. Check both immediate error handling and the poller's repeated-deferral cap. Do not change the deliberate empty-allowlist or disabled-labeling behavior.

**Gate:** HTTP contract tests plus a live pinned Laya integration test, and poller regressions proving no IMAP write or processed marker on infrastructure/configuration faults. Exercise 429/503, timeout, cancellation, invalid JSON, oversized replies, missing/unknown labels, stale readiness, and cross-origin redirects. Preserve privacy and admission-control tests.

### 4. Migrate labels, tuning, and operator surfaces

Introduce concise structured category descriptions and decision instructions in the existing per-user settings, through the current authenticated settings patterns. Keep the allowlist authoritative and keyword mappings unchanged. Carry the semantic tie-breaks from `TUNING.md` into descriptions/instructions: human correspondence, social security alerts, marketing from established providers, and receipts with offers still need those distinctions.

Seed stock definitions only for accounts using stock labels and unmodified shipped tuning. Preserve each original `tuning.md` byte-for-byte for rollback. Custom labels, custom install defaults, and arbitrary Markdown cannot be losslessly converted by a general parser: provide a migration preview/editor, require explicit review of their structured rubric, and identify unsupported configurations before enabling Laya for that deployment. Keep Ollama selected until all affected accounts are ready; do not silently ignore their customization or seed a deliberately empty allowlist.

Bound label counts/descriptions/instructions according to the evaluated token budget, and reject over-budget edits visibly rather than truncating policy silently. Upstream has no budget-validation HTTP endpoint: add one small internal `/validate-rubric` route in the packaged sidecar, reusing its pinned tokenizer and sequence-construction logic to report policy truncation, collapsed options, and remaining state budget. The Go settings handler calls it before accepting a rubric; a validator outage leaves existing settings untouched and returns an actionable error. Do not duplicate tokenization in Go or substitute a byte/character heuristic. Preserve per-user file permissions and atomic cross-process updates. Include the additive settings in restore drills and keep legacy tuning endpoints usable during the transition.

Update Prompt Tuning and System Health to show the selected classifier, readiness, model/revision, and migration action. Do not label entropy as accuracy, create an Uncertain mailbox, or install a `confidence < 0.20` rule. Abstention requires held-out calibration and an explicit product policy; it is deferred.

**Gate:** default/custom/empty-label migration, cross-user access tests, save/reload/restart, rollback to old tuning, and backup/restore. Existing client contracts remain compatible; update cross-repo docs if any public settings/status response changes.

### 5. Roll out and switch the default

Start with local replay evaluation; do not build permanent dual-inference machinery. Use opt-in canaries after the offline gates pass, inspect user-confirmed misclassifications and operational metrics, then obtain the required human approval for the production/configuration switch. Approval is for the tested release and measured limits, not for this planning document.

Fix `scripts/update-host.sh` before sidecars reach supported installs: it currently requires exactly one Compose image and rolls back only `kypost-server`. Publish an attested Laya image alongside KyPost, pin a compatible pair, and update/rollback both as a release unit. Extend its existing tests for second-image validation, either-service failures, previous-pair restoration, and model readiness. A green API health check alone cannot certify this migration. Keep the updater host-only.

Change the Compose default to Laya only after the gates and rubric migration pass. Keep Ollama as an explicit, temporary rollback profile for one stable release cycle, without running it alongside Laya by default. Log provider, model revision, readiness, queue/error counters and latency without correspondence contents. Start concurrency at 1; measure before raising it, accounting for separate API/daemon clients and the server's serialized worker.

**Rollback:** retain prior image digests, Compose/env configuration, original tuning, and the Ollama model cache. Stop classification work, restore the previous compatible image/config pair and provider, verify readiness, and resume pending work. Do not automatically erase model caches, reset processed checkpoints, or relabel historical mail. Reversing labels already applied is a separate user-scoped correction task.

### 6. Remove Ollama after the rollback window

Delete the Ollama payload and env defaults from the main Dockerfile, supervisor programs, startup/pull scripts and their obsolete tests, pull/warmup protocol, generation-output repair code, Ollama release polling/notifications, and `.github/workflows/ollama-bump.yml`. Retain general transport guards, redaction, label validation and classifier observability. Do not rewrite stored decision history or destructively drop legacy persisted fields merely to remove unused runtime code.

Remove the transitional provider toggle once only Laya remains. Keep old Ollama blobs operator-owned with a documented manual cleanup; never delete them during startup/upgrade. Preserve `crashexit`, bounded restart attempts for API/daemon, data ownership, TLS/proxy gates, and the existing coordinated backup shutdown timings.

## Change map and verification

| Area | Expected paths and work |
|---|---|
| Evaluation | `backend/cmd/modeleval/`: paired comparisons, preprocessing parity, pinned results and manifests |
| Adapter/application | `backend/internal/adapters/classifier/`, `backend/internal/app/`: provider transition, typed transport, readiness |
| Processing | `backend/internal/processor/`: error retirement/deferral handling; existing keyword, PGP, notification and retry regressions |
| Settings | `backend/internal/config/`, `backend/internal/api/server_settings.go`, frontend Prompt Tuning: structured rubric and migration; preserve backups |
| Observability | `backend/internal/api/ollama_version.go`, `backend/internal/ollamaupdate/`, frontend System Health and admin Logs: retire Ollama-specific monitoring and expose actual Laya readiness |
| Deployment | New `laya/`, root Dockerfile/Compose/supervisor/env example, `scripts/`, `share/AGENTS.md`: package, permissions, rollback pair and cache cleanup docs |
| CI/release | `.github/workflows/ci.yml`, image publication, updater tests, retire Ollama bump workflow: native architecture smoke and pinned artifacts |
| Documentation | README Features/API Highlights/Environment Variables/Project Structure, `TUNING.md`, `LOGGING.md`, `SECURITY.md`, `CHANGELOG.md`, `docs/RESTORE.md`, applicable AGENTS chains; `CONTRIBUTING.md` when CI enforcement changes |

For each implementation slice, run affected Go tests and `go vet`; then the existing full CI suite for merge. Run frontend checks when settings change, shell/updater tests for packaging, Compose validation with explicit `KYPOST_BIND`, and live model integration on supported architectures. Obtain a reviewer-agent security/adversarial pass for boundary and packaging changes; record surviving findings in the PR description per the contribution contract. Preserve required structured task/update logs with timestamp, actor, task ID, action, target, severity, result, and correlation ID.

Planning verification: traced current source, read issue body/comments, inspected pinned upstream source, checked local document links, and completed a read-only reviewer-agent pass. Its three findings were incorporated: gate bundled Ollama startup before rollout, provide a concrete tokenizer-validation route, and identify checkpoints through routing/revision metadata. At planning time no runtime or model changes were made. After approval, phase 1 added the evaluation path and isolated model runs; it does not enable Laya in the mail poller. Hardware quality, calibration, and automatic conversion of custom tuning remain rollout gates.

DOX ownership: `docs/` is root-owned. Phase 1 updates the backend and adapter AGENTS contracts, evaluation README, operator README and changelog alongside the shared preprocessing and evaluation tool. Later phases retain their documentation obligations above. Mirror this full plan and the execution report to the `kypost-laya-233` myslop folder.
