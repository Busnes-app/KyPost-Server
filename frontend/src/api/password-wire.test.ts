import { beforeEach, describe, expect, it, vi } from "vitest";

// Behavioural half of credential-transmission.test.ts: every api function that
// takes the account password, run against a recording transport. The
// plaintext must never appear in anything sent; the derived secret must.
const { sent, record } = vi.hoisted(() => {
  const sent: unknown[] = [];
  const record = (_path: string, body?: unknown) => {
    sent.push(body);
    return Promise.resolve({ ok: true, pgpRevision: 2 });
  };
  return { sent, record };
});
vi.mock("./client", () => ({
  getJSON: () => Promise.resolve({ salt: "c2FsdHNhbHRzYWx0c2FsdA==", iterations: 600_000 }),
  postJSON: record,
  putJSON: record,
  deleteJSON: record
}));
vi.mock("./stepup", () => ({ withSSOStepUp: (run: (h: Record<string, string>) => unknown) => run({}) }));
vi.mock("../lib/authSecret", () => ({ deriveAuthSecret: () => Promise.resolve("derived-secret") }));

import { reauthenticate } from "./auth";
import {
  deleteDeviceEnvelope,
  deletePGPIdentity,
  exportLegacyPGPKey,
  rewrapPGPKeyring,
  rewrapPGPPrivateKey,
  setIncomingEncryption,
  storeClientPGPIdentity
} from "./pgp";
import { deleteUserIMAPConfig, setUserRole, userAction } from "./users";

const PLAINTEXT = "the-account-plaintext-password";
const confirm = { ssoSession: false, accountPassword: PLAINTEXT };

describe("the account password never reaches the wire", () => {
  beforeEach(() => {
    sent.length = 0;
  });

  it.each([
    ["step-up", () => reauthenticate(PLAINTEXT, "123456")],
    ["delete identity", () => deletePGPIdentity(PLAINTEXT, 1)],
    ["store identity", () => storeClientPGPIdentity("pub", "{}", "generated", PLAINTEXT, 1)],
    ["rewrap", () => rewrapPGPPrivateKey("{}", PLAINTEXT, "FPR", 1)],
    ["rewrap keyring", () => rewrapPGPKeyring({ wrapped: "{}", password: PLAINTEXT, expectedFingerprint: "FPR", expectedRevision: 1, isCurrent: () => true })],
    ["export legacy key", () => exportLegacyPGPKey(PLAINTEXT)],
    ["delete device envelope", () => deleteDeviceEnvelope("dev", PLAINTEXT, 1)],
    ["incoming encryption", () => setIncomingEncryption(false, PLAINTEXT, null, false)],
    ["admin role change", () => setUserRole(confirm, "id", "user")],
    ["admin action", () => userAction(confirm, "id", "deactivate")],
    ["admin mailbox delete", () => deleteUserIMAPConfig(confirm, "id")]
  ])("%s", async (_name, run) => {
    await run();
    const wire = JSON.stringify(sent);
    expect(sent.length).toBeGreaterThan(0);
    expect(wire).not.toContain(PLAINTEXT);
    expect(wire).toContain("derived-secret");
    expect(wire).not.toMatch(/"(old|new|admin)?[Pp]assword"/);
  });
});
