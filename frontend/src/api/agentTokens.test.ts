import { afterEach, describe, expect, it, vi } from "vitest";
import { createAgentToken, setAgentTokenScope } from "./agentTokens";

const sent = vi.hoisted(() => ({ purposes: [] as (string | undefined)[] }));
vi.mock("./stepup", () => ({
  withSSOStepUp: (run: (h: Record<string, string>) => Promise<unknown>, purpose?: string) => {
    sent.purposes.push(purpose);
    return run({});
  }
}));
vi.mock("./client", () => ({ postJSON: vi.fn(async () => ({})), putJSON: vi.fn(async () => ({})), getJSON: vi.fn(), deleteJSON: vi.fn() }));

afterEach(() => {
  sent.purposes = [];
});

// A KySignOn confirmation for a token names the agent and the access level, never "an app wants access".
describe("agent token step-up", () => {
  it("names the agent and access level in every confirmation", async () => {
    const confirm = { ssoSession: true, accountPassword: "" };
    await createAgentToken("Hermes", "mcp:read", confirm);
    await setAgentTokenScope({ id: "a", label: "OpenClaw", scope: "mcp:read", createdAt: "", lastUsedAt: null, revokeAfter: "" }, "mcp:organize", confirm);
    expect(sent.purposes).toEqual([
      "Give the AI agent “Hermes” Read only access to your mail.",
      "Give the AI agent “OpenClaw” Read and organize access to your mail."
    ]);
  });
});
