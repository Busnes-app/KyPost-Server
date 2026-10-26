import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, renderHook, screen } from "@testing-library/react";
import { createElement } from "react";
import { ComposeWindow } from "../components/ComposeWindow";

// A scheduled send carries its time to the server and, on the browser PGP
// path, into every Date header and the encrypted draft copy.

const postJSON = vi.fn(async (_url: string, _body: unknown) => ({ scheduled: true }));
vi.mock("../api/client", () => ({
  HttpError: class HttpError extends Error {},
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback),
  selectedMailboxId: () => "",
  mailboxHeaders: () => ({})
}));

const sendClientEncryptedMail = vi.fn(async (_req: Record<string, unknown>) => ({ scheduled: true }));
vi.mock("../api/pgp", () => ({
  checkPGPRecipients: async () => ({ results: [] }),
  createSealedPickup: vi.fn(),
  getPGPDiscoverySettings: async () => null,
  resolveRecipientKeys: async (addresses: string[]) => ({ results: addresses.map((address) => ({ address, usable: true, publicKey: "key", tier: "verified" })) }),
  sendClientEncryptedMail: (req: Record<string, unknown>) => sendClientEncryptedMail(req)
}));
vi.mock("../api/sendas", () => ({ listSendAsAliases: async () => [] }));

const session = { clientProtected: false };
vi.mock("../lib/pgpSession", () => ({
  accountAddress: () => "alice@example.com",
  isClientProtected: () => session.clientProtected,
  needsUnlock: () => false,
  pgpCustody: () => (session.clientProtected ? "client" : "none"),
  pgpSessionState: () => ({ bootstrap: { keyring: { materialGeneration: 1 } } }),
  subscribePGPSession: () => () => {}
}));
vi.mock("../lib/keyVault", () => ({ isUnlocked: () => true }));

const buildEncryptedDeliveries = vi.fn(async (_envelope: { date?: Date }) => [{ recipients: ["bob@example.com"], ciphertext: "wire" }]);
const buildEncryptedDraft = vi.fn(async (_envelope: { date?: Date }) => "draft-ciphertext");
const sealToSelf = vi.fn(async (_text: string): Promise<string> => "sealed-test");
vi.mock("../lib/pgpClient", () => ({
  buildEncryptedDeliveries: (envelope: { date?: Date }) => buildEncryptedDeliveries(envelope),
  buildEncryptedDraft: (envelope: { date?: Date }) => buildEncryptedDraft(envelope),
  buildEncryptedSentCopy: async () => "sent-ciphertext",
  buildSignedDelivery: vi.fn(),
  sealToSelf: (text: string) => sealToSelf(text),
  encryptedAttachmentBudget: () => 1 << 20,
  OUTER_PLACEHOLDER_SUBJECT: "[Encrypted] Email Sent by KyPost"
}));

import { useCompose } from "./useCompose";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
  window.sessionStorage.clear();
});

function composeTo(encrypt: boolean) {
  const { result } = renderHook(() => useCompose({ userId: "u1", mailboxes: [] }));
  act(() => {
    result.current.setComposeTo({ tokens: [], draft: "bob@example.com" });
    result.current.setComposeSubject("later");
    result.current.setComposeEncrypt(encrypt);
  });
  return result;
}

describe("scheduled send", () => {
  const sendAt = new Date(Date.now() + 2 * 3_600_000);

  it("sends sendAt to /api/mail/send", async () => {
    session.clientProtected = false;
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail(sendAt));
    expect(postJSON).toHaveBeenCalledTimes(1);
    const [url, body] = postJSON.mock.calls[0] as [string, Record<string, unknown>];
    expect(url).toBe("/api/mail/send");
    expect(body.sendAt).toBe(sendAt.toISOString());
    expect(body.pgpDraft).toBeUndefined();
    expect(result.current.composeSuccess).toMatch(/^Scheduled for/);
  });

  it("gives a client-custody plain send an encrypted draft", async () => {
    session.clientProtected = true;
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail(sendAt));
    const body = postJSON.mock.calls[0][1] as Record<string, unknown>;
    expect(body.sendAt).toBe(sendAt.toISOString());
    expect(body.pgpDraft).toBe("draft-ciphertext");
  });

  it("dates browser-encrypted deliveries and the draft copy at sendAt", async () => {
    session.clientProtected = true;
    const result = composeTo(true);
    await act(() => result.current.sendComposeEmail(sendAt));
    expect(buildEncryptedDeliveries.mock.calls[0][0].date).toBe(sendAt);
    expect(buildEncryptedDraft.mock.calls[0][0].date).toBe(sendAt);
    const request = sendClientEncryptedMail.mock.calls[0][0];
    expect(request.sendAt).toBe(sendAt.toISOString());
    expect(request.draftCopy).toBe("draft-ciphertext");
  });

  it("an immediate send carries no schedule", async () => {
    session.clientProtected = true;
    const result = composeTo(true);
    await act(() => result.current.sendComposeEmail());
    expect(buildEncryptedDeliveries.mock.calls[0][0].date).toBeUndefined();
    const request = sendClientEncryptedMail.mock.calls[0][0];
    expect(request.sendAt).toBeUndefined();
    expect(request.draftCopy).toBeUndefined();
    expect(buildEncryptedDraft).not.toHaveBeenCalled();
  });
});

describe("compose error labels", () => {
  it("labels a failed draft save as a draft failure, not a send failure", async () => {
    session.clientProtected = false;
    postJSON.mockRejectedValueOnce(new Error("IMAP is not configured"));
    const result = composeTo(false);
    await act(() => result.current.saveComposeDraft());
    expect(postJSON.mock.calls[0][0]).toBe("/api/mail/draft");
    expect(result.current.composeError).toBe("Draft not saved: IMAP is not configured");
  });

  it("labels a failed send as a send failure", async () => {
    session.clientProtected = false;
    postJSON.mockRejectedValueOnce(new Error("SMTP refused"));
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    expect(result.current.composeError).toBe("Send failed: SMTP refused");
  });
});

describe("autosave failure notice", () => {
  it("warns on sealing failure and clears only after a successful save", async () => {
    vi.useFakeTimers();
    session.clientProtected = true;
    sealToSelf.mockRejectedValueOnce(new Error("RSA key is too weak"));
    const { result } = renderHook(() => useCompose({ userId: "autosave", mailboxes: [] }));
    act(() => {
      result.current.openComposeWindow();
      result.current.setComposeSubject("Keep this private text");
    });
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(result.current.composeAutosaveFailed).toBe(true);
    expect(result.current.composeSubject).toBe("Keep this private text");
    expect(window.sessionStorage.getItem("kypost-compose-draft:autosave")).toBeNull();
    const view = render(createElement(ComposeWindow, { compose: result.current, mailboxes: [] }));
    const notice = screen.getByText(/Autosave unavailable/);
    expect(notice.closest("dialog")?.open).toBe(true);
    expect(getComputedStyle(notice).display).not.toBe("none");
    expect(notice.closest('[role="status"]')).not.toBeNull();
    act(() => result.current.setComposeSubject("Supported save"));
    await act(() => vi.advanceTimersByTimeAsync(1000));
    expect(result.current.composeAutosaveFailed).toBe(false);
    view.rerender(createElement(ComposeWindow, { compose: result.current, mailboxes: [] }));
    expect(screen.queryByText(/Autosave unavailable/)).toBeNull();
  });

  it("ignores a late failure after the compose window closes", async () => {
    vi.useFakeTimers();
    session.clientProtected = true;
    let fail: (reason: Error) => void = () => {};
    sealToSelf.mockImplementationOnce(() => new Promise((_resolve, reject) => { fail = reject; }));
    const { result } = renderHook(() => useCompose({ userId: "autosave", mailboxes: [] }));
    act(() => {
      result.current.openComposeWindow();
      result.current.setComposeSubject("Pending save");
    });
    await act(() => vi.advanceTimersByTimeAsync(1000));
    act(() => result.current.closeComposeWindow());
    await act(async () => { fail(new Error("late refusal")); });
    expect(result.current.composeAutosaveFailed).toBe(false);
  });
});
