import { beforeEach, describe, expect, it, vi } from "vitest";

const { getJSON, postJSON, deriveAuthSecret } = vi.hoisted(() => ({
  getJSON: vi.fn(),
  postJSON: vi.fn(),
  deriveAuthSecret: vi.fn()
}));

vi.mock("./client", () => ({ getJSON, postJSON }));
vi.mock("../lib/authSecret", () => ({
  defaultIterations: () => 600_000,
  deriveAuthSecret
}));

import { credentialFields, deriveCredential, reauthenticate } from "./auth";

describe("credential serialization", () => {
  beforeEach(() => {
    getJSON.mockReset();
    postJSON.mockReset();
    deriveAuthSecret.mockReset();
  });

  it("step-up sends only the derived secret", async () => {
    getJSON.mockResolvedValue({ salt: "login-salt", iterations: 600_000 });
    deriveAuthSecret.mockResolvedValue("derived-secret");

    await reauthenticate("plaintext-password", "123456");

    expect(postJSON).toHaveBeenCalledWith("/api/auth/step-up", {
      authSecret: "derived-secret",
      code: "123456"
    });
  });

  it("sign-in never puts the password on the wire", async () => {
    // A legacy or unknown account gets a synthetic salt; the client cannot tell
    // and does not need to.
    getJSON.mockResolvedValue({ salt: "c2FsdHNhbHRzYWx0c2FsdA==", iterations: 600_000 });
    deriveAuthSecret.mockResolvedValue("derived-secret");

    const credential = await deriveCredential("someone", "plaintext-password");

    expect(credentialFields(credential)).toEqual({ authSecret: "derived-secret" });
    expect(credentialFields(credential, "old")).toEqual({ oldAuthSecret: "derived-secret" });
    expect(credentialFields(credential, "admin")).toEqual({ adminAuthSecret: "derived-secret" });
    expect(JSON.stringify(credential)).not.toContain("plaintext-password");
  });

  it("does not post credentials when the handshake fails", async () => {
    getJSON.mockRejectedValue(new Error("offline"));

    await expect(reauthenticate("plaintext-password", "123456")).rejects.toThrow("offline");
    expect(postJSON).not.toHaveBeenCalled();
  });
});
