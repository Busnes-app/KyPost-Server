import { beforeEach, describe, expect, it, vi } from "vitest";
import { createUser } from "./users";

const postJSON = vi.fn();
const withSSOStepUp = vi.fn((run: (headers: Record<string, string>) => Promise<unknown>) => run({}));

vi.mock("./client", () => ({ postJSON: (...args: unknown[]) => postJSON(...args) }));
vi.mock("./stepup", () => ({ withSSOStepUp: (run: (h: Record<string, string>) => Promise<unknown>) => withSSOStepUp(run) }));
vi.mock("./auth", () => ({
  deriveCredential: async () => ({ authSecret: "derived" }),
  credentialFields: (c: { authSecret: string }, prefix: string) => ({ [`${prefix}AuthSecret`]: c.authSecret })
}));

beforeEach(() => {
  postJSON.mockReset();
  postJSON.mockResolvedValue({});
  withSSOStepUp.mockClear();
});

describe("confirmed user-management changes", () => {
  it("adds the admin's derived credential beside the new user's password, through the step-up helper", async () => {
    await createUser({ ssoSession: false, accountPassword: "admin-pw" }, "new", "temp-pw", "user");
    expect(withSSOStepUp).toHaveBeenCalledOnce();
    expect(postJSON).toHaveBeenCalledWith("/api/users", { username: "new", password: "temp-pw", role: "user", adminAuthSecret: "derived" }, {});
  });

  it("sends no credential for a KySignOn session", async () => {
    await createUser({ ssoSession: true, accountPassword: "" }, "new", "temp-pw", "user");
    expect(postJSON).toHaveBeenCalledWith("/api/users", { username: "new", password: "temp-pw", role: "user" }, {});
  });
});
