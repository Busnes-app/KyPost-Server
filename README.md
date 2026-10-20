# KyPost

**Start here:** [Standalone installation](INSTALL.md) → [existing IMAP/SMTP account](docs/guides/IMAP.md).
KyIdentity, KyRecovery and the native clients are optional. Real mail access needs a working mailbox and sending service; a healthy web UI alone does not verify delivery.

KyPost is self-hosted email. Use it as a web client for the IMAP account you already have, or let it host your mailboxes as well. It encrypts with OpenPGP, so you can exchange end-to-end encrypted mail with anyone who uses PGP; interoperability depends on the correspondent’s PGP implementation and needs a real send/receive check for your configuration. A local Ollama model sorts your unencrypted mail into labels without it leaving your server, and encrypted mail is never sent to the model. The iOS, Android and Linux phone apps get native push notifications.

Hosting your own mailboxes is opt-in, and receiving mail for them is still in qualification. To rely on KyPost today, connect it to an existing IMAP server.

KyPost polls unread mail, classifies each message, and applies IMAP keywords. It also gives you a web UI to read mail, change the configuration, manage notifications, view logs, and compose mail. Compose supports send and draft save.

## Features

- IMAP web client: inbox reader, folders, compose with SMTP send, scheduled send and draft save, filter rules (GUI and Sieve).
- Sending quotas across immediate and scheduled mail: 60 messages an hour and 1000 recipients a day by default, adjustable by administrators per installation or account.
- Automatic keyword labels from an on-device embedding sorter backed by a local Ollama model; each account keeps its own labels. Labels are a hint, not a security boundary — see [Architecture](docs/ARCHITECTURE.md#architecture).
- PGP encryption and signing, recipient key discovery, Web Key Directory publishing and opt-in incoming encryption. Read [Where your PGP private key lives](docs/PGP-KEYS.md#where-your-pgp-private-key-lives) before relying on it.
- Contacts with CardDAV server and client, groups, and CSV/vCard import and export.
- Multi-user with admin and user roles; TOTP, recovery codes and push-approval MFA; OpenID Connect single sign-on; self-hosted proof-of-work login CAPTCHA by default.
- Browser push, and native push for the mobile apps through the [push relays](docs/CLIENTS-AND-PUSH.md#push-relays-cloudflare-workers).
- Opt-in native mailboxes: KyIdentity provisioning across several domains, quotas, receiving through Cloudflare Email Routing or an operator-supplied receiver (both still in qualification), operator-owned relay sending, sender blocks, quarantine, and mail import and export.
- Optional KyCalendar pairing: KyCalendar sends invitations, cancellations and replies from a user's own mailbox, and receives the DKIM-verified invitations that arrive there; each user opts in. See [docs/KYCALENDAR.md](docs/KYCALENDAR.md).
- Sealed configuration and state backups to KyRecovery or a local directory, with restic for mail databases.
- Single Docker container managed by supervisord, with shared JSON logs.

The full list, with the trade-offs each feature carries, is in [docs/FEATURES.md](docs/FEATURES.md).

## Licence

KyPost is released under the [MIT License](LICENSE.txt).

[![OctoCounts](https://api.octocounts.com/badge/Busnes-app/KyPost-Server/branch/main)](https://octocounts.com/github/Busnes-app/KyPost-Server/tree/main)

## Documentation

- [Running](docs/RUNNING.md) — Requirements, Quick Start, Ports. <a id="requirements"></a><a id="quick-start"></a><a id="ports"></a>
- [Architecture](docs/ARCHITECTURE.md) — Architecture, Project Structure. <a id="architecture"></a><a id="project-structure"></a>
- [PGP keys](docs/PGP-KEYS.md) — Where your PGP private key lives. <a id="where-your-pgp-private-key-lives"></a>
- [Accounts](docs/ACCOUNTS.md) — Session Behavior, Users and Roles. <a id="session-behavior"></a><a id="users-and-roles"></a>
- [AI sorting](docs/AI-SORTING.md) — Environment Variables. <a id="environment-variables"></a><a id="choosing-a-classifier-model"></a><a id="running-without-ai-sorting"></a>
- [Clients and push](docs/CLIENTS-AND-PUSH.md) — Mobile App Pairing (Native), Push Relays (Cloudflare Workers). <a id="mobile-app-pairing-native"></a><a id="certificate-pinning-in-the-pairing-qr"></a><a id="push-relays-cloudflare-workers"></a>
- [Storage and recovery](docs/STORAGE-AND-RECOVERY.md) — Persistence, Backup and Restore. <a id="persistence"></a><a id="backup-and-restore"></a><a id="back-up"></a><a id="restore"></a><a id="verify-the-restore"></a>
- [Development](docs/DEVELOPMENT.md) — API Highlights, Build and Dev Checks. <a id="api-highlights"></a><a id="build-and-dev-checks"></a>
- [Operations](docs/OPERATIONS.md) — Operations, Updating KyPost, Troubleshooting. <a id="operations"></a><a id="updating-kypost"></a><a id="verifying-an-image-by-hand"></a><a id="moving-a-locally-built-install-onto-published-images"></a><a id="troubleshooting"></a><a id="ollama-or-model-issues"></a><a id="imap-connection-issues"></a><a id="smtp-send-issues"></a><a id="kypost-does-not-apply-labels"></a><a id="pwa-installation-on-firefox"></a>
