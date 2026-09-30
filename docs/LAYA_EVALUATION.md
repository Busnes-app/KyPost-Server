**Repo:** Busnes-app/KyPost-Server
**Worktree:** /home/yoshi/git/busnes.app/kypost-server (branch feat/laya-evaluation)

# Laya evaluation — 2026-09-28

## Decision

Keep Ollama in production. Both tested Laya checkpoints fail the approved plan's requirement to match or improve categorization quality. The initial amd64 comparison is complete; the broader phase 1 gates (held-out mail, supported architectures and resource validation) remain unmet. Phases 2–6 are gated. No production provider, container configuration, or stored mail labels were changed.

| Candidate | Correct / 60 | Accuracy | Macro F1 | Core / 40 | Traps / 12 | Injection correct / 8 |
|---|---:|---:|---:|---:|---:|---:|
| Ollama / nemotron-3-nano:4b | 50 | 83.3% | 0.831 | 38 | 8 | 4 |
| Laya English | 24 | 40.0% | 0.347 | 22 | 1 | 1 |
| Laya multilingual | 32 | 53.3% | 0.533 | 25 | 3 | 4 |

All three runs completed without transport/protocol errors. Errors would count against the denominator. Injection-target avoidance was 5/8, 3/8 and 5/8 respectively; avoiding the attacker's requested label is not the same as choosing the correct category. Typed output does not establish injection immunity or calibrated confidence.

The user resolved all four policy cases before final scoring: `core-primary-09` and `trap-09` are Primary, `core-social-09` is Promotions, and `trap-12` is Updates because its main purpose is an account notice. Laya inference preceded this adjudication. Only the job-alert gold label changed; all predictions were rescored against the same final corpus without changing message inputs. Original reports remain preserved alongside adjudicated scores.

## Reproducible evidence

- [Complete public synthetic results and manifests](../backend/cmd/modeleval/evidence/laya-2026-09-28.json): per-case predictions, probabilities, errors, timings, original corpus hashes, final scoring hash, confusion matrices, model artifact hashes and tokenizer diagnostics.
- [Evaluation commands](../backend/cmd/modeleval/README.md), [rubric](../backend/cmd/modeleval/laya-rubric.json), and [runtime reproduction notes and hashed dependency lock](../backend/cmd/modeleval/evidence/README.md).
- [Approved migration plan](LAYA_MIGRATION_PLAN.md) and [original issue #233](https://github.com/Busnes-app/KyPost-Server/issues/233).

The harness now shares the production redaction-before-truncation helper (256/512/2000 runes for sender/subject/body). The Ollama run uses the production client and options. Laya uses an explicit checkpoint, a four-category rubric, 1024 total / 384 head tokens, strict choice/routing/probability validation, and bounded HTTP responses. Reports checkpoint atomically with restrictive file permissions; incomplete runs cannot masquerade as complete results. Historical prompt-matrix mode remains available separately.

Laya runtime 0.3.21 used Python 3.14.7, torch 2.14.0+cpu and transformers 5.17.0, with model bundle `convaiinnovations/laya@55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851`. The 43 installed wheel versions matched the lock and its hash-enforced pip dry run passed. Both checkpoints started offline from cached artifacts. Ollama 0.34.4 used model digest `6cc467f054393a55e98a74098abde0c762ffb6d1d8cd64becf30458f38886197`; the runtime archive was verified against the repository's published SHA-256 pin.

Tokenizer inspection found no rubric truncation or collapsed options. A diagnostic comparison of 512/192 and 1024/384 budgets produced identical sequences for these 60 cases (longest 312 tokens); it used a Python reproduction of default preprocessing, while the scored runs used the actual Go helper. It is diagnostic evidence, not a second inference run.

## Limits and performance observations

These are 60 synthetic development cases, not held-out real mail. They do not establish multilingual, custom-label or native arm64 suitability. No deployment sidecar, rubric migration, canary or cutover was built. The English report predates the final end-of-run identity comparison safeguard; its starting revision was recorded. Multilingual and Ollama also verified identity at completion.

On the shared AMD Ryzen AI 9 365 developer host, warm p50/p95 request times were Ollama 10,410/15,445 ms, English 886/988 ms and multilingual 299/331 ms. Laya used four torch threads; Ollama selected ten physical-core threads with a 5 GiB container limit. These are provisional observations under different resource settings, not controlled speedup claims. An earlier oversubscribed Ollama attempt was interrupted and excluded.

Multilingual process RSS was about 1.71 GiB before scoring and 1.75 GiB afterward, with a 2.30 GiB startup high-water mark. Ollama's cgroup peak reached its 5 GiB limit with no OOM kills; that includes cache and is not comparable to process RSS. A dedicated cold/warm and concurrency benchmark is still required before deployment.

The historical files separate nemotron-3-nano's 53/60 from a nemotron-mini run with 60 errors. Combining them as 53/120 does not describe the current model's accuracy. The issue's Laya 46/60 result lacks matching payload/rubric/runtime/hardware evidence, so the cause of the difference is unresolved. This result rejects these tested configurations for cutover, not every possible Laya configuration.

## Validation and next work

Go 1.26.6 full race tests, build, vet and golangci-lint 2.12.2 passed. govulncheck found no called vulnerabilities (one uncalled module advisory). Focused tests cover preprocessing parity, malformed/invalid Laya responses, errors in scoring, canonical gold labels, atomic report behavior and model identity across warmup/completion. Reviewer-agent findings were fixed and its follow-up found no remaining blockers. Remote CI has not run; no commit or PR was created.

The next useful work is quality diagnosis: recover the earlier Laya experiment's exact inputs/rubric, separate development and held-out cases, and test a revised rubric or checkpoint without tuning on the final test set. Only a passing paired comparison should reopen packaging and production integration. Preserve Ollama and its model cache as the working baseline. Evaluation servers and the task-owned Ollama container were stopped; existing model blobs were retained.

DOX pass: backend and adapter contracts, the operator README, evaluation README and changelog were updated. Root ownership/index and deployment/client contract docs remain unchanged because no new durable ownership boundary, production provider or wire contract was introduced. The unrelated `docs/CALDAV_CALENDAR_SPEC.md` was left untouched.

Prepared with OpenAI Codex (GPT-6); reviewed by a separate agent. Durable evidence is in this repository; this report and the updated plan are mirrored to the `kypost-laya-233` myslop folder.
