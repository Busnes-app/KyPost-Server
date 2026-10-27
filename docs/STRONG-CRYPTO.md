# Key strength and the Go TLS profile

[Security policy](../SECURITY.md) · [TLS](TLS.md) · [PGP keys](PGP-KEYS.md) · [Assurance case](ASSURANCE-CASE.md)

This guide covers the keys KyPost's own code generates, accepts and verifies,
and the optional process setting that restricts its outbound TLS to strong
certificate keys. It is not a FIPS certification and does not certify a
deployment. Stock upstream components shipped beside KyPost keep their own
policies; see [Out of scope](#out-of-scope).

## Preserve existing data and authority

Keep the config, state and private volumes (`/kypost/private` holds the master
keys and pairing, PoW and VAPID keys), `.env`, the KyRecovery pin and every
Compose overlay you already use. A refused key is a reason to replace that one
key or certificate, never to regenerate master keys, delete accounts or touch
stored mail. KyPost refuses an undersized key with an error naming it; it never
rewrites, rotates or deletes a stored key.

## Key rules in every mode

| Boundary | Default | Smaller keys |
| --- | --- | --- |
| Master keys (`imap-config.key`, `totp-secret.key`, `pgp-private-key.key`, `pickup-store.key`, scheduled-send, domain-relay and bulk-backup keys) | 32 random bytes, generated in `SECRET_DIR` | A file that does not decode to exactly 32 bytes is refused, never overwritten (`backend/internal/cryptutil`). AES-256-GCM; HKDF-SHA256 subkeys. |
| `PAIRING_SECRET` (HMAC-SHA256 for pickup links, device pairing, QR key exchange, native-restore evidence) | 32 random bytes in `pairing.key` | An operator value under 32 bytes is refused and the features stay off. |
| `POW_SECRET` (proof-of-work CAPTCHA HMAC-SHA256) | 32 random bytes in `pow.key` | Under 16 bytes is refused; login CAPTCHA then fails closed. |
| KyCalendar shared secret (`KYPOST_KYCALENDAR_SECRET_FILE`) | Operator-supplied | Under 32 bytes is refused at startup. |
| OIDC ID tokens (browser sign-in, step-up, native sign-on) | Verified by `coreos/go-oidc`; RS/PS 256–512 and ES256–512 only, never HMAC | Every RSA key in the provider's JWKS must have at least 2048 modulus bits, or the whole set is refused and sign-in fails (`backend/internal/sso/jwks_strength.go`). |
| OIDC back-channel logout tokens | `ky-primitives/oidcverify`, RS256 | RSA keys under 2048 bits are refused by `oidcverify`. |
| Server OpenPGP (legacy server-held keys, incoming encryption) | Existing user and contact keys | New encryption and signatures refuse an RSA primary or selected subkey under 2048 bits (go-crypto's own floor is 2047), naming the key fingerprint (`backend/internal/pgpmail/keyfloor.go`). Stored keys stay importable and old mail stays readable. |
| Browser OpenPGP | Generates Curve25519 keys | RSA under 2048 bits refused for encryption, signing and signature trust ([PGP keys](PGP-KEYS.md)). |
| Native device key delivery | HPKE DHKEM(P-256, HKDF-SHA256), AES-256-GCM | Fixed suite; nothing to select. |
| Sealed backup capsules | HPKE X-Wing (ML-KEM-768 + X25519), AES-256-GCM, to the pinned KyRecovery key | Fixed suite and key size. |
| Inbound HTTPS (`TLS_CERT_FILE`) | Off; operator certificate | An RSA certificate key under 2048 bits is refused at startup, and a renewal to one keeps the current certificate. TLS 1.2 minimum. |
| Receiving certificate (`receiving config`) | Operator certificate | RSA under 2048 bits is refused before Maddy is configured. |
| Browser push (VAPID, RFC 8291) | P-256 key generated in `CONFIG_DIR`; aes128gcm | Subscriptions must carry a valid P-256 key. |
| Push relays | FCM RS256 service-account key; APNs ES256 `.p8` | FCM refuses RSA under 2048 bits (`worker/src/fcm.ts`); APNs imports only P-256. |
| Cloudflare continuous receiving | Ed25519 routing-table signatures, 32-byte pickup tokens | Fixed sizes. |
| TOTP | 20 random bytes (HMAC-SHA1, RFC 6238) | Only server-generated and sealed; there is no import path. HMAC-SHA1 is a keyed PRF here; no collision resistance is relied on. |
| Generated tokens | Session and CSRF tokens 192 random bits; relay API keys 256, stored as SHA-256 | Not operator-supplied. |

Passwords are not keys: the browser derives an auth secret with PBKDF2-SHA256
(600,000 iterations) and HKDF, and the server stores it as Argon2id. Recovery
codes (60 bits) and six-digit TOTP codes are rate-limited authenticators, not
keys. SHA-1 appears only where a protocol fixes it (the WKD local-part hash) and
as a non-secret CardDAV path key.

## Enable the Go TLS profile

Set `GODEBUG=fips140=on` for the container. Use `on`; KyPost is not tested under
the diagnostic `only` mode. Go's TLS clients then accept only certificate chains
whose every certificate, including a root you trust explicitly, uses RSA-2048+,
ECDSA P-256/P-384/P-521 or Ed25519 keys, with FIPS-approved versions and cipher
suites. That covers each TLS client in KyPost's code: IMAP, SMTP submission and
the domain relay, OIDC discovery/token/JWKS, CardDAV, WKD lookups, web and
native push, KyCalendar, KyRecovery, release checks and the pairing pin probe.
Certificate and hostname verification stay on. A private CA does not waive the
floor: replace a weak chain, never disable verification.

Append `docker-compose.strong-crypto.yml` last to the chain you already use
(keep the receiving, Rspamd and LAN-DNS overlays):

```sh
docker compose -f docker-compose.yml [your overlays] -f docker-compose.strong-crypto.yml up -d
```

or add it to `COMPOSE_FILE` in `.env`. The overlay sets only `GODEBUG` on the
`kypost-server` service. `docker compose exec kypost-server kypost-server ...`
subcommands inherit it. For a process started another way, add
`GODEBUG=fips140=on` to its environment and restart it.

`python3 scripts/check-strong-crypto-compose.py` renders five chains (base,
LAN DNS, Rspamd, receiving, receiving with Rspamd and LAN DNS) with synthetic
inputs, starts nothing, and fails if the overlay changes anything but that one
variable.

Cleartext opt-ins have no keys to check, so the profile does not make them
safe: leave `ALLOW_INSECURE_SMTP`, `allowInsecureIssuer` and
`ALLOW_INSECURE_HTTP` off.

## Configure the independent HTTPS edge

Unless `TLS_CERT_FILE` is set, KyPost serves plain HTTP for its proxy (see
[TLS](TLS.md)). Use TLS 1.2/1.3 and a verified RSA-2048+ or P-256+ certificate
chain at the proxy. For Nginx/OpenSSL:

```nginx
ssl_protocols TLSv1.2 TLSv1.3;
ssl_ciphers 'DEFAULT@SECLEVEL=2';
```

`GODEBUG` does not configure the proxy, browsers or mail clients. Check the
edge before rollout.

## Out of scope

These ship beside KyPost or are reached by it, but are stock upstream software
with their own key and TLS policy. This guide makes no claim about them:

- **Maddy** (receiving overlay): it terminates inbound SMTP TLS. KyPost only
  checks the certificate it hands over.
- **Rspamd** (`docker-compose.rspamd.yml`) and its DNS lookups.
- **Ollama** and its model downloads, and **restic** for bulk backups. Both run
  in the KyPost container and inherit `GODEBUG`, but their behaviour under it
  is theirs.
- **Remote IMAP/SMTP servers, relay providers, CardDAV, WKD and push services**
  (Google FCM, Apple APNs, browser push services) and Cloudflare's hosted
  Workers, R2 and Email Routing TLS.
- **Identity providers** (KyIdentity, Authentik, Keycloak and others): KyPost
  checks the keys they publish, not how they protect them.
- **Reverse proxies, cloudflared, browsers and mail or calendar clients.**
- **KyIdentity, KyRecovery, KyCalendar** and other suite services: each has its
  own guide.

## Verification

CI runs every backend package under the race detector twice, plainly and with
`GODEBUG=fips140=on`, then the Compose check. The regressions:

- `backend/internal/sso/jwks_strength_test.go`: real RS256 tokens from
  1024/2047-bit keys refused on browser exchange, native sign-on and
  back-channel logout; 2048 accepted; JWKS casing, mixed sets and malformed input.
- `backend/internal/pgpmail/keyfloor_test.go`: RSA-1024/2047 and 2047-bit
  subkeys refused for encryption, incoming encryption and signing, even beside a
  strong recipient; RSA-2048 accepted; weak keys still import.
- `backend/internal/api/strong_crypto_test.go`: 1024/2047-bit inbound
  certificates refused at startup and on reload; a P-256 leaf under a
  1024/2047-bit trusted root refused in the profile and accepted otherwise, a
  2048-bit root accepted in both.
- `backend/internal/app/receiving_config_test.go`: a 2047-bit receiving
  certificate refused.

Local tests use generated keys and local servers; they do not prove a live
provider, relay or proxy, and the container has not been qualified under the
profile with Maddy, Ollama or restic. After rollout check sign-in, sending,
IMAP sync, push and a backup.

References: [Go FIPS 140-3 mode](https://go.dev/doc/security/fips140),
[RFC 4226 key requirements](https://www.rfc-editor.org/rfc/rfc4226#section-4),
[NIST SP 800-131A Rev. 2](https://nvlpubs.nist.gov/nistpubs/SpecialPublications/NIST.SP.800-131Ar2.pdf).
