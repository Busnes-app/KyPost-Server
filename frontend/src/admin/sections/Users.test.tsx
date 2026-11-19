import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Users } from "./Users";

vi.mock("../../auth", () => ({ useAuth: () => ({ authenticated: true, userId: "admin", ssoSession: false }) }));
vi.mock("./AssignMailbox", () => ({ AssignMailbox: () => null }));
vi.mock("../../api/users", () => ({
  listUsers: vi.fn().mockResolvedValue([]),
  createUser: vi.fn().mockResolvedValue({ id: "u2", username: "opti" }),
  resetUserPassword: vi.fn(),
  setUserRole: vi.fn(),
  setUserKeySetupRequired: vi.fn(),
  setUserSendQuota: vi.fn(),
  userAction: vi.fn()
}));

afterEach(cleanup);

it("shows a created user as a success, not an error", async () => {
  render(<Users />);
  await userEvent.type(screen.getByLabelText("Your account password"), "admin-pw");
  await userEvent.type(screen.getByLabelText("Username"), "opti");
  await userEvent.type(screen.getByLabelText("Temporary Password"), "temp-pw");
  await userEvent.click(screen.getByRole("button", { name: "Create User" }));
  const notice = await screen.findByText(/User opti created/);
  expect(notice.className).toBe("notice notice-success");
});

it("draws success notices in the success colour, not the accent errors share", async () => {
  const { readFileSync } = await import("node:fs");
  const css = readFileSync("src/styles.css", "utf8");
  const rule = /\.notice-success \{([^}]*)\}/.exec(css)?.[1] ?? "";
  expect(rule).toContain("--ky-success");
  expect(rule).not.toContain("--accent");
});
