**Repo:** Busnes-app/KyPost-Server
**Worktree:** /home/yoshi/git/busnes.app/kypost-server (branch feat/laya-evaluation)

# Six-label classification evaluation — 2026-09-28

The user requested a rerun with Spam and Unsure. This experiment reruns the same three model configurations with six allowed labels, preserving all 60 original messages and expected labels and adding ten Spam and ten Unsure cases. The original four-label results remain in [the first report](LAYA_EVALUATION.md).

Spam means clear fraud, phishing, extortion or abusive unsolicited mail and takes priority when that evidence exists. Ordinary promotions and classifier-directed instructions alone are insufficient. Unsure means insufficient readable evidence to determine purpose; it replaces the old Updates fallback. It is a selectable label, not a numerical confidence threshold or an error fallback.

The new cases and expected labels were agent-authored and fixed before inference. They have not received independent human adjudication. Spam cases include credential theft, advance fees, gift-card impersonation, money-mule recruitment and extortion. Unsure cases include empty, unreadable, attachment-only, encrypted and missing-context messages. The encrypted case exercises only the evaluator: the production poller bypasses model classification of encrypted mail.

## Results

| Model | All 80 | Original 60 | Spam / 10 | Unsure / 10 | Macro F1 |
|---|---:|---:|---:|---:|---:|
| ollama | 64/80 (80.0%) | 45/60 | 10 | 9 | 0.811 |
| laya-english | 33/80 (41.2%) | 24/60 | 9 | 0 | 0.320 |
| laya-multilingual | 32/80 (40.0%) | 19/60 | 8 | 5 | 0.382 |

| Model | Spam precision / recall | Unsure precision / recall | False Spam | False Unsure |
|---|---:|---:|---:|---:|
| ollama | 90.9% / 100.0% | 60.0% / 90.0% | 1 | 6 |
| laya-english | 31.0% / 90.0% | 0.0% / 0.0% | 20 | 0 |
| laya-multilingual | 50.0% / 80.0% | 20.8% / 50.0% | 8 | 19 |

All 240 requests completed with no transport/protocol errors. Ollama remains ahead;
this rerun does not pass the Laya migration quality gate. Production remains unchanged.

On the unchanged original 60, Ollama fell from 50 to 45 correct, English stayed at
24, and multilingual fell from 32 to 19. English never selected Unsure and selected
Spam for 20 non-Spam cases. Multilingual selected Unsure for 19 cases with a different
expected label. Its additional abstentions therefore cannot be treated as a quality
improvement. Zero precision is reported by convention when a label was never selected.

Warm p50/p95 request times were 10,245/11,503 ms for Ollama, 1,023/1,151 ms for English,
and 335/410 ms for multilingual. These remain observations on a shared host, not
controlled speedup claims.

False Spam/Unsure counts are assignments of those labels to cases with a different expected label, out of 70 negatives for each. Per-class precision/recall, all predictions, model identity and timing metadata are in the [six-label evidence](../backend/cmd/modeleval/evidence/laya-six-label-2026-09-28.json).

## Method and limits

- [Corpus](../backend/cmd/modeleval/corpus-six-label.json), [Ollama tuning](../backend/cmd/modeleval/TUNING.six-label.md), [Laya rubric](../backend/cmd/modeleval/laya-rubric-six-label.json), and [reproduction commands](../backend/cmd/modeleval/README.md#six-label-experiment) are saved separately from the four-label inputs.
- Same pinned Ollama 0.34.4 / nemotron-3-nano:4b and Laya 0.3.21 English/multilingual checkpoints as the first experiment. Laya bundle revision: `55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851`. Shared production redaction and length limits apply to every provider. All model identities are checked after warmup and again at completion.
- The six Laya option descriptions and instructions fit without truncation or option collapse on both tokenizers at 1024 total / 384 head tokens. The Spam wording was shortened during tokenizer validation before either Laya inference run; it was not tuned against predictions.
- Runs were sequential on the same shared amd64 developer host: Ollama selected ten threads with a 5 GiB container limit, Laya used four threads. Warm request latencies are diagnostic only. There is no new native arm64 or representative real-mail validation.
- The added cases are deliberately obvious examples, especially Unsure. They do not measure calibrated abstention on subtle ambiguity or validate a production spam filter. This is an 80-case synthetic development benchmark with unequal category sizes. Compare the original-60 column with the earlier 60-case scores; do not attribute differences in overall accuracy solely to the added categories.
- Fixture IDs, original-60 equality, gold-label membership, corpus/tuning hashes, summary totals and macro F1 were checked. Both provider dry runs passed. No Go code changed in this rerun; earlier Go test results remain documented in the first report. No production configuration, classifier defaults or mail labels changed.

Evaluation services were stopped after completion; existing model caches were retained. DOX pass: the evaluation README and changelog document the experiment. Owning AGENTS files and deployment/client contracts stay unchanged because this adds experimental inputs and evidence within the existing evaluation boundary. The unrelated CalDAV document remains untouched.

Prepared with OpenAI Codex (GPT-6). This report and the updated migration plan are mirrored to the `kypost-laya-233` myslop folder.
