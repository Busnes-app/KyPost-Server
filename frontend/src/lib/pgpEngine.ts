// pgpEngine: the OpenPGP operations the browser needs, run by gopenpgp
// compiled to WebAssembly (backend/cmd/pgpwasm), the same library and KyPost
// profile as the server. Keys and messages cross it armored, as they are
// stored. Tests drive openpgp.js through the same interface as an interop
// target (src/test/openpgpEngine.ts); production has no other engine.

export type KeyProfile = "v4" | "v6";

export type EngineKey = { armoredPrivateKey: string; armoredPublicKey: string; fingerprint: string };

export type KeyInfo = {
  fingerprint: string;
  version: number;
  isPrivate: boolean;
  /** Some secret packet is passphrase-encrypted or a GNU dummy. */
  locked: boolean;
  /** Keys in the input; the rest of this describes the first. */
  count: number;
  /** Unix seconds, 0 for none. */
  expires: number;
  /** Binary transferable public key, as the engine serializes it. */
  publicKey: Uint8Array;
  /** publicKey split into packets, for an order-free comparison. */
  packets: Uint8Array[];
  /** Primary first, then subkeys; upper-case hex. */
  keyIDs: string[];
  fingerprints: string[];
  algorithms: number[];
  encryptionAlgorithm: number;
};

/** One signature found in a message. keyID or fingerprint may be "" when the packet omits it. */
export type SignatureCheck = { keyID: string; fingerprint: string; valid: boolean };

export interface PgpEngine {
  readonly name: string;
  generate(userIDs: { name: string; email: string }[], profile: KeyProfile): Promise<EngineKey>;
  /** Returns the key unlocked. */
  importPrivate(armoredKey: string, passphrase: string): Promise<EngineKey>;
  inspect(armoredKey: string): Promise<KeyInfo>;
  /** One delivery group: every recipient key must be the same version. */
  encrypt(opts: { text: string; encryptionKeys: string[]; signingKey?: string }): Promise<string>;
  signDetached(data: Uint8Array, signingKey: string): Promise<{ armoredSignature: string; hashAlgorithm: number }>;
  /** decryptionKeys is the whole ring; the engine picks the member that opens it. */
  decrypt(opts: { armoredMessage: string; decryptionKeys: string[]; verificationKeys: string[]; maxBytes: number }):
    Promise<{ data: string; signatures: SignatureCheck[]; quantumSafe?: boolean }>;
  verifyDetached(opts: { data: Uint8Array; armoredSignature: string; verificationKeys: string[] }): Promise<SignatureCheck[]>;
  armor(data: Uint8Array, type: "PGP MESSAGE" | "PGP SIGNATURE" | "PGP PUBLIC KEY BLOCK"): Promise<string>;
  dearmor(armored: string): Promise<Uint8Array>;
}

/** The browser's engine. Loads the module on first use, not before. */
export async function pgpEngine(): Promise<PgpEngine> {
  return (await import("./pgpEngineWasm")).wasmEngine;
}
