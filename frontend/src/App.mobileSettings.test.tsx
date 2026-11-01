// Settings is the account footer's gear (and the phone header's), not a nav
// row: the nav holds mail only, and the gear opens a menu of the Settings tabs.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router";
import type { ReactNode } from "react";
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

let role = "user";
let originalScrollIntoView: typeof Element.prototype.scrollIntoView;

beforeEach(() => {
  originalScrollIntoView = Element.prototype.scrollIntoView;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url === "/api/auth/me") return json(200, { authenticated: true, userId: "u1", username: "gwen", role });
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
  role = "user";
});

function Where() {
  const { pathname, search } = useLocation();
  return <output data-testid="where">{pathname + search}</output>;
}

async function renderAt(path: string, extra?: ReactNode) {
  render(<MemoryRouter initialEntries={[path]}><App />{extra}</MemoryRouter>);
  await screen.findByText(/Primary inbox|Settings sections/, {}, { timeout: 5000 }).catch(() => {});
  return document.querySelector(".ky-sidebar-foot .ky-user-actions") as HTMLElement;
}

const menuLabels = (menu: HTMLElement) => within(menu).getAllByRole("menuitem", { hidden: true }).map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : ""));

describe("the Settings gear's menu", () => {
  it("lists the everyday user's Settings tabs in tab order, with the open one current", async () => {
    const footer = await renderAt("/settings/mail");
    await screen.findByRole("navigation", { name: "Settings sections" }, { timeout: 5000 });
    const gear = within(footer).getByRole("button", { name: "Settings" });
    expect(gear.getAttribute("aria-haspopup")).toBe("menu");
    expect(gear.getAttribute("aria-current")).toBe("page");
    const menu = document.getElementById(gear.getAttribute("aria-controls")!)!;
    expect(menu.getAttribute("role")).toBe("menu");
    expect(menuLabels(menu)).toEqual(["Account", "Phones & apps", "Appearance", "Mail*", "Notifications", "Email Labels", "Status"]);
    expect(within(menu).getAllByRole("menuitem", { hidden: true }).map((a) => a.getAttribute("href"))).toEqual([
      "/settings/account", "/settings/apps", "/settings/appearance", "/settings/mail", "/settings/notifications", "/settings/automation", "/settings/status"
    ]);
    // The phone header has its own menu, under another id, with the same items.
    const phoneGear = within(document.querySelector(".ky-mobile-actions") as HTMLElement).getByRole("button", { name: "Settings" });
    expect(phoneGear.getAttribute("aria-controls")).not.toBe(gear.getAttribute("aria-controls"));
    expect(menuLabels(document.getElementById(phoneGear.getAttribute("aria-controls")!)!)).toEqual(menuLabels(menu));
  });

  it("gives an administrator Server and Diagnostics in place of Status", async () => {
    role = "admin";
    const footer = await renderAt("/read");
    const gear = await within(footer).findByRole("button", { name: "Settings" });
    expect(gear.getAttribute("aria-current")).toBeNull();
    const menu = document.getElementById(gear.getAttribute("aria-controls")!)!;
    expect(menuLabels(menu)).toEqual(["Account", "Phones & apps", "Appearance", "Mail", "Notifications", "Email Labels", "Server", "Diagnostics"]);
  });

  it("opens on Account without following it, moves with the keys, and closes", async () => {
    const user = userEvent.setup();
    const footer = await renderAt("/read", <Where />);
    const gear = within(footer).getByRole("button", { name: "Settings" });
    const menu = document.getElementById(gear.getAttribute("aria-controls")!)!;
    expect(menu.hidden).toBe(true);

    gear.focus();
    await user.keyboard("{Enter}");
    expect(gear.getAttribute("aria-expanded")).toBe("true");
    expect(menu.hidden).toBe(false);
    expect(document.activeElement?.textContent).toBe("Account");
    // The gear never lands on Account (and its password prompt) by itself.
    expect(screen.getByTestId("where").textContent).toBe("/read");

    await user.keyboard("{ArrowDown}");
    expect(document.activeElement?.textContent).toBe("Phones & apps");
    await user.keyboard("{End}");
    expect(document.activeElement?.textContent).toBe("Status");
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement?.textContent).toBe("Account");
    await user.keyboard("{Home}");
    expect(document.activeElement?.textContent).toBe("Account");
    await user.keyboard("{Escape}");
    expect(menu.hidden).toBe(true);
    expect(document.activeElement).toBe(gear);

    await user.keyboard(" ");
    expect(menu.hidden).toBe(false);
    await user.keyboard("{Escape}");
    await user.keyboard("{ArrowUp}");
    expect(document.activeElement?.textContent).toBe("Status");
    await user.tab();
    expect(menu.hidden).toBe(true);
    expect(gear.getAttribute("aria-expanded")).toBe("false");

    await user.click(gear);
    expect(menu.hidden).toBe(false);
    await user.click(document.body);
    expect(menu.hidden).toBe(true);
    expect(screen.getByTestId("where").textContent).toBe("/read");
  });

  it("opens the chosen tab and closes, focus back on the gear", async () => {
    const user = userEvent.setup();
    const footer = await renderAt("/read", <Where />);
    const gear = within(footer).getByRole("button", { name: "Settings" });
    await user.click(gear);
    await user.click(within(footer).getByRole("menuitem", { name: "Notifications" }));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/settings/notifications"));
    expect(document.getElementById(gear.getAttribute("aria-controls")!)!.hidden).toBe(true);
    expect(document.activeElement).toBe(gear);
    const sections = await screen.findByRole("navigation", { name: "Settings sections" }, { timeout: 5000 });
    expect(within(sections).getByRole("link", { name: "Notifications" }).getAttribute("aria-current")).toBe("page");
    expect(gear.getAttribute("aria-current")).toBe("page");
    expect(menuLabels(document.getElementById(gear.getAttribute("aria-controls")!)!)).toContain("Notifications*");
  });

  it("lands a bare /settings on Phones & apps, not on Account's password check", async () => {
    await renderAt("/settings", <Where />);
    const sections = await screen.findByRole("navigation", { name: "Settings sections" }, { timeout: 5000 });
    expect(within(sections).getByRole("link", { name: "Phones & apps" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByTestId("where").textContent).toBe("/settings/apps");
  });
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
    // Only the hidden Settings menu holds words.
    const bare = footer.cloneNode(true) as HTMLElement;
    bare.querySelectorAll(".ky-menu-list").forEach((m) => m.remove());
    expect(bare.textContent?.trim()).toBe("");
    // The phone header carries the same two buttons.
    const phone = document.querySelector(".ky-mobile-head .ky-mobile-actions") as HTMLElement;
    expect([...phone.querySelectorAll(".ky-icon-button")].map((b) => b.getAttribute("aria-label"))).toEqual(["Settings", "Sign out"]);

    await user.click(within(footer).getByRole("button", { name: "Settings" }));
    await user.click(within(footer).getByRole("menuitem", { name: "Account" }));

    const sections = await screen.findByRole("navigation", { name: "Settings sections" });
    expect(sections.closest(".ky-sidebar")).toBeNull();
    expect([...sections.querySelectorAll("a")].slice(0, 4).map((a) => a.textContent)).toEqual(["Account", "Phones & apps", "Appearance", "Mail"]);
    expect(within(sections).getByRole("link", { name: "Account" }).getAttribute("aria-current")).toBe("page");
    expect(within(sections).queryByRole("link", { name: "Security" })).toBeNull();
    expect(within(footer).getByRole("button", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
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
