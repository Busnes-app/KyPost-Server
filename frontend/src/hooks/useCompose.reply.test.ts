import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { loadDraftSnapshot, saveDraftSnapshot } from "../app/draftAutosave";

// A reply whose receiving address is unknown never sends from the primary:
// From stays empty and the send is refused until the user picks one.

const postJSON = vi.fn(async (_url: string, _body: unknown) => ({ scheduled: true }));
vi.mock("../api/client", () => ({
  HttpError: class HttpError extends Error {
    constructor(message: string, public status: number, public body: unknown) { super(message); }
  },
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
// Send delay Off: sends go straight out.
vi.mock("../api/scheduled", () => ({ getSendDelay: async () => 0, cancelScheduled: vi.fn() }));

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
  window.sessionStorage.clear();
});

const mailboxes = [{ id: "", kind: "primary" as const, addresses: ["alice@example.com", "quiet.otter.4821@example.com"] }];

describe("reply identity", () => {
  it("never defaults to the primary when the receiving address is unknown", async () => {
    const { result } = renderHook(() => useCompose({ userId: "u1", mailboxes }));
    act(() => result.current.openDraftInCompose({ from: null, sentTo: "bob@example.com", subject: "Re: hi", intent: "reply", mailbox: "" }));
    expect(result.current.composeFrom).toBe("");
    expect(result.current.composeFromRequired).toBe(true);
    await act(() => result.current.sendComposeEmail());
    expect(postJSON).not.toHaveBeenCalled();
    expect(result.current.composeError).toMatch(/Choose the address to reply from/);
    act(() => result.current.setComposeFrom("quiet.otter.4821@example.com"));
    await act(() => result.current.sendComposeEmail());
    expect(postJSON).toHaveBeenCalledWith("/api/mail/send", expect.objectContaining({ from: "quiet.otter.4821@example.com" }));
  });

  it("answers as the masked address it was delivered to", () => {
    const { result } = renderHook(() => useCompose({ userId: "u1", mailboxes }));
    act(() => result.current.openDraftInCompose({ from: "quiet.otter.4821@example.com", sentTo: "bob@example.com", intent: "reply", mailbox: "" }));
    expect(result.current.composeFrom).toBe("quiet.otter.4821@example.com");
    expect(result.current.composeFromRequired).toBe(false);
  });
});

describe("reply identity through draft recovery", () => {
  it("keeps a required From choice empty and blocking after autosave and restore", async () => {
    const { result } = renderHook(() => useCompose({ userId: "u1", mailboxes }));
    act(() => result.current.openDraftInCompose({ from: null, sentTo: "bob@example.com", subject: "Re: hi", body: "<p>reply</p>", intent: "reply", mailbox: "" }));
    await waitFor(async () => expect(await loadDraftSnapshot("u1")).toMatchObject({ subject: "Re: hi", from: "" }), { timeout: 3000 });
    act(() => result.current.closeComposeWindow());
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(result.current.composeSubject).toBe("Re: hi"));
    expect(result.current.composeFrom).toBe("");
    expect(result.current.composeFromRequired).toBe(true);
    await act(() => result.current.sendComposeEmail());
    expect(postJSON).not.toHaveBeenCalled();
    expect(result.current.composeError).toMatch(/Choose the address to reply from/);
  });

  it("asks again when the saved From is no longer one of the mailbox's addresses", async () => {
    await saveDraftSnapshot("u1", { to: "bob@example.com", cc: "", bcc: "", subject: "Re: shop", body: "<p>x</p>", attachments: [], mailbox: "", from: "paused.fox.1234@example.com" });
    const { result } = renderHook(() => useCompose({ userId: "u1", mailboxes }));
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(result.current.composeSubject).toBe("Re: shop"));
    expect(result.current.composeFrom).toBe("");
    expect(result.current.composeFromRequired).toBe(true);
  });

  it("restores a From that is still the mailbox's", async () => {
    await saveDraftSnapshot("u1", { to: "bob@example.com", cc: "", bcc: "", subject: "Re: kept", body: "<p>x</p>", attachments: [], mailbox: "", from: "quiet.otter.4821@example.com" });
    const { result } = renderHook(() => useCompose({ userId: "u1", mailboxes }));
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(result.current.composeSubject).toBe("Re: kept"));
    expect(result.current.composeFrom).toBe("quiet.otter.4821@example.com");
    expect(result.current.composeFromRequired).toBe(false);
  });
});
