/// <reference types="vitest" />
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { brotliCompressSync, constants, gzipSync } from "node:zlib";

// The OpenPGP engine is ~9 MB of WebAssembly, and the server sends static
// files as they are on disk. Emit .br and .gz beside it; handleFrontend
// (backend/internal/api/server.go) serves the one the browser accepts.
const precompressWasm: Plugin = {
  name: "kypost-precompress-wasm",
  apply: "build",
  generateBundle(_options, bundle) {
    for (const asset of Object.values(bundle)) {
      if (asset.type !== "asset" || !asset.fileName.endsWith(".wasm")) continue;
      const source = Buffer.from(asset.source);
      this.emitFile({ type: "asset", fileName: `${asset.fileName}.br`, source: brotliCompressSync(source, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } }) });
      this.emitFile({ type: "asset", fileName: `${asset.fileName}.gz`, source: gzipSync(source, { level: 9 }) });
    }
  }
};

export default defineConfig({
  plugins: [react(), precompressWasm],
  server: {
    port: 5173
  },
  test: {
    // jsdom, not node: the email sanitizer under test uses DOMParser and
    // DOMPurify, both of which need a real DOM.
    environment: "jsdom",
    setupFiles: ["src/test/setup.ts", "src/test/pgpWasmSetup.ts"],
    // Component tests run past the 5s default when every worker is busy.
    testTimeout: 15000,
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"]
  }
});
