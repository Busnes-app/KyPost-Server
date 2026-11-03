import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// List-level wiring: deep links, search, selection across tabs, delete in
// Trash and bulk print. Each was wrong only in how the pieces met, so the
// component is driven as a user would.

const getJSON = vi.fn();
const postJSON = vi.fn();
vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  getBytes: vi.fn(),
  selectedMailboxId: () => "",
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  toErrorMessage: (_e: unknown, fallback: string) => fallback
}));

vi.mock("../lib/pgpSession", () => ({
  isClientProtected: () => false,
  needsUnlock: () => false,
  subscribePGPSession: () => () => {}
}));

import { ReadPage } from "./ReadPage";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const row = (messageId: string, subject: string) => ({
  messageId,
  sender: "someone@example.com",
  subject,
  status: "read",
  atUtc: "2026-07-27T10:00:00Z"
});

let inbox: Promise<unknown>;

beforeEach(() => {
  vi.clearAllMocks();
  inbox = Promise.resolve({ tabs: ["Primary", "News"], byTab: { Primary: [row("m1", "First")], News: [row("m2", "Second")] } });
  getJSON.mockImplementation((url: string) => {
    if (url.startsWith("/api/inbox")) return inbox;
    if (url.startsWith("/api/labels")) return Promise.resolve({ configured: [], imap: [] });
    if (url.startsWith("/api/mail/body")) return Promise.resolve({ body: `body of ${new URLSearchParams(url.split("?")[1]).get("messageId")}`, bodyMode: "plain" });
    if (url.startsWith("/api/mail/attachments")) return Promise.resolve({ ok: true, attachments: [] });
    if (url.startsWith("/api/mail/search")) return Promise.resolve({ results: [row("m9", "Found")] });
    return Promise.resolve({});
  });
  postJSON.mockResolvedValue({ ok: true, results: [], failed: [] });
});

function renderAt(path: string, folders?: { path: string; deletable: boolean; special?: string }[]) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ReadPage folders={folders} />
    </MemoryRouter>
  );
}

const urls = () => getJSON.mock.calls.map((call) => String(call[0]));

describe("ReadPage list actions", () => {
  it("keeps a notification deep link until the inbox has loaded, then opens it", async () => {
    let release: (v: unknown) => void = () => {};
    inbox = new Promise((resolve) => (release = resolve));
    renderAt("/?message=m2&tab=News");
    await waitFor(() => expect(urls().some((u) => u.startsWith("/api/inbox"))).toBe(true));
    release({ tabs: ["Primary", "News"], byTab: { Primary: [row("m1", "First")], News: [row("m2", "Second")] } });
    await waitFor(() => expect(urls()).toContainEqual(expect.stringMatching(/^\/api\/mail\/body\?messageId=m2/)));
  });

  it("searches the listed folder, never a literal INBOX", async () => {
    const user = userEvent.setup();
    renderAt("/");
    await user.type(await screen.findByRole("searchbox", { name: "Search mail" }), "invoice{Enter}");
    const search = await waitFor(() => {
      const u = urls().find((x) => x.startsWith("/api/mail/search"));
      expect(u).toBeDefined();
      return u!;
    });
    expect(new URLSearchParams(search.split("?")[1]).has("mailbox")).toBe(false);
  });

  it("clears the selection when the tab changes", async () => {
    const user = userEvent.setup();
    renderAt("/");
    await user.click(await screen.findByRole("checkbox", { name: "Select email First" }));
    expect(screen.getByRole("toolbar", { name: "Selected messages" })).toBeDefined();
    await user.click(screen.getByRole("button", { name: /^News/ }));
    await user.click(await screen.findByRole("checkbox", { name: "Select email Second" }));
    await user.click(within(screen.getByRole("toolbar", { name: "Selected messages" })).getByRole("button", { name: "Mark read" }));
    await waitFor(() => expect(postJSON).toHaveBeenCalledWith("/api/inbox/actions", expect.objectContaining({ action: "read", messageIds: ["m2"] })));
  });

  it("asks before deleting from Trash, which cannot be undone", async () => {
    inbox = Promise.resolve({ tabs: ["Trash"], byTab: { Trash: [row("t1", "Binned")] } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const user = userEvent.setup();
    renderAt("/?mailbox=Trash");
    await user.click(await screen.findByRole("checkbox", { name: "Select email Binned" }));
    await user.click(within(screen.getByRole("toolbar", { name: "Selected messages" })).getByRole("button", { name: "Delete" }));
    expect(confirm).toHaveBeenCalledWith("Permanently delete this message? This cannot be undone.");
    expect(postJSON).not.toHaveBeenCalledWith("/api/inbox/actions", expect.anything());
  });

  it("recognises Trash by its server role, whatever it is called", async () => {
    inbox = Promise.resolve({ tabs: ["Papierkorb"], byTab: { Papierkorb: [row("t1", "Binned")] } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const user = userEvent.setup();
    renderAt("/?mailbox=Papierkorb", [{ path: "Papierkorb", deletable: false, special: "trash" }]);
    await user.click(await screen.findByRole("checkbox", { name: "Select email Binned" }));
    await user.click(within(screen.getByRole("toolbar", { name: "Selected messages" })).getByRole("button", { name: "Delete" }));
    expect(confirm).toHaveBeenCalled();
    expect(postJSON).not.toHaveBeenCalledWith("/api/inbox/actions", expect.anything());
  });

  it("asks for an unrecognised folder while folder roles are unknown", async () => {
    inbox = Promise.resolve({ tabs: ["Papierkorb"], byTab: { Papierkorb: [row("t1", "Binned")] } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const user = userEvent.setup();
    renderAt("/?mailbox=Papierkorb");
    await user.click(await screen.findByRole("checkbox", { name: "Select email Binned" }));
    await user.click(within(screen.getByRole("toolbar", { name: "Selected messages" })).getByRole("button", { name: "Delete" }));
    expect(confirm).toHaveBeenCalledWith("Delete this message? If this folder is your Trash, this cannot be undone.");
    expect(postJSON).not.toHaveBeenCalledWith("/api/inbox/actions", expect.anything());
  });

  it("deletes without asking outside Trash", async () => {
    const confirm = vi.spyOn(window, "confirm");
    const user = userEvent.setup();
    renderAt("/");
    await user.click(await screen.findByRole("checkbox", { name: "Select email First" }));
    await user.click(within(screen.getByRole("toolbar", { name: "Selected messages" })).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(postJSON).toHaveBeenCalledWith("/api/inbox/actions", expect.objectContaining({ action: "delete" })));
    expect(confirm).not.toHaveBeenCalled();
  });

  it("fetches each selected message's body before printing", async () => {
    const user = userEvent.setup();
    renderAt("/");
    await user.click(await screen.findByRole("checkbox", { name: "Select email First" }));
    await user.click(within(screen.getByRole("toolbar", { name: "Selected messages" })).getByRole("button", { name: "Print" }));
    await waitFor(() => expect(urls()).toContainEqual(expect.stringMatching(/^\/api\/mail\/body\?messageId=m1/)));
    await waitFor(() => expect(document.querySelector("iframe.print-frame")?.getAttribute("srcdoc") ?? "").toContain("body of m1"));
  });
});
