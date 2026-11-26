# OpenPGP v6 and RFC 9980 fixtures

Test-only keys and messages shared by the server and the clients. **Never use these keys for
anything else; the private keys are public.** Every file except this README and `crosscheck/` is
made by `backend/internal/pgpmail/pgpfixtures_test.go` from a fixed seed and the fixed time
2026-11-25T00:00:00Z; the untagged test requires the committed bytes, and `SHA256SUMS` lists them.
Regenerate only on purpose:

```sh
cd backend && go test -tags pgpfixturegen -run TestPGPFixturesRegenerateByteIdentical ./internal/pgpmail
```

| File | Content |
|---|---|
| `v4.{pub,sec}.asc` | v4 EdDSA Ed25519 (22) primary, ECDH Curve25519 (18) subkey: today's browser key shape |
| `v6.{pub,sec}.asc` | v6 Ed25519 (27) primary, X25519 (25) subkey |
| `v6-pqc.{pub,sec}.asc` | v6 ML-DSA-65+Ed25519 (30) primary, ML-KEM-768+X25519 (35) subkey (RFC 9980) |
| `v4-pqc.{pub,sec}.asc` | v4 key as `v4`, plus an ML-KEM-768+X25519 (35) encryption subkey made a minute later |
| `v4.msg.asc` | v3 PKESK to the cv25519 subkey, SEIPDv1, AES-256 |
| `v4-pqc.msg.asc` | v3 PKESK to the ID 35 subkey, SEIPDv1, AES-256 |
| `v6-pqc.msg.asc` | v6 PKESK to the ID 35 subkey, SEIPDv2, AES-256, OCB |
| `v6-pqc.sig.asc` | v6 detached ML-DSA-65+Ed25519 signature over `message.txt` |
| `message.txt` | The plaintext of every message and the signed text |
| `golden-incoming.eml`, `golden-incoming.v4.eml` | A stored message and its incoming encryption to `v4` (`EncryptStoredMIME`), pinned byte for byte |

Cross-checked with Bouncy Castle 1.87 (`crosscheck/BcCheck.java`): every message and the golden
decrypt, the signature verifies and is refused over altered text. Sequoia and OpenPGP.js v7 are not
checked yet (P3).
