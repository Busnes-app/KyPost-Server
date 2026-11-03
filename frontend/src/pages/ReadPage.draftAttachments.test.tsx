import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// An ordinary saved draft keeps its attachments as parts of the stored
// message. Reopening it must hand them to the composer, or Send goes out
// without them and nothing says so.

const getJSON = vi.fn();
const getBytes = vi.fn();
const postJSON = vi.fn();
vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  getBytes: (url: string) => getBytes(url),
  mailboxHeaders: (id: string) => ({ "X-KyPost-Mailbox": id }),
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

afterEach(cleanup);

const draft = {
  messageId: "42",
  sender: "me@example.com",
  sentTo: "a@example.com",
  subject: "Invoice draft",
  status: "read",
  atUtc: "2026-09-14T10:00:00Z",
  hasAttachments: true
};

beforeEach(() => {
  vi.clearAllMocks();
  getJSON.mockImplementation((url: string) => {
    if (url.startsWith("/api/inbox")) return Promise.resolve({ tabs: ["Primary"], byTab: { Primary: [draft] } });
    if (url.startsWith("/api/labels")) return Promise.resolve({ configured: [], imap: [] });
    if (url.startsWith("/api/mail/body")) return Promise.resolve({ body: "see attached", bodyMode: "plain" });
    if (url.startsWith("/api/mail/attachments")) {
      return Promise.resolve({ ok: true, attachments: [{ index: 1, name: "invoice.pdf", mimeType: "application/pdf", size: 3 }] });
    }
    return Promise.resolve({});
  });
  getBytes.mockResolvedValue(new TextEncoder().encode("pdf"));
  postJSON.mockResolvedValue({ ok: true, results: [], failed: [] });
});

function renderDrafts(onOpenDraft: (p: unknown) => void) {
  return render(
    <MemoryRouter initialEntries={["/?mailbox=Drafts"]}>
      <ReadPage onOpenDraft={onOpenDraft} />
    </MemoryRouter>
  );
}

describe("opening an ordinary draft", () => {
  it("restores its attachments", async () => {
    const onOpenDraft = vi.fn();
    const user = userEvent.setup();
    renderDrafts(onOpenDraft);

    await user.click(await screen.findByText("Invoice draft"));

    await waitFor(() => expect(onOpenDraft).toHaveBeenCalledTimes(1));
    expect(onOpenDraft.mock.calls[0][0]).toMatchObject({
      subject: "Invoice draft",
      attachments: [{ name: "invoice.pdf", mimeType: "application/pdf", dataBase64: btoa("pdf"), size: 3 }]
    });
    expect(getBytes).toHaveBeenCalledWith(expect.stringMatching(/^\/api\/mail\/attachment\?messageId=42&mailbox=Drafts&index=1$/));
  });

  it("does not open a partial draft when an attachment cannot be fetched", async () => {
    getBytes.mockRejectedValue(new Error("gone"));
    const onOpenDraft = vi.fn();
    const user = userEvent.setup();
    renderDrafts(onOpenDraft);

    await user.click(await screen.findByText("Invoice draft"));

    expect(await screen.findByText(/could not load this draft's attachments/)).toBeTruthy();
    expect(onOpenDraft).not.toHaveBeenCalled();
  });
});
