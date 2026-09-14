// pgpClient: the OpenPGP operations that must happen in the browser once the
// private key is client-protected, because the server has no copy it can
// open.
//
// openpgp is imported dynamically everywhere here for the same reason
// ContactsPage already does it: it is a large bundle and no page needs it
// until the user actually touches PGP.

import { type BoundSignerKey } from "../api/pgp";
import { requireUnlockedKey } from "./keyVault";
import { parseMimeContent, type BodyMode, type MimeAttachment, type ProtectedHeaders } from "./mimeContent";

type OpenPGP = typeof import("openpgp");
type PublicKey = Awaited<ReturnType<OpenPGP["readKey"]>>;

async function openpgp(): Promise<OpenPGP> {
  return import("openpgp");
}

export type GeneratedIdentity = {
  armoredPrivateKey: string;
  armoredPublicKey: string;
  fingerprint: string;
};

/**
 * Generates a new keypair in the browser. The private key never leaves it
 * unwrapped.
 *
 * curve25519Legacy, not openpgp.js v6's modern `type: "curve25519"`. The
 * "legacy" name is misleading: it is the RFC 4880-compatible EdDSA/Curve25519
 * key that GnuPG, Thunderbird, and this server's own gopenpgp generator all
 * produce and accept. The modern option emits an RFC 9580 v6 key that much
 * of the ecosystem still rejects, which for a mail client means recipients
 * silently unable to read anything you send them.
 */
export async function generateIdentity(
  name: string,
  email: string,
  additionalEmails: string[] = []
): Promise<GeneratedIdentity> {
  const pgp = await openpgp();
  // Every address the account has proven it owns becomes a User ID. Both WKD
  // serving and Autocrypt advertising refuse a key that does not carry the
  // address in question, so a key with only the primary address silently
  // fails to publish for verified aliases. Mirrors pgpmail.GenerateIdentity.
  const seen = new Set([email.trim().toLowerCase()]);
  const userIDs = [{ name, email: email.trim() }];
  for (const extra of additionalEmails) {
    const addr = extra.trim();
    if (!addr || seen.has(addr.toLowerCase())) {
      continue;
    }
    seen.add(addr.toLowerCase());
    userIDs.push({ name, email: addr });
  }
  const { privateKey, publicKey } = await pgp.generateKey({
    type: "ecc",
    curve: "curve25519Legacy",
    userIDs,
    format: "armored"
  });
  const parsed = await pgp.readKey({ armoredKey: publicKey });
  return {
    armoredPrivateKey: privateKey,
    armoredPublicKey: publicKey,
    fingerprint: parsed.getFingerprint().toUpperCase()
  };
}

/**
 * Reads an armored private key the user is importing, unlocking it with
 * passphrase if it carries one.
 *
 * The returned private key is always decrypted: it is about to be wrapped
 * under the account password instead, so carrying its original passphrase
 * forward would mean two secrets to lose rather than one.
 */
export async function importIdentity(
  armoredPrivateKey: string,
  passphrase: string
): Promise<GeneratedIdentity> {
  const pgp = await openpgp();
  let key = await pgp.readPrivateKey({ armoredKey: armoredPrivateKey.trim() });
  if (!key.isDecrypted()) {
    if (!passphrase) {
      throw new Error("This key is passphrase-protected. Enter its passphrase to import it.");
    }
    key = await pgp.decryptKey({ privateKey: key, passphrase });
  }
  return {
    armoredPrivateKey: key.armor(),
    armoredPublicKey: key.toPublic().armor(),
    fingerprint: key.getFingerprint().toUpperCase()
  };
}

export type DecryptedMessage = {
  body: string;
  /**
   * Which MIME part `body` came from, read off the decrypted entity's own
   * Content-Type — the client-side counterpart to the server's `bodyMode`.
   *
   * Undefined only for an inline-PGP message, which decrypts to bare text with
   * no MIME headers to read. Route it through displayBody (pages/read/body.ts)
   * so that one case gets the fallback and nothing else does.
   */
  bodyMode?: BodyMode;
  signed: boolean;
  verified: boolean;
  signerFingerprint: string;
  /**
   * The address book binds a key to this sender but it fails its TOFU pin, so
   * the server withheld the key material and nothing could be checked. Distinct
   * from having no key at all — see read/signature.ts.
   */
  signerConflict: boolean;
  /** Decoded in memory from the entity this browser checked. Never posted anywhere. */
  attachments: MimeAttachment[];
  /** Parts refused by the MIME limits rather than decoded. */
  attachmentsOmitted: number;
  /** Headers carried inside the ciphertext: the real Subject, and for a draft its recipients. */
  protectedHeaders: ProtectedHeaders;
};

/**
 * Plaintext ceiling for one decrypted message, mirroring
 * mailmsg.MaxInboundMessageBytes. openpgp.js applies it to the decompressed
 * stream, so a small compressed packet that inflates past it fails inside
 * the library instead of after the allocation it was aiming for.
 */
export const MAX_DECRYPTED_BYTES = 25 * 1024 * 1024;

/**
 * Reads the bound signer keys and indexes each one's bound addresses by
 * fingerprint, so a signature can be checked against the addresses the ADDRESS
 * BOOK gives its key rather than against anything the key says about itself.
 *
 * Nothing here parses a User ID, and that is the point. The browser used to
 * decide "does this key belong to the sender" by comparing the parsed email of
 * each User ID — openpgp.js's parse, while the server had pinned the key using
 * go-crypto's. The two disagree on adversarial User IDs in both directions, so
 * the browser could vouch for a key the server's own binding rejected. Worse,
 * the check was forgeable on its own terms whichever parser won: User IDs are
 * self-asserted and a key may carry as many as its owner likes, so one key with
 * `Mallory <mallory@evil.example>` and `Bob <bob@example.com>` is pinned under
 * Mallory's contact and then verifies mail claiming to be from Bob.
 *
 * The server now sends the binding it applied — see boundSignerKeys in
 * pgp_receive.go — and this compares address strings.
 */
async function readBoundSignerKeys(
  pgp: OpenPGP,
  signerKeys: BoundSignerKey[]
): Promise<{ keys: PublicKey[]; addressesByFingerprint: Map<string, string[]> }> {
  const keys: PublicKey[] = [];
  const addressesByFingerprint = new Map<string, string[]>();
  for (const entry of signerKeys) {
    const trimmed = entry?.publicKey?.trim();
    if (!trimmed) {
      continue;
    }
    try {
      const key = await pgp.readKey({ armoredKey: trimmed });
      keys.push(key);
      addressesByFingerprint.set(
        key.getFingerprint().toUpperCase(),
        (entry.addresses ?? []).map((a) => a.trim().toLowerCase())
      );
    } catch {
      // One unparseable contact key must not cost every other signer their
      // verification.
    }
  }
  return { keys, addressesByFingerprint };
}

/**
 * Decrypts a PGP/MIME payload the server handed through untouched, using the
 * unlocked key. Throws VaultLockedError if the vault is locked.
 *
 * signerKeys are the contact keys the server holds, each labelled with the
 * addresses the address book binds it to. Which one signed is not known in
 * advance, so all are offered and whichever actually produced the signature is
 * identified — but `verified` is true only when that key is bound to
 * senderAddress. `verified` means "the sender signed this", not "somebody did".
 */
export async function decryptMessage(
  payload: string,
  signerKeys: BoundSignerKey[],
  senderAddress: string
): Promise<DecryptedMessage> {
  const pgp = await openpgp();
  const privateKey = await pgp.readPrivateKey({ armoredKey: requireUnlockedKey() });

  const armored = extractArmoredMessage(payload);
  const message = await pgp.readMessage({ armoredMessage: armored });

  const { keys: verificationKeys, addressesByFingerprint } = await readBoundSignerKeys(
    pgp,
    signerKeys
  );
  const result = await pgp.decrypt({
    message,
    decryptionKeys: privateKey,
    verificationKeys: verificationKeys.length > 0 ? verificationKeys : undefined,
    expectSigned: false,
    config: { maxDecompressedMessageSize: MAX_DECRYPTED_BYTES }
  });

  let signed = false;
  let verified = false;
  let signerFingerprint = "";
  const signatures = result.signatures ?? [];
  if (signatures.length > 0) {
    signed = true;
    const wanted = senderAddress.trim().toLowerCase();
    for (const signature of signatures) {
      try {
        // `verified` rejects rather than returning false on a bad signature.
        await signature.verified;
        const keyID = signature.keyID.toHex().toUpperCase();
        const match = verificationKeys.find((k) =>
          k.getKeys().some((sub) => sub.getKeyID().toHex().toUpperCase() === keyID)
        );
        signerFingerprint = match ? match.getFingerprint().toUpperCase() : keyID;
        // A cryptographically valid signature from SOME key in the address
        // book proves only that someone signed this. The badge claims the
        // SENDER signed it, so the key that actually produced the signature
        // must be one the address book binds to the sender's address. Without
        // this, an attacker whose key the reader had auto-pinned — Autocrypt
        // harvest and WKD auto-trust both pin without asking — could sign a
        // message, put anyone in the From header, and be vouched for by the UI.
        const bound = match
          ? addressesByFingerprint.get(match.getFingerprint().toUpperCase())
          : undefined;
        verified = Boolean(wanted && bound?.includes(wanted));
        break;
      } catch {
        // Try the next signature; an unverifiable one is not fatal.
      }
    }
  }

  // The decrypted payload is a MIME entity, not display text: headers,
  // boundaries and an encoded body. Parsing it here keeps the reader from
  // showing "Content-Type: text/html" and a boundary marker as part of the
  // message, and recovers the render mode from the part's own Content-Type so
  // nothing downstream has to sniff the bytes. An inline-PGP message has no MIME
  // headers; parseMimeContent returns null and the raw text is the body.
  const raw = typeof result.data === "string" ? result.data : String(result.data);
  const parsed = parseMimeContent(raw);

  return {
    body: parsed ? parsed.body : raw,
    bodyMode: parsed?.mode,
    // MIME detached signatures are present but unchecked; only the packet
    // verification above can establish a sender-bound verification result.
    signed: signed || Boolean(parsed?.hasDetachedSignature),
    verified,
    signerFingerprint,
    signerConflict: hasSignerConflict(signerKeys),
    attachments: parsed?.attachments ?? [],
    attachmentsOmitted: parsed?.attachmentsOmitted ?? 0,
    protectedHeaders: parsed?.protectedHeaders ?? {}
  };
}

/**
 * Verifies a detached signature over an RFC 3156 signed part, and returns the
 * body parsed out of the part it just verified.
 *
 * That pairing is the contract, not a convenience. The caller renders this
 * `body` in place of the server's, so the badge describes the bytes on screen.
 * Returning a verdict while the reader looks at a separately-parsed copy from
 * the inbox response would make "signature verified" a decoration: the two
 * copies come from different parsers and, against a hostile server, from
 * different content entirely.
 *
 * signedPartBase64 is base64 because these are the exact bytes the sender
 * hashed — headers, CRLFs and transfer encoding intact. Anything that
 * re-encodes them on the way here (JSON's UTF-8, a MIME re-serialization)
 * breaks the hash and turns every signed message into a warning.
 *
 * `verified` is true only when the key that actually produced the signature is
 * one the ADDRESS BOOK binds to senderAddress — see readBoundSignerKeys. A
 * valid signature from any other key the reader happens to hold sets
 * signerFingerprint and leaves verified false, which the badge renders as
 * "does not match sender" rather than as a pass.
 *
 * No private key, so no vault unlock: this works while the vault is locked.
 */
export async function verifySignedMessage(
  signedPartBase64: string,
  armoredSignature: string,
  signerKeys: BoundSignerKey[],
  senderAddress: string
): Promise<DecryptedMessage> {
  const pgp = await openpgp();

  const raw = atob(signedPartBase64);
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i += 1) {
    bytes[i] = raw.charCodeAt(i);
  }

  // Decoded as text only to READ it. Verification below uses the bytes.
  const partText = new TextDecoder("utf-8").decode(bytes);
  const parsed = parseMimeContent(partText);
  const body = parsed ? parsed.body : partText;
  const bodyMode = parsed?.mode;

  const { keys: verificationKeys, addressesByFingerprint } = await readBoundSignerKeys(
    pgp,
    signerKeys
  );

  let verified = false;
  let signerFingerprint = "";
  try {
    // No bound key means nothing to check the signature against. Short-circuit
    // rather than calling verify with an empty key list: the outcome is the
    // same "could not be checked" state, but it is stated here instead of
    // arriving as an exception from inside openpgp.js.
    if (verificationKeys.length === 0) {
      throw new Error("no key is bound to this sender");
    }
    const result = await pgp.verify({
      // binary, not text: the part's bytes are already in canonical CRLF form
      // and must reach the hash untouched. Handing openpgp.js a string invites
      // it to re-canonicalize content that is already exactly right.
      message: await pgp.createMessage({ binary: bytes }),
      signature: await pgp.readSignature({ armoredSignature }),
      verificationKeys,
      expectSigned: false
    });
    const wanted = senderAddress.trim().toLowerCase();
    for (const signature of result.signatures ?? []) {
      try {
        // `verified` rejects rather than returning false on a bad signature.
        await signature.verified;
        const keyID = signature.keyID.toHex().toUpperCase();
        const match = verificationKeys.find((k) =>
          k.getKeys().some((sub) => sub.getKeyID().toHex().toUpperCase() === keyID)
        );
        signerFingerprint = match ? match.getFingerprint().toUpperCase() : keyID;
        const boundAddresses = match
          ? addressesByFingerprint.get(match.getFingerprint().toUpperCase())
          : undefined;
        verified = Boolean(wanted && boundAddresses?.includes(wanted));
        break;
      } catch {
        // Try the next signature; an unverifiable one is not fatal.
      }
    }
  } catch {
    // An unreadable signature, or no key to check it with. The message stays
    // readable and the badge says it could not be checked — never that it
    // failed, which would accuse a sender on the strength of our own gap.
  }

  return {
    body,
    bodyMode,
    // The caller only reaches this function for a message the server flagged as
    // carrying a detached signature, so `signed` is what got us here.
    signed: true,
    verified,
    signerFingerprint,
    signerConflict: hasSignerConflict(signerKeys),
    attachments: parsed?.attachments ?? [],
    attachmentsOmitted: parsed?.attachmentsOmitted ?? 0,
    protectedHeaders: parsed?.protectedHeaders ?? {}
  };
}

/**
 * Whether the address book binds a key to this sender that fails its TOFU pin.
 *
 * The server sends such an entry with its addresses but no key material, so it
 * contributes nothing to verification — which is exactly why the reader needs
 * to be told it exists. A changed key is the one event TOFU is for, and without
 * this it rendered as the same shrug as an unknown correspondent.
 */
function hasSignerConflict(signerKeys: BoundSignerKey[]): boolean {
  return signerKeys.some((k) => k?.conflict === true);
}

/** A compose attachment as it goes into the encrypted entity: already base64. */
export type EncryptedAttachment = {
  name: string;
  mimeType: string;
  dataBase64: string;
};

/** Mirror of maxClientCiphertextBytes in pgp_send_client.go. */
const MAX_SEND_PGP_REQUEST_BYTES = 64 * 1024 * 1024;

/**
 * How many decoded attachment bytes one encrypted send can carry, given how
 * many ciphertext copies the request holds (one per delivery group plus the
 * Sent copy).
 *
 * Two ceilings, both server-side: every copy must fit the inbound message cap
 * or the recipient's server refuses it, and all copies together must fit the
 * send-pgp request. Attachments are base64 inside the entity and the entity is
 * armored again, so 16/9 of the raw bytes reach the wire; 1 MiB is left for
 * headers, body and armor overhead. This is checked before encrypting so the
 * refusal names the limit instead of arriving as a 413 after the work.
 */
export function encryptedAttachmentBudget(copies: number): number {
  const perCopy = Math.min(MAX_DECRYPTED_BYTES, MAX_SEND_PGP_REQUEST_BYTES / Math.max(1, copies));
  // Never negative: past ~36 copies the headroom exceeds the share, and a
  // negative allowance would refuse a send that carries no files at all.
  return Math.max(0, Math.floor((perCopy * 9) / 16) - 1024 * 1024);
}

/** One encrypted delivery: a full PGP/MIME message plus its recipients. */
export type EncryptedDelivery = {
  recipients: string[];
  ciphertext: string;
};

/**
 * The outer, unencrypted envelope of a PGP/MIME message.
 *
 * These headers are the message as far as a receiving MTA is concerned:
 * /api/mail/send-pgp relays the bytes verbatim and synthesizes nothing. An
 * earlier version of this module emitted only Content-Type and MIME-Version,
 * producing messages with no From, To, Subject, or Date — malformed, and
 * rejected or rendered blank by receiving clients. The server now refuses
 * such a delivery (validatePGPMimeDelivery) rather than relaying it.
 *
 * bcc is deliberately absent: BCC recipients get their own delivery and must
 * not appear in anyone's headers, exactly as the server-side path does it.
 */
export type MessageEnvelope = {
  from: string;
  to: string[];
  cc?: string[];
  subject: string;
  date?: Date;
};

/**
 * Encrypts (and optionally signs) a message body once per delivery group.
 *
 * BCC recipients get their own delivery each, so they never appear in one
 * another's encryption key list — the same split the server-side path makes,
 * and the reason this takes groups rather than one flat recipient list.
 *
 * The real Subject is moved inside the encrypted part as a protected header
 * and replaced on the outside with a placeholder, mirroring
 * pgpmail.EncryptMIME. Encrypting the body while leaving the subject in
 * cleartext would give away most of what encryption was for.
 */
export async function buildEncryptedDeliveries(
  envelope: MessageEnvelope,
  contentType: string,
  body: string,
  groups: { recipients: string[]; publicKeys: string[] }[],
  sign: boolean,
  attachments: EncryptedAttachment[] = []
): Promise<EncryptedDelivery[]> {
  const pgp = await openpgp();
  const signingKeys = sign ? [await pgp.readPrivateKey({ armoredKey: requireUnlockedKey() })] : undefined;
  const protectedContent = buildProtectedContent(contentType, body, { subject: envelope.subject }, attachments);

  const deliveries: EncryptedDelivery[] = [];
  for (const group of groups) {
    if (group.recipients.length === 0 || group.publicKeys.length === 0) {
      continue;
    }
    const encryptionKeys = await readPublicKeys(pgp, group.publicKeys);
    const armored = await pgp.encrypt({
      message: await pgp.createMessage({ text: protectedContent }),
      encryptionKeys,
      signingKeys,
      format: "armored"
    });
    deliveries.push({
      recipients: group.recipients,
      ciphertext: wrapAsPGPMime(envelope, String(armored))
    });
  }
  return deliveries;
}

/**
 * Builds a signed, unencrypted delivery: RFC 3156 multipart/signed with a
 * detached signature over the exact bytes of the signed part. Needs no
 * recipient keys, so every recipient shares one delivery; Bcc addresses are
 * envelope recipients only and never appear in the headers.
 *
 * The signed part is the same protected-headers entity an encrypted send
 * carries (Subject repeated inside, body, attachments), base64 so it is 7-bit
 * safe. The signature is a binary signature over those bytes, which is what
 * verifySignedMessage and GnuPG both hash.
 */
export async function buildSignedDelivery(
  envelope: MessageEnvelope,
  contentType: string,
  body: string,
  recipients: string[],
  attachments: EncryptedAttachment[] = []
): Promise<EncryptedDelivery> {
  const pgp = await openpgp();
  const signingKey = await pgp.readPrivateKey({ armoredKey: requireUnlockedKey() });
  const signedPart = buildProtectedContent(contentType, body, { subject: envelope.subject }, attachments, true);
  const armoredSignature = String(
    await pgp.sign({
      message: await pgp.createMessage({ binary: new TextEncoder().encode(signedPart) }),
      signingKeys: signingKey,
      detached: true,
      format: "armored"
    })
  );
  const signature = await pgp.readSignature({ armoredSignature });
  const hash = MICALG_BY_HASH[signature.packets[0].hashAlgorithm ?? -1] ?? "pgp-sha256";
  return { recipients, ciphertext: wrapAsSignedMime(envelope, signedPart, armoredSignature, hash) };
}

/** RFC 3156 micalg names by OpenPGP hash algorithm id (RFC 9580 section 9.5). */
const MICALG_BY_HASH: Record<number, string> = {
  2: "pgp-sha1",
  8: "pgp-sha256",
  9: "pgp-sha384",
  10: "pgp-sha512",
  11: "pgp-sha224",
  12: "pgp-sha3-256",
  14: "pgp-sha3-512"
};

/**
 * Builds the Sent-folder copy: the same protected content the recipients get,
 * encrypted to the SENDER'S OWN key and wrapped as PGP/MIME.
 *
 * This used to be the composer's raw HTML, posted to the server in the clear.
 * On a client-custody account that quietly gave away everything the deliveries
 * were protecting — the body and the real subject of every message — to a
 * server whose stated property is that it cannot read your mail.
 *
 * The encryption key is derived from the UNLOCKED PRIVATE KEY in this browser,
 * not fetched from the server. That matters: if the server supplied "your"
 * public key, a compromised or hostile one could hand back an attacker's key
 * and every Sent copy would be encrypted to them, with nothing on screen
 * looking any different.
 *
 * Signing is optional and mirrors the send: a copy of a signed message is
 * signed. It changes nothing about who can read it.
 */
export async function buildEncryptedSentCopy(
  envelope: MessageEnvelope,
  contentType: string,
  body: string,
  sign: boolean,
  attachments: EncryptedAttachment[] = []
): Promise<string> {
  const pgp = await openpgp();
  const ownKey = await pgp.readPrivateKey({ armoredKey: requireUnlockedKey() });
  const signingKeys = sign ? [ownKey] : undefined;
  const armored = await pgp.encrypt({
    message: await pgp.createMessage({ text: buildProtectedContent(contentType, body, { subject: envelope.subject }, attachments) }),
    encryptionKeys: ownKey.toPublic(),
    signingKeys,
    format: "armored"
  });
  return wrapAsPGPMime(envelope, String(armored));
}

/**
 * Builds a draft: the whole compose state, encrypted to the sender's own key
 * and wrapped as PGP/MIME, for a verbatim IMAP APPEND.
 *
 * To, Cc and Bcc travel inside the ciphertext as protected headers next to
 * the Subject, because they are what the composer needs back and Bcc must not
 * appear on the outside at all. The outer envelope carries To and Cc as the
 * Sent copy does, so a mail client lists the draft sensibly.
 */
export async function buildEncryptedDraft(
  envelope: MessageEnvelope & { bcc?: string[] },
  contentType: string,
  body: string,
  attachments: EncryptedAttachment[] = []
): Promise<string> {
  const pgp = await openpgp();
  const ownKey = await pgp.readPrivateKey({ armoredKey: requireUnlockedKey() });
  const content = buildProtectedContent(
    contentType,
    body,
    { subject: envelope.subject, to: envelope.to.join(", "), cc: (envelope.cc ?? []).join(", "), bcc: (envelope.bcc ?? []).join(", ") },
    attachments
  );
  const armored = await pgp.encrypt({
    message: await pgp.createMessage({ text: content }),
    encryptionKeys: ownKey.toPublic(),
    format: "armored"
  });
  return wrapAsPGPMime(envelope, String(armored));
}

/**
 * Seals text to the sender's own key, for state that must survive a reload
 * without sitting in web storage as plaintext. Opened by openSealedToSelf
 * once the vault is unlocked again.
 */
export async function sealToSelf(text: string): Promise<string> {
  const pgp = await openpgp();
  const ownKey = await pgp.readPrivateKey({ armoredKey: requireUnlockedKey() });
  return String(
    await pgp.encrypt({ message: await pgp.createMessage({ text }), encryptionKeys: ownKey.toPublic(), format: "armored" })
  );
}

export async function openSealedToSelf(armored: string): Promise<string> {
  const pgp = await openpgp();
  const privateKey = await pgp.readPrivateKey({ armoredKey: requireUnlockedKey() });
  const result = await pgp.decrypt({
    message: await pgp.readMessage({ armoredMessage: armored }),
    decryptionKeys: privateKey,
    config: { maxDecompressedMessageSize: MAX_DECRYPTED_BYTES }
  });
  return typeof result.data === "string" ? result.data : String(result.data);
}

// Matches pgpmail.OuterPlaceholderSubject so both send paths look identical
// on the wire.
export const OUTER_PLACEHOLDER_SUBJECT = "[Encrypted] Email Sent by KyPost";

/**
 * Wraps the real content in an RFC 5322 protected-headers part carrying the
 * true Subject, mirroring pgpmail.protectContent. The receiving side lifts it
 * back out (pgpmail.ExtractProtectedSubject).
 *
 * Attachments follow the body as their own parts of the same multipart/mixed,
 * base64 in 76-column lines with the headers mailmsg.Build writes, so the
 * entity a recipient decrypts is the one the server-side path used to make.
 */
function buildProtectedContent(
  contentType: string,
  body: string,
  protectedHeaders: ProtectedHeaders,
  attachments: EncryptedAttachment[] = [],
  encodeBody = false
): string {
  // A signed part must be 7-bit safe, headers included; an encrypted one is
  // never seen by transport, and readers decode either form.
  const clean = encodeBody ? encodeHeaderWord(sanitizeHeaderValue(protectedHeaders.subject ?? "")) : sanitizeHeaderValue(protectedHeaders.subject ?? "");
  const boundary = `kypost-protected-${randomToken()}`;
  const lines = [`Content-Type: multipart/mixed; boundary="${boundary}"; protected-headers="v1"`, ""];
  // Address headers are protected only when a caller asks (drafts). They sit
  // in the entity's own header block, where readProtectedHeaders finds them;
  // the legacy-display part below stays Subject-only for other clients.
  for (const name of ["Bcc", "Cc", "To"] as const) {
    const value = sanitizeHeaderValue(protectedHeaders[name.toLowerCase() as "to" | "cc" | "bcc"] ?? "");
    if (value) lines.unshift(`${name}: ${value}`);
  }
  if (clean) {
    lines.unshift(`Subject: ${clean}`);
    lines.push(
      `--${boundary}`,
      'Content-Type: text/rfc822-headers; protected-headers="v1"',
      "Content-Disposition: inline",
      "",
      `Subject: ${clean}`,
      ""
    );
  }
  if (encodeBody) {
    // A signed part travels in the clear and must be 7-bit safe (RFC 3156
    // section 5): base64 keeps every byte the signature covers intact.
    lines.push(`--${boundary}`, `Content-Type: ${contentType}`, "Content-Transfer-Encoding: base64", "", ...wrapBase64(base64Utf8(body)), "");
  } else {
    lines.push(`--${boundary}`, `Content-Type: ${contentType}`, "", body, "");
  }
  for (const attachment of attachments) {
    const name = attachmentFilenameParams(attachment.name);
    lines.push(
      `--${boundary}`,
      `Content-Type: ${attachmentMediaType(attachment.mimeType)}${name.contentType}`,
      "Content-Transfer-Encoding: base64",
      `Content-Disposition: attachment${name.disposition}`,
      "",
      ...wrapBase64(attachment.dataBase64),
      ""
    );
  }
  lines.push(`--${boundary}--`, "");
  return lines.join("\r\n");
}

/** A syntactically valid media type, or the binary fallback the server uses. */
function attachmentMediaType(mimeType: string): string {
  const clean = sanitizeHeaderValue(mimeType).split(";")[0].trim().toLowerCase();
  return /^[a-z0-9][a-z0-9!#$&^_.+-]*\/[a-z0-9][a-z0-9!#$&^_.+-]*$/.test(clean) ? clean : "application/octet-stream";
}

/**
 * The name= and filename= parameter strings for one attachment.
 *
 * An ASCII-safe quoted value always goes out, because that is what every
 * reader understands; when the name has anything else, the RFC 2231 form
 * carries the real name alongside it, which is how Go's mime.FormatMediaType
 * and the clients this is tested against exchange non-ASCII filenames.
 */
function attachmentFilenameParams(rawName: string): { contentType: string; disposition: string } {
  const name = sanitizeHeaderValue(rawName) || "attachment";
  const ascii = name.replace(/[^\x20-\x7e]/g, "_").replace(/["\\]/g, "_");
  const needsExtended = ascii !== name;
  const extended = needsExtended ? `; filename*=utf-8''${encodeRFC2231(name)}` : "";
  return {
    contentType: `; name="${ascii}"${needsExtended ? extended.replace("filename*", "name*") : ""}`,
    disposition: `; filename="${ascii}"${extended}`
  };
}

/** RFC 2231 percent-encoding: everything outside the attribute-char set. */
function encodeRFC2231(value: string): string {
  return Array.from(new TextEncoder().encode(value), (b) => {
    const c = String.fromCharCode(b);
    return /[A-Za-z0-9!#$&+\-.^_`|~]/.test(c) ? c : `%${b.toString(16).toUpperCase().padStart(2, "0")}`;
  }).join("");
}

/** RFC 2045 76-column lines, as mailmsg.writeWrappedBase64 emits them. */
function wrapBase64(encoded: string): string[] {
  const clean = encoded.replace(/\s+/g, "");
  const out: string[] = [];
  for (let i = 0; i < clean.length; i += 76) {
    out.push(clean.slice(i, i + 76));
  }
  return out;
}

async function readPublicKeys(pgp: OpenPGP, armoredKeys: string[]) {
  const keys = [];
  for (const armored of armoredKeys) {
    const trimmed = armored?.trim();
    if (!trimmed) {
      continue;
    }
    try {
      keys.push(await pgp.readKey({ armoredKey: trimmed }));
    } catch {
      // One unparseable contact key must not cost every other recipient
      // their encryption, or every other signer their verification.
    }
  }
  return keys;
}

// RFC 3156 boundary. Fixed rather than random: it is only required to not
// occur in the body, and an ASCII-armored PGP block cannot contain it.
const PGP_MIME_BOUNDARY = "kypost-pgp-boundary";

/** Flattens CR/LF so a header value cannot inject extra headers. */
function sanitizeHeaderValue(value: string): string {
  return value.replace(/[\r\n]+/g, " ").trim();
}

function randomToken(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

/**
 * Wraps an armored PGP message as a complete RFC 5322 message with an RFC
 * 3156 multipart/encrypted body.
 *
 * This emits the full envelope — From, To, Cc, Subject, Date, MIME-Version —
 * not just the Content-Type. /api/mail/send-pgp relays these bytes verbatim,
 * so anything omitted here is simply absent from the delivered mail.
 */
function wrapAsPGPMime(envelope: MessageEnvelope, armoredMessage: string): string {
  const from = sanitizeHeaderValue(envelope.from);
  if (!from) {
    // The server binds every delivery's From to the account and refuses an
    // empty one; better to say so here than to emit an unparseable header.
    throw new Error("No sender address is known for this account yet. Reload and try again.");
  }
  const boundary = `${PGP_MIME_BOUNDARY}-${randomToken()}`;
  const headers = [
    `From: ${from}`,
    `To: ${envelope.to.map(sanitizeHeaderValue).filter(Boolean).join(", ")}`
  ];
  const cc = (envelope.cc ?? []).map(sanitizeHeaderValue).filter(Boolean);
  if (cc.length > 0) {
    headers.push(`Cc: ${cc.join(", ")}`);
  }
  // The real subject is inside the ciphertext as a protected header; this is
  // the placeholder the server-side path uses too.
  headers.push(`Subject: ${OUTER_PLACEHOLDER_SUBJECT}`);
  headers.push(`Date: ${(envelope.date ?? new Date()).toUTCString()}`);
  headers.push("MIME-Version: 1.0");
  headers.push(`Content-Type: multipart/encrypted; protocol="application/pgp-encrypted"; boundary="${boundary}"`);

  return [
    ...headers,
    "",
    "This is an OpenPGP/MIME encrypted message (RFC 3156).",
    `--${boundary}`,
    "Content-Type: application/pgp-encrypted",
    "Content-Description: PGP/MIME version identification",
    "",
    "Version: 1",
    "",
    `--${boundary}`,
    'Content-Type: application/octet-stream; name="encrypted.asc"',
    "Content-Description: OpenPGP encrypted message",
    'Content-Disposition: inline; filename="encrypted.asc"',
    "",
    armoredMessage.trim(),
    "",
    `--${boundary}--`,
    ""
  ].join("\r\n");
}

/**
 * Wraps a signed part and its detached signature as a complete RFC 5322
 * message with an RFC 3156 multipart/signed body. Unlike the encrypted
 * wrapper the real Subject goes on the outside: nothing here is secret, only
 * attributable. The CRLF before each boundary belongs to the delimiter (RFC
 * 2046 5.1.1), so the signed part is reproduced byte for byte.
 */
function wrapAsSignedMime(envelope: MessageEnvelope, signedPart: string, armoredSignature: string, micalg: string): string {
  const from = sanitizeHeaderValue(envelope.from);
  if (!from) {
    throw new Error("No sender address is known for this account yet. Reload and try again.");
  }
  const boundary = `${PGP_MIME_BOUNDARY}-${randomToken()}`;
  const headers = [
    `From: ${from}`,
    `To: ${envelope.to.map(sanitizeHeaderValue).filter(Boolean).join(", ")}`
  ];
  const cc = (envelope.cc ?? []).map(sanitizeHeaderValue).filter(Boolean);
  if (cc.length > 0) {
    headers.push(`Cc: ${cc.join(", ")}`);
  }
  headers.push(`Subject: ${encodeHeaderWord(sanitizeHeaderValue(envelope.subject))}`);
  headers.push(`Date: ${(envelope.date ?? new Date()).toUTCString()}`);
  headers.push("MIME-Version: 1.0");
  headers.push(`Content-Type: multipart/signed; micalg="${micalg}"; protocol="application/pgp-signature"; boundary="${boundary}"`);

  return [
    ...headers,
    "",
    "This is an OpenPGP/MIME signed message (RFC 4880 and 3156).",
    `--${boundary}`,
    signedPart,
    `--${boundary}`,
    'Content-Type: application/pgp-signature; name="signature.asc"',
    "Content-Description: OpenPGP digital signature",
    'Content-Disposition: attachment; filename="signature.asc"',
    "",
    armoredSignature.trim(),
    "",
    `--${boundary}--`,
    ""
  ].join("\r\n");
}

/** An RFC 2047 encoded word when the value is not plain ASCII, else as is. */
function encodeHeaderWord(value: string): string {
  return /^[\x20-\x7e]*$/.test(value) ? value : `=?UTF-8?B?${base64Utf8(value)}?=`;
}

/** UTF-8 text as standard base64. */
function base64Utf8(text: string): string {
  let binary = "";
  for (const byte of new TextEncoder().encode(text)) binary += String.fromCharCode(byte);
  return btoa(binary);
}

/**
 * Pulls the armored PGP block out of whatever the server passed through —
 * either a bare armored message or a full multipart/encrypted body.
 */
export function extractArmoredMessage(payload: string): string {
  const begin = payload.indexOf("-----BEGIN PGP MESSAGE-----");
  const endMarker = "-----END PGP MESSAGE-----";
  const end = payload.indexOf(endMarker);
  if (begin === -1 || end === -1) {
    return payload.trim();
  }
  return payload.slice(begin, end + endMarker.length);
}
