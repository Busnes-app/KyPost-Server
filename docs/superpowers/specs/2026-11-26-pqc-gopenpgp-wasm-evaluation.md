# gopenpgp WebAssembly as KyPost's browser OpenPGP engine: evaluation and production

Date: 2026-11-26. Spec A is `docs/superpowers/specs/2026-11-25-pqc-openpgp-v6-design.md`.

## Decision (owner, 2026-11-26)

**Full replacement.** gopenpgp compiled to WebAssembly is the browser's only OpenPGP engine, for v4 and v6
reads and every existing browser operation (spec A Q5 as recorded in #750; MySlop posts 9302 and 9316). This
was not the hybrid the evaluation below recommended; the evaluation is kept as the evidence behind the
choice. v6 key generation, rollout and device conversion stay in P4.

## What production does (this PR)

- **Every browser OpenPGP path runs the bridge** (`frontend/src/lib/pgpEngine.ts` → `pgpEngineWasm.ts` →
  `backend/cmd/pgpwasm`):
  - keyring parsing (`pgpKeyring.ts`), recovery and device snapshot checks, contact key info
    (`PGPKeyInfo.tsx`);
  - import, v4 generation, encrypt+sign per delivery group, decrypt with the ring;
  - detached sign and verify, and the sender-bound trust decision in `pgpClient.ts`.
- **Ring members keep their stored armor byte for byte.** The bridge reports, per key:
  - lock state: any secret packet encrypted or a GNU dummy is refused inside a ring, and the legacy single
    key path still accepts a dummy primary;
  - the number of keys in a block;
  - expiry;
  - the public packets, which the snapshot check compares as a multiset, so User ID order may differ but a
    changed revocation fails.
- **openpgp.js is gone from production.** It is a dev dependency, used as the interop target
  (`src/test/openpgpEngine.ts`) and to make test fixtures. `pgpEngine.production.test.ts` fails if a
  production file imports it or it returns to `dependencies`. The built `dist` carries no openpgp.js code.
- **Loading.**
  - The module and Go's `wasm_exec.js` load on first PGP use, with Vite content-hashed names, so a cached
    old loader can never meet a new module.
  - A failed load (missing, corrupt, blocked) throws `PgpEngineLoadError` with a user-facing message that
    holds no cause details, and is not cached: the next operation retries.
- **Build.**
  - `scripts/build-pgp-wasm.sh` sets `GOTOOLCHAIN` to go.mod's exact `toolchain` line and checks the result.
  - `npm run build:pgp-wasm` calls it. The image builds the module in the Go stage and copies it into the
    Node stage before `npm run build`. CI builds it before typecheck.
  - A build without the module fails.
- **Transfer.** Static files are served as they are on disk, so the build emits `.br` and `.gz` beside the
  module. `handleFrontend` sends the smallest one the browser accepts, as `application/wasm` (1.80 MB
  brotli instead of 8.95 MB).
- **CSP.**
  - Only the app shell document (`index.html` and SPA routes) gets `'wasm-unsafe-eval'` in `script-src`.
    API responses, assets and every other response keep the base policy.
  - `'unsafe-eval'` and `'unsafe-inline'` never appear (`TestCSPAllowsWasmOnlyForTheAppShell`).
  - `handleFrontend` computes the policy itself, because `withDeadline` gives handlers a fresh header map;
    the first browser run caught a version that edited the middleware's header and shipped an empty
    policy instead.

### Production evidence

- `browser/pgp-wasm.spec.mjs` loads the built engine chunk in Chromium through the real server, whose
  document CSP it checks. The module arrives as `application/wasm` with `Content-Encoding: br`, and no CSP
  violation is reported. Under that policy it:
  - generates v4 keys and round-trips Unicode text, encrypted and signed;
  - refuses a wrong recipient and a signature checked against the wrong signer's key;
  - opens the openpgp.js-made ring fixture (`testdata/pgp-keyring-v1.json`);
  - opens the v4, v4+ID 35 and v6 ML-KEM messages and verifies the ML-DSA-65+Ed25519 signature in
    `testdata/openpgp-v6`.
- **Failure handling:** a 404 and a corrupt module each give `PgpEngineLoadError`, and the next call after
  the fault clears succeeds.
- **Measured there** (Chromium via Playwright 1.65.1, loopback, desktop CPU): 93 ms from first import to a
  generated v4 key, cold. The JS heap read about 10 MB (`performance.memory`, coarse).
- **Cross-implementation.** The `testdata/openpgp-v6` fixtures are made by go-crypto and cross-checked with
  Bouncy Castle 1.87 (`testdata/openpgp-v6/README.md`), and the engine opens and verifies them. The engine
  and openpgp.js 6.3.3 interoperate both ways for v4.
  - **Gap:** no Sequoia, and no openpgp.js v7, which is unreleased and has no RFC 9980 interop.
- **Key handling, as the code does it, no stronger:**
  - The bridge keeps no key between calls. Each call parses the armored key, uses it, calls gopenpgp's
    `ClearPrivateParams` and zeroes its own copies of inputs and plaintext.
  - Go's GC does not zero freed memory, and CIRCL's ML-KEM key struct is left to the GC (gopenpgp's own
    note), so copies can remain in wasm linear memory until overwritten.
  - The unlocked key also stays an immutable JS string in `keyVault.ts`, as before.
  - Nothing in the bridge logs or makes network calls; it imports no networking package, by reading.
  - No test proves memory erasure.

## Evaluation (before the decision)

### Recommendation at the time

**Hybrid now, decide "replace" at P4.** Keep openpgp.js 6.3.3 for every v4 operation and run gopenpgp WASM,
loaded lazily, for v6 / RFC 9980 (P3 read support, P4 generation). Do not pin an openpgp.js v7 pre-release
commit for PQC.

- **It works today and the alternative does not.** The wasm engine passes all 27 `src/lib/` test files (407
  tests, including every `pgpClient`, key-floor, keyring and vault test) unchanged, interoperates with
  openpgp.js in both directions, and makes, encrypts to, signs with, decrypts and verifies RFC 9980 v6 keys
  (ML-DSA-65+Ed25519 / ML-KEM-768+X25519, SEIPDv2 OCB). openpgp.js has no released RFC 9980 support.
- **One implementation.** The browser then runs the same gopenpgp v3.5.2 / go-crypto v1.5.2 and the same
  `internal/pgpprofile` code as the server and the Mac, so the shared fixtures test one implementation, not
  two, and there is no openpgp.js-vs-go-crypto disagreement left to find (spec A already hit one on User IDs).
- **The cost is size and first-use latency, not operation speed** (numbers below). The module is 1.80 MB
  brotli (8.94 MB raw) against openpgp.js's 107 KB, and is ready 58 ms after a cold load from loopback
  (208 ms at 4x CPU throttle; network time comes on top). openpgp.js is about 3x faster per v4 operation
  (about 1 ms against 3.5 ms for a 16 KiB encrypt+sign), and v6 PQC on wasm takes about 6 ms. Every operation
  stays under 60 ms even at 4x throttle. The load cost is paid once per session, only by someone who touches
  PGP, and HTTP caching removes the download after the first visit.
- **Replace (drop openpgp.js) is plausible but not yet earned.** It needs the four gaps under "What a full
  replacement still needs" closed, mainly that keyring parsing still runs on openpgp.js. Revisit at P4 with
  the same suite; if the gaps close, removing openpgp.js removes an npm crypto dependency and a second
  OpenPGP parser from the page.
- **openpgp.js v7 stays a fallback option,** not the plan: pin it only if the CSP change or the size proves
  unacceptable, and then gate it on the same cross-implementation fixtures.

### What was built

| Piece | Where | Notes |
|---|---|---|
| Shared profile | `backend/internal/pgpprofile` | `V6()`: gopenpgp `profile.PQC()` with AEAD **OCB** (spec A "The KyPost profile"). `RequireStrongRSA` moved here from `pgpmail/keyfloor.go`, which now calls it, so the browser and server run one RSA-2048 floor. Imports only gopenpgp |
| wasm bridge | `backend/cmd/pgpwasm` (`GOOS=js GOARCH=wasm` only) | Registers `globalThis.kypostPGP`: generate (v4, v6), importPrivate, inspect, encrypt(+sign) per delivery group, signDetached, decrypt with a ring, verifyDetached, armor, dearmor. Bytes in and out as `Uint8Array`. Each function is one gopenpgp call; no crypto logic of its own |
| Build | `scripts/build-pgp-wasm.sh` | `-trimpath -ldflags=-s`, output `frontend/.pgp-wasm/` (ignored). **`wasm_exec.js` is generated, not checked in:** copied from `$(go env GOROOT)/lib/wasm` of the toolchain that built the module, so the two can never drift. go.mod's `toolchain go1.27.4` picks it |
| Engine interface | `frontend/src/lib/pgpEngine.ts` | `pgpClient.ts` reaches OpenPGP only through it. `pgpEngineOpenpgp.ts` holds the previous openpgp.js calls with the same options; `pgpEngineWasm.ts` lazy-loads the wasm |
| Dev switch | `VITE_PGP_ENGINE=gopenpgp-wasm` | Build-time; production never sets it. `npm run dev` serves `.pgp-wasm/` at `/pgp-wasm/` (serve-only Vite plugin) |
| Tests | `pgpEngine.parity.test.ts`, `cmd/pgpwasm/bridge_test.go`, `pgpprofile_test.go` | CI's frontend job builds the wasm, runs `npm test` with the parity file required (`KYPOST_REQUIRE_PGP_WASM=1`), reruns `src/lib/` on the wasm engine, and runs the Go bridge tests under Node |
| Benchmark | `frontend/browser/pgp-wasm-bench/` | Real Chromium via Playwright on 127.0.0.1:5391, KyPost's production CSP |

#### Behaviour differences found and fixed in the bridge

The first wasm run passed 60 of 66 pgpClient/keyring tests. Each failure was a real difference, fixed to match
what KyPost already promises:

1. **Line endings.** openpgp.js turns a text literal's canonical CRLF back into LF; gopenpgp returns the bytes
   (it sanitizes only text-signed messages). The bridge encrypts text with `Utf8()` and normalizes on decrypt.
2. **RSA-2048 signature trust.** go-crypto's floor is 2047, so an RSA-2047 signature verified. The bridge marks
   a signature valid only if `pgpprofile.RequireStrongRSA` accepts its signer, the rule the server uses.
3. **Import keeps the user's bytes.** gopenpgp re-armors differently from openpgp.js; an unprotected import now
   returns the input armor unchanged instead of re-serializing it.

Two more rules the bridge enforces, from spec A: one delivery group never mixes v4 and v6 keys, and a v6
delivery is signed with the v6 key (Q10).

### Measurements

Chromium 159.0.8154.6 (Playwright 1.65.1) on Major (Fedora, 20 cores), Go go1.27.4, fresh browser
context per run (cold cache). Median of 15 (desktop) and 5 (4x throttle) runs; 16 KiB body. Raw output: `2026-11-26-pqc-gopenpgp-wasm-bench.json` beside this note.

**Size**

| Asset | Raw | gzip -9 | brotli -11 |
|---|---|---|---|
| `kypost-pgp.wasm` | 8,938,965 | 2,444,197 | 1,798,246 |
| `wasm_exec.js` | 16,992 | | |
| `openpgp.min.mjs` 6.3.3 | 395,333 | 129,781 | 106,842 |

**Cold load** (fresh context, served from loopback, so network transfer is excluded: at 10 Mbit/s the brotli wasm adds about 1.4 s, openpgp.js about 0.09 s)

| | Desktop | 4x CPU throttle |
|---|---|---|
| wasm: fetch + compile + instantiate | 37.5 ms | 130.2 ms |
| wasm: ready (Go `main` registered the API) | 57.7 ms | 208.3 ms |
| openpgp.js: `import()` | 29 ms | 78.7 ms |

**Per operation, median ms (max)**, Curve25519 v4 and RFC 9980 v6

| Operation | openpgp.js v4 | wasm v4 | wasm v6 PQC | openpgp.js v4, 4x | wasm v4, 4x | wasm v6, 4x |
|---|---|---|---|---|---|---|
| generate key | 0.9 (7.3) | 0.9 (11.4) | 3.8 (7.1) | 6.6 (29.5) | 4.5 (34.6) | 17.8 (52.3) |
| encrypt + sign | 1 (4.5) | 3.5 (16.4) | 5.9 (8.4) | 4.6 (21.3) | 15.8 (58.4) | 28.5 (46.1) |
| decrypt + verify | 1.2 (5.5) | 3.7 (9.1) | 5.6 (7.8) | 8 (23.5) | 28.7 (40.1) | 27.6 (37.8) |
| detached sign | 0.3 (0.6) | 1.3 (2.8) | 3 (4.5) | 1.5 (3) | 7.3 (9.1) | 14.3 (19.5) |
| detached verify | 0.3 (0.5) | 1.2 (2) | 1.9 (2.9) | 1.5 (2.5) | 5.4 (11.8) | 12.6 (14) |

**Sizes on the wire**: v4 public key 757 B armored (openpgp.js 807 B); v6 PQC public key 18,581 B armored (spec A estimated ~18.5 KB); a 16 KiB body encrypted+signed is 22,782 B armored for v4 and 28,858 B for v6.

**Memory** (after the run)

| | wasm linear memory | JS heap used |
|---|---|---|
| wasm engine, desktop | 12.06 MB | 1.82 MB |
| openpgp.js engine, desktop | — | 3.96 MB |

Reading: openpgp.js is about 3x faster per v4 operation, but every operation on either engine is under 58 ms even at 4x throttle, well below what a user notices. The wasm engine's peak linear memory, about 11 MiB, is small for an Android Chrome tab (whose limits are in the hundreds of MB; not measured on a device). The real costs are the 1.80 MB download and the 208 ms throttled start, both once per session, and only when the user touches PGP.

### Interop

| Check | Result | Test |
|---|---|---|
| v4: openpgp.js encrypts+signs → wasm decrypts+verifies, and the reverse, and each with itself | pass | `pgpEngine.parity.test.ts` |
| v4 detached signatures both ways; one flipped byte fails on both engines | pass | same |
| Wrong recipient: neither engine opens a message with a key it was not sent to | pass | same; `TestRefusals` |
| Armor/dearmor round trip identical bytes across engines | pass | same |
| The whole existing suite with openpgp.js-made fixtures consumed by the wasm engine | 27 files, 407 tests pass | `VITE_PGP_ENGINE=gopenpgp-wasm vitest run src/lib/` |
| wasm v4 output is v3 PKESK + SEIPDv1 (what the server writes) and go-crypto opens and verifies it; a `crypto.PGP()` message (the call `pgpmail.EncryptStoredMIME` makes, `mime.go:179`) opens in the bridge | pass | `TestV4InteropWithServerPath` |
| wasm v6: only v6 PKESKs to ML-KEM-768+X25519 (35), SEIPDv2 OCB, ML-DSA-65+Ed25519 (30) signature; go-crypto (the server's library) opens and verifies it | pass | `TestV6IsPQAndOpensWithGoCrypto` |
| Server profile `pgpprofile.V6()` makes 30/35 keys and SEIPDv2 OCB AES-256 | pass | `TestV6ProfileIsPQWithOCB` |
| openpgp.js 6.3.3 reads a wasm v6 PQC key | refuses, as expected | parity test |
| P0 fixtures (`testdata/openpgp-v6`) | pass: wasm opens `v4`, `v4-pqc` (v4 key with an ID 35 subkey) and `v6-pqc` messages and verifies `v6-pqc.sig.asc`; openpgp.js 6.3 opens `v4` and refuses `v6-pqc`. Run against bracken's uncommitted working copy (read only), since PR 1 is not pushed yet | parity test, `KYPOST_PGP_FIXTURES=<dir>` |

`internal/pgpmail` itself does not compile for js/wasm (`internal/fsutil` uses `flock` and `statfs`), so the
bridge test uses the same gopenpgp calls rather than the server function. That is why the profile lives in
its own package.

#### Mutations (each fails with the change, passes without)

| Mutation | openpgp.js | wasm |
|---|---|---|
| M1 `buildEncryptedDeliveries` encrypts to the sender instead of the recipients | 3 of 42 pgpClient tests fail | 3 of 42 fail |
| M2 `judgeSignatures` drops the address-book binding | 3 of 42 fail | 3 of 42 fail |
| M3 `pgpprofile.V6()` uses GCM instead of OCB | n/a | `TestV6ProfileIsPQWithOCB` fails |
| M4 the bridge's one-version-per-group check removed | n/a | parity test fails (1) |
| M5 the bridge's RSA floor on signature trust removed | n/a | 2 key-floor tests fail |

### Security review

#### CSP

KyPost's CSP (`backend/internal/api/security_headers.go`) has `script-src 'self'` unless the Friendly
Captcha provider is configured, which already adds `'wasm-unsafe-eval'`. Chromium refuses to compile the
module without it; proven by the benchmark, which loads the page under the production policy first:

```
CompileError: WebAssembly.instantiateStreaming(): Compiling or instantiating WebAssembly module violates the following Content Security policy directive because 'unsafe-eval' is not an allowed source of script in the following Content Security Policy directive: "script-src 'self'".
```

Chromium's message names `'unsafe-eval'`, but `'wasm-unsafe-eval'` is enough: the same run with only that
source added loads and runs every operation. The change for the wasm engine is exactly one source in
`script-src`, on every install (header as served in the benchmark, no CAPTCHA provider):

```
Content-Security-Policy: default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https: http:; media-src 'self' data: https: http:; font-src 'self' data:; connect-src 'self'; worker-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'
```

`'wasm-unsafe-eval'` allows compiling WebAssembly only. It does not allow `eval`, `new Function` or inline
script (CSP Level 3, "wasm-unsafe-eval"); the module itself is same-origin (`'self'`), so an attacker needs to
get bytes served from this origin first. It does widen what a script that already runs can do (compile wasm
from a fetched or constructed buffer), which matters little once script execution is already lost. This PR
did not change the server's CSP at the time; production now does, for the app shell only (see "What
production does").

#### Constant-time posture

- **openpgp.js 6.3.3:** RFC 9580 Ed25519/X25519 (algorithms 25-27) go to WebCrypto when the browser has it
  (`dist/openpgp.mjs`, `webCrypto.generateKey('Ed25519' | 'X25519')`), otherwise a bundled tweetnacl port.
  KyPost's own keys are the legacy EdDSA/ECDH algorithms 22/18, which run in JavaScript (tweetnacl,
  @noble/curves). The noble authors state that JIT compilers and garbage collection make constant time
  "extremely hard" in JavaScript (noble-curves README). openpgp.js v7's ML-KEM/ML-DSA come from
  @noble/post-quantum, pure JavaScript (not read in this run; unverified).
- **gopenpgp WASM:** everything runs in Go compiled to wasm: Go's X25519/Ed25519 and AES, and CIRCL's ML-DSA,
  through go-crypto. Go's crypto is written to be constant time in Go's own code generation, but under js/wasm
  there is no assembly and the browser's wasm JIT has the last word. WebAssembly gives no constant-time
  guarantee (Watt et al., "CT-Wasm", POPL 2019, the reason that proposal exists). **Unverified:** no one has
  measured timing leakage for either engine in a browser. Neither is better established than the other; both
  are "constant-time source, no guarantee after the JIT". The one real difference: openpgp.js can hand
  RFC 9580 curves to WebCrypto, which runs native code; the wasm engine never does.
- Randomness: Go's `crypto/rand` on js/wasm calls `crypto.getRandomValues` (Go documentation for
  `crypto/rand`), the same source openpgp.js uses.

#### Secret-key lifetime

- **Same as today in JavaScript.** The unlocked private key lives as an armored JS string in `keyVault.ts`
  module memory for both engines. JS strings are immutable and cannot be zeroed; that does not change.
- **In wasm:** every call copies the key into Go memory, parses it, uses it and calls gopenpgp's
  `ClearPrivateParams` (keys, rings, handles), and the bridge zeroes its own input and plaintext copies. Gaps:
  gopenpgp notes it cannot reach inside CIRCL's ML-KEM key struct and leaves that to the GC
  (`crypto/key_clear.go`, `clearMlKemECDHPrivateKey`); Go's GC does not zero freed memory; and the wasm
  linear memory only grows, so old copies can survive in it until overwritten. So: better than openpgp.js
  (which zeroes nothing), not a guarantee. A stronger design keeps the parsed key inside wasm for the
  session instead of re-sending armor per call; out of scope here.
- A panic in the bridge is caught and returned as an error string; the Go runtime keeps running. go-crypto
  errors carry no key material in the paths tested; not audited beyond that.

#### Supply chain

- **wasm:** one Go module tree the server already pins and builds (`backend/go.sum`): gopenpgp v3.5.2,
  go-crypto v1.5.2, CIRCL, x/crypto. Already under `govulncheck` in CI and Dependabot for Go. The build is
  `-trimpath`; reproducibility of the wasm itself was not checked (the server binary's is, in CI).
- **openpgp.js:** one npm package that bundles its crypto (tweetnacl, @noble/*) into its dist; audited by
  `npm audit`. A v7 pin is a git dependency on an unreleased commit, which Dependabot handling is unproven
  (spec A, Q5).
- Replacing openpgp.js entirely would remove the browser's only npm OpenPGP dependency.

### What a full replacement still needed, and where it stands

1. **Keyring parsing:** done; `pgpKeyring.ts` reads rings through `engine.inspect`.
2. **Contact key info:** done; `PGPKeyInfo.tsx` uses `engine.inspect`.
3. **Keeping the parsed key inside wasm for the session:** not done. The bridge stays stateless by design,
   so nothing secret persists in wasm between calls by code; see "Secret-key lifetime".
4. **Mobile memory:** not done. No Android device run (CPU throttling only).

## Not done, unproven

- No real Android device or memory cap. The "Android Chrome-class" numbers are 4x CPU throttling on desktop
  Chromium, with memory reported, not limited.
- Timing side channels: unmeasured for both engines.
- wasm build reproducibility: **not reproducible across environments.** The module built on Major and the
  one built in the image differ in bytes (8,951,242 against 8,950,891 B), though both use Go 1.27.4 and
  `-trimpath`; the cause is not investigated.
- Memory erasure of key material inside wasm: not proven by any test.
- Sequoia and openpgp.js v7 interop: not run.
