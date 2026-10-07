# Ponytail, lazy senior dev mode

You are a lazy senior developer. Lazy means efficient, not careless. The best code is the code never written.

Before writing any code, stop at the first rung that holds:

1. Does this need to be built at all? (YAGNI)
2. Does it already exist in this codebase? Reuse the helper, util, or pattern that's already here, don't re-write it.
3. Does the standard library already do this? Use it.
4. Does a native platform feature cover it? Use it.
5. Does an already-installed dependency solve it? Use it.
6. Can this be one line? Make it one line.
7. Only then: write the minimum code that works.

The ladder runs after you understand the problem, not instead of it: read the task and the code it touches, trace the real flow end to end, then climb.

Bug fix = root cause, not symptom: a report names a symptom. Grep every caller of the function you touch and fix the shared function once — one guard there is a smaller diff than one per caller, and patching only the path the ticket names leaves a sibling caller still broken.

Rules:

- No abstractions that weren't explicitly requested.
- No new dependency if it can be avoided.
- No boilerplate nobody asked for.
- Deletion over addition. Boring over clever. Fewest files possible.
- Shortest working diff wins, but only once you understand the problem. The smallest change in the wrong place isn't lazy, it's a second bug.
- Question complex requests: "Do you actually need X, or does Y cover it?"
- Pick the edge-case-correct option when two stdlib approaches are the same size, lazy means less code, not the flimsier algorithm.
- Mark intentional simplifications with a `ponytail:` comment. If the shortcut has a known ceiling (global lock, O(n²) scan, naive heuristic), the comment names the ceiling and the upgrade path.

Not lazy about: understanding the problem (read it fully and trace the real flow before picking a rung, a small diff you don't understand is just laziness dressed up as efficiency), input validation at trust boundaries, error handling that prevents data loss, security, accessibility, the calibration real hardware needs (the platform is never the spec ideal, a clock drifts, a sensor reads off), anything explicitly requested. Lazy code without its check is unfinished: non-trivial logic leaves ONE runnable check behind, the smallest thing that fails if the logic breaks (an assert-based demo/self-check or one small test file; no frameworks, no fixtures). Trivial one-liners need no test.

(Yes, this file also applies to agents working on the ponytail repo itself. Especially to them.)

# DOX framework

- DOX is highly performant AGENTS.md hierarchy installed here
- Agent must follow DOX instructions across any edits

## Core Contract

- AGENTS.md files are binding work contracts for their subtrees
- Work products, source materials, instructions, records, assets, and durable docs must stay understandable from the nearest applicable AGENTS.md plus every parent AGENTS.md above it

## Read Before Editing

1. Read the root AGENTS.md
2. Identify every file or folder you expect to touch
3. Walk from the repository root to each target path
4. Read every AGENTS.md found along each route
5. If a parent AGENTS.md lists a child AGENTS.md whose scope contains the path, read that child and continue from there
6. Use the nearest AGENTS.md as the local contract and parent docs for repo-wide rules
7. If docs conflict, the closer doc controls local work details, but no child doc may weaken DOX

Do not rely on memory. Re-read the applicable DOX chain in the current session before editing.

## Update After Editing

Every meaningful change requires a DOX pass before the task is done.

Update the closest owning AGENTS.md when a change affects:

- purpose, scope, ownership, or responsibilities
- durable structure, contracts, workflows, or operating rules
- required inputs, outputs, permissions, constraints, side effects, or artifacts
- user preferences about behavior, communication, process, organization, or quality
- AGENTS.md creation, deletion, move, rename, or index contents

Update parent docs when parent-level structure, ownership, workflow, or child index changes. Update child docs when parent changes alter local rules. Remove stale or contradictory text immediately. Small edits that do not change behavior or contracts may leave docs unchanged, but the DOX pass still must happen.

## Hierarchy

- Root AGENTS.md is the DOX rail: project-wide instructions, global preferences, durable workflow rules, and the top-level Child DOX Index
- Child AGENTS.md files own domain-specific instructions and their own Child DOX Index
- Each parent explains what its direct children cover and what stays owned by the parent
- The closer a doc is to the work, the more specific and practical it must be

## Child Doc Shape

- Create a child AGENTS.md when a folder becomes a durable boundary with its own purpose, rules, responsibilities, workflow, materials, or quality standards
- Work Guidance must reflect the current standards of the project or user instructions; if there are no specific standards or instructions yet, leave it empty
- Verification must reflect an existing check; if no verification framework exists yet, leave it empty and update it when one exists

Default section order:
- Purpose
- Ownership
- Local Contracts
- Work Guidance
- Verification
- Child DOX Index

## Style

- Keep docs concise, current, and operational
- Document stable contracts, not diary entries
- Put broad rules in parent docs and concrete details in child docs
- Prefer direct bullets with explicit names
- Do not duplicate rules across many files unless each scope needs a local version
- Delete stale notes instead of explaining history
- Trim obvious statements, repeated rules, misplaced detail, and warnings for risks that no longer exist

## Closeout

1. Re-check changed paths against the DOX chain
2. Update nearest owning docs and any affected parents or children
3. Refresh every affected Child DOX Index
4. Remove stale or contradictory text
5. Run existing verification when relevant
6. Report any docs intentionally left unchanged and why

## Root-owned files

`Dockerfile`, `docker-compose.yml`, `supervisord.conf`, `.env.example`, `README.md`, `SECURITY.md`, `CHANGELOG.md`, `LICENSE.txt`, `LOGGING.md`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md` and everything under `docs/` are owned here, not by any child.

- **`README.md` is the operator-facing surface, and it is the one that drifts.** Its Features list, API Highlights, Environment Variables and Project Structure sections each mirror something the code decides. A change that adds a route, an env var, a top-level directory, or a user-visible capability updates them in the same change set — a child `AGENTS.md` being correct does not discharge this, because operators do not read `AGENTS.md`.
- **`docs/` holds the cross-repo contracts** the client repos (`kypost-android`, `kypost-Linux`, `kypost-for-Mac`) implement against: `E2E_PGP.md`, `PLATFORM_BASELINE.md`, `WKD_Publishing.md`, `WEBMAIL_HANDOFF.md`, plus the operator-facing `Reverse_Proxy_Networking.md`. Changing a behaviour another repo implements means changing the doc in the same change set; these files are the only place that agreement is written down.

- **PGP lifecycle changes** (historical keys, retirement, revocation, alias UIDs or device keyring delivery) must read [docs/PGP_KEY_LIFECYCLE.md](docs/PGP_KEY_LIFECYCLE.md). It is the proposed Tier 5 design; the shipped wire contract remains `docs/E2E_PGP.md`.

- **`CONTRIBUTING.md` is the contribution contract.** It states the user contract (secure by default, every convenience-for-security trade-off signposted where the user reads it), the mandatory AI-attribution rules, and the two merge gates: all CI jobs green, plus an adversarial review pass whose surviving findings go in the PR description. A change to what CI enforces, to the rejection criteria, or to the review skills used belongs in that file in the same change set. **Its PR checklist covers only what a human must attest to.** CI is enforced server-side and blocks the merge on its own, so it carries no checkbox — a box you tick for a machine that has already decided teaches contributors that ticking boxes is the point. Do not add one back; the gate is not missing because the checklist is silent about it.
- **`CODE_OF_CONDUCT.md`** bounds the adversarial review practice: hostility points at code, never at a person. Do not soften the review standard to satisfy it, and do not use the review personas to excuse conduct it forbids.

- **Every build input is pinned, and "every" includes the base images.** All three `FROM` lines carry `tag@sha256:...`; the Ollama tarball carries its published SHA-256. A tag is a mutable pointer — `debian:stable-slim` moves on each point release and even an exact `golang:1.26.6` is republished when its own base is rebuilt — so a tag-only `FROM` means two builds of the same commit ship different userlands, which is the property the Ollama pin exists to prevent. Bump tag and digest together; a digest that no longer matches its tag is a silent lie about what is being built. Re-resolve with `docker buildx imagetools inspect <image>:<tag> --format '{{.Manifest.Digest}}'`.
- The runtime stage's `apt-get update` is the one deliberate exception. Pinning package versions would freeze the runtime on whatever CVEs the base digest shipped with, and this image parses hostile MIME, vCards and OpenPGP packets. The digest fixes the base; apt keeps it patched.
- The Go build carries `-trimpath` and `CGO_ENABLED=0`. Same rule as the digests: without `-trimpath` the checkout's absolute path is baked into the binary, so two builds of one commit differ by where they were built.
- **The runtime user owns `/kypost` and nothing else.** `chown -R kypost:kypost` covers the four data directories only; `/opt/kypost` stays root-owned, because it holds `entrypoint.sh` — which Docker re-executes AS ROOT on every restart, from the container's writable layer, so `/opt` is not reset by one — and the frontend assets the API serves with a one-year immutable cache. Making those writable by the account that parses hostile MIME, vCards and OpenPGP turns any file-write bug into persistent stored XSS, and into root-in-container after a restart. The runtime user needs only read+execute there. Config, private-key and state roots are mode `0700` in the image and are taken into root ownership with `CAP_CHOWN` and reset to `0700` before bootstrap at every entrypoint start, then handed back to the runtime user without requiring `CAP_FOWNER`; changing root modes does not recursively rewrite descendant modes. `entrypoint.sh` chowns the data volumes itself at runtime, which is the copy that has to be right.
- **`KYPOST_BIND` has no default and `docker compose up` refuses to start without it.** The port publishes plain HTTP unless `TLS_CERT_FILE` is set, and with `TRUSTED_PROXY_CIDRS` set anything that can reach 5866 directly forges `X-Forwarded-For` past every IP-keyed lockout. Both `0.0.0.0` and `127.0.0.1` have shipped as the default and both were wrong for somebody; only the operator knows how their proxy arrives. Do not restore a default to make a first run quieter.
- **Non-loopback cleartext requires `ALLOW_INSECURE_HTTP=true`.** The entrypoint receives `KYPOST_BIND` and refuses a remote cleartext publish unless inbound TLS is configured or the operator explicitly accepts the risk. Keep loopback HTTP available for a local TLS proxy. Unset refuses too — the entrypoint cannot see the real publish address, so this is an acknowledgement gate and an operator who never says how the port is reached is who it is for. Anything starting the image outside compose must therefore pass `KYPOST_BIND` (the CI smoke test does); do not add an empty-string arm to quieten a bare `docker run`.
- **Bounded `startretries`, and FATAL exits the container.** `supervisord` has no backoff between restart attempts, so a large `startretries` is a hot loop rather than resilience; its default of 3 leaves PID 1 healthy in front of a dead service, because Docker restart policies react to a container exiting and never to a healthcheck. The pair that works is 20 retries plus the `crashexit` event listener taking PID 1 down, letting `restart: unless-stopped` (which does back off) restart the container. Changing either half alone reintroduces one of the two failure modes.

- **KyRecovery backup contract** lives in `kyrecovery-server/zero_code_pairing_handoff_spec.md` (v2.0.0) and the suite `AGENTS.md` "KyRecovery integration"; the shipped product adapter uses `ky-primitives/recoveryclient` v0.5.1, with operator procedures in `docs/RESTORE.md`. Do not copy the spec into this repo.

- **Shutdown gives ordinary HTTP requests 20 seconds, drains active backups for up to 16 minutes, and joins native outbound workers (45-second connection/TLS plus 45-second SMTP and 30-second local finalization).** Supervisor stops separate program groups serially, allowing 1000 seconds each for API/daemon; compose allows 36 minutes overall. Keep the cumulative program waits plus 60 seconds of teardown within the Docker budget; `scripts/check-supervisor-shutdown.py` enforces this. Optional `docker-compose.lan-dns.yml` requires explicit `KYPOST_DNS`; base compose retains host DNS.

## User Preferences

- Optional `docker-compose.rspamd.yml` enables strict local pre-commit native SMTP scanning. Read [receiving spam setup](docs/RECEIVING_SETUP.md#optional-rspamd-sidecar) before changing policy, transport, sidecar permissions or metadata. Preserve accepted replay and exact mailbox bytes; scanning does not authorize mail movement or ownership. Read [Cloudflare pilot](docs/CLOUDFLARE_RECEIVING.md) before changing hosted capture, frozen claims, pickup credentials or scanner provenance; the one-message profile retains provider copies and does not enable continuous reception.

- Native restore authority prerequisites are captured against pinned KyIdentity source in [docs/NATIVE_RESTORE_AUTHORITY.md](docs/NATIVE_RESTORE_AUTHORITY.md). Ordinary active-user resync is insufficient to prove previously offboarded/deleted subjects; offline restore revokes native device/push registrations, CardDAV app passwords and pairing authority, while no hold release is supported. Held native login/session/SSO/MFA authority is also refused; retain an independently provisioned legacy recovery admin. [docs/NATIVE_RESTORE_REFERENCES.md](docs/NATIVE_RESTORE_REFERENCES.md) records cached-ID reuse and the generation-bound HTTP/notification reference boundary; Protected held repair reconciles existing native activity/roles and revokes transport credentials using fresh signed evidence; it preserves mail/PGP material and the hold. Read the authority contract before changing repair or completion. Physical-client and recovery activation qualification remain pending.

- Native compose/client-PGP sending (primary or an administrator-assigned active alias) is opt-in and uses fresh admission, durable claims and joined recovery workers. Pickup/system routing, provider readiness and restore reconciliation remain pending. Read [docs/NATIVE_OUTBOX.md](docs/NATIVE_OUTBOX.md) before changing queue claims, key derivation, retry or Sent obligations.

- Protected domain relay settings are available through Server → Mail domain and the admin API, with fresh DNS/issuer proof, encrypted credentials and sealed backup validation. A protected saved-relay check proves only transient TLS/AUTH, with no mail submission; saving configuration alone proves no provider readiness; native primary sends additionally require explicit mode and fresh account/domain admission; read [docs/DOMAIN_RELAY.md](docs/DOMAIN_RELAY.md) before changing relay authority, generations, transport or recovery.

- Turnkey domain mail stack: extend mature KyPost from a frontend for external mail services into the integrated user-facing product, with mailbox storage/reception and automatic KyIdentity account provisioning behind it. Preserve established server/client contracts; Android leads Linux and iOS, so appliance delivery must avoid requiring simultaneous client rewrites. Evaluate mail engines against provisioning, lifecycle reconciliation and existing labels/PGP contracts; Mailflare is a candidate, not a selection.
- Mail sources: three supported, permanent options. Native mailboxes receiving through Cloudflare Email Routing (hosted Worker pickup; pilot in [docs/CLOUDFLARE_RECEIVING.md](docs/CLOUDFLARE_RECEIVING.md), continuous design in [docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md](docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md)) are primary and the owner's live migration target; native mailboxes receiving through bundled Maddy on the operator's own port 25 serve operators who do not want mail passing through Cloudflare; an existing external IMAP/SMTP account remains a continuing option for anyone who wants neither, not a legacy path to retire. One receiving profile per deployment, chosen in setup; none may be removed or made a dependency of another.
- Native mail must support several domains per deployment and several mailboxes per KyIdentity user, each mailbox owning its own addresses across those domains; do not encode one-domain or one-mailbox-per-user assumptions in new storage, routing or provisioning. KyIdentity owns the person, their active state and their primary mailbox/address; KyPost administrators (never users) add extra mailboxes, addresses and aliases. Read [docs/NATIVE_ADDRESSING_V2.md](docs/NATIVE_ADDRESSING_V2.md) before changing domain, mailbox or address storage.
- Block abusive senders with an escalating cooldown in both receiving profiles. Automatic blocks are per sender address and count a message only when envelope sender equals From, DKIM passes aligned to that domain, and SPF passes on the receiving MTA's own result (the Cloudflare profile has no automatic blocks until it can establish SPF); automatic domain blocks require several distinct abusive addresses on a domain the deployment has never accepted good mail from, so one free account cannot block a shared provider. Administrators can block/unblock any address or domain manually.
- Inbound message size cap is 25 MiB (about ten phone photos; Cloudflare's own maximum) for both receiving profiles.
- Provide a user mail import and export tool: import from an external IMAP account (and standard files) into a native mailbox, and export a native mailbox to standard formats, so users can move in and out without lock-in. Setup must state the trade-off: Cloudflare and its administrators can read ordinary mail in transit and at rest in the bucket; Maddy needs reachable inbound port 25 and exposes the operator's address. Scope (decided 2026-10-07): users import from an external IMAP account and from mbox/EML files, and export a mailbox or folder as mbox and a zip of EML files; self-service for the user's own mailboxes only, administrators cannot run it on a user's behalf. Export: [mail export](docs/NATIVE_PROVISIONING.md#mail-export); import from mbox/EML files: [mail import](docs/NATIVE_PROVISIONING.md#mail-import); import from an external IMAP account: [import from another mail account](docs/NATIVE_PROVISIONING.md#import-from-another-mail-account).
- Quarantined native deliveries are released or discarded by administrators through an admin UI and a CLI, both step-up confirmed and audited, showing envelope metadata only (never message bodies); release goes only to the frozen original mailbox, and refuses (discard remaining) unless that mailbox still exists, is active and is owned by the same subject. API and CLI: [quarantine release](docs/NATIVE_PROVISIONING.md#quarantine-release); the admin UI is pending.
- Each operator owns their outgoing relay account, credentials, billing and provider relationship; Busnes supplies software, not a relay service. Default external sending uses that provider. One guided deployment/domain setup covers KyIdentity pairing, DNS/TLS, relay readiness, backups and delivery diagnostics; users need no per-account mail-server setup.
- Mail-domain setup now has credential-gated admin challenge/verification APIs; opt-in `KYPOST_NATIVE_MAIL=true` provisions retained signed subjects and selects prepared mailboxes in API/daemon; separate `KYPOST_NATIVE_RECEIVING=true` selects local receiving commands/import for qualification, without installing a public receiver. Optional `docker-compose.receiving.yml` adds explicit supervised startup and SMTP publishing with operator-owned pinned engine/TLS mounts; base startup stays disabled. Read [docs/NATIVE_PROVISIONING.md](docs/NATIVE_PROVISIONING.md) before changing domain proof, allocation, issuer/link ownership or lock order. Controlled receiver configuration/setup is owned by [docs/RECEIVING_SETUP.md](docs/RECEIVING_SETUP.md); `scripts/setup-mail.sh` guides the operator through confirmed base rebuild/public dotenv inputs, existing protected UI, optional new-spool init and receiver launch, without collecting credentials or changing DNS.
- Mail stack implementation: read [docs/TURNKEY_MAIL_STACK_PLAN.md](docs/TURNKEY_MAIL_STACK_PLAN.md) before reception, native mailbox storage, provisioning or domain-relay work. It separates durable receiving, KyPost-owned storage and outgoing delivery; it is approved implementation direction, not shipped capability. [Phase 1 evidence](docs/TURNKEY_MAIL_PHASE1.md) and [receiver qualification](docs/RECEIVING_GATEWAY_ASSESSMENT.md) state the runnable checks and unresolved adoption gates.
- Keep `TUNING.md` compact for limited models while preserving classification rules and untrusted-input handling. Retain the `## Allowed Labels` bullet list and `[Insert Email Content Here]` placeholder required by the loader.
- Use the shared JSON logger from ky-primitives. No audit-chain service is part of this integration; supervisord owns stderr capture/rotation (see `LOGGING.md`).

## Child DOX Index

- `receiving-worker/` — Independent Cloudflare Email Workers: the one-message pilot and the continuous protocol (signed routing table, R2 queue, authenticated pickup and rotation), with private R2 retention. See [receiving-worker/AGENTS.md](receiving-worker/AGENTS.md).

- `backend/` — Go 1.26.6 classification engine, HTTP API, IMAP adapter, Ollama adapter, poller, config, state, health, logging, redaction, sealed backups, internal receiving buffer, permanent mailbox core and encrypted native outbox; produces the `kypost-server` binary. See [backend/AGENTS.md](backend/AGENTS.md). Contains nested children: `backend/internal/backup/`, `backend/internal/cfreceiving/`, `backend/internal/adapters/`, `backend/internal/contacts/`, `backend/internal/groups/`, `backend/internal/mailcache/`, `backend/internal/ingress/`, `backend/internal/mailbox/`.
- `frontend/` — React 19 / TypeScript SPA for config, monitoring, decision audit, and log streaming. See [frontend/AGENTS.md](frontend/AGENTS.md).
- `scripts/` — Container initialization, process orchestration (supervisord), Ollama model management, host-side image updates, guided operator setup and fixed optional Rspamd policy/qualification. See [scripts/AGENTS.md](scripts/AGENTS.md).
- `share/` — Persistent Ollama model blob cache bind-mounted from the host; never committed to git. See [share/AGENTS.md](share/AGENTS.md).
- `push-relay-shared/` — The push relay: shared Cloudflare Worker logic (API-key issuance, rate limiting, device-token ownership) plus the `RelayCoordinator` Durable Object, and the contract for both provider Workers. See [push-relay-shared/AGENTS.md](push-relay-shared/AGENTS.md).
- `worker/`, `worker-apns/` — The FCM and APNs deployments of that relay: per-provider `handleSend` plus wrangler config, everything else imported from `push-relay-shared/`. Governed by [push-relay-shared/AGENTS.md](push-relay-shared/AGENTS.md); they hold no rules of their own.

# AI Governance v2 Ultra-Lite

Use this policy when context is tight. If any gate fails, stop and escalate.

1. Follow precedence: Security/Legal > Safety/Data Integrity > Reliability > Performance > Convenience.
2. Act only on verifiable evidence; label assumptions and validate before execution.
3. Before any critical operation, validate preconditions (scope, permissions, dependencies, rollback path).
4. Production: no mock/synthetic data, no silent fallback, no hardcoded secrets.
5. Security baseline: validate/sanitize/type-check inputs; enforce least privilege; block unsafe dynamic execution.
6. Required logs per task: timestamp, actor, task_id, action, target, severity, result, correlation_id.
7. Failures must be explicit, human-readable, and include remediation steps.
8. Tests required: unit + integration for new logic; regression for high-impact changes; CI must pass.
9. Approval gates: reviewer-agent for security-sensitive work; human approval for major prod, security, config, or role changes.
10. Change control: update docs + changelog/manifest in same change set; define rollback for major changes; document exceptions with owner, risk, and expiry.
