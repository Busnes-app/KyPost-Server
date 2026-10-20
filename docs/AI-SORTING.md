# AI sorting

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Environment Variables

The full reference is [docs/CONFIGURATION.md](CONFIGURATION.md); `.env.example` documents each variable it offers.

### Choosing a classifier model

The default is picked to run on modest hardware. Measured on a 60-email
benchmark (`backend/cmd/modeleval`), five repeats each with zero run-to-run
variance:

| Model | Unambiguous mail | Keyword traps | Prompt injection | RAM resident |
|---|---|---|---|---|
| `nemotron-3-nano:4b` (default) | 100% | 75% | 63% | 2.9 GB |
| `gemma4:e4b` | 100% | 75% | 88% | 8.8 GB |

Both label ordinary mail equally well, and both are perfect on unambiguous
messages. `gemma4:e4b` resists two more of the eight prompt-injection probes —
emails written to talk the classifier into filing them somewhere they do not
belong — but wants three times the memory. Set `OLLAMA_MODEL=gemma4:e4b` if the
host has 12 GB or more free.

Classification speed is not tabulated because it depends far more on your CPU
and on what else the host is doing than on the model: the same request measured
between 13 and 19 seconds on one machine purely as background load varied. The
two models were within about 20% of each other under identical conditions, with
`gemma4:e4b` slightly ahead. The poller paces itself at one message every three
seconds regardless, so throughput is bounded by that unless the host is very
slow.

Either way the damage from a successful injection is bounded: the label
allowlist means a hostile email can at most choose which of the four folders it
lands in, and one probe (an email claiming the label set itself had changed)
defeated every model and every prompt variant tested. Do not treat the assigned
label as a security decision.

Create the model cache directory once before the first run:

```bash
mkdir -p share/ollama/models
```

### Running without AI sorting

Set `KYPOST_DISABLE_AI_SORTING=true` in `.env` to turn off automatic labels for
every account. Filter rules still run, and all other new mail gets the default
label (Primary). Ollama is not started, no model is pulled, and the server never
contacts Ollama. Releases up to and including 0.4.2 ignore this variable and
always start Ollama. To drop Ollama from the image as well (about 1.7 GB smaller),
build it yourself:

```bash
docker compose build --build-arg INSTALL_OLLAMA=false
```

An image built that way never starts Ollama. If you leave AI sorting on with it,
point `OLLAMA_BASE_URL` at an Ollama running elsewhere.
