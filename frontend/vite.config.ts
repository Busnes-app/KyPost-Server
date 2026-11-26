/// <reference types="vitest" />
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { createReadStream, existsSync } from "node:fs";
import { resolve } from "node:path";

// Dev-only gopenpgp wasm engine (lib/pgpEngine.ts). The dev server serves
// scripts/build-pgp-wasm.sh output at /pgp-wasm/; builds never include it.
const wasmEngine = process.env.VITE_PGP_ENGINE === "gopenpgp-wasm";
const wasmDir = resolve(import.meta.dirname, ".pgp-wasm");
const serveWasm: Plugin = {
  name: "kypost-pgp-wasm",
  apply: "serve",
  configureServer(server) {
    server.middlewares.use("/pgp-wasm/", (req, res, next) => {
      const name = { "/kypost-pgp.wasm": "kypost-pgp.wasm", "/wasm_exec.js": "wasm_exec.js" }[req.url ?? ""];
      if (!name || !existsSync(resolve(wasmDir, name))) return next();
      res.setHeader("Content-Type", name.endsWith(".wasm") ? "application/wasm" : "text/javascript");
      createReadStream(resolve(wasmDir, name)).pipe(res);
    });
  }
};

// Expected build warning, deliberately not filtered: `Module "crypto" has been
// externalized for browser compatibility` from @hpke/common (algorithm.js,
// utils/misc.js). It reaches `import("crypto")` only when globalThis.crypto is
// undefined, its Node fallback; browsers always define it, so the empty stub
// Vite substitutes is never loaded. Re-check if @hpke changes that guard.
export default defineConfig({
  plugins: [react(), serveWasm],
  server: {
    port: 5173
  },
  test: {
    // jsdom, not node: the email sanitizer under test uses DOMParser and
    // DOMPurify, both of which need a real DOM.
    environment: "jsdom",
    setupFiles: ["src/test/setup.ts", ...(wasmEngine ? ["src/test/pgpWasmSetup.ts"] : [])],
    // Component tests run past the 5s default when every worker is busy.
    testTimeout: 15000,
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"]
  }
});
