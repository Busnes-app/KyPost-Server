import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";

// A scheduled send carries its time to the server and, on the browser PGP
// path, into every Date header and the encrypted draft copy.

const postJSON = vi.fn(async (_url: string, _body: unknown) => ({ scheduled: true }));
vi.mock("../api/client", () => ({
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
vi.mock("../lib/pgpClient", () => ({
  buildEncryptedDeliveries: (envelope: { date?: Date }) => buildEncryptedDeliveries(envelope),
  buildEncryptedDraft: (envelope: { date?: Date }) => buildEncryptedDraft(envelope),
  buildEncryptedSentCopy: async () => "sent-ciphertext",
  buildSignedDelivery: vi.fn(),
  encryptedAttachmentBudget: () => 1 << 20,
  OUTER_PLACEHOLDER_SUBJECT: "[Encrypted] Email Sent by KyPost"
}));

import { useCompose } from "./useCompose";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
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
