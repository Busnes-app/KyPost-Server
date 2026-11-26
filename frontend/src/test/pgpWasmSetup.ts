// Starts the gopenpgp wasm from disk for vitest (Node has no dev server).
// Missing artifacts fail here, loudly: run scripts/build-pgp-wasm.sh.
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

if (!globalThis.kypostPGP) {
  const dir = resolve(import.meta.dirname, "../../.pgp-wasm");
  await import(/* @vite-ignore */ pathToFileURL(resolve(dir, "wasm_exec.js")).href);
  const go = new globalThis.Go!();
  const { instance } = await WebAssembly.instantiate(await readFile(resolve(dir, "kypost-pgp.wasm")), go.importObject);
  void go.run(instance);
}
