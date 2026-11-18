import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// Conversation view: one list row per conversation whose actions cover all of
// it, and a reader whose actions cover only the open message.

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

const mail = (messageId: string, sender: string, subject: string, atUtc: string, status = "read") => ({ messageId, sender, subject, status, atUtc });
const rows = [
  mail("2", "Bob <bob@example.com>", "Re: Plan", "2026-11-01T12:00:00Z", "unread"),
  mail("9", "Carol <carol@example.com>", "Invoice", "2026-11-01T11:00:00Z"),
  mail("1", "Alice <alice@example.com>", "Plan", "2026-11-01T09:00:00Z")
];
const conversations = [
  { id: "1", messageIds: ["1", "2"], sent: [{ ...mail("s1", "Me <me@example.com>", "Re: Plan", "2026-11-01T10:00:00Z"), mailbox: "Sent" }] }
];

let grouping = true;
beforeEach(() => {
  vi.clearAllMocks();
  grouping = true;
  getJSON.mockImplementation((url: string) => {
    if (url === "/api/settings/mail") return Promise.resolve({ conversations: grouping });
    if (url.startsWith("/api/inbox/conversations")) return Promise.resolve({ conversations });
    if (url.startsWith("/api/inbox?")) return Promise.resolve({ tabs: ["Primary"], byTab: { Primary: rows }, cursor: 1 });
    if (url.startsWith("/api/mail/body")) return Promise.resolve({ body: `body of ${new URLSearchParams(url.split("?")[1]).get("messageId")}`, bodyMode: "plain" });
    return Promise.resolve({ configured: [], imap: [] });
  });
  postJSON.mockResolvedValue({ ok: true, results: [], failed: [] });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function renderPage() {
  render(
    <MemoryRouter initialEntries={["/"]}>
      <ReadPage />
    </MemoryRouter>
  );
}

const actionCalls = () => postJSON.mock.calls.filter(([url]) => url === "/api/inbox/actions").map(([, body]) => body as { action: string; messageIds: string[] });

describe("conversation view", () => {
  it("lists a conversation once, labelled with its message count, and acts on all of it", async () => {
    const user = userEvent.setup();
    renderPage();
    const select = await screen.findByRole("checkbox", { name: "Select conversation Re: Plan, 3 messages" });
    expect(screen.getByRole("button", { name: /^Conversation, 3 messages\.\s*Alice, me, Bob/ })).toBeDefined();
    expect(screen.queryByRole("checkbox", { name: /Select email Plan/ })).toBeNull();
    expect(screen.getByRole("checkbox", { name: "Select email Invoice" })).toBeDefined();
    await user.click(select);
    await user.click(within(screen.getByRole("toolbar", { name: "Selected messages" })).getByRole("button", { name: "Archive" }));
    // Your reply stays in Sent: only this folder's members move.
    await waitFor(() => expect(actionCalls()).toContainEqual(expect.objectContaining({ action: "archive", messageIds: ["1", "2"] })));
  });

  it("opens the newest unread message, stacks the rest, and acts on the open one alone", async () => {
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole("button", { name: /^Conversation, 3 messages\./ }));
    await waitFor(() => expect(getJSON).toHaveBeenCalledWith(expect.stringMatching(/^\/api\/mail\/body\?messageId=2/)));
    expect(await screen.findByText("3 messages in this conversation")).toBeDefined();
    const older = screen.getByRole("button", { name: /^Open message from Alice/ });
    const reply = screen.getByLabelText(/^Your reply/);
    expect(older.compareDocumentPosition(reply) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(actionCalls()).toContainEqual(expect.objectContaining({ action: "delete", messageIds: ["2"] })));
  });

  it("shows your reply in place from Sent without moving it", async () => {
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole("button", { name: /^Conversation, 3 messages\./ }));
    const details = (await screen.findByLabelText(/^Your reply/)).closest("details")!;
    details.open = true;
    details.dispatchEvent(new Event("toggle"));
    await waitFor(() => expect(getJSON).toHaveBeenCalledWith("/api/mail/body?messageId=s1&mailbox=Sent"));
    expect(await screen.findByText("body of s1")).toBeDefined();
    expect(actionCalls().some((c) => c.messageIds.includes("s1"))).toBe(false);
  });

  it("lists every message on its own when the user turned grouping off", async () => {
    grouping = false;
    renderPage();
    expect(await screen.findByRole("checkbox", { name: "Select email Plan" })).toBeDefined();
    expect(screen.getByRole("checkbox", { name: "Select email Re: Plan" })).toBeDefined();
    expect(getJSON.mock.calls.some(([url]) => String(url).startsWith("/api/inbox/conversations"))).toBe(false);
  });
});
