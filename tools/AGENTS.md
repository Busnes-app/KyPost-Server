# Tools

## Purpose

Pins the Go CI tools (`golangci-lint`, `govulncheck`, `deadcode`) as `tool` directives so their versions are reviewable and Dependabot bumps them.

## Ownership

`tools/go.mod` and `tools/go.sum`.

## Local Contracts

- A separate module, not `tool` lines in `backend/go.mod`: a tool's dependency tree joins the module's build list, so golangci-lint's hundreds of deps would raise versions the app compiles against. Here they cannot.
- `tools/go.mod` requires Go >= 1.27.2; run tools with that toolchain (`GOTOOLCHAIN=go1.27.2` if yours is older).
- Dependabot watches `/tools` (`.github/dependabot.yml`). Go marks tool modules `// indirect`, which Dependabot skips, so each tool is allowed by name; a new tool needs a new `allow` entry.

## Work Guidance

- Bump a pin from `tools/`: `go get -tool <package>@<version>`, then `go mod tidy`. Commit `go.mod` and `go.sum` together.
- Never add app code or app dependencies here.

## Verification

CI runs these from `backend/`:

- `go tool -modfile=../tools/go.mod golangci-lint run ./...`
- `go tool -modfile=../tools/go.mod govulncheck ./...`
- `go tool -modfile=../tools/go.mod deadcode -test=false ./cmd/...`, compared against `backend/deadcode.allow`

## Child DOX Index

No child AGENTS.md files.
