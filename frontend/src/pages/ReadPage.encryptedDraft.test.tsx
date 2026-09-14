import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// An encrypted draft is opened the way an encrypted message is read: decrypted
// here, with its recipients and subject taken from the protected headers inside
// the ciphertext and its attachments handed to the composer as bytes this
// browser decoded. The server's copy of the row carries a placeholder subject
// and no body, and nothing here asks it for either.

const getJSON = vi.fn();
const postJSON = vi.fn();
vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  toErrorMessage: (_e: unknown, fallback: string) => fallback
}));

const getPGPMessagePayload = vi.fn();
vi.mock("../api/pgp", () => ({
  getPGPMessagePayload: (mailbox: string, id: string) => getPGPMessagePayload(mailbox, id)
}));

const decryptMessage = vi.fn();
vi.mock("../lib/pgpClient", () => ({
  decryptMessage: (...args: unknown[]) => decryptMessage(...args),
  verifySignedMessage: vi.fn()
}));

const vault = vi.hoisted(() => ({ locked: false }));
vi.mock("../lib/pgpSession", () => ({
  isClientProtected: () => true,
  needsUnlock: () => vault.locked,
  subscribePGPSession: () => () => {}
}));

const unlockDialog = vi.fn();
vi.mock("../components/PgpUnlockDialog", () => ({
  PgpUnlockDialog: (props: { open: boolean }) => {
    unlockDialog(props.open);
    return null;
  }
}));

import { ReadPage } from "./ReadPage";

afterEach(cleanup);

const encryptedDraft = {
  messageId: "42",
  sender: "me@example.com",
  sentTo: "outer@example.com",
  subject: "[Encrypted] Email Sent by KyPost",
  status: "read",
  atUtc: "2026-09-14T10:00:00Z",
  pgpEncrypted: true
};

beforeEach(() => {
  vi.clearAllMocks();
  vault.locked = false;
  getJSON.mockImplementation((url: string) => {
    if (url.startsWith("/api/inbox")) return Promise.resolve({ tabs: ["Primary"], byTab: { Primary: [encryptedDraft] } });
    if (url.startsWith("/api/labels")) return Promise.resolve({ configured: [], imap: [] });
    return Promise.resolve({});
  });
  postJSON.mockResolvedValue({ ok: true, results: [] });
  getPGPMessagePayload.mockResolvedValue({ messageId: 42, mailbox: "Drafts", encryptedPayload: "cipher", signerKeys: [] });
});

function renderDrafts(onOpenDraft: (p: unknown) => void) {
  return render(
    <MemoryRouter initialEntries={["/?mailbox=Drafts"]}>
      <ReadPage onOpenDraft={onOpenDraft} />
    </MemoryRouter>
  );
}

describe("opening an encrypted draft", () => {
  it("restores recipients, subject, body and attachments from the ciphertext", async () => {
    decryptMessage.mockResolvedValue({
      body: "<p>draft body</p>",
      bodyMode: "html",
      signed: false,
      verified: false,
      signerFingerprint: "",
      signerConflict: false,
      attachments: [{ name: "n.txt", mimeType: "text/plain", bytes: new TextEncoder().encode("note") }],
      attachmentsOmitted: 0,
      protectedHeaders: { subject: "Plans", to: "a@example.com", cc: "c@example.com", bcc: "hidden@example.com" }
    });
    const onOpenDraft = vi.fn();
    const user = userEvent.setup();
    renderDrafts(onOpenDraft);

    await user.click(await screen.findByText("[Encrypted] Email Sent by KyPost"));

    await waitFor(() => expect(onOpenDraft).toHaveBeenCalledTimes(1));
    expect(onOpenDraft.mock.calls[0][0]).toEqual({
      sentTo: "a@example.com",
      cc: "c@example.com",
      bcc: "hidden@example.com",
      subject: "Plans",
      body: "<p>draft body</p>",
      attachments: [{ name: "n.txt", mimeType: "text/plain", dataBase64: btoa("note"), size: 4 }]
    });
    expect(getPGPMessagePayload).toHaveBeenCalledWith("Drafts", "42");
    // The server was never asked for a body or an attachment.
    expect(getJSON.mock.calls.map((call) => String(call[0])).filter((u) => /\/api\/mail\/(body|attachment)/.test(u))).toEqual([]);
  });

  it("falls back to the outer recipients when the draft protects only the Subject", async () => {
    decryptMessage.mockResolvedValue({
      body: "plain body",
      bodyMode: "plain",
      signed: false,
      verified: false,
      signerFingerprint: "",
      signerConflict: false,
      attachments: [],
      attachmentsOmitted: 0,
      protectedHeaders: { subject: "Only subject" }
    });
    const onOpenDraft = vi.fn();
    const user = userEvent.setup();
    renderDrafts(onOpenDraft);

    await user.click(await screen.findByText("[Encrypted] Email Sent by KyPost"));

    await waitFor(() => expect(onOpenDraft).toHaveBeenCalledTimes(1));
    expect(onOpenDraft.mock.calls[0][0]).toMatchObject({ sentTo: "outer@example.com", subject: "Only subject", body: "plain body" });
  });

  it("asks for an unlock instead of opening anything while the vault is locked", async () => {
    vault.locked = true;
    const onOpenDraft = vi.fn();
    const user = userEvent.setup();
    renderDrafts(onOpenDraft);

    await user.click(await screen.findByText("[Encrypted] Email Sent by KyPost"));

    await waitFor(() => expect(unlockDialog).toHaveBeenCalledWith(true));
    expect(onOpenDraft).not.toHaveBeenCalled();
    expect(getPGPMessagePayload).not.toHaveBeenCalled();
  });
});
