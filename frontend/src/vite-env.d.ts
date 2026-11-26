/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Dev-only OpenPGP engine switch; see lib/pgpEngine.ts. */
  readonly VITE_PGP_ENGINE?: "openpgp.js" | "gopenpgp-wasm";
}
