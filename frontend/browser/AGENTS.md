# Browser harness

## Purpose

Drive the built frontend through Chromium (Playwright) against a real, disposable KyPost server with its production CSP, CSRF and sign-in, to capture screenshots and measurements and to host browser specs.

## Ownership

This directory owns the disposable server launch (`server.mjs`), the global setup (`setup.mjs`) and the specs (`*.spec.mjs`). The parent owns `playwright.config.mjs`, the `test:browser` script and the exact `@playwright/test` pin.

## Local Contracts

- `server.mjs` builds nothing. It expects a prebuilt `frontend/dist` and a server binary at `frontend/.browser/server` (both ignored), seeds the first-run admin with `--mode bootstrap-admin` (`MUST_CHANGE_PASSWORD=true`), then runs the default mode with a minimal environment (`PATH`, `CONFIG_DIR`, `STATE_DIR`, `LOG_DIR`, `SECRET_DIR`, `FRONTEND_DIR`, `WEB_PORT=5391`) in an owned temporary directory, deleted on exit. Server log: `test-results/server.log`.
- The server has no bind-address setting and listens on all interfaces on 5391 for the run; the browser uses `http://127.0.0.1:5391` only. Credentials are disposable test values.
- `setup.mjs` signs in once through the real form (proof of work and credential derivation run in the browser), rotates the forced password, sets Server → Security encryption policy to key Optional / incoming Off (a fresh server requires a key, which needs a mail account), dismisses the offered setup with "Not now", creates `CONTACTS` through `POST /api/contacts` with the app's CSRF header, and saves `test-results/admin-state.json`, which every spec reuses. One sign-in per run keeps it far from any lockout; a step-up (ReauthGate) is a password confirm, not a sign-in.
- No mail account or IMAP fixture is needed: compose opens and the recipient field, autocomplete and validation work without one. Specs that send or read mail would need the kypost-demo-IMAP fixture on loopback ports (see `docs/ui-fleet-2026-10-26/drivers/run.sh.txt`).
- Real authentication and API state only: do not mock responses, relax CSP/CSRF or block service workers.
- `capture.spec.mjs` writes `<widget>-<project>.png` and `measurements-<project>.json` to `$SHOTS_DIR` (default `test-results/shots`): widgets `recipients`, `recipients-error`, `panel-tabs`, `security-tabs`, `page-tabs`. Measurements select by class names (`.compose-token-*`, `.recipient-autocomplete*`, `.config-tab(s)`, `.sec-tab(s)`, `.contacts-page-tab`); keep those classes or update the `TARGETS` map. The JSON also holds the CSP header, `<style>` elements present at load and added after it, and `securitypolicyviolation` events and CSP console messages.
- Projects are light/dark × 390/1280 wide (900 tall), one worker. Port 5391 is shared with other products' suites: run under `flock /tmp/ky-heavy-test.lock`.

## Work Guidance

- Organisation suggestions (`/api/suite/people`) need KyIdentity, which this harness does not run, so `organisationHeader` measures `null`.
- Contacts paginate at `CONTACTS_PER_PAGE` (20, `src/pages/contacts/formState.ts`); setup creates 25.

## Verification

From `frontend/`, with Node 26:

```sh
npm ci && npm run build
(cd ../backend && go build -buildvcs=false -trimpath -o ../frontend/.browser/server ./cmd)
npx playwright install chromium   # once per Playwright version
SHOTS_DIR=/path/to/shots flock /tmp/ky-heavy-test.lock npm run test:browser
```

## Child DOX Index

None.
