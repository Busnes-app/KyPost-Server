# KyPost standalone installation

## Before you start

These steps are for a **new, independent installation** from this repository. Use the
README’s upgrade/restore procedure for an existing instance. Keep its volumes, keys,
configuration and Compose overlays; do not replace its environment files with examples.

Use Linux with Docker Engine and Docker Compose 2.24 or newer, Git, and permission to use Docker.
Choose a release/revision and use its matching Compose files and documentation. Image tags
can move: follow the README/Compose attestation and digest-pinning procedure before relying
on a published image. Source builds use the separate build overlay where provided.

Clone this repository, enter its directory, then run the product steps below:

```sh
git clone https://github.com/Busnes-app/KyPost-Server.git
cd KyPost-Server
```

Each service needs its own directory and persistent storage. Several products use host
port 8080 by default; install them on separate hosts or configure distinct host ports and
matching public origins. For remote users, provide a TLS reverse proxy and a hostname you
control. Trust only the proxy’s actual peer address, and forward the original scheme/client
address according to the product’s proxy guide.

## Install and connect a mailbox

KyPost can use local accounts and an existing external IMAP/SMTP service without the rest
of the Ky suite. Mailbox hosting is opt-in and receiving is still in qualification.

```sh
[ -e .env ] || (umask 077; cp .env.example .env)
chmod 600 .env
# Review .env: KYPOST_BIND=127.0.0.1 keeps HTTP local to the proxy host.
docker compose config --quiet
docker compose pull
docker compose up -d
docker compose exec kypost-server cat /kypost/config/first-run-password.txt
```

Open `http://localhost:5866` on that host, sign in as `admin`, and replace the generated
password. Read bootstrap credentials only in a private terminal. Remove the password file
after saving the replacement in your password manager. Create a separate everyday mailbox
account; administrator access and mailbox use are separate roles.

Follow [external IMAP setup](docs/guides/IMAP.md) to enter the everyday account’s IMAP and
SMTP settings. **Test Email Settings tests IMAP only.** Send a message to a controlled
external mailbox, confirm receipt, reply, and confirm the reply arrives before declaring
mail delivery working. Draft saving and scheduled delivery also need the configured mail
services. Configure [TLS](docs/TLS.md) before remote access.

AI sorting is optional. The default model can use about 2.9 GB resident memory and the
container limit defaults to 8 GB. Follow [running without AI sorting](docs/AI-SORTING.md#running-without-ai-sorting)
for a smaller deployment. The base Compose creates `kypost-net`; if a network already
exists, use [the shared-network guide](docs/Reverse_Proxy_Networking.md#when-the-network-already-exists).

For a source build, use the README’s build instructions (`docker compose up --build -d`);
this differs from the published-image updater. Keep all configured volumes and models.
Recovery: [restore runbook](docs/RESTORE.md). KyIdentity, KyRecovery, native push and
KyCalendar are optional integrations with their own setup, not prerequisites for IMAP use.

## Keep it recoverable

Preserve the database **and deployment keys**, not just the image. Configure recovery using
the linked restore runbook, retain an independent copy, and complete a scratch restore with
custodian cards before treating recovery as verified. A synthetic drill uses a throwaway
key and does not prove that the real cards are available or correct.

For upgrades, back up first, read the chosen release’s migration notes, and reuse the same
Compose project, volume mounts and overlay selection for every command. `docker compose
pull` followed by `docker compose up -d` updates a published-image installation; a digest pin
must be deliberately changed after verification. A source install must keep its build
overlay and rebuild. `docker compose down -v` deletes managed volumes: it is not an upgrade.

## What verification means

The startup checks above prove only the checks they perform. Use the README’s development
section and the repository CI workflow for source/build tests; a passing health response or
screenshot is not a substitute. Record the revision, commands, environment and skips when
reporting results. Design plans and dated evidence remain records of their stated scope.
