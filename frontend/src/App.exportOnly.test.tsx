import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";

// Hosted mode, while the account's organisation is scheduled for deletion: an export-only
// session sees only the export page, and the app asks the server for nothing but the export.

const { getJSON } = vi.hoisted(() => ({ getJSON: vi.fn() }));
vi.mock("./api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  postJSON: async () => ({}),
  putJSON: async () => ({}),
  deleteJSON: async () => ({}),
  HttpError: class HttpError extends Error {
    status = 0;
    body: unknown = null;
  },
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback),
  selectMailbox: () => {},
  selectedMailboxId: () => "",
  onMailboxGone: () => {},
  mailboxHeaders: () => ({})
}));

import { App } from "./App";

afterEach(() => {
  cleanup();
  getJSON.mockReset();
});

describe("App for an export-only session", () => {
  it("shows only the export, and requests only what the server admits", async () => {
    getJSON.mockImplementation(async (url: string) => {
      if (url === "/api/auth/me") return { authenticated: true, userId: "u1", role: "user", ssoSession: true, exportOnly: true };
      if (url === "/api/mailboxes") return { mailboxes: [{ id: "u1", addresses: [{ address: "alice@example.test" }] }] };
      if (url === "/api/export/folders") return { folders: ["INBOX"] };
      throw new Error("unexpected request " + url);
    });
    render(
      <MemoryRouter initialEntries={["/settings/account?tab=export"]}>
        <App />
      </MemoryRouter>
    );
    expect(await screen.findByText("This account is being deleted")).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Export mail" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sign out" })).toBeTruthy();
    expect(screen.queryByRole("navigation")).toBeNull();
    const asked = new Set(getJSON.mock.calls.map(([u]) => u as string));
    expect([...asked].sort()).toEqual(["/api/auth/me", "/api/export/folders", "/api/mailboxes"]);
  });
});
