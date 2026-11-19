import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import { createElement } from "react";
import { HttpError } from "../api/client";
import { ComposeWindow } from "../components/ComposeWindow";

// A scheduled send carries its time to the server and, on the browser PGP
// path, into every Date header and the encrypted draft copy.

const postHeaders = vi.fn((_headers: Record<string, string>) => undefined);
const postJSON = vi.fn(async (_url: string, _body: unknown): Promise<Record<string, unknown>> => ({ scheduled: true }));
vi.mock("../api/client", () => ({
  HttpError: class HttpError extends Error {
    constructor(message: string, public status: number, public body: unknown) { super(message); }
  },
  postJSON: (url: string, body: unknown, headers?: Record<string, string>) => { postHeaders(headers ?? {}); return postJSON(url, body); },
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

const getSendDelay = vi.fn(async () => 0);
const cancelScheduled = vi.fn(async (_mailbox: string, _id: string): Promise<{ ok: boolean; warning?: string }> => ({ ok: true }));
vi.mock("../api/scheduled", () => ({
  getSendDelay: () => getSendDelay(),
  cancelScheduled: (mailbox: string, id: string) => cancelScheduled(mailbox, id)
}));

const session: { clientProtected: boolean; custody?: string; locked?: boolean } = { clientProtected: false };
vi.mock("../lib/pgpSession", () => ({
  accountAddress: () => "alice@example.com",
  isClientProtected: () => session.clientProtected,
  needsUnlock: () => session.locked ?? false,
  pgpCustody: () => session.custody ?? (session.clientProtected ? "client" : "none"),
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

/** A promise and its resolver, both ready before anything awaits it. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

afterEach(() => {
  session.custody = undefined;
  session.locked = undefined;
  // Leftover one-shot answers must not leak into the next test.
  getSendDelay.mockReset().mockResolvedValue(0);
  cancelScheduled.mockReset().mockResolvedValue({ ok: true });
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
    expect(request.sendDelay).toBeUndefined();
    expect(request.draftCopy).toBeUndefined();
    expect(buildEncryptedDraft).not.toHaveBeenCalled();
  });

  it("a scheduled send is not delayed on top", async () => {
    session.clientProtected = false;
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail(sendAt));
    const body = postJSON.mock.calls[0][1] as Record<string, unknown>;
    expect(body.sendAt).toBe(sendAt.toISOString());
    expect(body.sendDelay).toBeUndefined();
    expect(getSendDelay).not.toHaveBeenCalled();
  });
});

describe("send delay", () => {
  const heldAnswer = (seconds: number) => ({ scheduled: true, delayed: true, id: "held-1", sendAt: new Date(Date.now() + seconds * 1000).toISOString() });

  it("asks the server to hold a plain send, then Cancel sending reopens it", async () => {
    session.clientProtected = false;
    getSendDelay.mockResolvedValueOnce(20);
    postJSON.mockResolvedValueOnce(heldAnswer(20));
    const result = composeTo(false);
    const before = Date.now();
    await act(() => result.current.sendComposeEmail());
    const body = postJSON.mock.calls[0][1] as Record<string, unknown>;
    expect(body.sendDelay).toBe(true);
    expect(body.sendAt).toBeUndefined();
    const held = result.current.composeHolds[0];
    expect(held.id).toBe("held-1");
    expect(held.endsAt).toBeGreaterThan(before);
    expect(held.endsAt).toBeLessThanOrEqual(Date.now() + 20_000);
    expect(result.current.composeOpen).toBe(false);

    await act(() => result.current.cancelHeldSend("held-1"));
    expect(cancelScheduled).toHaveBeenCalledWith("", "held-1");
    expect(result.current.composeHolds).toEqual([]);
    expect(result.current.composeOpen).toBe(true);
    expect(result.current.composeSubject).toBe("later");
    expect(JSON.stringify(result.current.composeTo)).toContain("bob@example.com");
    expect(result.current.composeNotice).toMatch(/^Sending canceled\. Edit it and send again\. A copy is in Drafts\.$/);
    expect(result.current.composeNotice.toLowerCase()).not.toContain("undo");
  });

  it("with the delay Off sends at once and asks for no hold", async () => {
    session.clientProtected = false;
    postJSON.mockResolvedValueOnce({ ok: true } as never);
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    expect((postJSON.mock.calls[0][1] as Record<string, unknown>).sendDelay).toBeUndefined();
    expect(result.current.composeHolds).toEqual([]);
  });

  it("holds a browser-encrypted send with its encrypted draft, dated now, and restores Encrypt on cancel", async () => {
    session.clientProtected = true;
    getSendDelay.mockResolvedValueOnce(10);
    sendClientEncryptedMail.mockResolvedValueOnce(heldAnswer(10));
    const result = composeTo(true);
    await act(() => result.current.sendComposeEmail());
    const request = sendClientEncryptedMail.mock.calls[0][0];
    expect(request.sendDelay).toBe(true);
    expect(request.sendAt).toBeUndefined();
    expect(request.draftCopy).toBe("draft-ciphertext");
    expect(buildEncryptedDeliveries.mock.calls[0][0].date).toBeUndefined();
    expect(buildEncryptedDraft.mock.calls[0][0].date).toBeUndefined();
    await act(() => result.current.cancelHeldSend("held-1"));
    expect(result.current.composeEncrypt).toBe(true);
  });

  it("says it is too late when release already started", async () => {
    session.clientProtected = false;
    getSendDelay.mockResolvedValueOnce(10);
    postJSON.mockResolvedValueOnce(heldAnswer(10));
    cancelScheduled.mockRejectedValueOnce(new HttpError("request failed: 409", 409, {}));
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    await act(() => result.current.cancelHeldSend("held-1"));
    expect(result.current.composeHolds[0].late).toBe(true);
    expect(result.current.composeOpen).toBe(false);
  });

  it("keeps a bar for every message still held", async () => {
    session.clientProtected = false;
    getSendDelay.mockResolvedValue(60);
    postJSON.mockResolvedValueOnce({ ...heldAnswer(60), id: "a" }).mockResolvedValueOnce({ ...heldAnswer(60), id: "b" });
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    act(() => {
      result.current.setComposeTo({ tokens: [], draft: "carol@example.com" });
      result.current.setComposeSubject("second");
    });
    await act(() => result.current.sendComposeEmail());
    expect(result.current.composeHolds.map((h) => h.id)).toEqual(["a", "b"]);
    await act(() => result.current.cancelHeldSend("b"));
    expect(result.current.composeHolds.map((h) => h.id)).toEqual(["a"]);
    expect(result.current.composeSubject).toBe("second");
  });

  it("never reopens a canceled message after sign-out, even for the next user", async () => {
    session.clientProtected = false;
    getSendDelay.mockResolvedValueOnce(30);
    postJSON.mockResolvedValueOnce(heldAnswer(30));
    const cancel = deferred<{ ok: boolean }>();
    cancelScheduled.mockImplementationOnce(() => cancel.promise);
    const { result, rerender } = renderHook(({ userId }) => useCompose({ userId, mailboxes: [] }), { initialProps: { userId: "u1" } });
    act(() => {
      result.current.setComposeTo({ tokens: [], draft: "bob@example.com" });
      result.current.setComposeSubject("private plans");
    });
    await act(() => result.current.sendComposeEmail());
    let canceling: Promise<void> = Promise.resolve();
    act(() => { canceling = result.current.cancelHeldSend("held-1"); });
    await waitFor(() => expect(cancelScheduled).toHaveBeenCalledTimes(1));
    act(() => result.current.resetHeldSends());
    rerender({ userId: "u2" });
    await act(async () => { cancel.resolve({ ok: true }); await canceling; });
    expect(result.current.composeOpen).toBe(false);
    expect(result.current.composeSubject).toBe("");
    expect(result.current.composeHolds).toEqual([]);
    for (let i = 0; i < window.sessionStorage.length; i++) {
      expect(window.sessionStorage.getItem(window.sessionStorage.key(i)!)).not.toContain("private plans");
    }
  });

  it("drops a send whose sign-in changed while it was pending, leaving compose alone", async () => {
    session.clientProtected = false;
    const delay = deferred<number>();
    getSendDelay.mockImplementationOnce(() => delay.promise);
    const { result, rerender } = renderHook(({ userId }) => useCompose({ userId, mailboxes: [] }), { initialProps: { userId: "u1" } });
    act(() => {
      result.current.setComposeTo({ tokens: [], draft: "bob@example.com" });
      result.current.setComposeSubject("private plans");
    });
    let sending: Promise<void> = Promise.resolve();
    act(() => { sending = result.current.sendComposeEmail(); });
    // The delay is read after earlier awaits; switch accounts only while it is pending.
    await waitFor(() => expect(getSendDelay).toHaveBeenCalledTimes(1));
    act(() => result.current.resetHeldSends());
    rerender({ userId: "u2" });
    await act(async () => { delay.resolve(0); await sending; });
    expect(postJSON).not.toHaveBeenCalled();
    expect(result.current.composeError).toBe("");
    expect(result.current.composeSuccess).toBe("");
    expect(result.current.composeHolds).toEqual([]);
  });

  it("names the account that pressed Send on the send request", async () => {
    session.clientProtected = false;
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    expect(postHeaders.mock.calls[0][0]).toMatchObject({ "X-KyPost-User": "u1" });
  });

  it("a secure-link send is not held, so it needs no unlock or encrypted draft", async () => {
    session.clientProtected = false;
    session.custody = "unknown";
    getSendDelay.mockResolvedValueOnce(30);
    const result = composeTo(true);
    act(() => result.current.setComposeSendLinkForKeyless(true));
    await act(() => result.current.sendComposeEmail());
    expect(result.current.composeError).toBe("");
    const body = postJSON.mock.calls[0][1] as Record<string, unknown>;
    expect(body.allowPickupFallback).toBe(true);
    expect(body.sendDelay).toBeUndefined();
    expect(body.pgpDraft).toBeUndefined();
    expect(buildEncryptedDraft).not.toHaveBeenCalled();
  });

  it("with the key locked, shows only the unlock prompt and holds the send once unlocked", async () => {
    session.clientProtected = false;
    session.custody = "client";
    session.locked = true;
    getSendDelay.mockResolvedValue(10);
    postJSON.mockResolvedValueOnce(heldAnswer(10));
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    expect(postJSON).not.toHaveBeenCalled();
    expect(result.current.composeUnlockOpen).toBe(true);
    expect(result.current.composeError).toBe("");
    session.locked = false;
    act(() => result.current.settleSendAfterUnlock(true));
    await waitFor(() => expect(result.current.composeHolds).toHaveLength(1));
    expect((postJSON.mock.calls[0][1] as Record<string, unknown>).sendDelay).toBe(true);
    expect(postJSON).toHaveBeenCalledTimes(1);
    expect(result.current.composeError).toBe("");
  });

  it("resumes with what compose holds when unlocked, not what it held at Send", async () => {
    session.clientProtected = false;
    session.custody = "client";
    session.locked = true;
    getSendDelay.mockResolvedValue(10);
    postJSON.mockResolvedValueOnce(heldAnswer(10));
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    const settle = result.current.settleSendAfterUnlock; // as the dialog captured it at submit
    act(() => result.current.setComposeTo({ tokens: [], draft: "carol@example.com" }));
    session.locked = false;
    act(() => settle(true));
    await waitFor(() => expect(postJSON).toHaveBeenCalledTimes(1));
    expect((postJSON.mock.calls[0][1] as Record<string, unknown>).to).toBe("carol@example.com");
  });

  it("a canceled unlock sends nothing and says so", async () => {
    session.clientProtected = true;
    session.locked = true;
    getSendDelay.mockResolvedValue(10);
    const result = composeTo(true);
    await act(() => result.current.sendComposeEmail());
    act(() => result.current.settleSendAfterUnlock(false));
    expect(result.current.composeError).toBe("Not sent: your PGP key is still locked.");
    act(() => result.current.settleSendAfterUnlock(true));
    expect(sendClientEncryptedMail).not.toHaveBeenCalled();
  });

  it("sends nothing when the delay cannot be read", async () => {
    session.clientProtected = false;
    getSendDelay.mockRejectedValueOnce(new Error("The Send delay setting could not be read. Reload and try again."));
    const result = composeTo(false);
    await act(() => result.current.sendComposeEmail());
    expect(postJSON).not.toHaveBeenCalled();
    expect(result.current.composeError).toBe("Send failed: The Send delay setting could not be read. Reload and try again.");
  });
});

describe("compose error labels", () => {
  it.each([false, true])("keeps compose open with the recipient correction (encrypted=%s)", async (encrypted) => {
    session.clientProtected = encrypted;
    const message = "The mail server rejected bob@example.com: the recipient domain could not be found. Check the recipient address and try again.";
    const error = new HttpError("request failed: 422", 422, { code: "invalid_recipient", error: message });
    if (encrypted) sendClientEncryptedMail.mockRejectedValueOnce(error);
    else postJSON.mockRejectedValueOnce(error);
    const result = composeTo(encrypted);
    act(() => {
      result.current.openComposeWindow();
      result.current.setComposeTo({ tokens: [], draft: "bob@example.com" });
      result.current.setComposeSubject("later");
      result.current.setComposeEncrypt(encrypted);
    });
    await act(() => result.current.sendComposeEmail());
    expect(result.current.composeError).toBe(`Send failed: ${message}`);
    expect(result.current.composeOpen).toBe(true);
    expect(result.current.composeSubject).toBe("later");
    expect(result.current.composeTo.draft).toBe("bob@example.com");
  });

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
