# Clients and push

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Mobile App Pairing (Native)

The backend handles mobile pairing directly. It does not require Novu.

- Nothing to configure: the pairing secret is generated on first start and kept at `/kypost/private/pairing.key`. Set `PAIRING_SECRET` only if you run multiple replicas that must share one.
- Required: set `SERVER_BASE_URL` so that QR code payloads point to the correct public backend URL. Use an `https://` URL: pairing tokens, pickup links and QR key-exchange URLs are all built from it, and each carries a bearer credential in the query string. Unset, the pairing panel reports "set SERVER_BASE_URL" and mints no token — the address a credential is sent to is not something a request's `Host` header may choose, and there is no safe guess. It is also what the pairing QR's certificate pin is read from. See [docs/TLS.md](TLS.md) and [Certificate pinning](#certificate-pinning-in-the-pairing-qr) below.
- Keep all pairing secrets on the server only.

Desktop pairing behavior:

- Security's Devices tab renders a QR code link with `sub`, `hash`, `srv`, `reg`, `pt`, and — when the serving certificate can be read — `pin`.
- Set `SERVER_BASE_URL` in `.env`. Then `srv` and `reg` always point to the deployment address that the mobile app must use. Nobody enters a server URL by hand.
- `pt` is a signed pairing token. It is valid for 90 seconds.
- The UI shows a 4px countdown bar under the QR code. The bar shrinks over 90 seconds and changes from green to red. It is red for the last 15 seconds.
- The mobile app scans the QR code and registers its push token through `reg`. If `reg` is absent, the app uses `srv` plus `/api/notifications/native/register` instead.

### Certificate pinning in the pairing QR

The pairing request is the one call that carries the pairing token, the push
endpoint and the app's WebPush keys. Without a pin the app sends all of it and
only *then* decides whether to trust the certificate it just used. On a network
with a locally trusted CA — an MDM root, a certificate someone installed, a
captive portal — an interceptor reads the token, registers its own device
against your server first, and hands the app back credentials it controls.

So the QR carries `pin`: the SHA-256 of your serving certificate's public key.
The app pins the registration handshake to that one key *before* it sends
anything, and refuses a certificate that does not match.

**Set `SERVER_BASE_URL` to your public `https://` URL and this works with no
further configuration.** At the moment it builds each QR, the server makes a
verified TLS connection to that URL and reads the certificate you are actually
serving — whether that is Caddy, Traefik, nginx-proxy-manager, a Cloudflare
tunnel, or this server's own `TLS_CERT_FILE`. Because it is read fresh every
time, certificate renewal needs no action from you.

Two things worth knowing:

- **Behind a proxy, you are pinned to the proxy.** With Cloudflare in front,
  this closes the hostile-network hole between the phone and Cloudflare. It does
  not make the tunnel end to end; Cloudflare still terminates TLS.
- **A private or self-signed CA gets no pin.** The server will not tell the app
  "trust only this key" on the word of a certificate it could not verify. Use
  `TLS_CERT_FILE`/`TLS_KEY_FILE` to terminate TLS here instead, and the pin is
  read from that certificate directly.

A probe failure — an unreachable URL, a router that will not route your public
hostname back to itself — simply omits `pin`, and pairing proceeds on trust on
first use. It never breaks a pairing; it only declines to protect one.
Already-paired devices are unaffected. A missing `SERVER_BASE_URL` is the one
case that is not merely unpinned: there is then no address to put in the QR at
all, so pairing refuses rather than guessing one from the request.

Native registration behavior:

- `POST /api/notifications/native/register` validates the pairing token. It stores the native device metadata and token in the backend state.
- `GET /api/notifications/native/devices` lists the paired native devices and their reported enrollment envelope versions. Older devices default to v2; webmail refuses setup for devices that require a newer format. V3 delivery and conversion remain gated.
- `DELETE /api/notifications/native/devices` removes one paired native device by `deviceId`.
- `POST /api/notifications/native/unpair` revokes all paired native devices for the signed-in user.

Firebase credential guidance:

- The backend never holds Firebase credentials. It never reads `google-services.json`.
- A central **push relay** (a Cloudflare Worker) delivers native push. The relay holds the one Firebase service account that the published mobile app is built against. This lets anyone run their own server with the same app, with no Firebase account and no recompile.
- `google-services.json` belongs in the mobile project, usually at `app/google-services.json` in the Android app module. Never commit it.

## Push Relays (Cloudflare Workers)

Cloudflare Workers deliver native push. The project maintainer runs them.
- **Android/FCM**: [`worker/`](../worker) — Firebase Cloud Messaging relay
- **iOS/APNs**: [`worker-apns/`](../worker-apns) — Apple Push Notification service relay

Self-hosters set one variable and the server does the rest — it registers itself
with the relay on first start, over its public `/register` endpoint, and persists
the key it is issued under `SECRET_DIR`. No operator involvement, no ticket, no
waiting.

- Android: set `PUSH_RELAY_URL` (Firebase relay)
- iOS: set `APNS_RELAY_URL` (APNs relay)

The server looks for a key in three places, in order: the `PUSH_RELAY_KEY` /
`APNS_RELAY_KEY` environment variable, then the key file from a previous
registration, and only then does it register. So restarts reuse the key on disk
rather than minting a new one.

Two things are worth knowing before you rely on it:

- **The relay keeps one active key per public IP address.** Two self-hosted
  servers behind the same address will take the key from each other, newest
  registration wins, and the displaced server's push silently stops working. If
  that is your setup, ask the operator for keys and pin them with
  `PUSH_RELAY_KEY` / `APNS_RELAY_KEY` instead.
- **Registration can be closed.** The operator can turn `/register` off — for
  abuse, or cost. If it is closed, the server logs the refusal and native push
  stays off; `App Pull` delivery works regardless and needs no relay at all.

Self-hosters need no Firebase account and no Apple Developer account. You never recompile the app.

Relay operators deploy both Workers and can still mint keys by hand. See [`worker/README.md`](../worker/README.md) and [`worker-apns/README.md`](../worker-apns/README.md) for setup, secrets, and key management.
