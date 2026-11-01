// Settings is the account footer's gear (and the phone header's), not a nav
// row: the nav holds mail only, and Settings opens on its personal sections.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router";
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

describe("Settings behind the account footer's gear", () => {
  it("leaves the nav to mail, and opens Settings with the personal sections first", async () => {
    const user = userEvent.setup();

    render(<MemoryRouter initialEntries={["/read"]}><App /></MemoryRouter>);
    await screen.findByText("Primary inbox", {}, { timeout: 5000 });

    const nav = screen.getByRole("navigation", { name: "Primary" });
    expect(nav.closest(".ky-sidebar")).toBeTruthy();
    for (const name of ["Settings", "Appearance", "Mail"]) {
      expect(within(nav).queryByRole("link", { name })).toBeNull();
    }

    // Footer: icon-only buttons, gear first; the words "Sign out" never show.
    const footer = document.querySelector(".ky-sidebar-foot .ky-user-actions") as HTMLElement;
    const buttons = [...footer.querySelectorAll(".ky-icon-button")];
    expect(buttons.map((b) => b.getAttribute("aria-label"))).toEqual(["Settings", "Sign out"]);
    expect(buttons.map((b) => b.getAttribute("title"))).toEqual(["Settings", "Sign out"]);
    expect(footer.textContent?.trim()).toBe("");
    // The phone header carries the same two buttons.
    const phone = document.querySelector(".ky-mobile-head .ky-mobile-actions") as HTMLElement;
    expect([...phone.querySelectorAll(".ky-icon-button")].map((b) => b.getAttribute("aria-label"))).toEqual(["Settings", "Sign out"]);

    await user.click(within(footer).getByRole("link", { name: "Settings" }));

    const sections = await screen.findByRole("navigation", { name: "Settings sections" });
    expect(sections.closest(".ky-sidebar")).toBeNull();
    expect([...sections.querySelectorAll("a")].slice(0, 4).map((a) => a.textContent)).toEqual(["Account", "Phones & apps", "Appearance", "Mail"]);
    expect(within(sections).getByRole("link", { name: "Account" }).getAttribute("aria-current")).toBe("page");
    expect(within(sections).queryByRole("link", { name: "Security" })).toBeNull();
    expect(within(footer).getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    // No nav row is current while Settings is open.
    expect(nav.querySelectorAll(".sidebar-link-active")).toHaveLength(0);
    // The phone header hides the sidebar foot, so the licence line also closes the page.
    const contentFoot = document.querySelector(".ky-content > footer.ky-content-foot") as HTMLElement;
    expect(within(contentFoot).getByRole("button", { name: /MIT License/ })).toBeTruthy();
    expect(within(sections).queryByRole("button", { name: /MIT License/ })).toBeNull();
  });

  it("sends the old Security URL, sub-paths and query included, to the Account tab", async () => {
    function Where() {
      const { pathname, search } = useLocation();
      return <output data-testid="where">{pathname + search}</output>;
    }
    render(<MemoryRouter initialEntries={["/settings/security/x?tab=export&export=proxy"]}><App /><Where /></MemoryRouter>);
    const sections = await screen.findByRole("navigation", { name: "Settings sections" }, { timeout: 5000 });
    expect(within(sections).getByRole("link", { name: "Account" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByTestId("where").textContent).toBe("/settings/account?tab=export&export=proxy");
  });

  it("sends the retired /apps page to Settings > Phones & apps", async () => {
    render(<MemoryRouter initialEntries={["/apps"]}><App /></MemoryRouter>);
    const sections = await screen.findByRole("navigation", { name: "Settings sections" }, { timeout: 5000 });
    expect(within(sections).getByRole("link", { name: "Phones & apps" }).getAttribute("aria-current")).toBe("page");
  });
});
