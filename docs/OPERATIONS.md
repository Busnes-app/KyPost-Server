# Operations

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Operations

Runtime checks:

```bash
docker compose ps
docker compose logs -f kypost-server
docker compose exec kypost-server ps aux
docker compose exec kypost-server ls -la /kypost/config /kypost/state
docker volume ls | grep kypost
```

Persistence behavior:

- `docker compose up --build` keeps the named volumes.
- `docker compose down -v` removes the named volumes and the stored app data.

## Updating KyPost

KyPost checks GitHub releases hourly. When a newer KyPost release is found, it
emails the primary admin once and shows the update in Settings → Admin →
Server → Application. The container reports availability but never controls Docker on
its host.

See [`CHANGELOG.md`](../CHANGELOG.md) for what changed in each release and for the
upgrade/rollback matrix. Take a backup before upgrading — see
[Backup and Restore](STORAGE-AND-RECOVERY.md#backup-and-restore).

Published releases are available from GitHub Container Registry. From the
checkout, apply the current `stable` image with health-gated rollback:

```bash
./scripts/update-host.sh
```

The script resolves `stable` to an immutable digest, verifies its GitHub build
attestation with `gh attestation verify`, and preserves that exact digest for
rollback. It requires Docker Compose v2, Docker Buildx, and the GitHub CLI
(`gh`), and checks for each up front; it fails closed
when either verification or the health check fails. To stay on a specific
release instead, set `KYPOST_VERSION=0.4.3` in `.env` before running it.

### Verifying an image by hand

Without `update-host.sh` (`gh` logged in), verify the version tag, not `:stable`:
`:stable` moves, so resolve the version it should be and verify that
(`gh release view --repo Busnes-app/KyPost-Server --json tagName --jq .tagName` prints the newest).
The attestation must come from `release-image.yml` at that tag and record the commit the tag
points to, and the image you pull must be that digest:

```bash
v=<release version you intend to run, e.g. 0.4.3>
sha=$(gh api repos/Busnes-app/KyPost-Server/commits/refs/tags/v$v --jq .sha) \
  && d=$(docker buildx imagetools inspect ghcr.io/busnes-app/kypost-server:$v --format '{{.Manifest.Digest}}') \
  && gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/release-image.yml@refs/tags/v$v \
  && [ "$(gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/release-image.yml@refs/tags/v$v \
       --format json --jq '.[0].verificationResult.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit')" = "$sha" ] \
  && docker pull ghcr.io/busnes-app/kypost-server:$v \
  && docker image inspect ghcr.io/busnes-app/kypost-server:$v --format '{{range .RepoDigests}}{{println .}}{{end}}' \
       | grep -qxF "ghcr.io/busnes-app/kypost-server@$d"
```

`:main` is built from main's tip by `publish-main.yml`; name the commit yourself, and the
attestation must record it:

```bash
sha=<full commit sha of main's tip, e.g. $(git rev-parse origin/main)>
d=$(docker buildx imagetools inspect ghcr.io/busnes-app/kypost-server:main --format '{{.Manifest.Digest}}') \
  && gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/publish-main.yml@refs/heads/main \
  && [ "$(gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/publish-main.yml@refs/heads/main \
       --format json --jq '.[0].verificationResult.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit')" = "$sha" ] \
  && docker pull ghcr.io/busnes-app/kypost-server:main \
  && docker image inspect ghcr.io/busnes-app/kypost-server:main --format '{{range .RepoDigests}}{{println .}}{{end}}' \
       | grep -qxF "ghcr.io/busnes-app/kypost-server@$d"
```

Then start that digest, not the tag. Compose builds the image name from `KYPOST_IMAGE` and
`KYPOST_VERSION`, so a tag can resolve to another image than the one you verified, and a
`KYPOST_IMAGE` left on the retired `ghcr.io/busness-app` namespace runs an untrusted image.
Set `KYPOST_IMAGE` to the official name (or remove it) in `.env` first, then, in the same shell
as `$d`:

```bash
grep -q '^KYPOST_IMAGE=' .env && ! grep -qxF 'KYPOST_IMAGE=ghcr.io/busnes-app/kypost-server' .env \
  && echo "fix KYPOST_IMAGE in .env first" >&2 \
  || { o=$(mktemp ./.kypost-manual.XXXXXX.yml) \
       && printf 'services:\n  kypost-server:\n    image: %s\n' "ghcr.io/busnes-app/kypost-server@$d" >"$o" \
       && docker compose -f docker-compose.yml -f "$o" up -d --no-build kypost-server; rm -f "$o"; }
```

`--no-build` stops Compose from building the image from source when the pull did not land.
Then set `KYPOST_VERSION` to that version (or `main`) in `.env`, so later updates follow it.

### Moving a locally built install onto published images

**Every install created before 2026-08-25 needs this once.** There were no
published images before then, so any install older than that is running one it
built itself.

`update-host.sh` refuses a locally built image: it has no published immutable
digest, so there is no rollback target to preserve, and the updater will not
guess at one. Rebuilding from source with `docker compose up --build -d` keeps
you on a local build and hits the same refusal next time. The one-time move onto
published images is:

```bash
git pull --ff-only     # pick up the current compose file
docker compose pull    # fetch the published image over the locally built tag
docker compose up -d   # recreate the container against it
```

Confirm it took — this is the exact property `update-host.sh` tests:

```bash
docker image inspect ghcr.io/busnes-app/kypost-server:stable \
  --format '{{range .RepoDigests}}{{println .}}{{end}}'
```

One `ghcr.io/busnes-app/kypost-server@sha256:...` line means the migration
worked and `./scripts/update-host.sh` will run from now on. No output means the
image is still locally built.

Your data is untouched by this: config, state and private keys live in the four
named volumes, not in the image. Take a backup first anyway — see
[Backup and Restore](STORAGE-AND-RECOVERY.md#backup-and-restore).

To keep building from source instead, that is still supported — `docker-compose.yml`
retains its `build:` stanza. You update with `git pull --ff-only && docker compose
up --build -d` and simply do not use `update-host.sh`.

Automatic updates are opt-in and require a systemd host. Run this from the
checkout to install a daily timer (03:15 local time plus up to one hour of
jitter). It enables systemd lingering for the Docker-operating user so the
timer continues after logout and reboot; if that needs approval, run the
printed `sudo loginctl enable-linger <user>` command once and rerun it:

```bash
./scripts/install-auto-update.sh
```

Disable it with `systemctl --user disable --now kypost-update.timer`. The timer
runs as the Docker-operating user and uses the same host-side updater, not code
inside the container. On systems without systemd, schedule
`./scripts/update-host.sh --auto` with
`KYPOST_AUTO_UPDATE=true` in the scheduler environment.

## Troubleshooting

### Ollama or model issues

- Check the logs with `docker compose logs -f kypost-server`.
- Confirm that the model pull completed for your `OLLAMA_MODEL`.
- `ollama-model.log` in `/kypost/logs` holds the model installer's own output,
  including its pull retries; `ollama.log` holds the Ollama runtime's.
- If necessary, restart with `docker compose restart`.

### IMAP connection issues

- Verify the host, port, username, password, and mailbox in Settings → Mail →
  Email Settings. IMAP needs the implicit-TLS port (normally 993); STARTTLS on
  143 is not supported.
- Run Test Email Settings there.
- Check `daemon.err.log` and `api.err.log` for authentication, TLS, and keyword failures.

### SMTP send issues

- Verify the SMTP host and port in Settings → Mail → Email Settings. SMTP signs
  in with the IMAP username and password.
- Port 465 requires implicit TLS. KyPost supports it.
- If your provider requires app passwords, use them.
- `smtp submission refused: server did not offer STARTTLS` means exactly that —
  the server advertised no STARTTLS, so KyPost refused rather than sending your
  message and password in the clear. Fix the relay, or, for a plaintext relay on
  a network you trust, set `ALLOW_INSECURE_SMTP=true` and understand what you
  are giving up.
- Check `api.err.log` for `mail send failed` details.
- `smtp: acceptance uncertain` means the final DATA acknowledgment was lost or
  invalid. The relay may already have accepted the message; check its logs and
  the recipient before retrying to avoid duplicate mail. A pickup notification
  with this result keeps its encrypted message until it is opened or expires.

### KyPost does not apply labels

- Confirm that the labels exist in the allowlist and the tuning file.
- Confirm that the unread inbox holds eligible messages.
- Check the Decisions page and the poller logs.

### PWA installation on Firefox

- The install button lives on the **Get the apps** page (sidebar, under Settings), alongside links to the native clients.
- Firefox can omit the install prompt event that Chromium browsers emit.
- KyPost still provides a service worker and a manifest. The installation flow differs by browser.
