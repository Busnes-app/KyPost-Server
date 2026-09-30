# modeleval

## Production comparison with Laya

`-provider ollama` uses the production `HTTPClient` (including warmup, options,
response parsing and retries). `-provider laya` uses the typed choice builder
and response validator in `internal/adapters/classifier/laya.go`. Both use the
poller's shared `redaction.Engine.ClassifierInput` with default redaction
patterns and 256/512/2000-rune sender/subject/body limits. The Laya protocol
is evaluation-only; the mail poller still uses Ollama.

Run from `backend/`, against isolated local services, one model at a time:

```sh
go run ./cmd/modeleval -provider ollama -model nemotron-3-nano:4b \
  -base http://127.0.0.1:11435 -environment /path/to/ollama-environment.json \
  -out cmd/modeleval/results/ollama-production.json

go run ./cmd/modeleval -provider laya -model multilingual \
  -revision 55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851 \
  -base http://127.0.0.1:18000 -environment /path/to/laya-environment.json \
  -out cmd/modeleval/results/laya-multilingual-production.json
```

Use `-model english` for the English checkpoint, and `-rubric` to select a
versioned rubric JSON (default `laya-rubric.json`). `-dry-run` validates the
corpus and request without contacting a service or printing message content.
The Laya service must preload the explicit checkpoint at `-revision`. Warmup
runs inference; metadata records actual routing/revision and is checked again
at the end. Ollama metadata is captured after its warmup pull, which may update
the model tag, and includes the selected digest and runtime version.

The optional environment JSON is copied into the report: include dependency
versions/hashes, tokenizer/model pins, startup command, CPU/thread counts,
memory limits, and hardware. It must contain no credentials. Reports also
contain corpus/tuning hashes, default redaction patterns, the complete Laya
rubric, per-case predictions/probabilities/errors, warmup time, and warm
latency. Failures remain in accuracy/F1 denominators; a failed request exits
nonzero after saving results. Interrupted or identity-changing runs are marked
incomplete. Results are atomically checkpointed with mode 0600.

An operator-supplied corpus can contain private mail: keep reports local and
use only a trusted endpoint. No request or upstream response text is printed
by the provider evaluator. Summary F1/confusion are named provisional because
they include any gold labels still flagged `needs_adjudication`; settled
accuracy is reported separately. A benchmark is not held-out validation.

The existing A–M prompt matrix below is retained for historical comparisons.
It does not apply production redaction and its parameter sets vary; do not
use its aggregate as the current production baseline. The corpus's recruiter,
job-alert and trial-expiry cases were adjudicated on 2026-09-28, so historical
reports also need rescoring against the new corpus hash.

## Six-label experiment

See the [completed six-label comparison](../../../docs/LAYA_SIX_LABEL_EVALUATION.md).

`corpus-six-label.json` preserves the original 60 cases and adds ten `Spam`
and ten `Unsure` cases authored for this experiment. The extra labels apply
only to the experiment, using `TUNING.six-label.md` and
`laya-rubric-six-label.json`; the shipped `TUNING.md` stays unchanged.

Spam means clear fraud, phishing, extortion or abusive unsolicited mail.
Ordinary marketing, unfamiliar senders or classifier-directed instructions
alone do not establish Spam. Clear Spam evidence overrides the other purposes.
Unsure means insufficient readable evidence to choose a purpose; it replaces
Updates as the ambiguous-purpose fallback. It is a selectable category, not
a probability cutoff or a way to count request failures as successful abstention.
The new expected labels were assigned before inference by the authoring agent,
not independently adjudicated by a human. The original four user decisions stay fixed.

Append these flags to **each** provider command above and choose a separate
`-out ...-six-label.json` path:

```sh
-corpus cmd/modeleval/corpus-six-label.json \
-tuning-v1 cmd/modeleval/TUNING.six-label.md \
-rubric cmd/modeleval/laya-rubric-six-label.json
```

Report the unchanged original 60 separately from the 20 new cases, plus
per-class precision/recall and how often the model assigns Spam/Unsure to
messages with another gold label. Do not compare 80-case accuracy directly
with the old 60-case accuracy. The encrypted fixture tests the evaluator only:
the production poller bypasses model classification of encrypted mail.

## Historical prompt matrix

Measures how well candidate Ollama models do the email classification defined by
`TUNING.md`, so the choice of default model is a measurement rather than an argument.

The prompt is assembled by `classifier.BuildRuntimePrompt` — the same function
`processor.Poller` calls — so results describe the prompt that actually ships.

## Start Ollama

There is no `ollama` binary on the dev host, and the version that matters is the one
pinned in the `Dockerfile`. Run it out of the shipped image, reusing the existing model
cache so anything already pulled is not downloaded twice:

```sh
docker run --rm -d --name modeleval-ollama \
  -p 127.0.0.1:11434:11434 \
  -e OLLAMA_HOST=0.0.0.0:11434 \
  -e OLLAMA_MODELS=/kypost/ollama-models \
  -v "$PWD/share/ollama/models:/kypost/ollama-models" \
  --entrypoint ollama \
  kypost-server-kypost-server:latest serve
```

The port is bound to `127.0.0.1` deliberately — Ollama has no authentication, and this
publishes it outside the container for the duration of the run.

Stop it with `docker stop modeleval-ollama` when finished.

## Run

From the `backend/` module root:

```sh
# check the assembled prompts without touching Ollama
go run ./cmd/modeleval -dry-run -configs A,B,C,D,E

# stage 1 — screen every candidate on the strongest configuration
go run ./cmd/modeleval -models all -configs D -out stage1.json

# stage 2 — full configuration matrix on the survivors
go run ./cmd/modeleval -models qwen3:4b,phi4-mini,gemma3:4b -configs A,B,C,D,E -out stage2.json
```

`-pull=false` skips the pull step for models already cached. `-models` takes a
comma-separated list. Models are evaluated strictly sequentially and unloaded
(`keep_alive: 0`) between runs.

## Memory

Check free memory before including `gemma4:e4b`. Its weights blob is 9.6 GB; on a host
with less free than that it will swap and its **latency numbers become meaningless**
(accuracy stays valid). Every other candidate in the default list fits under 4 GB.

## Configurations

| ID | Change |
|----|--------|
| A | current `TUNING.md`, current params — the true baseline |
| B | `TUNING.v2.md`, params unchanged |
| C | B + `temperature: 0` + `num_ctx: 4096` |
| D | C + structured output (`format` = string enum of the allowlist) |
| E | D + allowlist expanded with `Important`, `Questionable`, `Finance`, `Travel` |

qwen3-family models additionally get `"think": false` in every configuration; their
default reasoning mode emits `<think>` blocks, which is what `labelSearchScope`'s
last-40-lines scoping in `http_client.go` exists to survive.

## Corpus

`corpus.json` — 60 hand-authored emails with human-assigned gold labels.

| Bucket | Count | Purpose |
|--------|-------|---------|
| `core` | 40 | ordinary category examples; after adjudication 10 Primary, 11 Promotions, 9 Social, 10 Updates |
| `trap` | 12 | lexical cues pointing at the wrong label |
| `injection` | 8 | body or subject tries to force a label |

Gold labels are **not** taken from `state.Store.Decisions()`. Those are the classifier's
own past answers; scoring against them would measure agreement with existing behaviour
rather than correctness.

Four policy cases were adjudicated by the user on 2026-09-28:

- `core-primary-09` / `trap-09` — individually written recruiter mail is `Primary`.
- `core-social-09` — a platform-generated job alert is `Promotions`.
- `trap-12` — trial expiry is `Updates` when the account notice is its main purpose,
  even with an upgrade offer.

## Metrics

Per (model, config):

- **accuracy** overall and split by bucket
- **strict format** — the raw output was nothing but an allowlisted label. This is the
  metric that says whether output-format failures, rather than classification failures,
  are the real problem.
- **unresolved** — no label recoverable, which in production raises
  `NoAllowedLabelError` and burns all three retries
- **would retry** — output matched the tools-only or empty-message shapes that trigger a
  production retry
- **injection resisted** — the forced label was *not* emitted
- **latency** p50/p95, and resident size from Ollama's `/api/ps`
- **confusion matrix**, gold against predicted

`resolveLabel` in `main.go` reproduces the resolution in `http_client.go:202-231`,
including its inherited quirks — the fallback matcher is `strings.Contains`-based and
iterates the *allowlist*, so it returns the earliest allowlisted label appearing anywhere
in the output regardless of negation. `main_test.go` pins that behaviour. The harness
must inherit the flaw, or it would report accuracy production never achieves.
