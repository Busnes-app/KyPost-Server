// The webmail mailbox switcher, driven through the real api/client so the
// X-KyPost-Mailbox header is observed on the wire (a stubbed fetch).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { App } from "./App";
import { getJSON } from "./api/client";

vi.mock("./lib/pgpSession", () => ({
  subscribePGPSession: () => () => {},
  loadPGPSession: async () => ({ bootstrap: null, unlocked: false }),
  clearPGPSession: () => {},
  isClientProtected: () => false,
  pgpCustody: () => "other",
  needsUnlock: () => false,
  accountAddress: () => ""
}));

const TEAM = "mbx-11111111-2222-4333-8444-555555555555";
const primary = { id: "u1", kind: "primary", addresses: [{ address: "me@urlxl.us", kind: "primary" }], quotaBytes: 0 };
const team = {
  id: TEAM,
  kind: "extra",
  addresses: [{ address: "team-alias@urlxl.us", kind: "alias" }, { address: "team-test@urlxl.us", kind: "primary" }],
  quotaBytes: 0
};

type Call = { url: string; method: string; mailbox: string | null; body: string };
let calls: Call[] = [];
let mailboxList: unknown;
let goneMailbox = "";
let goneInboxOnly = false;

function json(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => "application/json" },
    json: async () => body,
    text: async () => JSON.stringify(body)
  } as unknown as Response;
}

function message(subject: string) {
  return { messageId: subject, sender: "a@b.test", subject, body: "", status: "read", atUtc: "2026-10-01T10:00:00Z" };
}

beforeEach(() => {
  calls = [];
  goneMailbox = "";
  goneInboxOnly = false;
  mailboxList = { mailboxes: [primary, team] };
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit = {}) => {
    const headers = (init.headers ?? {}) as Record<string, string>;
    const mailbox = headers["X-KyPost-Mailbox"] ?? null;
    calls.push({ url, method: init.method ?? "GET", mailbox, body: typeof init.body === "string" ? init.body : "" });
    if (url === "/api/auth/me") return json(200, { authenticated: true, userId: "u1", username: "gwen", role: "user" });
    if (url === "/api/mailboxes") return json(200, mailboxList);
    if (mailbox && mailbox === goneMailbox && (!goneInboxOnly || url.startsWith("/api/inbox?"))) return json(404, { error: "mailbox not found" });
    if (url.startsWith("/api/inbox/folders")) return json(200, { folders: [{ path: mailbox ? "TeamOnly" : "MineOnly", deletable: true }] });
    if (url.startsWith("/api/inbox?")) return json(200, { tabs: ["Primary"], byTab: { Primary: [message(mailbox ? "Team secret" : "Primary hello")] }, cursor: 1 });
    if (url === "/api/mail/send") return json(200, { ok: true });
    return json(200, {});
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.sessionStorage.clear();
});

function renderApp() {
  render(<MemoryRouter initialEntries={["/read"]}><App /></MemoryRouter>);
}

const switcher = () => screen.findByRole("group", { name: "Your mailboxes" });
const sent = (prefix: string) => calls.filter((c) => c.url.startsWith(prefix));

describe("webmail mailbox switcher", () => {
  it("is hidden with one mailbox and shows each mailbox by its primary address with two", async () => {
    mailboxList = { mailboxes: [primary] };
    renderApp();
    await screen.findByText("Primary hello");
    await waitFor(() => expect(sent("/api/mailboxes")).toHaveLength(1));
    expect(screen.queryByRole("group", { name: "Your mailboxes" })).toBeNull();
    cleanup();

    mailboxList = { mailboxes: [primary, team] };
    renderApp();
    const links = within(await switcher()).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(["me@urlxl.us", "team-test@urlxl.us"]);
    expect(links[0].getAttribute("aria-current")).toBe("true");
    expect(links[1].getAttribute("aria-current")).toBeNull();
  });

  it("sends the header for an extra mailbox only, and shows nothing of the other mailbox after a switch", async () => {
    const user = userEvent.setup();
    renderApp();
    await screen.findByText("Primary hello");
    expect(calls.every((c) => c.mailbox === null)).toBe(true);

    calls = [];
    await user.click(within(await switcher()).getByRole("link", { name: "team-test@urlxl.us" }));
    await screen.findByText("Team secret");
    expect(screen.queryByText("Primary hello")).toBeNull();
    expect(await screen.findByText("TeamOnly")).toBeTruthy();
    expect(screen.queryByText("MineOnly")).toBeNull();
    expect(sent("/api/inbox?").every((c) => c.mailbox === TEAM)).toBe(true);
    expect(sent("/api/inbox/folders").every((c) => c.mailbox === TEAM)).toBe(true);
    // Per-user routes never carry it.
    expect(calls.filter((c) => !/^\/api\/(inbox|mail\/)/.test(c.url)).every((c) => c.mailbox === null)).toBe(true);
    expect(within(await switcher()).getByRole("link", { name: "team-test@urlxl.us" }).getAttribute("aria-current")).toBe("true");

    calls = [];
    await user.click(within(await switcher()).getByRole("link", { name: "me@urlxl.us" }));
    await screen.findByText("Primary hello");
    expect(screen.queryByText("Team secret")).toBeNull();
    expect(sent("/api/inbox?").length).toBeGreaterThan(0);
    expect(calls.every((c) => c.mailbox === null)).toBe(true);
  });

  it("offers the selected mailbox's addresses as From and sends from it", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.click(within(await switcher()).getByRole("link", { name: "team-test@urlxl.us" }));
    await screen.findByText("Team secret");
    await user.click(screen.getByRole("button", { name: "New Email" }));
    const from = screen.getByRole("combobox", { name: "FROM:" }) as HTMLSelectElement;
    expect(Array.from(from.options).map((o) => o.value)).toEqual(["team-test@urlxl.us", "team-alias@urlxl.us"]);
    expect(from.value).toBe("team-test@urlxl.us");

    await user.type(screen.getByLabelText("To recipients"), "x@y.test{Enter}");
    await user.type(screen.getByPlaceholderText("Subject"), "hello");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(calls.filter((c) => c.url === "/api/mail/send")).toHaveLength(1));
    const [send] = calls.filter((c) => c.url === "/api/mail/send");
    expect(send.mailbox).toBe(TEAM);
    expect(JSON.parse(send.body).from).toBe("team-test@urlxl.us");
  });

  it("offers the primary's addresses as From for the primary", async () => {
    const user = userEvent.setup();
    renderApp();
    await switcher();
    await user.click(screen.getByRole("button", { name: "New Email" }));
    const from = screen.getByRole("combobox", { name: "FROM:" }) as HTMLSelectElement;
    expect(Array.from(from.options).map((o) => o.value)).toEqual(["me@urlxl.us"]);
  });

  it("keeps an open compose on its mailbox when the selection changes underneath it", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.click(within(await switcher()).getByRole("link", { name: "team-test@urlxl.us" }));
    await screen.findByText("Team secret");
    await user.click(screen.getByRole("button", { name: "New Email" }));
    await user.type(screen.getByLabelText("To recipients"), "x@y.test{Enter}");
    // A background read refused for the mailbox moves the selection to the primary.
    goneMailbox = TEAM;
    goneInboxOnly = true;
    await expect(getJSON("/api/inbox?limit=1")).rejects.toThrow("mailbox not found");
    await screen.findByText("This mailbox is no longer available. Showing your primary mailbox.");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(calls.filter((c) => c.url === "/api/mail/send")).toHaveLength(1));
    expect(calls.find((c) => c.url === "/api/mail/send")?.mailbox).toBe(TEAM);
  });

  it("keeps the selection across a reload", async () => {
    window.sessionStorage.setItem("kypost-mailbox:u1", TEAM);
    renderApp();
    await screen.findByText("Team secret");
    expect(sent("/api/inbox?")[0].mailbox).toBe(TEAM);
    expect(screen.queryByText("Primary hello")).toBeNull();
  });

  it("falls back to the primary with a notice when the mailbox is gone", async () => {
    window.sessionStorage.setItem("kypost-mailbox:u1", TEAM);
    goneMailbox = TEAM;
    renderApp();
    expect((await screen.findByRole("alert")).textContent).toBe("This mailbox is no longer available. Showing your primary mailbox.");
    await screen.findByText("Primary hello");
    expect(sent("/api/inbox?").slice(-1)[0]?.mailbox).toBeNull();
    expect(window.sessionStorage.getItem("kypost-mailbox:u1")).toBeNull();
    // The list is read again after the refusal.
    await waitFor(() => expect(sent("/api/mailboxes").length).toBeGreaterThan(1));
  });

  it("drops a remembered mailbox the list no longer holds", async () => {
    window.sessionStorage.setItem("kypost-mailbox:u1", TEAM);
    mailboxList = { mailboxes: [primary] };
    renderApp();
    await screen.findByRole("alert");
    await screen.findByText("Primary hello");
    expect(sent("/api/inbox?").slice(-1)[0]?.mailbox).toBeNull();
  });

  it("ignores the list for a non-native account and never sends the header", async () => {
    mailboxList = { mailboxes: [] };
    const user = userEvent.setup();
    renderApp();
    await screen.findByText("Primary hello");
    await user.click(screen.getByRole("button", { name: "New Email" }));
    expect(screen.queryByRole("group", { name: "Your mailboxes" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "FROM:" })).toBeNull();
    expect(calls.every((c) => c.mailbox === null)).toBe(true);
  });

  it("hides the switcher for a malformed list", async () => {
    mailboxList = { mailboxes: [primary, { ...team, id: "../u2" }] };
    renderApp();
    await screen.findByText("Primary hello");
    await waitFor(() => expect(sent("/api/mailboxes")).toHaveLength(1));
    expect(screen.queryByRole("group", { name: "Your mailboxes" })).toBeNull();
  });
});
