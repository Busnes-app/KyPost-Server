// The phone navigation strip's "Settings" button must actually reveal the
// settings links. On a phone the sidebar is a short scrolling strip and the
// links render at its bottom, so tapping the button looked like it did nothing
// (#456). Opening them must scroll the group into view and mark the control
// expanded.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { App } from "./App";

vi.mock("./lib/pgpSession", () => ({
  subscribePGPSession: () => () => {},
  loadPGPSession: async () => ({ bootstrap: null, unlocked: false }),
  clearPGPSession: () => {},
  isClientProtected: () => false,
  pgpCustody: () => "other",
  needsUnlock: () => false,
  accountAddress: () => "",
}));

function json(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => "application/json" },
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response;
}

let originalScrollIntoView: typeof Element.prototype.scrollIntoView;

beforeEach(() => {
  originalScrollIntoView = Element.prototype.scrollIntoView;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url === "/api/auth/me") return json(200, { authenticated: true, userId: "u1", username: "gwen", role: "user" });
    if (url === "/api/mailboxes") return json(200, { mailboxes: [] });
    if (url.startsWith("/api/inbox/folders")) return json(200, { folders: [] });
    if (url.startsWith("/api/inbox?")) {
      return json(200, {
        tabs: ["Primary"],
        byTab: { Primary: [{ messageId: "m1", sender: "a@b.test", subject: "Primary inbox", status: "read", atUtc: "2026-10-01T10:00:00Z" }] },
        cursor: 1,
      });
    }
    return json(200, {});
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Element.prototype.scrollIntoView = originalScrollIntoView;
  window.sessionStorage.clear();
});

describe("Settings and Appearance in the nav", () => {
  it("are direct links, and Settings opens a page whose sections sit inside the content", async () => {
    const user = userEvent.setup();

    render(<MemoryRouter initialEntries={["/read"]}><App /></MemoryRouter>);
    await screen.findByText("Primary inbox", {}, { timeout: 5000 });

    const nav = screen.getByRole("navigation", { name: "Primary" });
    expect(nav.closest(".ky-sidebar")).toBeTruthy();
    expect(within(nav).getByRole("link", { name: "Appearance" }).getAttribute("href")).toBe("/settings/appearance");
    const settings = within(nav).getByRole("link", { name: "Settings" });
    expect(within(nav).queryByRole("link", { name: "Mail" })).toBeNull();

    await user.click(settings);

    const sections = await screen.findByRole("navigation", { name: "Settings" });
    expect(sections.closest(".ky-sidebar")).toBeNull();
    expect(within(sections).getByRole("link", { name: "Mail" }).getAttribute("aria-current")).toBe("page");
    expect(within(sections).getByRole("link", { name: "Get the apps" })).toBeTruthy();
    expect(within(sections).queryByRole("link", { name: "Appearance" })).toBeNull();
    // One active item in the sidebar: Settings, not Appearance or a folder.
    expect([...nav.querySelectorAll(".sidebar-link-active")].map((el) => el.textContent?.trim())).toEqual(["Settings"]);
    // The phone header hides the sidebar foot, so the licence line also closes the page.
    const contentFoot = document.querySelector(".ky-content > footer.ky-content-foot") as HTMLElement;
    expect(within(contentFoot).getByRole("button", { name: /MIT License/ })).toBeTruthy();
    expect(within(sections).queryByRole("button", { name: /MIT License/ })).toBeNull();
  });
});
