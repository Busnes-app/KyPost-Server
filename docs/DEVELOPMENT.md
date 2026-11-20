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

## Native mail harness

`scripts/dev/native-mail-harness.sh` runs native mail end to end on one Linux
x86_64 machine, for browser checks of plus and masked addresses and anything
else that needs a real KyIdentity, DNS proof, receiving and a relay. It is a
developer tool, not a deployment profile. It has run only on the maintainers'
Fedora workstation; other distributions are untested.

**What it runs.** Everything is in one `unshare -rmn` namespace, so it uses
loopback only and nothing listens on a host interface:

| Piece | Address inside the namespace | Notes |
| --- | --- | --- |
| dnsmasq | `127.0.0.1:53` | Answers `harness.test` A, MX and the `_kypost-mail` TXT proof. `/etc/resolv.conf` and `/etc/nsswitch.conf` are bind-mounted so Go, glibc and Chromium all ask it |
| KyIdentity | `https://id.harness.test` (`10.77.0.2:443`) | A real server from your checkout: OIDC issuer and signed directory source. A small Node TLS front (`tls-front.mjs`) stands in for the production proxy |
| KyPost | `https://mail.harness.test:8443` | This checkout, `KYPOST_NATIVE_MAIL=true`. Receiving is switched on only after the domain proves, as in production |
| Maddy 0.9.5 | `mx.harness.test:25` (`10.77.0.3`) | Started by the production `scripts/start-receiving.sh`, which checks the hash and generates the config |
| kypost-demo-IMAP fixture | `smtp.relay.test` (`10.77.0.4`): 465, 587, 993 | The domain relay. `fixture-smtps.mjs` adds the implicit-TLS 465 listener the relay needs, reusing the fixture's own SMTP session; relayed mail lands in persona `charlie`'s Sent Items |

The `10.77.0.x` addresses are added to the namespace's `lo`, because KyIdentity
refuses loopback webhook targets; it accepts private ones with its existing
`KYIDENTITY_ALLOW_PRIVATE_CALLBACKS`. A throwaway CA signs one certificate for
all hosts. Servers trust it through `SSL_CERT_FILE`, and Chromium through an
NSS database, so no TLS check is skipped.

**No fakes in KyPost.** No build tag, flag or environment variable in the
KyPost binary turns on a fake. DNS comes from dnsmasq through the system
resolver, identity from a real KyIdentity, and mail from real SMTP. The only
stand-ins are outside KyPost: the TLS front and the fixture's 465 listener.

**Inputs.** Build the frontend first (`cd frontend && npm ci && npm run build`).
Then set:

- `KPH_KYIDENTITY_SRC`: a KyIdentity-server checkout. The harness builds
  `cmd/kyidentity` from it, and serves `css/` and `fonts/` from it.
- `KPH_FIXTURE`: a kypost-demo-IMAP checkout with `npm ci` run (tested at `31cf283`).
- `KPH_MADDY`: the pinned Maddy binary. Fetch and verify it as in
  [native mail section 9](guides/NATIVE_MAIL.md#9-receiving-profile-b-maddy-on-port-25),
  keeping the binary anywhere outside the checkout.
- `KPH_PLAYWRIGHT`: the path to `@playwright/test`'s `index.mjs`, unless Node
  can resolve `@playwright/test` itself. Chromium must already be installed.
- Optional `KPH_DIR`: the state directory, default `~/.cache/kypost-native-harness`. `down` deletes it only if `up` created it.
  Keep it on a disk with over 5 GiB free; KyPost refuses mail below its reserve.

The host needs `unshare` and `nsenter` (util-linux 2.38+, unprivileged user
namespaces enabled), plus `dnsmasq`, `openssl`, `certutil`, `jq`, `curl`, `ip`,
`go` and `node`.

**Run.**

```bash
H=scripts/dev/native-mail-harness.sh
bash $H up          # build, boot, KyPost apply-setup, publish the TXT proof
bash $H exec node scripts/dev/native-mail-harness/checks.mjs admin-setup
bash $H provision   # KyIdentity apply-setup and directory push, receiving init, Maddy
bash $H exec node scripts/dev/native-mail-harness/checks.mjs check3   # plus addressing
bash $H exec node scripts/dev/native-mail-harness/checks.mjs check4   # masked addresses
bash $H down        # stop everything in the namespace, delete the state directory
```

- `admin-setup` signs in as the bootstrap administrator, verifies the domain,
  saves and checks the relay, and sets the encryption key policy to Optional
  with incoming encryption off.
- `provision` runs KyIdentity's `apply-setup` (administrator `idp-admin`,
  everyday `owner@harness.test`, client `kypost` with its signed suite webhook)
  and `resync-system --wait`.
- `check3` and `check4` sign `owner` in through KyIdentity and write
  screenshots plus `evidence.json` (SMTP replies and relayed headers) to
  `$KPH_DIR/shots`.

Other commands:

- `bash $H send FROM TO SUBJECT [BODY]`: delivers to the MX over STARTTLS.
- `bash $H relay-sent`: lists what the relay accepted.
- `bash $H exec CMD`: runs anything else inside the namespace.

Logs are in `$KPH_DIR/logs`, and generated secrets in `$KPH_DIR/secrets`
(throwaway values).
