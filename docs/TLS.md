# Getting TLS in front of KyPost

**Before exposing this to a network, get TLS in front of it.** By default
KyPost serves plain HTTP. The session cookie is marked `Secure` only when the
request demonstrably arrived over TLS, so on a bare `http://` deployment the
cookie is sent in the clear on every request. `http://` on localhost, for one
machine, is fine. Compose refuses a non-loopback cleartext bind unless you
explicitly set `ALLOW_INSECURE_HTTP=true`; that escape hatch is for a
deliberately trusted network, not a TLS substitute.

`KYPOST_BIND` decides which interface port 5866 is published on, and it has
no default — compose will not start until you set it. An unproxied 5866 is
plain HTTP, and with `TRUSTED_PROXY_CIDRS` set, anything that reaches it
directly can forge `X-Forwarded-For` and bypass the lockouts. Use
`127.0.0.1` for a proxy on this host, your LAN IP for a proxy elsewhere, or
`0.0.0.0` to publish everywhere deliberately. **Check where your proxy
actually reaches this container from before choosing**: loopback publishing
severs a proxy that arrives by the host's LAN address, which is the usual
shape for cloudflared or an nginx on another machine.
Better still, run the proxy as a container on `kypost-net` — the network the
compose file defines for exactly this — and point it at
`http://KyPost-Server:5866`. That network has DNS, so the name keeps working
across rebuilds, and the path ignores published ports entirely so nothing
needs publishing. The snippet for joining it from another compose project,
and how to recover from the two Docker errors this setup produces, are in
[Reverse_Proxy_Networking.md](Reverse_Proxy_Networking.md).

**This is what makes the client IP correct, not a nicety.** A proxy on a
*separate* Docker network — or one that reaches this container through the
published port, even from `kypost-net` — is source-NATed on the way in, so the
server sees the same gateway (`10.89.0.1` for the published port) for every
caller. That address is the
lockout key, so every user shares one bucket, and the MFA sign-in push names
the gateway instead of whoever is signing in.

Three ways to get TLS, and they are not equivalent:

**1. Terminate TLS in KyPost.** Set `TLS_CERT_FILE` and `TLS_KEY_FILE` to
mounted certificate paths (see `.env.example` and the commented volume in
`docker-compose.yml`). This is the only option where "did this arrive over
TLS?" is answered by the connection itself rather than by a header, so
`TRUSTED_PROXY_CIDRS` does not apply at all. Renewals are picked up without a
restart, which matters because a restart logs everyone out. Setting only one
of the two paths is a startup error, not a fallback to cleartext.
Certificates are deliberately never baked into the image. An RSA certificate
key below 2048 bits is refused at startup, and a renewal to one keeps the
current certificate; see [key strength](STRONG-CRYPTO.md).

**2. Cloudflare Tunnel.** cloudflared gives the browser a real HTTPS origin,
which is all the login proof-of-work needs, with no TLS configuration of your
own.

**3. A reverse proxy you run** (nginx, Caddy).
nginx speaks HTTP/1.0 to the upstream by default; set
`proxy_http_version 1.1;`. Mail export refuses mbox over HTTP/1.0, where a
cut-off download is indistinguishable from a complete one.

Options 2 and 3 need `TRUSTED_PROXY_CIDRS` set to the proxy's address — the
address you pinned for a proxy container on `kypost-net` (e.g.
`10.89.0.10/32`), or, for a proxy on the same host reaching `127.0.0.1:5866`,
the `kypost-net` gateway `10.89.0.1/32` (Docker hands the connection over from
there, not from `127.0.0.1`; trust it only with `KYPOST_BIND=127.0.0.1`).
A malformed entry refuses startup. Putting the proxy on
that network is what makes such an address meaningful, but it is not a
substitute for setting this: with it empty, forwarded headers are discarded
and every caller is keyed as the proxy. Only with it set does the server believe
`X-Forwarded-Proto`/`-Host`/`-For`, which is what marks the cookie `Secure`
and keys the login and CardDAV lockouts off the real caller rather than the
proxy. **Name the proxy's address specifically, not a wide range:** any peer
inside the range you name can forge its own client IP and bypass every rate
limit and lockout keyed on it. This replaces the old
`TRUST_PROXY_HEADERS=true`, which is no longer read — it trusted forwarded
headers on *every* connection from any peer, so it was only ever safe when
nothing but the proxy could reach the port.

`TRUSTED_PROXY_HEADER` names the one header the client address is read from;
the other is ignored, because a proxy passes unknown headers through from the
client:

- nginx or Caddy: `x-forwarded-for` (the default). In nginx,
  `proxy_set_header X-Forwarded-For $remote_addr;`
  (`$proxy_add_x_forwarded_for` also works; the right-most element is used).
- cloudflared: `cf-connecting-ip`. cloudflared appends its own hop to
  `X-Forwarded-For`, so XFF would make every visitor look like the tunnel.
- cloudflared → nginx → KyPost: `cf-connecting-ip`, with nginx passing it on
  (`proxy_set_header CF-Connecting-IP $http_cf_connecting_ip;`) and reachable
  only from cloudflared, or a client can send its own.

Any other value refuses startup.

If the proxy runs on a **different host**, combine option 1 with 2 or 3 — that
hop carries session cookies across a real network. A self-signed certificate
is enough there; tell the proxy to skip verification (cloudflared
`noTLSVerify: true`, nginx `proxy_ssl_verify off`).

**Verify rather than assume.** Sign in and fetch `GET /api/status`: `clientIp`
must be your own public address and `proxyHeadersTrusted` must be `true`
(option 1 reports `false`, correctly — it trusts no headers). If `clientIp` is
a loopback or bridge address, every user is sharing one lockout key and the
session cookie is not being marked `Secure`.
