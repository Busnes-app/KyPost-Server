// The gopenpgp WebAssembly engine. scripts/build-pgp-wasm.sh writes the module
// and its toolchain's wasm_exec.js to .pgp-wasm/; Vite gives both hashed names.
import type { KeyInfo, PgpEngine, SignatureCheck } from "./pgpEngine";

type Result = { error?: string } & Record<string, unknown>;
type Bridge = Record<string, (req: Record<string, unknown>) => Result>;

declare global {
  var kypostPGP: Bridge | undefined;
  var Go: (new () => { importObject: WebAssembly.Imports; run(instance: WebAssembly.Instance): Promise<void> }) | undefined;
}

export class PgpEngineLoadError extends Error {
  constructor() {
    super("The encryption engine could not load. Check your connection and reload the page.");
    this.name = "PgpEngineLoadError";
  }
}

let ready: Promise<Bridge> | undefined;

async function start(): Promise<Bridge> {
  // Tests preload the module from disk (src/test/pgpWasmSetup.ts).
  if (globalThis.kypostPGP) return globalThis.kypostPGP;
  await import("../../.pgp-wasm/wasm_exec.js");
  const go = new globalThis.Go!();
  const url = new URL("../../.pgp-wasm/kypost-pgp.wasm", import.meta.url);
  const { instance } = await WebAssembly.instantiateStreaming(fetch(url), go.importObject);
  void go.run(instance); // main registers kypostPGP, then blocks in select{}
  if (!globalThis.kypostPGP) throw new Error("kypostPGP not registered");
  return globalThis.kypostPGP;
}

/** Loads once. A failed load is not cached, so the next operation retries. */
export function loadBridge(): Promise<Bridge> {
  ready ??= start().catch(() => {
    ready = undefined;
    // The cause (network, CSP, a corrupt module) stays out of the UI.
    throw new PgpEngineLoadError();
  });
  return ready;
}

const enc = new TextEncoder();
const dec = new TextDecoder();
const bytes = (s: string) => enc.encode(s);

async function call(op: string, req: Record<string, unknown>): Promise<Result> {
  const out = (await loadBridge())[op](req);
  if (out.error) throw new Error(out.error);
  return out;
}

const keyOf = (r: Result) => ({
  armoredPrivateKey: r.privateKey as string,
  armoredPublicKey: r.publicKey as string,
  fingerprint: r.fingerprint as string
});

export const wasmEngine: PgpEngine = {
  name: "gopenpgp-wasm",
  generate: async (userIDs, profile) => keyOf(await call("generate", { userIDs, profile })),
  importPrivate: async (key, passphrase) => {
    try {
      return keyOf(await call("importPrivate", { key: bytes(key), passphrase: bytes(passphrase) }));
    } catch (err) {
      if (err instanceof Error && err.message === "passphrase required") {
        throw new Error("This key is passphrase-protected. Enter its passphrase to import it.");
      }
      throw err;
    }
  },
  inspect: async (key) => (await call("inspect", { key: bytes(key) })) as unknown as KeyInfo,
  encrypt: async ({ text, encryptionKeys, signingKey }) =>
    (await call("encrypt", { data: bytes(text), encryptionKeys: encryptionKeys.map(bytes), signingKey: signingKey ? bytes(signingKey) : undefined })).message as string,
  signDetached: async (data, signingKey) => {
    const r = await call("signDetached", { data, signingKey: bytes(signingKey) });
    return { armoredSignature: r.signature as string, hashAlgorithm: r.hashAlgorithm as number };
  },
  decrypt: async ({ armoredMessage, decryptionKeys, verificationKeys, maxBytes }) => {
    const r = await call("decrypt", {
      message: bytes(armoredMessage),
      decryptionKeys: decryptionKeys.map(bytes),
      verificationKeys: verificationKeys.map(bytes),
      maxBytes
    });
    return { data: dec.decode(r.data as Uint8Array), signatures: r.signatures as SignatureCheck[], quantumSafe: r.quantumSafe === true };
  },
  reencrypt: async ({ armoredMessage, decryptionKeys, encryptionKey, maxBytes }) => {
    const r = await call("reencrypt", {
      message: bytes(armoredMessage),
      decryptionKeys: decryptionKeys.map(bytes),
      encryptionKey: bytes(encryptionKey),
      maxBytes
    });
    if (typeof r.message !== "string" || r.message.length === 0) {
      throw new Error("reencrypt returned empty ciphertext");
    }
    return r.message;
  },
  verifyDetached: async ({ data, armoredSignature, verificationKeys }) =>
    (await call("verifyDetached", { data, signature: bytes(armoredSignature), verificationKeys: verificationKeys.map(bytes) })).signatures as SignatureCheck[],
  armor: async (data, type) => (await call("armor", { data, type })).armored as string,
  dearmor: async (armored) => (await call("dearmor", { armored: bytes(armored) })).data as Uint8Array
};
