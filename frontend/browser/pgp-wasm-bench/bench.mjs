// Evaluation benchmark: gopenpgp wasm vs openpgp.js 6.3.3 in real Chromium.
// Usage (from frontend/, after scripts/build-pgp-wasm.sh):
//   flock ~/.cache/ky-browser-5391.lock node browser/pgp-wasm-bench/bench.mjs
// Serves on 127.0.0.1:5391 with KyPost's production CSP (security_headers.go,
// no CAPTCHA provider), with and without 'wasm-unsafe-eval'.
import { createServer } from "node:http";
import { readFileSync, statSync } from "node:fs";
import { gzipSync, brotliCompressSync, constants } from "node:zlib";
import { chromium } from "@playwright/test";

const here = new URL(".", import.meta.url);
const wasmDir = new URL("../../.pgp-wasm/", here);
const files = {
  "/": [new URL("index.html", here), "text/html"],
  "/page.mjs": [new URL("page.mjs", here), "text/javascript"],
  "/wasm_exec.js": [new URL("wasm_exec.js", wasmDir), "text/javascript"],
  "/kypost-pgp.wasm": [new URL("kypost-pgp.wasm", wasmDir), "application/wasm"],
  "/openpgp.min.mjs": [new URL("../../node_modules/openpgp/dist/openpgp.min.mjs", here), "text/javascript"]
};
const BASE_CSP = (script) => `default-src 'self'; script-src ${script}; style-src 'self' 'unsafe-inline'; img-src 'self' data: https: http:; media-src 'self' data: https: http:; font-src 'self' data:; connect-src 'self'; worker-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'`;
let csp = BASE_CSP("'self'");

const server = createServer((req, res) => {
  const f = files[req.url];
  if (!f) { res.statusCode = 404; return res.end(); }
  res.setHeader("Content-Type", f[1]);
  res.setHeader("Content-Security-Policy", csp);
  res.end(readFileSync(f[0]));
}).listen(5391, "127.0.0.1");

const wasm = readFileSync(files["/kypost-pgp.wasm"][0]);
const js = readFileSync(files["/openpgp.min.mjs"][0]);
const size = (b) => ({ raw: b.length, gzip: gzipSync(b, { level: 9 }).length, brotli: brotliCompressSync(b, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } }).length });
const result = { goVersion: readFileSync(new URL("GOVERSION", wasmDir), "utf8").trim(), sizes: { wasm: size(wasm), openpgpMinMjs: size(js), wasmExecJs: statSync(files["/wasm_exec.js"][0]).size } };

const browser = await chromium.launch();
result.chromium = browser.version();
async function inPage(engine, n, cpuRate = 1) {
  console.error(`bench: ${engine} n=${n} cpu=${cpuRate}x`);
  const context = await browser.newContext(); // fresh cache: a cold load
  const page = await context.newPage();
  const cdp = await context.newCDPSession(page);
  if (cpuRate > 1) await cdp.send("Emulation.setCPUThrottlingRate", { rate: cpuRate });
  await cdp.send("Performance.enable");
  const errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  await page.goto("http://127.0.0.1:5391/");
  try {
    const out = await page.evaluate(async ([e, k]) => (await import("/page.mjs")).run(e, k), [engine, n]);
    const metrics = Object.fromEntries((await cdp.send("Performance.getMetrics")).metrics.map((m) => [m.name, m.value]));
    out.jsHeapUsedBytes = metrics.JSHeapUsedSize;
    out.jsHeapTotalBytes = metrics.JSHeapTotalSize;
    return out;
  } catch (err) {
    return { engine, error: String(err).split("\n")[0], console: errors };
  } finally {
    await context.close();
  }
}

result.cspWithout = await inPage("wasm", 1);
csp = BASE_CSP("'self' 'wasm-unsafe-eval'");
result.csp = csp;
result.desktop = { wasm: await inPage("wasm", 15), openpgp: await inPage("openpgp", 15) };
// Android Chrome-class approximation: 4x CPU throttle (memory is reported, not capped).
result.cpu4x = { wasm: await inPage("wasm", 5, 4), openpgp: await inPage("openpgp", 5, 4) };
await browser.close();
server.close();
console.log(JSON.stringify(result, null, 2));
