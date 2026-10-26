import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createPrivateKey, generateKeyPairSync } from "node:crypto";
import { once } from "node:events";
import { copyFile, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { stripTypeScriptTypes } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { Readable } from "node:stream";
import { test } from "node:test";
import workerd from "workerd";

// Run the actual provider function in the lockfile's native runtime. Node alone
// does not reproduce workerd's byte-rounded RSA algorithm.modulusLength.
test("native Worker refuses literal short RSA keys and retains valid signing/cache", { timeout: 30_000 }, async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "kypost-fcm-"));
  try {
    const modules = [];
    for (const name of ["worker/src/fcm", "push-relay-shared/base64url", "push-relay-shared/push-relay-common", "push-relay-shared/token-cache"]) {
      const source = await readFile(new URL(name + ".ts", new URL("../../", import.meta.url)), "utf8");
      const file = "module-" + modules.length + ".js";
      await writeFile(join(dir, file), stripTypeScriptTypes(source));
      modules.push(`(name=${JSON.stringify(name)},esModule=embed ${JSON.stringify(file)})`);
    }
    await copyFile(new URL("../node_modules/workerd/workerd.capnp", import.meta.url), join(dir, "workerd.capnp"));
    await writeFile(join(dir, "main.js"), `
      import {sendFcmMessage} from './worker/src/fcm';
      export default {async fetch(request,env) {
        let puts=0;
        const cache={get:async()=>env.CACHED||null,put:async()=>{puts++},delete:async()=>{}};
        try {
          const result=await sendFcmMessage({clientEmail:'fixture@example.invalid',privateKeyPem:env.KEY,projectId:'fixture'},cache,{token:'fixture-device',title:'fixture',body:'fixture'});
          const stats=await (await env.PROVIDER.fetch("https://fixture.invalid/stats")).json();
          return Response.json({result,puts,...stats});
        } catch(e) {
          const stats=await (await env.PROVIDER.fetch("https://fixture.invalid/stats")).json();
          return Response.json({error:String(e),puts,...stats});
        }
      }};
    `);
    await writeFile(join(dir, "provider.js"), `
      const decode=s=>Uint8Array.from(atob(s.replace(/-/g,'+').replace(/_/g,'/')),c=>c.charCodeAt(0));
      let oauthRequests=0,sends=0;
      export default {async fetch(request,env) {
        if(request.url==="https://fixture.invalid/stats") return Response.json({oauthRequests,sends});
        if(request.url==='https://oauth2.googleapis.com/token') {
          oauthRequests++;
          const form=await request.formData();
          const assertion=form.get('assertion');
          if(typeof assertion!=='string') return new Response('',{status:400});
          const [header,claims,signature]=assertion.split('.');
          const key=await crypto.subtle.importKey('spki',decode(env.PUBLIC.replace(/-----[^-]+-----|\\s/g,'')),{name:'RSASSA-PKCS1-v1_5',hash:'SHA-256'},false,['verify']);
          const valid=await crypto.subtle.verify('RSASSA-PKCS1-v1_5',key,decode(signature),new TextEncoder().encode(header+'.'+claims));
          if(!valid) return new Response('',{status:403});
          return Response.json({access_token:'fixture-access-token',expires_in:3600});
        }
        if(request.url!=='https://fcm.googleapis.com/v1/projects/fixture/messages:send'||request.headers.get('Authorization')!=='Bearer fixture-access-token') return new Response('',{status:403});
        sends++;
        return Response.json({name:'fixture-message'});
      }};
    `);
    const template = await readFile(new URL("../wrangler.toml.example", import.meta.url), "utf8");
    const date = /^compatibility_date\s*=\s*"([\d-]+)"/m.exec(template)?.[1];
    assert.ok(date, "the shipped Worker must declare its compatibility date");
    // The package API returns the native executable. Its CLI shim does not
    // forward the additional control descriptor when install scripts are off.
    const binary = workerd.default;
    for (const bits of [1024, 2040, 2041, 2047, 2048, 3072]) {
      const pair = generateKeyPairSync("rsa", {
        modulusLength: bits,
        privateKeyEncoding: { type: "pkcs8", format: "pem" },
        publicKeyEncoding: { type: "spki", format: "pem" },
      });
      assert.equal(createPrivateKey(pair.privateKey).asymmetricKeyDetails?.modulusLength, bits);
      await writeFile(join(dir, "key.pem"), pair.privateKey, { mode: 0o600 });
      await writeFile(join(dir, "public.pem"), pair.publicKey);
      for (const cached of bits === 1024 ? [false, true] : [false]) {
        await t.test(`${bits} bits, cached=${cached}`, async () => {
          // An explicit internet service suppresses implicit external networking
          // and system CA loading; every provider request stays in this fixture.
          await writeFile(join(dir, "config.capnp"), `
            using Workerd=import "workerd.capnp";
            const config :Workerd.Config=(services=[
              (name="main",worker=(modules=[(name="main.js",esModule=embed "main.js"),${modules.join(",")}],compatibilityDate=${JSON.stringify(date)},bindings=[(name="PROVIDER",service="internet"),(name="KEY",text=embed "key.pem"),(name="CACHED",text=${JSON.stringify(cached ? "fixture-access-token" : "")})],globalOutbound="internet")),
              (name="internet",worker=(modules=[(name="provider.js",esModule=embed "provider.js")],compatibilityDate=${JSON.stringify(date)},bindings=[(name="PUBLIC",text=embed "public.pem")]))
            ],sockets=[(name="http",address="127.0.0.1:0",http=(),service="main")]);
          `);
          const child = spawn(binary, ["serve", join(dir, "config.capnp"), "--control-fd=3"], {
            cwd: dir, stdio: ["ignore", "ignore", "pipe", "pipe"],
          });
          const stopped = once(child, "exit").catch(() => undefined);
          let diagnostics = "";
          child.stderr?.on("data", chunk => { diagnostics = (diagnostics + String(chunk)).slice(-8192); });
          try {
            const control = child.stdio[3];
            assert.ok(control instanceof Readable);
            const lines = createInterface({ input: control });
            let event: unknown;
            try {
              const [line] = await once(lines, "line", { signal: AbortSignal.timeout(10_000) }).catch(error => {
                throw new Error("native workerd did not start: " + diagnostics, { cause: error });
              });
              event = JSON.parse(line);
            } finally { lines.close(); }
            assert.ok(typeof event === "object" && event !== null && "port" in event && typeof event.port === "number", diagnostics);
            const response = await fetch(`http://127.0.0.1:${event.port}/`, { signal: AbortSignal.timeout(5000) });
            const result: unknown = await response.json();
            assert.ok(typeof result === "object" && result !== null && "puts" in result && typeof result.puts === "number", diagnostics);
            if (bits < 2048 && !cached) {
              assert.ok("error" in result && typeof result.error === "string", `${bits}-bit signing key was accepted`);
              assert.match(result.error, /RSA.*2048/);
              assert.equal(result.puts, 0, "refused key reached the token cache");
              assert.ok("oauthRequests" in result && result.oauthRequests === 0, "refused key reached OAuth");
              assert.ok("sends" in result && result.sends === 0, "refused key reached FCM");
            } else {
              assert.ok("result" in result, diagnostics);
              assert.deepEqual(result.result, { ok: true }, diagnostics);
              assert.equal(result.puts, cached ? 0 : 1);
              assert.ok("oauthRequests" in result && result.oauthRequests === (cached ? 0 : 1));
              assert.ok("sends" in result && result.sends === 1);
            }
          } finally {
            child.kill();
            await stopped;
          }
        });
      }
    }
  } finally { await rm(dir, { recursive: true, force: true }); }
});
