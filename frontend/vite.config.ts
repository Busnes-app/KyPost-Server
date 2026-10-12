/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

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
