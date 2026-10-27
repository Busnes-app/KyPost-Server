// Out-of-order responses: each test holds a request open, lets a newer one
// finish, then releases the old one and checks it changed nothing.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useNavigate } from "react-router";

const getJSON = vi.fn();
const postJSON = vi.fn();

vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  selectedMailboxId: () => "",
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  downloadFile: vi.fn(),
  toErrorMessage: (_e: unknown, fallback: string) => fallback
}));

vi.mock("../api/pgp", () => ({ getPGPMessagePayload: vi.fn() }));
vi.mock("../lib/pgpClient", () => ({ decryptMessage: vi.fn(), verifySignedMessage: vi.fn() }));
vi.mock("../lib/pgpSession", () => ({
  isClientProtected: () => false,
  needsUnlock: () => false,
  subscribePGPSession: () => () => {}
}));
vi.mock("../components/PgpUnlockDialog", () => ({ PgpUnlockDialog: () => null }));

import { ReadPage } from "./ReadPage";

afterEach(cleanup);

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

const base = { sender: "s@example.com", body: "body", status: "read", atUtc: "2026-08-11T10:00:00Z" };
const msgA = { ...base, messageId: "1", subject: "Message A", hasAttachments: true };
const msgB = { ...base, messageId: "2", subject: "Message B", hasAttachments: true, keywords: ["alpha", "beta"] };

let routes: Record<string, (url: string) => Promise<unknown>>;

beforeEach(() => {
  vi.clearAllMocks();
  routes = {};
  getJSON.mockImplementation((url: string) => {
    const hit = Object.keys(routes).find((prefix) => url.startsWith(prefix));
    if (hit) return routes[hit](url);
    if (url.startsWith("/api/inbox")) return Promise.resolve({ tabs: ["Primary"], byTab: { Primary: [msgA, msgB] } });
    if (url.startsWith("/api/labels")) return Promise.resolve({ configured: [], imap: [] });
    return Promise.resolve({});
  });
  postJSON.mockResolvedValue({ ok: true, failed: [] });
});

function GoToArchive() {
  const navigate = useNavigate();
  return <button onClick={() => navigate("/read?mailbox=Archive")}>Go to Archive</button>;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/read"]}>
      <GoToArchive />
      <ReadPage />
    </MemoryRouter>
  );
}

describe("ReadPage request ordering", () => {
  it("drops a late attachment list for a message that is no longer open", async () => {
    const slowA = deferred<unknown>();
    routes["/api/mail/attachments"] = (url) =>
      url.includes("messageId=1")
        ? slowA.promise
        : Promise.resolve({ ok: true, attachments: [{ index: 0, name: "b.pdf", size: 1 }] });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByText("Message A"));
    await user.click(screen.getByRole("button", { name: "Close" }));
    await user.click(screen.getByText("Message B"));
    await screen.findByText(/b\.pdf/);

    await act(async () => slowA.resolve({ ok: true, attachments: [{ index: 3, name: "a-secret.pdf", size: 1 }] }));
    expect(screen.queryByText(/a-secret\.pdf/)).toBeNull();
    expect(screen.getByText(/b\.pdf/)).toBeTruthy();
  });

  it("ignores an older search response and one that lands after Clear", async () => {
    const pending: Record<string, ReturnType<typeof deferred<unknown>>> = {};
    routes["/api/mail/search"] = (url) => {
      const q = new URL(url, "http://x").searchParams.get("q") ?? "";
      pending[q] = deferred<unknown>();
      return pending[q].promise;
    };
    const user = userEvent.setup();
    renderPage();
    await screen.findByText("Message A");
    const input = screen.getByRole("textbox", { name: "Search mail" });

    await user.type(input, "old{Enter}");
    await user.clear(input);
    await user.type(input, "new{Enter}");
    await act(async () => pending.new.resolve({ results: [msgB] }));
    await act(async () => pending.old.resolve({ results: [msgA, msgB] }));
    expect(screen.getByText("Found 1 result")).toBeTruthy();

    await user.clear(input);
    await user.type(input, "late{Enter}");
    await user.click(screen.getByRole("button", { name: "Clear" }));
    await act(async () => pending.late.resolve({ results: [msgA] }));
    expect(screen.queryByText(/^Found/)).toBeNull();
  });

  it("drops a search response for the folder the user left", async () => {
    const inboxSearch = deferred<unknown>();
    routes["/api/mail/search"] = () => inboxSearch.promise;
    const user = userEvent.setup();
    renderPage();
    await screen.findByText("Message A");

    await user.type(screen.getByRole("textbox", { name: "Search mail" }), "q{Enter}");
    await user.click(screen.getByRole("button", { name: "Go to Archive" }));
    await waitFor(() => expect(getJSON).toHaveBeenCalledWith(expect.stringMatching(/^\/api\/inbox.*Archive/)));
    await act(async () => inboxSearch.resolve({ results: [msgA] }));

    expect(screen.queryByText(/^Found/)).toBeNull();
  });

  it("keeps both of two quick keyword removals", async () => {
    const calls: Array<ReturnType<typeof deferred<unknown>>> = [];
    postJSON.mockImplementation(() => {
      const d = deferred<unknown>();
      calls.push(d);
      return d.promise;
    });
    routes["/api/mail/attachments"] = () => Promise.resolve({ ok: true, attachments: [] });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByText("Message B"));
    await user.click(screen.getByRole("button", { name: "Remove keyword alpha" }));
    await user.click(screen.getByRole("button", { name: "Remove keyword beta" }));
    await waitFor(() => expect(calls).toHaveLength(2));
    await act(async () => calls[0].resolve({ ok: true, failed: [] }));
    await act(async () => calls[1].resolve({ ok: true, failed: [] }));

    expect(screen.queryByRole("button", { name: /Remove keyword/ })).toBeNull();
  });

  it("prints through a sandboxed frame, never a scriptable popup", async () => {
    const open = vi.spyOn(window, "open");
    routes["/api/mail/attachments"] = () => Promise.resolve({ ok: true, attachments: [] });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByText("Message A"));
    // The reader's own Print, after the toolbar's.
    const prints = screen.getAllByRole("button", { name: "Print", hidden: true });
    await user.click(prints[prints.length - 1]);

    expect(open).not.toHaveBeenCalled();
    const frame = document.querySelector<HTMLIFrameElement>("iframe.print-frame");
    expect(frame).not.toBeNull();
    const sandbox = frame!.getAttribute("sandbox") ?? "";
    expect(sandbox).not.toContain("allow-scripts");
    expect(sandbox).toContain("allow-modals");
    expect(frame!.getAttribute("srcdoc")).toContain("Message A");
  });
});
