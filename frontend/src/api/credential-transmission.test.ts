// @vitest-environment node
import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

// The account password never leaves the browser: every flow sends the derived
// authSecret through credentialFields (api/auth.ts), and the server accepts
// nothing else. This is a source scan because the property is about the whole
// tree, not one call site — a hand-built `password` field is how the last leak
// arrived.
function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) {
      out.push(...sourceFiles(path));
    } else if (/\.tsx?$/.test(entry) && !/\.test\.tsx?$/.test(entry)) {
      out.push(path);
    }
  }
  return out;
}

// Files that legitimately send a password that is NOT the account password.
const allowed: Record<string, string> = {
  "api/users.ts": "admin create/reset: the new account's temporary password, stored derived server-side",
  "admin/sections/AssignMailbox.tsx": "the external IMAP/CardDAV server's password",
  "settings/sections/EmailServer.tsx": "the external IMAP server's password",
  "settings/sections/CardDavClient.tsx": "the external CardDAV server's password",
  "settings/sections/MailImportAccount.tsx": "the other mail account's password"
};

// An object key named password/oldPassword/newPassword/adminPassword with a
// value (`password: x`), or shorthand (`{ password }`) inside a request call.
// Type annotations are excluded.
const explicitKey = /(?:^|[^\w.$"'])(?:old|new|admin)?[Pp]assword\s*:\s*(?!string\b|number\b|boolean\b)[^\s$]/;
const shorthandKey = /[{,]\s*(?:old|new|admin)?[Pp]assword\s*[,}]/;
const requestCall = /\b(?:postJSON|putJSON|deleteJSON|postBlob|fetch)\b/;
const comment = /^\s*(?:\/\/|\*|\/\*)/;
const sendsPassword = (line: string) =>
  !comment.test(line) && (explicitKey.test(line) || (requestCall.test(line) && shorthandKey.test(line)));

describe("the account password is never put in a request body", () => {
  it("has no password field outside the allowlist", () => {
    const root = join(__dirname, "..");
    const offenders: string[] = [];
    for (const file of sourceFiles(root)) {
      const rel = file.slice(root.length + 1).split("\\").join("/");
      if (rel in allowed) continue;
      readFileSync(file, "utf8")
        .split("\n")
        .forEach((line, i) => {
          if (sendsPassword(line)) offenders.push(`${rel}:${i + 1}: ${line.trim()}`);
        });
    }
    expect(offenders).toEqual([]);
  });

  it("scans the shapes it claims to", () => {
    for (const line of [
      "  password: currentPassword,",
      'postJSON("/api/x", { username, password })',
      "body: { oldPassword: x }",
      "adminPassword: p"
    ]) {
      expect(sendsPassword(line), line).toBe(true);
    }
    for (const line of ["  password: string;", "const [password, setPassword] = useState('')", "value={password}", "unlock(envelope, password)", " * password: prose", "`CardDAV password: ${e}`"]) {
      expect(sendsPassword(line), line).toBe(false);
    }
  });
});
