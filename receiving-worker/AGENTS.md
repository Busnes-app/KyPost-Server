# Receiving Worker

## Purpose

One-message Cloudflare Email Worker qualification pilot with private R2 retention and authenticated HTTPS pickup.

## Ownership

`receiving-worker/`; independent of the push relay Workers and credentials.

## Local Contracts

- Await conditional persistence of exact raw bytes and frozen envelope before handler success. An occupied slot rejects subsequent events; never overwrite or delete it through HTTP.
- Only the operator-installed 15-minute exact-recipient route can capture mail. The claim is an expectation; KyPost separately rechecks current domain, identity, revision, source and restore authority.
- Dedicated 256-bit pickup secret; authenticate before storage reads, HTTPS only, no public bucket, correspondence logs or request-selected scanner policy.
- Preserve retained bytes after expiry and failed pickup. Cleanup is a separate operator action after verified local delivery.

## Work Guidance

- Use JavaScript platform APIs and Node's built-in test runner. Keep this bounded pilot explicit; ongoing queueing and automatic cleanup require a separate contract.

## Verification

- `node --test receiving-worker/worker.test.mjs` checks capture limits, atomic refusal, private pickup and retention.
- `node receiving-worker/runtime-check.mjs` uses the existing pinned Wrangler/Miniflare dependency after `npm ci` in `worker/`; disposable workerd/R2 state proves conditional capture and exact retained pickup.

## Child DOX Index

None.
