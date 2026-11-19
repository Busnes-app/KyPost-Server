import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// AC6: when the session lookup gets no answer, a 5xx or a 429, the shell says KyPost is
// unavailable and offers Try again, never the sign-in form; a real signed-out answer still
// renders sign-in.

const { HttpError, getJSON } = vi.hoisted(() => {
  class HttpError extends Error {
    status: number;
    body: unknown = null;
    constructor(status: number) {
      super(`request failed: ${status}`);
      this.status = status;
    }
  }
  return { HttpError, getJSON: vi.fn() };
});
vi.mock("./api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  postJSON: async () => ({}),
  putJSON: async () => ({}),
  deleteJSON: async () => ({}),
  HttpError,
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback),
  selectMailbox: () => {},
  selectedMailboxId: () => "",
  onMailboxGone: () => {},
  mailboxHeaders: () => ({})
}));

import { App } from "./App";

function renderApp() {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <App />
    </MemoryRouter>
  );
}

afterEach(() => {
  cleanup();
  getJSON.mockReset();
});

describe("App when KyPost is unavailable", () => {
  for (const [name, failure] of [
    ["a 503", new HttpError(503)],
    ["a 429", new HttpError(429)],
    ["no answer", new TypeError("Failed to fetch")]
  ] as const) {
    it(`shows the unavailable state for ${name}, and Try again re-requests`, async () => {
      getJSON.mockImplementation(async (url: string) => {
        if (url === "/api/auth/me") throw failure;
        return {};
      });
      renderApp();
      expect(await screen.findByText("KyPost is temporarily unavailable")).toBeTruthy();
      expect(screen.getByText(/You have not been signed out/)).toBeTruthy();
      expect(screen.queryByLabelText(/password/i)).toBeNull();
      expect(screen.queryByRole("button", { name: /sign in/i })).toBeNull();
      const calls = getJSON.mock.calls.filter(([u]) => u === "/api/auth/me").length;
      const retry = screen.getByRole("button", { name: "Try again" });
      await userEvent.click(retry);
      await waitFor(() => expect(getJSON.mock.calls.filter(([u]) => u === "/api/auth/me").length).toBe(calls + 1));
      expect(retry.hasAttribute("disabled")).toBe(false);
    });
  }

  it("renders sign-in for a real signed-out answer, and after a successful retry", async () => {
    let down = true;
    getJSON.mockImplementation(async (url: string) => {
      if (url === "/api/auth/me") {
        if (down) throw new HttpError(503);
        return { authenticated: false };
      }
      return {};
    });
    renderApp();
    await screen.findByText("KyPost is temporarily unavailable");
    down = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(screen.queryByText("KyPost is temporarily unavailable")).toBeNull());
    expect(await screen.findByRole("button", { name: /sign in/i })).toBeTruthy();
  });

  it("treats a proxy's 401 as signed out", async () => {
    getJSON.mockImplementation(async (url: string) => {
      if (url === "/api/auth/me") throw new HttpError(401);
      return {};
    });
    renderApp();
    expect(await screen.findByRole("button", { name: /sign in/i })).toBeTruthy();
    expect(screen.queryByText("KyPost is temporarily unavailable")).toBeNull();
  });
});
