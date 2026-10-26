/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Expected build warning, deliberately not filtered: `Module "crypto" has been
// externalized for browser compatibility` from @hpke/common (algorithm.js,
// utils/misc.js). It reaches `import("crypto")` only when globalThis.crypto is
// undefined, its Node fallback; browsers always define it, so the empty stub
// Vite substitutes is never loaded. Re-check if @hpke changes that guard.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173
  },
  test: {
    // jsdom, not node: the email sanitizer under test uses DOMParser and
    // DOMPurify, both of which need a real DOM.
    environment: "jsdom",
    setupFiles: ["src/test/setup.ts"],
    // Component tests run past the 5s default when every worker is busy.
    testTimeout: 15000,
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"]
  }
});
