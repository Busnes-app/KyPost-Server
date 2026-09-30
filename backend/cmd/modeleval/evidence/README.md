# Laya migration evaluation evidence

Only public synthetic-corpus results belong here. Private-mail runs remain in
the ignored `../results/` directory and must not be copied into this directory.

`laya-cpu-amd64-requirements.txt` pins the exact 43 wheel artifacts used for the
2026-09-28 evaluation. It is a CPython 3.14/Linux amd64 CPU experiment lock,
not the production sidecar or an arm64 lock. All versions were compared with
the installed environment; `pip install --dry-run --require-hashes -r ...`
passed. Install into a fresh virtual environment, never the system Python.

The model snapshot is separately pinned to
`convaiinnovations/laya@55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851`.
The English checkpoint uses the snapshot root; multilingual uses its
`multilingual/` subfolder. Download the complete config, encoder, tokenizer,
and weight artifacts, not weights alone. With the locked Hugging Face Hub
version, offline startup also requires its cached `trees/<revision>.json`.

Start one checkpoint at a time after caching it:

```sh
HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 HF_HUB_DISABLE_TELEMETRY=1 \
LAYA_HOST=127.0.0.1 LAYA_PORT=18000 LAYA_DEVICE=cpu \
LAYA_MODELS=english LAYA_PRELOAD=1 LAYA_MAX_LOADED=1 \
LAYA_REVISION=55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851 \
LAYA_THREADS=4 OMP_NUM_THREADS=4 LAYA_MAX_CONCURRENT=1 \
  /path/to/venv/bin/laya-serve
```

Use `LAYA_MODELS=multilingual` for the second run. Run the Go commands in
the parent [README](../README.md) with the same checkpoint and revision.
Record model file hashes and startup details in `-environment`, and keep
timing/resource conclusions separate from categorization quality. The
benchmark's four adjudicated cases are documented in the corpus itself.

The [2026-09-28 evidence](laya-2026-09-28.json) preserves all three complete
reports and scores them against the user-adjudicated corpus. See the
[evaluation decision](../../../../docs/LAYA_EVALUATION.md) for results and limits.

The [six-label rerun](laya-six-label-2026-09-28.json) retains all 240 predictions,
six-class metrics, original-60 changes and both tokenizer audits. See the
[six-label report](../../../../docs/LAYA_SIX_LABEL_EVALUATION.md) for interpretation.
