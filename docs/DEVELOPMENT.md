# Development

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## API Highlights

The endpoint reference is [docs/API.md](API.md).

## Build and Dev Checks

These are the same gates CI runs. All of them must pass before a PR merges —
see [CONTRIBUTING.md](../CONTRIBUTING.md) for the full contract.

Backend:

```bash
cd backend
go build -buildvcs=false ./...
gofmt -l .            # must print nothing
go vet ./...
go test -race -count=1 -timeout=20m ./...
```

The `-timeout` is not optional. `internal/api` alone exceeds Go's default 600s
under `-race`, which is why CI splits it into its own job.

Frontend:

```bash
cd frontend
npm ci
npx tsc --noEmit
npm test -- --run
npm run build
```

Push relays (only if you touch `worker/`, `worker-apns/` or `push-relay-shared/`):

```bash
(cd worker && npm ci && npm run typecheck)
(cd worker-apns && npm ci && npm run typecheck)
./scripts/test-relays.sh
```

Install and update scripts:

```bash
shellcheck scripts/*.sh
scripts/update-host.test.sh
scripts/check-env-example.sh
```
