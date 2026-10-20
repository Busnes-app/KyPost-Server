import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AgentAccess, agentConfigs } from "./AgentAccess";
import { createAgentToken, listAgentTokens, revokeAgentToken, setAgentTokenScope, type AgentToken } from "../../../api/agentTokens";
import { releaseSecretHold, secretHoldReason } from "../../../lib/secretHold";

const auth = vi.hoisted(() => ({ ssoSession: false }));
vi.mock("../../../auth", () => ({ useAuth: () => auth }));
vi.mock("../../../api/agentTokens", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../api/agentTokens")>()),
  listAgentTokens: vi.fn(),
  createAgentToken: vi.fn(),
  setAgentTokenScope: vi.fn(),
  revokeAgentToken: vi.fn()
}));

const token: AgentToken = {
  id: "abc",
  label: "Hermes",
  scope: "mcp:read",
  createdAt: "2026-10-20T00:00:00Z",
  lastUsedAt: null,
  revokeAfter: "2027-01-18T00:00:00Z"
};
const secret = "kp_0123456789abcdef_" + "a".repeat(64);

afterEach(() => {
  cleanup();
  releaseSecretHold();
});
beforeEach(() => {
  vi.clearAllMocks();
  auth.ssoSession = false;
  vi.mocked(listAgentTokens).mockResolvedValue([token]);
  vi.mocked(createAgentToken).mockResolvedValue({ token: secret, agentToken: token });
  vi.mocked(setAgentTokenScope).mockResolvedValue(token);
  vi.mocked(revokeAgentToken).mockResolvedValue(undefined);
});

describe("AgentAccess", () => {
  it("creates a read-only token by default behind the password, shows it once and holds navigation", async () => {
    const onRevealed = vi.fn();
    render(<AgentAccess onRevealedTokenChange={onRevealed} />);
    expect(await screen.findByText(/revoked if unused by/)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Create token" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Hermes" } });
    // The button names the agent and the access level it grants.
    const create = screen.getByRole("button", { name: "Create token for “Hermes” (Read only)" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);

    const password = screen.getByLabelText("Account password to create a token or change its access") as HTMLInputElement;
    fireEvent.change(password, { target: { value: "account credential" } });
    fireEvent.click(create);
    await waitFor(() => expect(createAgentToken).toHaveBeenCalledWith("Hermes", "mcp:read", { ssoSession: false, accountPassword: "account credential" }));

    expect(await screen.findByText(secret)).toBeTruthy();
    expect(screen.getByText("New token for “Hermes” — Read only")).toBeTruthy();
    expect(password.value).toBe("");
    expect(onRevealed).toHaveBeenLastCalledWith(true);
    expect(secretHoldReason()).toMatch(/AI agent token/);
    // The configs read the token from the environment; it is never pasted into them.
    for (const c of agentConfigs(window.location.origin)) {
      expect(c.text).not.toContain(secret);
    }
    expect(screen.getAllByText(/\$\{KYPOST_TOKEN\}/).length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("button", { name: "Done — I saved it" }));
    expect(screen.queryByText(secret)).toBeNull();
    expect(secretHoldReason()).toBe("");
    expect(onRevealed).toHaveBeenLastCalledWith(false);
  });

  it("warns before widening a token, and needs its own confirmation", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    render(<AgentAccess />);
    const access = (await screen.findByLabelText("Access for Hermes")) as HTMLSelectElement;
    expect(access.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Account password to create a token or change its access"), { target: { value: "pw" } });
    fireEvent.change(access, { target: { value: "mcp:organize" } });
    expect(confirm).toHaveBeenCalledWith(expect.stringMatching(/^Give the AI agent “Hermes” Read and organize access to your mail\. .*Anyone who can email you/));
    expect(setAgentTokenScope).not.toHaveBeenCalled();

    confirm.mockReturnValue(true);
    fireEvent.change(access, { target: { value: "mcp:organize" } });
    await waitFor(() => expect(setAgentTokenScope).toHaveBeenCalledWith(token, "mcp:organize", { ssoSession: false, accountPassword: "pw" }));
    confirm.mockRestore();
  });

  it("confirms with KySignOn for an SSO session, and revokes after a confirm", async () => {
    auth.ssoSession = true;
    vi.spyOn(window, "confirm").mockReturnValue(true);
    render(<AgentAccess />);
    expect(await screen.findByText("Hermes")).toBeTruthy();
    expect(screen.queryByLabelText("Account password to create a token or change its access")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeAgentToken).toHaveBeenCalledWith("abc"));
  });
});
