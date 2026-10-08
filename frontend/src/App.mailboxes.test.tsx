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
// Returns a pending answer for a request the test wants to settle itself.
let hold: (url: string, mailbox: string | null) => Promise<Response> | undefined = () => undefined;

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
  return { messageId: subject, sender: "a@b.test", subject, status: "read", atUtc: "2026-10-01T10:00:00Z" };
}

beforeEach(() => {
  calls = [];
  goneMailbox = "";
  goneInboxOnly = false;
  hold = () => undefined;
  mailboxList = { mailboxes: [primary, team] };
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit = {}) => {
    const headers = (init.headers ?? {}) as Record<string, string>;
    const mailbox = headers["X-KyPost-Mailbox"] ?? null;
    calls.push({ url, method: init.method ?? "GET", mailbox, body: typeof init.body === "string" ? init.body : "" });
    const held = hold(url, mailbox);
    if (held) return held;
    if (url === "/api/auth/me") return json(200, { authenticated: true, userId: "u1", username: "gwen", role: "user" });
    if (url === "/api/mailboxes") return json(200, mailboxList);
    if (mailbox && mailbox === goneMailbox && (!goneInboxOnly || url.startsWith("/api/inbox?"))) return json(404, { error: "mailbox not found" });
    if (url.startsWith("/api/inbox/folders")) return json(200, { folders: [{ path: mailbox ? "TeamOnly" : "MineOnly", deletable: true }] });
    if (url.startsWith("/api/inbox?")) {
      const folder = new URLSearchParams(url.split("?")[1]).get("mailbox") ?? "";
      const subject = `${mailbox ? "Team" : "Primary"} ${folder || "inbox"}`;
      return json(200, { tabs: ["Primary"], byTab: { Primary: [{ ...message(subject), hasAttachments: true }] }, cursor: 1 });
    }
    if (url.startsWith("/api/mail/attachments?")) return json(200, { ok: true, attachments: [{ index: 0, name: "plan.pdf", mimeType: "application/pdf", size: 3 }] });
    if (url.startsWith("/api/mail/attachment?")) return { ok: true, status: 200, headers: { get: () => "application/pdf" }, blob: async () => new Blob(["pdf"]) } as unknown as Response;
    if (url.startsWith("/api/mail/body?")) return json(200, { body: "draft body", bodyMode: "plain" });
    if (url === "/api/mail/send") return json(200, { ok: true });
    if (url.startsWith("/api/contacts/search?")) return json(200, { contacts: [] });
    return json(200, {});
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.sessionStorage.clear();
});

function snapshot() {
  return {
    version: 2,
    savedAt: new Date().toISOString(),
    fields: { to: "x@y.test", cc: "", bcc: "", subject: "team draft", body: "<p>hi</p>", attachmentNames: [], mailbox: TEAM, from: "team-test@urlxl.us" }
  };
}

function renderApp() {
  render(<MemoryRouter initialEntries={["/read"]}><App /></MemoryRouter>);
}

const switcher = () => screen.findByRole<HTMLSelectElement>("combobox", { name: "Your mailboxes" });
const sent = (prefix: string) => calls.filter((c) => c.url.startsWith(prefix));

describe("webmail mailbox switcher", () => {
  it("is hidden with one mailbox and shows each mailbox by its primary address with two", async () => {
    mailboxList = { mailboxes: [primary] };
    renderApp();
    await screen.findByText("Primary inbox");
    await waitFor(() => expect(sent("/api/mailboxes")).toHaveLength(1));
    expect(screen.queryByRole("combobox", { name: "Your mailboxes" })).toBeNull();
    cleanup();

    mailboxList = { mailboxes: [primary, team] };
    renderApp();
    const select = await switcher();
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual(["me@urlxl.us", "team-test@urlxl.us"]);
    expect(select.selectedOptions[0]?.textContent).toBe("me@urlxl.us");
  });

  it("sends the header for an extra mailbox only, and shows nothing of the other mailbox after a switch", async () => {
    const user = userEvent.setup();
    renderApp();
    await screen.findByText("Primary inbox");
    expect(calls.every((c) => c.mailbox === null)).toBe(true);

    calls = [];
    await user.selectOptions(await switcher(), "team-test@urlxl.us");
    await screen.findByText("Team inbox");
    expect(screen.queryByText("Primary inbox")).toBeNull();
    expect(await screen.findByText("TeamOnly")).toBeTruthy();
    expect(screen.queryByText("MineOnly")).toBeNull();
    expect(sent("/api/inbox?").every((c) => c.mailbox === TEAM)).toBe(true);
    expect(sent("/api/inbox/folders").every((c) => c.mailbox === TEAM)).toBe(true);
    // Per-user routes never carry it.
    expect(calls.filter((c) => !/^\/api\/(inbox|mail\/)/.test(c.url)).every((c) => c.mailbox === null)).toBe(true);
    expect((await switcher()).selectedOptions[0]?.textContent).toBe("team-test@urlxl.us");

    calls = [];
    await user.selectOptions(await switcher(), "me@urlxl.us");
    await screen.findByText("Primary inbox");
    expect(screen.queryByText("Team inbox")).toBeNull();
    expect(sent("/api/inbox?").length).toBeGreaterThan(0);
    expect(calls.every((c) => c.mailbox === null)).toBe(true);
  });

  it("offers the selected mailbox's addresses as From and sends from it", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.selectOptions(await switcher(), "team-test@urlxl.us");
    await screen.findByText("Team inbox");
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
    await user.selectOptions(await switcher(), "team-test@urlxl.us");
    await screen.findByText("Team inbox");
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

  it("restores an autosaved draft into the mailbox it was written in", async () => {
    window.sessionStorage.setItem("kypost-compose-draft:u1", JSON.stringify(snapshot()));
    const user = userEvent.setup();
    renderApp();
    await switcher();
    await user.click(screen.getByRole("button", { name: "New Email" }));
    await screen.findByText(/written in team-test@urlxl.us/);
    const from = screen.getByRole("combobox", { name: "FROM:" }) as HTMLSelectElement;
    expect(from.value).toBe("team-test@urlxl.us");
    expect(Array.from(from.options).map((o) => o.value)).toEqual(["team-test@urlxl.us", "team-alias@urlxl.us"]);
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(calls.filter((c) => c.url === "/api/mail/send")).toHaveLength(1));
    expect(calls.find((c) => c.url === "/api/mail/send")?.mailbox).toBe(TEAM);
  });

  it("keeps an autosaved draft of an unavailable mailbox instead of restoring it elsewhere", async () => {
    window.sessionStorage.setItem("kypost-compose-draft:u1", JSON.stringify(snapshot()));
    mailboxList = { mailboxes: [primary] };
    const user = userEvent.setup();
    renderApp();
    await screen.findByText("Primary inbox");
    await waitFor(() => expect(sent("/api/mailboxes")).toHaveLength(1));
    await user.click(screen.getByRole("button", { name: "New Email" }));
    await screen.findByText(/An unsent draft from team-test@urlxl.us is kept/);
    expect((screen.getByPlaceholderText("Subject") as HTMLInputElement).value).toBe("");
    await user.type(screen.getByPlaceholderText("Subject"), "new");
    await new Promise((resolve) => window.setTimeout(resolve, 1200));
    expect(JSON.parse(window.sessionStorage.getItem("kypost-compose-draft:u1") ?? "{}").fields.subject).toBe("team draft");
  });

  it("downloads an extra mailbox's attachment through a button that carries the header", async () => {
    const createObjectURL = vi.fn(() => "blob:x");
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL, revokeObjectURL: () => {} }));
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    const user = userEvent.setup();
    renderApp();
    await user.selectOptions(await switcher(), "team-test@urlxl.us");
    await user.click(await screen.findByText("Team inbox"));
    const download = await screen.findByRole("button", { name: /plan\.pdf/ });
    expect(screen.queryByRole("link", { name: /plan\.pdf/ })).toBeNull();
    await user.click(download);
    await waitFor(() => expect(sent("/api/mail/attachment?")).toHaveLength(1));
    expect(sent("/api/mail/attachment?")[0].mailbox).toBe(TEAM);
    expect(click).toHaveBeenCalled();
    click.mockRestore();
  });

  it("keeps the primary's attachment a plain link", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.click(await screen.findByText("Primary inbox"));
    expect((await screen.findByRole("link", { name: /plan\.pdf/ })).getAttribute("href")).toContain("/api/mail/attachment?");
  });

  it("opens a draft in the mailbox it was read from even after a switch during its fetch", async () => {
    const pending: ((r: Response) => void)[] = [];
    hold = (url) => url.startsWith("/api/mail/body?") ? new Promise((resolve) => { pending.push(resolve); }) : undefined;
    const user = userEvent.setup();
    renderApp();
    await user.selectOptions(await switcher(), "team-test@urlxl.us");
    await user.click(await screen.findByRole("link", { name: "Drafts" }));
    await user.click(await screen.findByText("Team Drafts"));
    await waitFor(() => expect(sent("/api/mail/body?").length).toBeGreaterThan(0));
    await user.selectOptions(await switcher(), "me@urlxl.us");
    await screen.findByText("Primary inbox");
    for (const release of pending) release(json(200, { body: "draft body", bodyMode: "plain" }));
    const from = await screen.findByRole("combobox", { name: "FROM:" }) as HTMLSelectElement;
    expect(from.value).toBe("team-test@urlxl.us");
  });

  it("ignores a folder list answered for a mailbox no longer selected", async () => {
    let fail: (r: Response) => void = () => {};
    hold = (url, mailbox) => url === "/api/inbox/folders" && mailbox === null ? new Promise((resolve) => { fail = resolve; }) : undefined;
    const user = userEvent.setup();
    renderApp();
    await user.selectOptions(await switcher(), "team-test@urlxl.us");
    expect(await screen.findByText("TeamOnly")).toBeTruthy();
    fail(json(500, { error: "boom" }));
    await new Promise((resolve) => window.setTimeout(resolve, 50));
    expect(screen.getByText("TeamOnly")).toBeTruthy();
  });

  it("fills From once the mailbox list arrives after compose opened", async () => {
    let answer: (r: Response) => void = () => {};
    hold = (url) => url === "/api/mailboxes" ? new Promise((resolve) => { answer = resolve; }) : undefined;
    const user = userEvent.setup();
    renderApp();
    await screen.findByText("Primary inbox");
    await user.click(screen.getByRole("button", { name: "New Email" }));
    answer(json(200, { mailboxes: [primary, team] }));
    await screen.findByRole("combobox", { name: "FROM:" });
    await user.type(screen.getByLabelText("To recipients"), "x@y.test{Enter}");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(calls.filter((c) => c.url === "/api/mail/send")).toHaveLength(1));
    // The select alone would display its first option even with no From chosen.
    expect(JSON.parse(calls.find((c) => c.url === "/api/mail/send")?.body ?? "{}").from).toBe("me@urlxl.us");
  });

  it("names the open mailbox on the read page and in folder links", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.selectOptions(await switcher(), "team-test@urlxl.us");
    await screen.findByText("Team inbox");
    expect(document.querySelector(".read-mailbox-name")?.textContent).toBe("team-test@urlxl.us");
    expect(screen.getByRole("link", { name: "Sent" }).getAttribute("href")).toBe(`/read?mailbox=Sent&box=${TEAM}`);
  });

  it("shows a replayed folder URL of another mailbox as this mailbox's Inbox", async () => {
    window.sessionStorage.setItem("kypost-mailbox:u1", TEAM);
    render(<MemoryRouter initialEntries={["/read?mailbox=MineOnly"]}><App /></MemoryRouter>);
    await screen.findByText("Team inbox");
    expect(sent("/api/inbox?").every((c) => !c.url.includes("MineOnly"))).toBe(true);
    cleanup();
    calls = [];
    window.sessionStorage.clear();
    render(<MemoryRouter initialEntries={[`/read?mailbox=TeamOnly&box=${TEAM}`]}><App /></MemoryRouter>);
    await screen.findByText("Primary inbox");
    expect(sent("/api/inbox?").every((c) => !c.url.includes("TeamOnly"))).toBe(true);
  });

  it("keeps the selection across a reload", async () => {
    window.sessionStorage.setItem("kypost-mailbox:u1", TEAM);
    renderApp();
    await screen.findByText("Team inbox");
    expect(sent("/api/inbox?")[0].mailbox).toBe(TEAM);
    expect(screen.queryByText("Primary inbox")).toBeNull();
  });

  it("falls back to the primary with a notice when the mailbox is gone", async () => {
    window.sessionStorage.setItem("kypost-mailbox:u1", TEAM);
    goneMailbox = TEAM;
    renderApp();
    const notice = await screen.findByRole("alert");
    expect(notice.textContent).toContain("This mailbox is no longer available. Showing your primary mailbox.");
    await screen.findByText("Primary inbox");
    expect(sent("/api/inbox?").slice(-1)[0]?.mailbox).toBeNull();
    expect(window.sessionStorage.getItem("kypost-mailbox:u1")).toBeNull();
    // The list is read again after the refusal.
    await waitFor(() => expect(sent("/api/mailboxes").length).toBeGreaterThan(1));
    await userEvent.setup().click(within(notice).getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("drops a remembered mailbox the list no longer holds", async () => {
    window.sessionStorage.setItem("kypost-mailbox:u1", TEAM);
    mailboxList = { mailboxes: [primary] };
    renderApp();
    await screen.findByRole("alert");
    await screen.findByText("Primary inbox");
    expect(sent("/api/inbox?").slice(-1)[0]?.mailbox).toBeNull();
  });

  it("ignores the list for a non-native account and never sends the header", async () => {
    mailboxList = { mailboxes: [] };
    const user = userEvent.setup();
    renderApp();
    await screen.findByText("Primary inbox");
    await user.click(screen.getByRole("button", { name: "New Email" }));
    expect(screen.queryByRole("combobox", { name: "Your mailboxes" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "FROM:" })).toBeNull();
    expect(calls.every((c) => c.mailbox === null)).toBe(true);
  });

  it("hides the switcher for a malformed list", async () => {
    mailboxList = { mailboxes: [primary, { ...team, id: "../u2" }] };
    renderApp();
    await screen.findByText("Primary inbox");
    await waitFor(() => expect(sent("/api/mailboxes")).toHaveLength(1));
    expect(screen.queryByRole("combobox", { name: "Your mailboxes" })).toBeNull();
  });
});
