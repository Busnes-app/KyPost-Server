import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { App } from "../App";
import { AppRail, loadSuiteApps } from "./AppRail";

vi.mock("../lib/pgpSession", () => ({
  subscribePGPSession: () => () => {},
  loadPGPSession: async () => ({ bootstrap: null, unlocked: false }),
  clearPGPSession: () => {},
  isClientProtected: () => false,
  pgpCustody: () => "other",
  needsUnlock: () => false,
  accountAddress: () => ""
}));

const apps = [
  { id: "kycalendar", name: "KyCalendar", url: "https://cal.example.com/api/sso/kysignon/login" },
  { id: "kypost", name: "KyPost", url: "https://mail.example.com/api/auth/oidc/login" },
  { id: "kysomething", name: "Unknown app", url: "https://x.example.com/" }
];

function json(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => "application/json" },
    json: async () => body,
    text: async () => JSON.stringify(body)
  } as unknown as Response;
}

let suite: { status: number; body: unknown };
let urls: string[];

beforeEach(() => {
  urls = [];
  suite = { status: 200, body: { apps } };
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    urls.push(url);
    if (url === "/api/auth/me") return json(200, { authenticated: true, userId: "u1", username: "gwen", role: "user" });
    if (url === "/api/suite/apps") return json(suite.status, suite.body);
    if (url === "/api/mailboxes") return json(200, { mailboxes: [] });
    if (url.startsWith("/api/inbox/folders")) return json(200, { folders: [] });
    if (url.startsWith("/api/inbox?")) return json(200, { tabs: ["Primary"], byTab: { Primary: [] }, cursor: 1 });
    return json(200, {});
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("AppRail", () => {
  it("links every app in order, exactly as given, and marks KyPost current", () => {
    render(<AppRail apps={apps} />);
    const links = within(screen.getByRole("navigation", { name: "Apps" })).getAllByRole("link");
    expect(links.map((a) => a.getAttribute("aria-label"))).toEqual(["KyCalendar", "KyPost", "Unknown app"]);
    expect(links.map((a) => a.getAttribute("href"))).toEqual(apps.map((a) => a.url));
    expect(links.map((a) => a.getAttribute("aria-current"))).toEqual([null, "page", null]);
    expect(links.map((a) => a.getAttribute("target"))).toEqual([null, null, null]);
    for (const link of links) expect(link.querySelector("img")?.getAttribute("src")).toMatch(/^data:image\//);
  });

  it("renders nothing for an empty list", () => {
    const { container } = render(<AppRail apps={[]} />);
    expect(container.innerHTML).toBe("");
  });

  it("loads an empty list on failure or a malformed answer", async () => {
    suite = { status: 500, body: {} };
    expect(await loadSuiteApps()).toEqual([]);
    suite = { status: 200, body: { apps: "no" } };
    expect(await loadSuiteApps()).toEqual([]);
    suite = { status: 200, body: { apps: [{ id: "x", name: "X", url: "javascript:alert(1)" }] } };
    expect(await loadSuiteApps()).toEqual([]);
  });
});

function renderApp() {
  return render(<MemoryRouter initialEntries={["/read"]}><App /></MemoryRouter>);
}

describe("App rail in the shell", () => {
  it("fetches once and renders the rail at the top of the sidebar foot and the content foot", async () => {
    const { container } = renderApp();
    expect(await screen.findAllByRole("navigation", { name: "Apps" }, { timeout: 5000 })).toHaveLength(2);
    expect(container.querySelector(".ky-sidebar-foot > nav.ky-app-rail:first-child")).toBeTruthy();
    expect(container.querySelector(".ky-content-foot > nav.ky-app-rail:first-child")).toBeTruthy();
    expect(urls.filter((u) => u === "/api/suite/apps")).toHaveLength(1);
  });

  it("leaves the rail out when the list is empty", async () => {
    suite = { status: 200, body: { apps: [] } };
    renderApp();
    await screen.findByRole("navigation", { name: "Primary" }, { timeout: 5000 });
    await waitFor(() => expect(urls).toContain("/api/suite/apps"));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByRole("navigation", { name: "Apps" })).toBeNull();
  });
});
