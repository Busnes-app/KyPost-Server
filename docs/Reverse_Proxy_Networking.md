# Reverse proxy networking

How to put a proxy in front of KyPost so the server sees each visitor's real
address. This is referenced from `docker-compose.yml`, which keeps only a short
pointer at each setting.

## Why it matters

Every IP-keyed control depends on the peer address being the caller's: the
login, CardDAV, device and proof-of-work lockouts, the WKD rate limit, and the
address the MFA sign-in push reports.

Traffic that crosses from one Docker network to another — or that leaves and
comes back through the published port — is source-NATed on the way in, so the
peer address becomes a gateway (`10.89.0.1`, the `kypost-net` gateway, for the
published port) that is **the same for every caller**. All those lockouts then collapse into one shared bucket: enough
failures from anyone locks out everyone.

Put the proxy on `kypost-net` and address the container by name. Container-to-
container traffic ignores published ports, so nothing needs publishing at all.

Verify with `GET /api/status`: `clientIp` must be your own public address and
`proxyHeadersTrusted` must be `true`. Do not assume — check.

## Trusting the proxy

`TRUSTED_PROXY_CIDRS` names the proxy's address; `TRUSTED_PROXY_HEADER` names
the one header it reports the client in. Only that header is read: a proxy
passes other headers through from the client, so reading both would let a
client pick its own address.

| In front of KyPost | `TRUSTED_PROXY_CIDRS` | `TRUSTED_PROXY_HEADER` |
|---|---|---|
| nginx or Caddy container on `kypost-net` | its pinned address, e.g. `10.89.0.10/32` | `x-forwarded-for` |
| cloudflared container on `kypost-net` | its pinned address | `cf-connecting-ip` |
| cloudflared → nginx → KyPost | nginx's address | `cf-connecting-ip` |
| nginx or cloudflared on the host, via `127.0.0.1:5866` | `10.89.0.1/32` | as above |
| Cloudflare proxied DNS (CDN) → nginx → KyPost | nginx's address | `x-forwarded-for`, with nginx restoring the visitor address (below) |

- nginx with `x-forwarded-for`: `proxy_set_header X-Forwarded-For $remote_addr;`.
  `$proxy_add_x_forwarded_for` also works, because KyPost uses the right-most
  element, which nginx itself appended.
- cloudflared appends its own hop to `X-Forwarded-For`, so behind it use
  `cf-connecting-ip`. With nginx in between, nginx must pass the header on
  (`proxy_set_header CF-Connecting-IP $http_cf_connecting_ip;`) and must be
  reachable only from cloudflared, or a client can send its own.
- Behind Cloudflare's CDN, the peer nginx sees is a Cloudflare edge. Have nginx
  take the visitor from `CF-Connecting-IP` only from Cloudflare's ranges
  (`set_real_ip_from` each range from <https://www.cloudflare.com/ips/>,
  `real_ip_header CF-Connecting-IP;`) and pass
  `proxy_set_header X-Forwarded-For $remote_addr;`.
- A proxy on the host reaching the published port does not arrive from
  `127.0.0.1`. With rootful Docker (rootless Docker and Podman may differ; check
  `/api/status`), Docker hands the connection over from the `kypost-net` gateway,
  the first address of `KYPOST_SUBNET` (`10.89.0.1` by default), and every other
  process on the host arrives from it too. Trust it only with
  `KYPOST_BIND=127.0.0.1`; a proxy container on `kypost-net` is better.

A malformed CIDR or any other header value refuses startup.

## Joining the network from another Compose project

In the proxy's own compose file:

```yaml
services:
  cloudflared:
    networks:
      kypost-net:
        ipv4_address: 10.89.0.10    # pin it: TRUSTED_PROXY_CIDRS names this
networks:
  kypost-net:
    external: true
```

Then point the proxy at `http://KyPost-Server:5866` (this network has DNS, so
the name is stable across rebuilds) and set
`TRUSTED_PROXY_CIDRS=10.89.0.10/32` and, for cloudflared,
`TRUSTED_PROXY_HEADER=cf-connecting-ip` (see [Trusting the proxy](#trusting-the-proxy)).

`external: true` goes on the **proxy** side only. This Compose project creates
the network when it does not exist yet, so bring KyPost up first where you can.
A proxy that starts first fails with
`network kypost-net declared as external, but could not be found` — start it
again once KyPost is up, or see
[When the network already exists](#when-the-network-already-exists).

A proxy started with a plain `docker run` and no `--network` lands on Docker's
default bridge, not here, and reaches the server only through the published
port. If you are wiring up a proxy and every visitor's address comes out
identical, this is the first thing to check.

## Why the network is named, with an explicit subnet

A proxy in a different Compose project can join it by a name that does not
depend on this checkout's directory name, and a user-configured subnet is the
prerequisite for the pinned `ipv4_address` — Docker rejects pinned addresses on
any network it auto-configured, including the default bridge.

## When the network already exists

KyPost is the sole owner for new suite installations. Create the network through
the KyPost-owned network bootstrap before starting KyIdentity, KyNotes or a containerized NPM:

```sh
bash scripts/ensure-network.sh
```

The script creates only a bridge with KyPost's Compose ownership labels, or verifies
that the existing owner and subnet agree. It installs no mail service and touches
no credentials or volumes. For custom `.env` values, pass their exact name/subnet:
`bash scripts/ensure-network.sh MY_NETWORK MY_SUBNET`.

Those projects join it as `external: true`, and only the proxy's fixed `/32` is
trusted for forwarded headers. Keep its network membership and `ipv4_address` in
its Compose file so they survive replacement. This is one network per Docker host;
it does not provide cross-host DNS or routing.

If the network is already manually owned, preserve it as a legacy exception.
Do not remove it or relabel a live network to force ownership. The explicit KyPost
external override below retains that existing owner's lifecycle:


KyPost then uses it as is. Docker Compose 5.6 prints
`a network with name kypost-net exists but was not created by compose` and
continues. To declare it instead and drop the warning, put this in
`docker-compose.override.yml` next to `docker-compose.yml` (`!reset` needs
Compose 2.24 or later):

```yaml
networks:
  kypost-net:
    external: true
    ipam: !reset {}
```

`scripts/update-host.sh` and the manual digest start in
[the README](../README.md#verifying-an-image-by-hand) pass `-f docker-compose.yml`
and never read the override; with an existing network they rely on the warning
behaviour above.

Older Compose releases refuse a network without Compose's labels:

```
network kypost-net was found but has incorrect label
com.docker.compose.network set to "" (expected: "kypost-net")
```

Use the explicit external override above and include it in every `-f` invocation.
Do not disconnect consumers, delete the live network or change its labels to satisfy
Compose ownership. A legacy owner remains responsible for that network until the
operator schedules a separate migration. Explicit-file update commands must include
that override on older Compose versions; refusal is preferable to replacing the network.

## Address allocation

Ky services pin their own defaults (`.2` KyIdentity, `.5` KyPost, `.7`
KyNotes). Pin anything else outside those, high (`.200`, `.201`). Docker
allocates dynamic addresses from the low end of the subnet, so a container
joining unpinned can be handed an address you pinned for something else.

## Subnet collisions

If startup fails with `Pool overlaps with other one on this address space`, the
subnet is already taken. Change both:

```sh
KYPOST_SUBNET=10.90.0.0/24
KYPOST_IP=10.90.0.5
```

List what is taken:

```sh
docker network ls -q | xargs docker network inspect \
  -f '{{.Name}} {{range .IPAM.Config}}{{.Subnet}}{{end}}'
```

The default is `10.89.0.0/24` rather than a `172.x` one deliberately. Docker
auto-allocates project networks as /16s out of `172.17.0.0/12` — 172.17, 172.18,
172.19 and up — so any `172.x` default collides once a host has enough Compose
projects, and the collision surfaces as the error above rather than as anything
mentioning this file. `10.89.0.0/24` is outside that pool. Change it if it
clashes with your own LAN or VPN routes.

## The pairing QR reads your proxy's certificate

The mobile pairing QR carries a pin for the certificate the device will be
handed, so the app can pin the registration handshake before it discloses the
pairing token and its push credentials. See
[Certificate pinning in the pairing QR](../README.md#certificate-pinning-in-the-pairing-qr).

Your proxy holds that certificate, not this server — with `cloudflared` there is
no file to mount at all, because the device validates a Cloudflare edge
certificate. So the server reads it the same way the device will: at the moment
it builds each QR, it makes a verified TLS connection to `SERVER_BASE_URL` and
takes the leaf.

**Set `SERVER_BASE_URL` to your public `https://` URL.** That is the whole
configuration, and pairing does not work without it. No URL that carries a
credential is derived from the request's `Host` header — a caller who could
steer that would be choosing which key the app pins, and where the pairing
token is sent.

If your public hostname does not resolve back to the proxy from inside the
server's network — a self-hosted proxy behind a consumer router with no NAT
hairpinning is the usual case — the probe fails and the QR simply omits the pin,
leaving pairing exactly as it was. Split-horizon DNS pointing the hostname at
the proxy's LAN address fixes it; so does `cloudflared`, whose traffic leaves to
the anycast edge and never needs to come back in.
