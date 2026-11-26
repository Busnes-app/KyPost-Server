// The openpgp.js engine: the calls pgpClient made before the engine split,
// with the same options.
import type { PgpEngine, SignatureCheck } from "./pgpEngine";
import { decryptionKeysFor } from "./pgpKeyring";

type OpenPGP = typeof import("openpgp");
type VerificationResult = Awaited<ReturnType<OpenPGP["verify"]>>["signatures"][number];

// OpenPGP.js defaults to 2047. Apply a literal 2048-bit floor to new use
// and signature trust; its decryption path still opens retained historical keys.
const RSA_POLICY = { minRSABits: 2048 };

const pgp = (): Promise<OpenPGP> => import("openpgp");

async function checks(signatures: VerificationResult[]): Promise<SignatureCheck[]> {
  return Promise.all(signatures.map(async (s) => {
    // `verified` rejects rather than returning false on a bad signature.
    const valid = await s.verified.then(() => true, () => false);
    return { keyID: s.keyID.toHex().toUpperCase(), fingerprint: "", valid };
  }));
}

export const openpgpEngine: PgpEngine = {
  name: "openpgp.js",

  async generate(userIDs, profile) {
    const o = await pgp();
    if (profile !== "v4") throw new Error("openpgp.js 6.3 cannot make RFC 9980 keys");
    const { privateKey, publicKey } = await o.generateKey({ type: "ecc", curve: "curve25519Legacy", userIDs, format: "armored" });
    const parsed = await o.readKey({ armoredKey: publicKey });
    return { armoredPrivateKey: privateKey, armoredPublicKey: publicKey, fingerprint: parsed.getFingerprint().toUpperCase() };
  },

  async importPrivate(armoredKey, passphrase) {
    const o = await pgp();
    let key = await o.readPrivateKey({ armoredKey: armoredKey.trim() });
    if (!key.isDecrypted()) {
      if (!passphrase) throw new Error("This key is passphrase-protected. Enter its passphrase to import it.");
      key = await o.decryptKey({ privateKey: key, passphrase });
    }
    return { armoredPrivateKey: key.armor(), armoredPublicKey: key.toPublic().armor(), fingerprint: key.getFingerprint().toUpperCase() };
  },

  async inspect(armoredKey) {
    const key = await (await pgp()).readKey({ armoredKey });
    const members = key.getKeys();
    return {
      fingerprint: key.getFingerprint().toUpperCase(),
      version: key.keyPacket.version,
      isPrivate: key.isPrivate(),
      keyIDs: members.map((m) => m.getKeyID().toHex().toUpperCase()),
      fingerprints: members.map((m) => m.getFingerprint().toUpperCase()),
      algorithms: members.map((m) => m.keyPacket.algorithm)
    };
  },

  async encrypt({ text, encryptionKeys, signingKey }) {
    const o = await pgp();
    const keys = await Promise.all(encryptionKeys.map((armoredKey) => o.readKey({ armoredKey })));
    return String(await o.encrypt({
      message: await o.createMessage({ text }),
      encryptionKeys: keys,
      signingKeys: signingKey ? [await o.readPrivateKey({ armoredKey: signingKey })] : undefined,
      format: "armored",
      config: RSA_POLICY
    }));
  },

  async signDetached(data, signingKey) {
    const o = await pgp();
    const armoredSignature = String(await o.sign({
      message: await o.createMessage({ binary: data }),
      signingKeys: await o.readPrivateKey({ armoredKey: signingKey }),
      detached: true,
      format: "armored",
      config: RSA_POLICY
    }));
    const signature = await o.readSignature({ armoredSignature });
    return { armoredSignature, hashAlgorithm: signature.packets[0].hashAlgorithm ?? -1 };
  },

  async decrypt({ armoredMessage, decryptionKeys, verificationKeys, maxBytes }) {
    const o = await pgp();
    const message = await o.readMessage({ armoredMessage });
    const keys = await Promise.all(decryptionKeys.map((armoredKey) => o.readPrivateKey({ armoredKey })));
    const verifiers = await Promise.all(verificationKeys.map((armoredKey) => o.readKey({ armoredKey })));
    const result = await o.decrypt({
      message,
      decryptionKeys: decryptionKeysFor({ keys }, message),
      verificationKeys: verifiers.length > 0 ? verifiers : undefined,
      expectSigned: false,
      config: { ...RSA_POLICY, maxDecompressedMessageSize: maxBytes }
    });
    const data = typeof result.data === "string" ? result.data : String(result.data);
    return { data, signatures: await checks(result.signatures ?? []) };
  },

  async verifyDetached({ data, armoredSignature, verificationKeys }) {
    const o = await pgp();
    const result = await o.verify({
      // binary, not text: the part's bytes are already in canonical CRLF form.
      message: await o.createMessage({ binary: data }),
      signature: await o.readSignature({ armoredSignature }),
      verificationKeys: await Promise.all(verificationKeys.map((armoredKey) => o.readKey({ armoredKey }))),
      expectSigned: false,
      config: RSA_POLICY
    });
    return checks(result.signatures ?? []);
  },

  async armor(data, type) {
    const o = await pgp();
    const id = { "PGP MESSAGE": o.enums.armor.message, "PGP SIGNATURE": o.enums.armor.signature, "PGP PUBLIC KEY BLOCK": o.enums.armor.publicKey }[type];
    return o.armor(id, data);
  },

  async dearmor(armored) {
    const { data } = await (await pgp()).unarmor(armored);
    return data instanceof Uint8Array ? data : new Uint8Array(await new Response(data as ReadableStream<Uint8Array>).arrayBuffer());
  }
};
