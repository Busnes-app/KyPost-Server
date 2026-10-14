# Adapters

## Purpose

External protocol clients that isolate third-party integration details from the rest of the backend. Contains two sub-packages: `imap/` and `classifier/`.

## Ownership

All code under `backend/internal/adapters/`. Owned by the backend team. Changes to either adapter affect the classification loop and must be coordinated with `processor/`.

## Local Contracts

- `imap.MessageReference` / `ResolveMessageReference` translate only at wire boundaries through the native generation capability. Keep canonical numeric internal IDs; reject stale/foreign/bare native references before lookup/mutation. Missing native capability fails closed; legacy IMAP behavior stays unchanged.

- `imap.MailSourceIdentity` identifies external IMAP mode or native immutable owner/database namespace. Account state/cache admission precedes API/daemon mail operations; it does not detect changes between external IMAP endpoints or UIDVALIDITY — the poll checkpoint does.

- Poll checkpoints are `<namespace>:<uid>`; the namespace is the polled mailbox's UIDVALIDITY (read from the `STATUS` attribute list each `ListUnreadInbox`, never the mailbox name) plus a hash of host, port, username and mailbox. A bare `<uid>` is the pre-namespace format: it is trusted, and adopts the next namespace on every save, `ClampCheckpoint` included. A different namespace restarts the fetch at UID 0 and `UIDNamespaceReset` tells the poller to clear the processed set and deferrals (`state.Store.ResetUIDNamespace`); only unread mail is refetched. A server omitting UIDVALIDITY keeps bare checkpoints. Native checkpoints stay bare IDs.

### `imap/` — IMAP Client

- `raw_content.go` exposes pure MIME/overview helpers reused by the native mailbox Client, including existing PGP detection and single-sender binding. Parsing failures use sanitized `ErrMalformedMIME`; `Message`/`MessageContent.ParseError` mark one unreadable message without aborting a batch. Existing IMAP reads leave that flag false.

- `incoming_encryption.go` owns safe incoming replacements and subsequent rule actions. Reject non-INBOX polling configurations before capturing a source UID. Bind UIDs to the authenticated endpoint/account, UIDVALIDITY and exact content; APPEND must be byte-verified before targeted UID EXPUNGE. Require UIDPLUS/rev2 and preflight MOVE for terminal rules. Perform any folder discovery/create before the final identity guards, then use commands with automatic retries disabled. Reconcile uncertain APPEND/MOVE by marker plus exact ciphertext, never marker alone or global EXPUNGE.

- Split by concern: `client.go` (connection lifecycle, credentials, message fetch/search), `client_folders.go` (mailbox list/create/delete/rename, keyword apply/remove), `client_attachments.go` (attachment enumeration and fetch: `FetchRawMessage`, whose `UID LARGER` search refuses oversize before fetching, then `ParseRawContent`, the parser native mail uses, so both backends list the same parts; undisposed `text/calendar` parts are appended as `invite.ics` after real attachments and `NewAttachmentInfo` adds `calendarMethod`; the go-imap list paths — `ListUnreadInbox`, `ListUnreadMessages`, `GetMessageBodies` — set `HasAttachments` for such parts through `calendarInviteUIDs`, one best-effort `BODYSTRUCTURE` fetch for attachment-less messages, matching only part positions), `client_append.go` (draft and Sent APPEND), `protocol_safety.go` (the keyword/mailbox validators below)
- Wraps `go-imap` to fetch unread emails by UID range from a configured mailbox
- Reads IMAP credentials from an encrypted file at rest (decrypted at connection time via `SECRET_DIR`)
- Does not cache or buffer messages; callers receive a slice of messages and are responsible for processing
- **Every batch body fetch goes through `fetchEmailsBounded` (`fetch_bounded.go`), never `d.GetEmails` directly.** `go-imap` buffers and MIME-decodes EVERY message in a `GetEmails` call before returning any of it, at roughly 3.3x the wire bytes, so an unpaged call materialises a whole mailbox at once — measured at ~4.8 GiB of heap for a 1.5 GiB mailbox, against an 8 GiB container. `ListUnreadInbox` had carried a page size, a byte budget and a message count since it was written; `GetMessageBodies` and `ListUnreadMessages` called `GetEmails` with the whole UID list and inherited none of them. The budget is a RUNNING TOTAL across pages — comparing one page at a time leaves the caller's accumulated map unbounded, which is the accumulation that matters. Note the per-message `SEARCH LARGER` pre-filter bounds ONE message and is not a substitute; `ListUnreadMessages` has no size filter at all. The IMAP host is user-supplied, so an attacker serves the payload directly rather than filling a real mailbox
- **`FetchHeaderFields` takes an explicit mailbox and carries the same bounds.** `ensureConnectedLocked` always re-selects the configured folder, so a header fetch for any other mailbox must `selectMailboxLocked` like every read method. Header size is sender-controlled and `HEADER.FIELDS` has no byte cap, so it drops `SEARCH LARGER` UIDs and pages through `fetchEmailsBoundedWith`. Every UID it fetched gets an entry (empty when no field matched); an absent UID was not fetched and callers must treat its headers as unknown, never as absent. Unsolicited `* N FETCH (FLAGS …)` records are skipped
- Returns errors on connection failure; callers (processor) handle retry logic
- **An `APIClient` holds one live, authenticated connection for its whole life, and the caller owns closing it.** `Close()` is nil-safe and idempotent; it is deliberately NOT on the `Client` interface, because six test fakes implement that interface and have nothing to close — the two cache owners (`api`'s `userMailClient`/`invalidateUserMail` and `processor`'s `userMailClient`) type-assert `io.Closer` instead. Both rebuild their client whenever the stored credentials change, so an eviction that skips the close leaks a server-side IMAP session per credential save, per process, until the far end times it out — and providers cap concurrent sessions per account (Gmail: 15), so the leak surfaces as mail simply ceasing to sync
- **`go-imap` does not escape enough to be handed untrusted strings.** Keywords are interpolated raw into `UID STORE <uid> +FLAGS (%s)`, and mailbox names go into a quoted argument escaped by `AddSlashes`, which replaces only `"`. Neither handles CR/LF or `\`. Every keyword and every mailbox name reaching this package must therefore pass `ValidateKeyword` / `ValidateMailboxName` (`protocol_safety.go`) first, and every header name `ValidateHeaderFieldName` (enforced in `fetchHeaderFieldsLocked`; filter rules supply them) — these guards live on the adapter methods, not in the callers, because both the API handlers and the poller apply keywords. `selectMailboxLocked` is the single shared select path the read methods use so the mailbox guard cannot be forgotten by a new one. Held in place by `protocol_safety_test.go`
- **go-imap is a patched copy, `backend/third_party/go-imap` (`replace` in `go.mod`); upstream v0.1.28 has no dial hook or read limits.** Patch points, all marked `KyPost patch`, to re-apply when bumping upstream: `conn.go` `DialContext`; `exec.go` `MaxLiteralSize` (checked before `make`), `MaxLineSize` (`readLine` replaces every `ReadBytes`, joining once) and `MaxResponseSize` (`ErrResponseTooLarge`); `util.go` `excerpt`, which every error quoting server data uses (128 bytes plus the length: `parse.go`'s fetch, literal, paren and search errors, `exec.go`'s tagged failure, `append.go`, `idle.go`), because go-imap logs failed fetches and an error holding the response multiplies a full-size response into GiB; `CheckType` names the token type only, no token dump; `idle.go`'s UID is a 32-bit parse. Grep the new upstream for `Errorf` with response text before bumping. `client.go`'s `init` installs the hook and limits for the whole process (the API's connection test calls `goimap.New` too): every connect and reconnect dials through `netguard.DialContext`, and a literal is capped at `MaxInboundMessageBytes` + 1 MiB, a line at 64 MiB (UID SEARCH ALL) and one command at about 105 MiB, one page of `unreadFetchPageSize` (4) maximum-size bodies. The literal cap is checked before go-imap's `make`, so a body over it fails its whole page: any body fetch must exclude `LARGER` UIDs first or tolerate the error (only the archive-year lookup does the latter). Tests reach loopback with `allowLoopbackDial`; `bounded_read_test.go` holds the bounds
- Errors from those validators wrap `ErrUnsafeKeyword` / `ErrUnsafeMailbox` so callers can answer 400 rather than 502 (see `api.writeMailboxError`)

- **`import_source.go` (`ImportSource`) is the IMAP client for user mail import, deliberately not go-imap.** go-imap has no STARTTLS, keeps certificate verification in a process-wide variable and buffers whole responses. `OpenImportSource` takes an already-dialled `net.Conn`, the TLS server name and roots, and never retains the password. Its vocabulary stays `STARTTLS`, `LOGIN`, `LIST`, `EXAMINE`, `FETCH` (metadata), `UID FETCH (BODY.PEEK[])` and `LOGOUT`: adding a writing command, `SELECT` or a non-PEEK body fetch breaks the read-only promise the UI makes. PREAUTH greetings and bytes buffered after the STARTTLS answer are refused; responses are bounded (64 KiB outside literals, literal and per-response literal totals by the caller's cap), and the whole session by `maxBytes`, counted on every byte read after the bufio layer's source (`budgeted`, `ErrImportBudget`), unsolicited responses included; names sent back must pass `ValidateMailboxName` and be at most 255 bytes. Every parser loop returns the first error from `byte`, `skip`, `peek` or `spaces` (past the budget they fail without consuming, so an ignored error spins on a buffered byte), and `peek` also fails once the budget is spent; server numbers converted to `int` are checked against `math.MaxInt32` first. `TestImportSourceLineBudget` runs each case under a 2 s watchdog. Errors never carry server text. Checked by `import_source_test.go` and the API's `TestIMAPImport`

### `classifier/` — Classifier HTTP Client

- Sends classification requests to Ollama `/api/generate` via HTTP POST
- Admission control lives in `http_client.go`: `CLASSIFY_CONCURRENCY` (default 1) bounds in-flight generations via a channel semaphore, `CLASSIFY_PACE_MS` (default 0) inserts dead time between request starts. The pace was an unconditional 3 s, which capped the whole instance at 20 classifications/minute regardless of user count — it is not backpressure, since Ollama queues internally and the retry loop already backs off. Raise `CLASSIFY_CONCURRENCY` to match `OLLAMA_NUM_PARALLEL` or the extra capacity is unreachable
- Implements exponential backoff on transient HTTP errors
- `client.go` — high-level interface: accepts prompt + email text, returns raw model output string
- `http_client.go` — low-level transport: request construction, admission control, retry loop, `Stats()` for queue depth

- Classifier diagnostics use the process slog handler (shared JSON stderr). Never log model output, prompts or response snippets; report byte counts, operation names, transport failures and HTTP status. Warmup/retry/failure events survive the default info level. Never embed upstream HTTP bodies, JSON error text or the model response in returned errors: callers also log them. The adapter owns no log files.
- Failures callers act on are typed — `ErrCreditsExhausted`, `*StatusError` (HTTP status), `*NoAllowedLabelError` — and are matched with `errors.Is`/`errors.As`, never by message text.

### Shared Rules

- Adapters do not read or write application state (`STATE_DIR`)
- Adapters do not call other internal packages; they receive all config via constructor arguments
- All external I/O errors are returned to callers, not swallowed

## Work Guidance

- Keep each sub-package's external interface minimal: one constructor, one or two methods
- Admission and retry logic lives exclusively in `http_client.go`; do not duplicate in `client.go`
- The classify slot is held across the whole retry sequence but the WAIT for it is abandonable — keep the semaphore a channel selected against `ctx.Done()`, never a `sync.Mutex`, or a cancelled poll tick blocks behind another user's 15-second backoff
- IMAP credential decryption must use the same key derivation as `api/` encryption — coordinate any changes with `api/server_imap_config.go`

## Verification

- `go vet ./internal/adapters/...` must pass
- Integration tests requiring live IMAP or Ollama endpoints are run manually; unit tests mock the HTTP transport

## Child DOX Index

No child AGENTS.md files. `imap/` and `classifier/` are documented here.
