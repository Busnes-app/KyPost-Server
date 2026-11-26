// The gopenpgp engine: backend/cmd/pgpwasm compiled to WebAssembly, the same
// gopenpgp and KyPost profile (backend/internal/pgpprofile) as the server.
// Dev-only; built by scripts/build-pgp-wasm.sh into frontend/.pgp-wasm/.
import type { KeyInfo, PgpEngine, SignatureCheck } from "./pgpEngine";

type Result = { error?: string } & Record<string, unknown>;
type Bridge = Record<string, (req: Record<string, unknown>) => Result>;

declare global {
  var kypostPGP: Bridge | undefined;
  var Go: (new () => { importObject: WebAssembly.Imports; run(instance: WebAssembly.Instance): Promise<void> }) | undefined;
}

/** Where the dev server serves .pgp-wasm/ (vite.config.ts). */
const BASE = "/pgp-wasm/";

let ready: Promise<Bridge> | undefined;

/** Loads Go's wasm_exec.js and the module once. Tests preload them instead (src/test/pgpWasmSetup.ts). */
export function loadBridge(): Promise<Bridge> {
  ready ??= (async () => {
    if (!globalThis.kypostPGP) {
      if (!globalThis.Go) await import(/* @vite-ignore */ `${BASE}wasm_exec.js`);
      const go = new globalThis.Go!();
      const { instance } = await WebAssembly.instantiateStreaming(fetch(`${BASE}kypost-pgp.wasm`), go.importObject);
      void go.run(instance); // main registers kypostPGP, then blocks in select{}
    }
    if (!globalThis.kypostPGP) throw new Error("gopenpgp wasm did not start");
    return globalThis.kypostPGP;
  })();
  return ready;
}

const enc = new TextEncoder();
const dec = new TextDecoder();

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

export function wasmEngine(): PgpEngine {
  return {
    name: "gopenpgp-wasm",
    generate: async (userIDs, profile) => keyOf(await call("generate", { userIDs, profile })),
    importPrivate: async (key, passphrase) => {
      try {
        return keyOf(await call("importPrivate", { key: enc.encode(key), passphrase: enc.encode(passphrase) }));
      } catch (err) {
        if (err instanceof Error && err.message === "passphrase required") {
          throw new Error("This key is passphrase-protected. Enter its passphrase to import it.");
        }
        throw err;
      }
    },
    inspect: async (key) => (await call("inspect", { key: enc.encode(key) })) as unknown as KeyInfo,
    encrypt: async ({ text, encryptionKeys, signingKey }) =>
      (await call("encrypt", { data: enc.encode(text), encryptionKeys: encryptionKeys.map((k) => enc.encode(k)), signingKey: signingKey ? enc.encode(signingKey) : undefined })).message as string,
    signDetached: async (data, signingKey) => {
      const r = await call("signDetached", { data, signingKey: enc.encode(signingKey) });
      return { armoredSignature: r.signature as string, hashAlgorithm: r.hashAlgorithm as number };
    },
    decrypt: async ({ armoredMessage, decryptionKeys, verificationKeys, maxBytes }) => {
      const r = await call("decrypt", {
        message: enc.encode(armoredMessage),
        decryptionKeys: decryptionKeys.map((k) => enc.encode(k)),
        verificationKeys: verificationKeys.map((k) => enc.encode(k)),
        maxBytes
      });
      return { data: dec.decode(r.data as Uint8Array), signatures: r.signatures as SignatureCheck[] };
    },
    verifyDetached: async ({ data, armoredSignature, verificationKeys }) =>
      (await call("verifyDetached", { data, signature: enc.encode(armoredSignature), verificationKeys: verificationKeys.map((k) => enc.encode(k)) })).signatures as SignatureCheck[],
    armor: async (data, type) => (await call("armor", { data, type })).armored as string,
    dearmor: async (armored) => (await call("dearmor", { armored: enc.encode(armored) })).data as Uint8Array
  };
}
