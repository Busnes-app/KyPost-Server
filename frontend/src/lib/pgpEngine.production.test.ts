// @vitest-environment node
import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";

// gopenpgp WebAssembly is the browser's only OpenPGP engine. openpgp.js is a
// dev dependency for tests; a production import would ship a second parser
// that could disagree with the server's (docs/E2E_PGP.md, signer binding).
function productionFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((entry) => {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) return entry === "test" ? [] : productionFiles(path);
    return /\.tsx?$/.test(entry) && !/\.test\.tsx?$/.test(entry) ? [path] : [];
  });
}

describe("production OpenPGP engine", () => {
  it("no production source imports openpgp.js", () => {
    const root = join(import.meta.dirname, "..");
    const offenders = productionFiles(root).filter((file) =>
      /(?:from\s*|import\s*\(\s*)["']openpgp["']/.test(readFileSync(file, "utf8")));
    expect(offenders.map((file) => relative(root, file))).toEqual([]);
  });

  it("openpgp is not a runtime dependency", () => {
    const pkg = JSON.parse(readFileSync(join(import.meta.dirname, "../../package.json"), "utf8"));
    expect(Object.keys(pkg.dependencies)).not.toContain("openpgp");
  });
});
