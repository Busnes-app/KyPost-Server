# PGP keys

[Product overview](../README.md) · [Standalone installation](../INSTALL.md)

Commands and unlinked file paths below are relative to the repository root.

## Where your PGP private key lives

The location of your private key decides what PGP protects here. This question
gets its own section for that reason, not a bullet.

Client-protected keys are the only supported PGP key mode. Server-protected
key custody has been retired; old keys remain available for migration.

**Client-protected (end-to-end).** Your browser generates or imports the key.
The browser then wraps the key under a key derived from your account password.
It uses PBKDF2-HMAC-SHA256 with 600,000 iterations and AES-256-GCM. The browser
uploads only the wrapped blob and the public half.

The server stores that blob and cannot open it, and two things have to be true
for that to hold:

1. **Your password never reaches the server.** The browser stretches it with a
   per-account login salt (fetched from `GET /api/auth/login-params`) and splits
   the result: an authentication half, which is what gets sent, and a
   key-wrapping half, which never leaves the page.
2. **The two halves are derived under different salts.** The wrapping key uses a
   random salt stored inside the envelope; authentication uses the account's
   login salt. Neither value can be computed from the other without the password.

The server accepts only the derived authentication half, never the password. An
account still holding a hash from before derived authentication cannot sign in
until an admin resets its password; such accounts are flagged "needs password
reset" in Admin → Users.

Your browser decrypts and signs. A person who takes the disk, a backup, or the
memory of this process gets ciphertext.

**What this does not defend against.** This server ships the JavaScript that does
the derivation. A server that wants your password can serve a modified bundle
that sends it, and no amount of client-side cryptography prevents that. What you
get is protection against a server that keeps too much, against your password
reaching logs, heap dumps and backups, and against someone who obtains the data
at rest. That is the same trust boundary as every other end-to-end product
delivered through a browser.

Keep these recovery and unlock requirements in mind:

- **An admin password reset can strand the password-wrapped key.** Before
  you need a reset, use Security → Encryption to create a recovery copy and
  keep its separately displayed secret. The browser tests the encrypted file
  in memory, requests its download and, after you confirm saving the secret, stores a sealed copy on the server.
  After signing in with the reset password, **Use server recovery copy** can
  restore the current identity even if you lost the download. The server never
  receives the recovery secret or plaintext key. Keep an offline file too:
  deleting the identity removes the server copy, and a server loss removes it
  as well. **Run recovery drill** tests your copy and secret without changing
  the key; its date is recorded only in this browser, for the copy tested.
- **You unlock the key once for each browser session.** The browser holds the
  unwrapped key in page memory only, never in localStorage or sessionStorage.
  After a reload you must enter your password again.
- **KyPost does not add verified send-as addresses to your key
  automatically.** That edit re-signs the key and needs the private half. The
  browser makes the edit, not the background poller.

**Server-protected keys are retired.** KyPost no longer creates or imports a
private key it can open, and it refuses server-side PGP decryption and signing
with a legacy server-held key. The old generation/import endpoints return an
error directing clients to the wrapped-key endpoint.

Existing legacy keys are preserved so users can recover them. In Security →
Encryption, use **Migrate to end-to-end**: after confirming the account
credential, the browser receives the old key, wraps it under your password and
uploads the wrapped copy. That upload removes the server-readable copy. Keep
the legacy master key (`pgp-private-key.key`) available until those accounts
have migrated; the remaining export path is for recovery and migration, not a supported custody
choice. A server-stored **recovery copy** is different: it is sealed under the
user's separate recovery secret, which the server does not receive.

Other PGP boundaries:

- **Browser RSA minimum: 2048 bits.** New recipient encryption, signatures,
  encrypted Sent copies, drafts and sealed autosaves enforce this minimum on
  the primary key and selected subkey. A weak recipient causes a refusal;
  KyPost does not silently leave them out or send plaintext instead. Ask the
  correspondent for a supported key. Imported and retained private keys stay
  available to open historical mail, drafts and autosaves, including those
  encrypted with older RSA keys. A signature made with an undersized RSA key
  does not receive a verified verdict, even when the text remains readable.
  Preserve old keys and backups; retaining a key does not make it eligible for
  new encryption or signing. If autosave cannot seal or store a new draft,
  compose shows **Autosave unavailable** and preserves the previous snapshot.
- **Server RSA minimum: 2048 bits.** Server-side encryption (legacy
  server-held keys and incoming encryption) and signing refuse an RSA primary
  or selected subkey below 2048 bits and name the key. Incoming encryption
  leaves the original message in place. See [key strength](STRONG-CRYPTO.md).
  Keep the window open and copy the current text somewhere safe before leaving.
  This browser policy does not establish TLS or
  the independent native clients' policies.

- **Ordinary PGP/MIME does not encrypt subject lines.** Your mail provider sees
  them. KyPost protects the subject inside the encrypted part when
  it can, but the outer header remains.
- **Mobile push notifications are generic by default** for this reason. See
  Settings → Notifications.
- **A recipient without a key can get an optional one-time pickup link.**
  For a client-protected sender, the browser seals the message, uploads only
  ciphertext to `/api/pgp/pickup`, and carries the opening key in the link
  fragment. The recipient decrypts in their browser. The notification travels
  through SMTP, so the sending server and anyone who can read it can see the
  link and its key.
  A sender with no PGP identity of their own can instead opt into
  `/api/mail/send` with `encrypt: true` and `allowPickupFallback: true`.
  That fallback seals the subject and body with the server's `pickup-store.key`;
  the server can read the message until it is consumed or expires, up to seven
  days. Compose labels it **Secure link if no key (server-held, 7 days)**.
  Legacy server-custody accounts must migrate before encrypted sending.
  An encrypted send to a keyless recipient refuses unless the user enables
  the fallback. See
  [sealed pickup links](E2E_PGP.md#sealed-pickup-links).
