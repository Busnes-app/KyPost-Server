# Receiving Worker

## Purpose

Cloudflare Email Workers with private R2 retention and authenticated HTTPS pickup: the one-message qualification pilot (`worker.mjs`) and the continuous receiving protocol (`continuous.mjs`, wire contract in `docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md#wire-contract`).

## Ownership

`receiving-worker/`; independent of the push relay Workers and credentials. The deployment's `main` selects pilot or continuous; the two modules share no code so the pilot can be deleted whole once continuous is qualified.

## Local Contracts

- Await conditional (`If-None-Match: *`) persistence of exact raw bytes and frozen envelope before handler success; never overwrite a captured object.
- Pilot: only the operator-installed 15-minute exact-recipient route captures, into one immutable slot; an occupied slot rejects; no HTTP delete. Storage failure rejects.
- Continuous: routing comes only from an Ed25519-signed table (exact payload bytes with a context prefix, revision strictly increasing, at most 5 minutes ahead, refused after 14 days). Blocked senders and unknown recipients reject before the body is read. Storage failure throws, never rejects. Delete only on digest match. Credentials live in `credentials.json`; deployed secrets bootstrap epoch 1 only while it is absent; rotation needs the current key, epoch + 1 and an etag-conditional replace. Changing the wire contract means changing that doc section in the same change set.
- Dedicated 256-bit pickup bearer, checked before any mail or route read (continuous reads only `credentials.json` first); HTTPS only, strict paths, no redirects, no public bucket, no correspondence in logs, no request-selected scanner policy.
- The pilot preserves retained bytes after expiry and failed pickup; cleanup is a separate operator action.

## Work Guidance

- Use JavaScript platform APIs and Node's built-in test runner; no dependencies beyond the pinned Miniflare in `worker/` for runtime checks.

## Verification

- `node --test receiving-worker/*.test.mjs` (also run by `scripts/test-relays.sh`): pilot capture limits, atomic refusal, private pickup and retention; continuous signature, ordering, staleness, blocks, size caps, conditional capture, paging, digest delete, strict paths, bearer-before-read and rotation fencing.
- `node receiving-worker/runtime-check.mjs` and `node receiving-worker/runtime-continuous-check.mjs` use disposable workerd/R2 state (after `npm ci` in `worker/`); CI runs both.

## Child DOX Index

None.
