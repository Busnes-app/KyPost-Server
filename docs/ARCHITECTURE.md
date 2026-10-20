# Architecture

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Architecture

The container runs these processes:

- API server: `kypost-server --mode server`
- Polling daemon: `kypost-server --mode daemon`
- Ollama service: `ollama serve`
- One-shot startup pull: `ollama pull <configured model>`

Classification flow:

1. Fetch unread messages from IMAP (`INBOX` by default), plus their list/automation headers and any header a filter rule tests.
2. Run filter rules.
3. Pre-sort: a contact's mail gets `Primary` and skips steps 5-8 (and the rate limit); list or automated mail has `Primary` removed from the labels the model may choose. Only accounts whose label set includes `Primary` are affected.
4. Redact sensitive patterns, then embed the redacted sender, subject and body with the on-device sorter. If it is at least `EMBED_MIN_CONFIDENCE` sure, apply its label and skip steps 5-7 (and the rate limit).
5. Build the prompt from sender, subject, body, and tuning context.
6. Call Ollama `/api/generate`.
7. Match the output against the allowed labels.
8. Apply the IMAP keywords, and remember what was applied (as a vector and a hash) so a later relabel by the user can be learned.
9. Save the checkpoint and the decision history.

> **Labels are a hint, not a security boundary.** The classifier reads
> attacker-supplied text, so a sender can write instructions into their message
> and influence which keyword it gets. No small local model resists this
> reliably. Running `backend/cmd/modeleval` against the injection bucket of its
> corpus puts the shipped default at roughly 50–87% resistance depending on
> prompt config, and every model measured let some through. Treat it as a known
> property of the feature rather than a bug with a fix pending.
>
> The embedding sorter follows no instructions, so "file this under Primary"
> is just more words to it (it resisted all 8 of the corpus's injection probes),
> but a sender still chooses the words it reads and can make a message look
> like whatever label they prefer. Its training data is only your own relabels,
> never the LLM's answers or anything a sender controls on its own.
>
> What that buys an attacker is small and bounded: they can steer the label on
> **their own message** — typically into `Primary` instead of `Promotions`. The
> keyword allowlist is enforced in Go after the model answers (step 7), so
> output that is not an allowed label is discarded; a message cannot be labelled
> as something you never configured, cannot be moved, deleted, or marked read,
> and cannot affect any other message. The worst case is a promotional email
> that sorts itself into your main tab — the same thing a sender achieves by
> writing a more convincing subject line.
>
> Do not build a security control on top of these keywords: no filter rule that
> grants trust based on a label, and no "auto-archive anything labelled X."
> Every decision is recorded, so you can audit what the model actually did on
> the Decisions page.

## Project Structure

- `docker-compose.rspamd.yml`: optional pinned private scanner sidecar; fixed configuration in `scripts/rspamd/rspamd.conf`.
- `docker-compose.receiving.yml`: optional controlled receiver supervision and explicit SMTP publish with operator-supplied read-only engine/TLS mounts.

- `backend/internal/agentskill/`: the AI agent skill (`kypost/SKILL.md`), embedded and served at `/agent/skills/kypost/SKILL.md`; the MCP endpoint itself is `backend/internal/api/mcp.go`.
- `backend/internal/mailmsg/`: shared MIME/SMTP helpers and encrypted domain relay profile. Strict native implicit-TLS transport is internally qualified; native primary sending uses fresh admission and the durable outbox; provider readiness and pickup/system paths remain pending.

- `backend/internal/sso/`: signed directory lifecycle, admin mail-domain DNS proof, native reservations, opt-in prepare-before-publication account allocation and shared native outbound admission/recovery. See [provisioning contract](NATIVE_PROVISIONING.md).

- `backend/internal/cfreceiving/`: KyPost side of the continuous Cloudflare wire contract: pickup credentials, signed routing tables and rotations, the bounded Worker client and local pickup state. See [continuous receiving](CLOUDFLARE_CONTINUOUS_RECEIVING.md#kypost-side).
- `backend/internal/ingress/`: durable receiving-buffer core and receipt bridge, selected by opt-in local receiving commands and daemon import; public receiver packaging remains gated. See [receiving qualification](RECEIVING_GATEWAY_ASSESSMENT.md).
- `backend/internal/mailbox/`: internal permanent per-owner SQLite mail, metadata, receipt and change storage, with a complete internal mail Client and transactional incoming-encryption recovery. The internal importer commits these receipts before releasing receiving-buffer payloads. Source guards refuse switching or reusing references against another native database. Native API qualification uses fresh full snapshots; efficient scoped deltas remain pending; `KYPOST_NATIVE_MAIL=true` selects prepared local mailboxes. The internal directory reconciler retains primary-address/account reservations and preparation status; provisioning retries retained signed directory subjects when native mode is enabled. Internal new-account preparation atomically publishes an empty mailbox plus prebound state and refuses legacy/incomplete directories; matching preparations are validated on retry. See [implementation evidence](TURNKEY_MAIL_PHASE1.md#durable-directory-desired-state-and-native-account-preparation). Encrypted outbox claims and independent Sent receipts back admitted primary compose/client-PGP sending and API/daemon recovery; pickup/alias/system paths remain pending.

- `backend/internal/backup/`: KyRecovery adapter, collection and drill checks for state, native mailbox and receiving SQLite snapshots.
- `docs/RESTORE.md`: operator backup and offline restore procedure.

- `backend/`: Go API, poller, adapters, config, state, health, and the on-device embedding sorter (`internal/sorter`)
- `frontend/`: React and Vite UI
- `scripts/`: container entrypoint, supervisord orchestration, Ollama model management, host-side update helpers and the controlled native-domain setup wizard
- `push-relay-shared/`: shared Cloudflare Worker logic for the push relays — API-key issuance, rate limiting, device-token ownership, and the `RelayCoordinator` Durable Object
- `worker/`, `worker-apns/`: the FCM and APNs deployments of that relay. Each holds only its provider's `handleSend` plus its wrangler config; everything else is imported from `push-relay-shared/`
- `receiving-worker/`: independent Email Workers with private R2 retention and authenticated pickup: the one-message pilot (`docs/CLOUDFLARE_RECEIVING.md`) and the continuous protocol Worker, whose KyPost side is `backend/internal/cfreceiving/` (`docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md`); built-in Node tests
- `docs/`: the contracts the client repos implement against — [PLATFORM_BASELINE.md](PLATFORM_BASELINE.md) (what a client must implement to call itself a KyPost client), [E2E_PGP.md](E2E_PGP.md), [WKD_Publishing.md](WKD_Publishing.md), [WEBMAIL_HANDOFF.md](WEBMAIL_HANDOFF.md), [INBOX_PAYLOAD_HANDOFF.md](INBOX_PAYLOAD_HANDOFF.md), [DEVICE_ENROLLMENT.md](DEVICE_ENROLLMENT.md) — plus the operator guides [Reverse_Proxy_Networking.md](Reverse_Proxy_Networking.md) and [TLS.md](TLS.md), and the README overflow: [FEATURES.md](FEATURES.md), [CONFIGURATION.md](CONFIGURATION.md), [API.md](API.md)
- `share/`: host-side Ollama model blob cache, bind-mounted into the container. Never committed
- `tools/`: separate Go module pinning the CI tools (golangci-lint, govulncheck), run as `go tool -modfile=../tools/go.mod <tool>` from `backend/`; Dependabot bumps them
- `testdata/`, `fonts/`: test fixtures and the bundled webfonts
- `Dockerfile`: single image build (backend, frontend, Ollama runtime, pinned embedding model)
- `docker-compose.yml`: local orchestration
- `supervisord.conf`: in-container process supervision
- `AGENTS.md`: the contribution contract for automated agents. Every subtree with its own rules carries one
