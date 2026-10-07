# Logging

Go application logs use `ky-primitives/logging` JSON lines on stderr. `KY_LOG_LEVEL`
accepts debug, info, warn or error (default info); malformed values refuse startup.
Every line includes app, timestamp, level and RFC 5424 severity/facility. Backup
administrative events use authpriv; the flat SQLite backup audit remains separate.

The shared handler admits declared fields, caps string values and replaces control
characters. Undeclared fields are dropped and counted in `dropped_fields`. KyPost's
existing flat-string logger retains its sensitive-field redaction before this filter.
The allowlist bounds field names, not the meaning of values: callers must keep
passwords, tokens, private keys, CAPTCHA answers and correspondence out of messages
and diagnostic errors too. A source-harvesting regression test checks all production log keys against the declarations. Raw classifier output and upstream error bodies are never logged; warmup, retries and failures retain operation/status context at the default level.

The process opens no application log files or log-shipping sockets. Supervisord
captures and rotates `api.err.log` and `daemon.err.log`; the existing admin log
viewer defaults to the API stream. Historical app/classifier files remain readable
but receive no new output. Ollama and supervisord retain their own diagnostic logs.
Operators can collect captured streams with their existing logging agent. Direct
binary deployments collect stderr with their service manager.

The optional receiver has separate rotated `receiver.log` and `receiver.err.log`
streams (10 MiB each, three backups). Maddy emits envelope/IP/command metadata
rather than the Go JSON format and discards helper stderr; protect these logs.
Debug/wire logging remains disabled.

Continuous Cloudflare receiving logs actions (`publish`, `pickup`, `delete`,
`quarantine`, `refuse`, `fence`, `rotate`) with the R2 key or table revision as
correlation; never addresses, message content, bearers or keys.

Sender block changes log `receiving sender block change` with action
`block_sender` or `unblock_sender`, the kind (`address` or `domain`) as target,
the result and the block ID (a SHA-256 prefix) as correlation; never the blocked
address or domain (an unblock logs only the ID). API changes go to the API stream, CLI changes to the terminal.
The CLI logs the same action names (`list` as `list_sender_blocks`).
Automatic blocks (Maddy with Rspamd) log the same line from the receiving
command with actor `automatic`, action `block_sender`, result `blocked`, plus
`block_level` and `until_ms`; evidence work that is dropped or fails logs
`receiving sender evidence` (warning, result `dropped`) with the delivery ID as
correlation, and an unblock whose suppression could not be recorded logs it
with result `suppression-failed`. A damaged evidence file logs an error,
action `reset`, result `damaged-file-set-aside`. A backup that leaves out a bad
evidence file logs `backup skipped sender evidence` (result `skipped-malformed`
or `skipped-oversized`). None carries an address or message content.
An unreadable block list logs `cloudflare sender blocks unreadable` (error,
result `previous-blocks-kept`) on every publish attempt until repaired; blocks
left out of a full table log `cloudflare sender blocks truncated` (result
`truncated`) with the count.

The viewer is a transitional compatibility feature, not a new log platform.
