# Exposing the KyPost web UI with Cloudflare Tunnel

This guide puts the KyPost web UI (port 5866) on a public HTTPS hostname through
a Cloudflare Tunnel (`cloudflared`).

It covers the **web UI only**. Receiving mail through Cloudflare Email Routing is
a different thing, with its own setup and trade-offs: see
[NATIVE_MAIL.md](./NATIVE_MAIL.md). You can use either without the other.

## 1. Why a tunnel, and what it costs

What you get:

- **A real HTTPS origin.** The browser sees `https://mail.example.com` with a
  valid certificate. The login proof-of-work (the default `CAPTCHA_PROVIDER=pow`)
  needs a secure context, so on a plain `http://` LAN address nobody can sign in.
- **No inbound port.** `cloudflared` dials out to Cloudflare. Nothing on your
  router or firewall is opened, and port 5866 stays unpublished or loopback-only.
- **No certificate to manage** for the public side.

What it costs:

- **Cloudflare terminates TLS.** Traffic is decrypted at Cloudflare's edge and
  re-encrypted inside the tunnel. Cloudflare can see everything the browser and
  KyPost exchange: your password at sign-in, session cookies, and the mail the UI
  displays or sends. It could also alter it.
- **You depend on a third party** for reachability and on your Cloudflare
  account's security. The tunnel token is a credential: anyone holding it can
  run your tunnel.

If that is not acceptable, terminate TLS yourself instead: `TLS_CERT_FILE` /
`TLS_KEY_FILE` in KyPost, or your own nginx/Caddy. See the TLS options in the
[README Quick Start](../../README.md#quick-start).

## 2. Pick a topology

Every topology needs the same three answers: how `cloudflared` reaches KyPost,
which address KyPost sees it arrive from (that goes in `TRUSTED_PROXY_CIDRS`),
and which header carries the visitor's address (`cf-connecting-ip`).

| | A. cloudflared container on `kypost-net` (recommended) | B. cloudflared service on the same host | C. cloudflared → nginx → KyPost | D. cloudflared on another machine |
|---|---|---|---|---|
| cloudflared target | `http://KyPost-Server:5866` | `http://127.0.0.1:5866` | nginx; nginx → `http://KyPost-Server:5866` | `https://<KyPost host LAN IP>:5866` with `noTLSVerify: true` |
| `KYPOST_BIND` | `127.0.0.1` | `127.0.0.1` | `127.0.0.1` | the KyPost host's LAN IP |
| `TRUSTED_PROXY_CIDRS` | cloudflared's pinned address, e.g. `10.89.0.10/32` | `10.89.0.1/32` (the `kypost-net` gateway) | **nginx's** pinned address | the other machine's address as KyPost sees it, `/32` |
| `TRUSTED_PROXY_HEADER` | `cf-connecting-ip` | `cf-connecting-ip` | `cf-connecting-ip` | `cf-connecting-ip` |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | not needed | not needed | not needed | **yes** (self-signed is fine) |
| `ALLOW_INSECURE_HTTP` | leave unset | leave unset | leave unset | leave unset (TLS instead) |

All topologies: set `SERVER_BASE_URL=https://mail.example.com` (see
[step 4](#4-configure-kypost)).

Notes per topology:

- **A.** Container-to-container traffic on `kypost-net` keeps the real peer
  address and ignores published ports. Network design and recovery from Docker
  network errors: [Reverse_Proxy_Networking.md](../Reverse_Proxy_Networking.md).
  A cloudflared container on any other network (including a plain `docker run`
  with no `--network`) cannot use `127.0.0.1` (that is its own loopback) and
  reaches KyPost only source-NATed, so every visitor looks the same.
- **B.** A process on the host reaching the published `127.0.0.1:5866` arrives
  from the `kypost-net` gateway, the first address of `KYPOST_SUBNET`
  (`10.89.0.1` by default), not from `127.0.0.1`. Every other process on the
  host arrives from there too, so trust `10.89.0.1/32` **only** with
  `KYPOST_BIND=127.0.0.1`. This is documented for rootful Docker; rootless
  Docker and Podman may differ, so check with [step 6](#6-verify).
- **C.** nginx must pass the header on and must be reachable only from
  cloudflared, or a client can send its own `CF-Connecting-IP`:

  ```nginx
  proxy_http_version 1.1;
  proxy_set_header CF-Connecting-IP $http_cf_connecting_ip;
  ```

  `proxy_http_version 1.1;` matters: mail export refuses mbox over HTTP/1.0.
  If nginx sets `X-Forwarded-Proto`, it must say `https`. A
  `X-Forwarded-Proto $scheme` line reports `http` (cloudflared talks plain HTTP
  to nginx), and KyPost believes a trusted proxy's `X-Forwarded-Proto` over
  everything else, so the session cookie loses `Secure`.
- **D.** That hop carries session cookies across a real network, so encrypt it.
  KyPost refuses to start with a non-loopback `KYPOST_BIND` and no TLS unless you
  set `ALLOW_INSECURE_HTTP=true`; do not use that escape hatch for this. Mount a
  certificate (the commented volume in `docker-compose.yml` mounts at
  `/kypost/tls`) and set:

  ```sh
  TLS_CERT_FILE=/kypost/tls/fullchain.pem
  TLS_KEY_FILE=/kypost/tls/privkey.pem
  ```

  Set both or neither; one alone is a startup error. KyPost re-reads the pair
  when it changes, so renewals need no restart. To find the address for
  `TRUSTED_PROXY_CIDRS`, see [step 6](#6-verify).

## 3. Create the tunnel

Pick one method. Replace `mail.example.com` with your hostname; its domain must
be on your Cloudflare account.

### Dashboard (token)

1. In the Cloudflare dashboard open **Zero Trust → Networks → Tunnels** and
   create a tunnel of type **Cloudflared**. Name it `kypost`.
2. Copy the tunnel token from the install command it shows. Treat it as a
   secret.
3. Add a public hostname (newer dashboards call these published application
   routes): `mail.example.com`, service type and URL from the table above, for
   example `HTTP` + `KyPost-Server:5866` for topology A.
4. For topology D, set the service type to `HTTPS` and enable **No TLS Verify**
   under the hostname's additional TLS settings.

### CLI (locally managed)

```sh
cloudflared tunnel login
cloudflared tunnel create kypost
cloudflared tunnel route dns kypost mail.example.com
```

`create` writes a credentials file `<TUNNEL-UUID>.json`. Then a `config.yml`:

```yaml
tunnel: <TUNNEL-UUID>
credentials-file: /etc/cloudflared/<TUNNEL-UUID>.json
ingress:
  - hostname: mail.example.com
    service: http://KyPost-Server:5866   # topology A; see the table for others
  - service: http_status:404             # everything else
```

For topology D:

```yaml
  - hostname: mail.example.com
    service: https://192.168.1.5:5866
    originRequest:
      noTLSVerify: true
```

Run it with `cloudflared tunnel run kypost` (host service), or mount
`config.yml` and the credentials file into the container.

### cloudflared container on `kypost-net` (topology A)

In a **separate** directory with its own `compose.yml`, so KyPost's compose file
stays untouched:

```yaml
services:
  cloudflared:
    image: cloudflare/cloudflared:latest   # pin a release tag
    command: tunnel --no-autoupdate run
    environment:
      TUNNEL_TOKEN: ${TUNNEL_TOKEN:?set TUNNEL_TOKEN}
    restart: unless-stopped
    networks:
      kypost-net:
        ipv4_address: 10.89.0.10   # TRUSTED_PROXY_CIDRS names this
networks:
  kypost-net:
    external: true
```

Put `TUNNEL_TOKEN=...` in that directory's `.env` and keep it out of git.

- `external: true` belongs on this side only. KyPost's compose project creates
  and owns `kypost-net`, so start KyPost first.
- Pin the address. A Docker-assigned one moves on recreate and
  `TRUSTED_PROXY_CIDRS` then silently stops matching.
- Keep `kypost-net` to the proxy and KyPost, or pin addresses high (`.200`):
  Docker hands out dynamic addresses from the low end of the subnet.
- If you changed `KYPOST_SUBNET` or `KYPOST_NETWORK`, use your values here.

For topology C, pin nginx on `kypost-net` the same way and leave cloudflared on
a network that reaches only nginx.

## 4. Configure KyPost

In KyPost's `.env`, for topology A:

```sh
KYPOST_BIND=127.0.0.1
TRUSTED_PROXY_CIDRS=10.89.0.10/32
TRUSTED_PROXY_HEADER=cf-connecting-ip
SERVER_BASE_URL=https://mail.example.com
```

For B, C and D change `KYPOST_BIND` and `TRUSTED_PROXY_CIDRS` per the
[table](#2-pick-a-topology) (and add the TLS lines for D).

Why each line:

- `KYPOST_BIND` is required; compose refuses to start without it. `127.0.0.1`
  keeps 5866 off your LAN. The tunnel does not need a published port in
  topology A at all.
- `TRUSTED_PROXY_CIDRS` is the only thing that makes KyPost believe forwarded
  headers. Empty, every visitor is keyed as cloudflared: one shared lockout
  bucket for everyone, and the MFA sign-in push names the proxy instead of the
  person signing in. Bare IPs and CIDRs only, no hostnames.
- `TRUSTED_PROXY_HEADER=cf-connecting-ip`, because cloudflared appends its own
  hop to `X-Forwarded-For`, which would make every visitor look like the tunnel.
  KyPost reads only the one header you name.
- `SERVER_BASE_URL` is the public origin, with no path. It is required for
  mobile pairing, desktop pairing and Single Sign-On (it is the OIDC
  `redirect_uri`), and pickup links and PGP QR key exchange use it. An `https://`
  value also marks the session cookie `Secure` when no trusted proxy says
  otherwise. Details: [CONFIGURATION.md](../CONFIGURATION.md).

Pairing QR certificate pin: with cloudflared there is no certificate file to
mount, because the phone validates Cloudflare's edge certificate. When it builds
each pairing QR, KyPost makes a verified TLS connection to `SERVER_BASE_URL` and
pins the certificate it gets back. Tunnel traffic leaves for Cloudflare's edge,
so this works without NAT hairpinning. If the probe fails, the QR omits the pin
and the app falls back to trust-on-first-use. See
[Certificate pinning in the pairing QR](../../README.md#certificate-pinning-in-the-pairing-qr).

Every variable is explained in `.env.example`; the deeper rationale is in
[SECURITY.md](../../SECURITY.md).

## 5. Restart

1. In KyPost's directory: `docker compose up -d` (recreates the container with
   the new `.env`). A restart logs every user out; sessions are in memory.
2. Start cloudflared: `docker compose up -d` in its directory (topology A), or
   restart the host service.
3. Watch for startup errors: `docker compose logs -f kypost-server`. Match any
   error against [Troubleshooting](#9-troubleshooting).

## 6. Verify

Do not assume; check.

1. Open `https://mail.example.com/api/installer/network`. It needs no sign-in and
   returns `clientIp`, `proxyHeadersTrusted` and `scheme`. Expect:
   - `clientIp`: your own public address (compare with any "what is my IP"
     site).
   - `proxyHeadersTrusted`: `true`.
   - `scheme`: `https`.
2. Sign in. If the login proof-of-work never completes, the page is not on
   HTTPS.
3. Fetch `https://mail.example.com/api/status` while signed in: `clientIp` and
   `proxyHeadersTrusted` must match step 1.
4. In the browser's developer tools (Storage / Application → Cookies), the
   session cookie has `Secure` set.
5. From another machine on your LAN, `http://<KyPost host LAN IP>:5866` must not
   connect (topologies A–C).

KyPost deliberately does not log client IPs, so `/api/status` and
`/api/installer/network` are the check, not the logs.

**Finding the address to trust** (topology D, or when unsure): leave
`TRUSTED_PROXY_CIDRS` empty for a moment, restart, and open
`/api/installer/network` through the tunnel. `clientIp` is then the peer address
KyPost sees cloudflared arriving from. KyPost also logs an error naming it as
`peer`. Put that address in `TRUSTED_PROXY_CIDRS` as `/32` and restart.

If `clientIp` is `127.0.0.1`, `10.89.0.1` or another bridge address, every user
shares one lockout key.

## 7. Optional: Cloudflare Access

Cloudflare Access can put a Cloudflare login in front of the hostname. It
works for the browser UI, but several KyPost clients are not browsers and cannot
complete an Access login. Without bypass rules these break:

- The mobile app: pairing registration (`/api/notifications/native/register`),
  notification pull, MFA push approval and contact sync (`/api/...` routes the
  app calls with its device credential).
- CardDAV clients: `/.well-known/carddav` and `/dav/`.
- External recipients opening pickup links: `/pickup/...`.
- Other mail clients looking up your keys over WKD: `/.well-known/openpgpkey/`.
- Your identity provider's back-channel logout, if you use SSO:
  `/api/auth/oidc/backchannel-logout`.

KyPost has no Access integration; it does not read Access headers. Treat Access
as an extra wall for the browser, and test every client you use after enabling
it.

## 8. Security checklist

- [ ] `KYPOST_BIND=127.0.0.1` (A–C). Never `0.0.0.0` when tunnelling: an
  unproxied 5866 is cleartext, and with `TRUSTED_PROXY_CIDRS` set anything that
  reaches it directly can forge its client address and skip every lockout.
- [ ] `ALLOW_INSECURE_HTTP` unset. It is for a deliberately trusted network,
  not a TLS substitute.
- [ ] `TRUSTED_PROXY_CIDRS` names cloudflared's (or nginx's) **single** address
  as `/32`. Never a subnet such as `10.89.0.0/24`, never `0.0.0.0/0`: any peer in
  the range can forge its own client IP.
- [ ] `10.89.0.1/32` only together with `KYPOST_BIND=127.0.0.1`.
- [ ] `cf-connecting-ip` is trusted only from cloudflared, or from an nginx that
  only cloudflared can reach.
- [ ] cloudflared's address on `kypost-net` is pinned, and the network holds only
  the proxy and KyPost.
- [ ] Topology D: the hop to KyPost uses `TLS_CERT_FILE` / `TLS_KEY_FILE`.
- [ ] `SERVER_BASE_URL` is `https://` and matches the public hostname exactly.
- [ ] Tunnel token or credentials file is out of git and readable only by you.
- [ ] The retired `TRUST_PROXY_HEADERS` is removed from `.env`.
- [ ] [Step 6](#6-verify) passes.

## 9. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `docker compose up` stops with `set KYPOST_BIND in .env — ...` | `KYPOST_BIND` has no default | Set it per the [table](#2-pick-a-topology) |
| Container exits: `refusing non-loopback cleartext HTTP; configure TLS_CERT_FILE/TLS_KEY_FILE or set ALLOW_INSECURE_HTTP=true` | Non-loopback `KYPOST_BIND` without TLS | A–C: use `127.0.0.1`. D: configure TLS |
| Startup error `TRUSTED_PROXY_CIDRS: <value> is not a CIDR or IP address` | Typo, or a hostname such as `cloudflared` | Use the IP, e.g. `10.89.0.10/32` |
| Startup error `TRUSTED_PROXY_HEADER must be x-forwarded-for or cf-connecting-ip` | Any other value | `TRUSTED_PROXY_HEADER=cf-connecting-ip` |
| Startup error `TLS_CERT_FILE is set but TLS_KEY_FILE is not; ...` (or the reverse) | Only one TLS path set | Set both or neither |
| Log: `TRUSTED_PROXY_CIDRS is empty but requests are arriving with forwarding headers, ...` | Trust not configured; every caller keyed as the proxy | Set `TRUSTED_PROXY_CIDRS` to the logged `peer` address |
| Log: `TRUSTED_PROXY_HEADER is missing or names a trusted proxy's own address, so every caller shares one lockout key. ...` | Header left at `x-forwarded-for` behind cloudflared, or nginx (C) not passing `CF-Connecting-IP` | `TRUSTED_PROXY_HEADER=cf-connecting-ip`; check the nginx line in [topology C](#2-pick-a-topology) |
| Log: `TRUST_PROXY_HEADERS is set but is RETIRED and ignored, ...` | Old setting | Remove it; set `TRUSTED_PROXY_CIDRS` |
| Log: `login proof-of-work is ON by default but no TLS was detected in front of this server. ...` | Neither `TRUSTED_PROXY_CIDRS` nor an `https://` `SERVER_BASE_URL` is set | Complete [step 4](#4-configure-kypost) |
| Log: `SERVER_BASE_URL is not set; native and desktop pairing are disabled ...` | No public URL configured | `SERVER_BASE_URL=https://mail.example.com` |
| Everyone locked out at once; MFA push shows `10.89.0.1` or another bridge address | All visitors share one lockout key: trust empty, CIDR not matching, or cloudflared reaching KyPost through the published port from another network | Fix per [step 6](#6-verify); put cloudflared on `kypost-net`. Lockouts expire on their own (the account lockout is 15 minutes) |
| `clientIp` correct but `proxyHeadersTrusted` is `false` | You are seeing the direct peer; `TRUSTED_PROXY_CIDRS` does not match it | Use the address from [step 6](#6-verify) |
| Session cookie lacks `Secure` | Trust not configured, or nginx (C) sends `X-Forwarded-Proto: http` | Set `SERVER_BASE_URL=https://...` and trust; fix nginx's `X-Forwarded-Proto` |
| cloudflared starts with `network kypost-net declared as external, but could not be found` | cloudflared started before KyPost | Start KyPost, then cloudflared again |
| `Pool overlaps with other one on this address space` or `incorrect label` | Subnet clash, or network created by hand | [Reverse_Proxy_Networking.md](../Reverse_Proxy_Networking.md) |
| Cloudflare shows 502 / Bad gateway | cloudflared cannot reach the service URL: not on `kypost-net` (name does not resolve), `127.0.0.1` used from inside a container, KyPost still starting (first boot pulls a model; healthcheck allows 180 s), or scheme mismatch (`http://` to a TLS-enabled KyPost, or `https://` to a self-signed one without `noTLSVerify`) | Check `docker compose ps` and cloudflared's own log for the exact connection error |
| Sign-in page never finishes the check | Page reached over plain `http://` | Use the tunnel hostname, not the LAN address |
