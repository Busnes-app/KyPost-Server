// pgpEngine: the OpenPGP operations pgpClient needs, behind one interface so
// openpgp.js and gopenpgp compiled to WebAssembly can be compared on the same
// test suite (docs/superpowers/specs/2026-11-26-pqc-gopenpgp-wasm-evaluation.md).
// Keys and messages cross it armored, as pgpClient already stores them.

export type KeyProfile = "v4" | "v6";

export type EngineKey = { armoredPrivateKey: string; armoredPublicKey: string; fingerprint: string };

export type KeyInfo = {
  fingerprint: string;
  version: number;
  isPrivate: boolean;
  /** Primary first, then subkeys; upper-case hex. */
  keyIDs: string[];
  fingerprints: string[];
  algorithms: number[];
};

/** One signature found in a message. keyID or fingerprint may be "" when the packet omits it. */
export type SignatureCheck = { keyID: string; fingerprint: string; valid: boolean };

export interface PgpEngine {
  readonly name: "openpgp.js" | "gopenpgp-wasm";
  generate(userIDs: { name: string; email: string }[], profile: KeyProfile): Promise<EngineKey>;
  /** Returns the key unlocked. */
  importPrivate(armoredKey: string, passphrase: string): Promise<EngineKey>;
  inspect(armoredKey: string): Promise<KeyInfo>;
  /** One delivery group: every recipient key must be the same version. */
  encrypt(opts: { text: string; encryptionKeys: string[]; signingKey?: string }): Promise<string>;
  signDetached(data: Uint8Array, signingKey: string): Promise<{ armoredSignature: string; hashAlgorithm: number }>;
  /** decryptionKeys is the whole ring; the engine picks the member that opens it. */
  decrypt(opts: { armoredMessage: string; decryptionKeys: string[]; verificationKeys: string[]; maxBytes: number }):
    Promise<{ data: string; signatures: SignatureCheck[] }>;
  verifyDetached(opts: { data: Uint8Array; armoredSignature: string; verificationKeys: string[] }): Promise<SignatureCheck[]>;
  armor(data: Uint8Array, type: "PGP MESSAGE" | "PGP SIGNATURE" | "PGP PUBLIC KEY BLOCK"): Promise<string>;
  dearmor(armored: string): Promise<Uint8Array>;
}

// Dev-only switch, fixed at build time: production builds never set
// VITE_PGP_ENGINE, so the wasm module is not even fetched.
const selected = import.meta.env.VITE_PGP_ENGINE === "gopenpgp-wasm" ? "gopenpgp-wasm" : "openpgp.js";

let current: Promise<PgpEngine> | undefined;

export function pgpEngine(name: PgpEngine["name"] = selected): Promise<PgpEngine> {
  if (name !== selected) return load(name);
  current ??= load(name);
  return current;
}

// Only dev and test builds, or a build that selected it, can reach the wasm
// engine; a production build drops the chunk entirely.
const wasmAllowed = import.meta.env.MODE !== "production" || selected === "gopenpgp-wasm";

async function load(name: PgpEngine["name"]): Promise<PgpEngine> {
  if (name === "gopenpgp-wasm") {
    if (!wasmAllowed) throw new Error("The gopenpgp wasm engine is not part of this build.");
    return (await import("./pgpEngineWasm")).wasmEngine();
  }
  return (await import("./pgpEngineOpenpgp")).openpgpEngine;
}
