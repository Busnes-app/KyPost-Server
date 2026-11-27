import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { PGPKeyInfo, validatePGPKey } from "./PGPKeyInfo";

afterEach(cleanup);
it.each(["v4", "v6", "v6-pqc"])("parses and displays the complete %s contact fingerprint", async name => {
  const key = readFileSync(resolve(import.meta.dirname, "../../../../testdata/openpgp-v6", `${name}.pub.asc`), "utf8");
  const info = await validatePGPKey(key);
  if (!info || "error" in info) throw new Error("fixture did not parse");
  expect(info.fingerprint).toHaveLength(name === "v4" ? 40 : 64);
  render(<PGPKeyInfo armoredKey={key} />);
  expect(await screen.findByText(`Fingerprint: ${info.fingerprint}`)).toBeTruthy();
});
