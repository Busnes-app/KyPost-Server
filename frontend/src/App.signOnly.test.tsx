import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// Sign without Encrypt on a client-custody account: one signed delivery to
// every recipient, no recipient keys resolved, the Sent copy still ciphertext.

const getJSON = vi.fn();
const postJSON = vi.fn(async (_url: string, _body: unknown) => ({}));
vi.mock("./api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  putJSON: async () => ({}),
  deleteJSON: async () => ({}),
  HttpError: class HttpError extends Error {
    status = 0;
    body: unknown = null;
  },
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback)
}));

const resolveRecipientKeys = vi.fn();
const sendClientEncryptedMail = vi.fn(async (_req: unknown) => ({ ok: true }));
vi.mock("./api/pgp", () => ({
  resolveRecipientKeys: (...args: unknown[]) => resolveRecipientKeys(...args),
  sendClientEncryptedMail: (req: unknown) => sendClientEncryptedMail(req),
  createSealedPickup: vi.fn(),
  getPGPDiscoverySettings: async () => null
}));

vi.mock("./lib/pgpSession", () => ({
  subscribePGPSession: () => () => {},
  loadPGPSession: async () => ({ bootstrap: null, unlocked: true }),
  clearPGPSession: () => {},
  isClientProtected: () => true,
  pgpCustody: () => "client",
  needsUnlock: () => false,
  accountAddress: () => "me@example.com"
}));

const buildSignedDelivery = vi.fn(async (_env: unknown, _ct: string, _body: string, recipients: string[]) => ({ recipients, ciphertext: "signed-bytes" }));
const buildEncryptedDeliveries = vi.fn();
vi.mock("./lib/pgpClient", () => ({
  buildSignedDelivery: (...args: [unknown, string, string, string[]]) => buildSignedDelivery(...args),
  buildEncryptedDeliveries: (...args: unknown[]) => buildEncryptedDeliveries(...args),
  buildEncryptedSentCopy: async () => "sent-ciphertext",
  buildEncryptedDraft: async () => "draft",
  encryptedAttachmentBudget: () => 1 << 20,
  sealToSelf: async (t: string) => t,
  openSealedToSelf: async (t: string) => t,
  decryptMessage: vi.fn(),
  verifySignedMessage: vi.fn(),
  OUTER_PLACEHOLDER_SUBJECT: "[Encrypted] Email Sent by KyPost"
}));

vi.mock("./components/PgpUnlockDialog", () => ({ PgpUnlockDialog: () => null }));

import { App } from "./App";

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
});

beforeEach(() => {
  vi.clearAllMocks();
  getJSON.mockImplementation(async (url: string) => {
    if (url === "/api/auth/me") return { authenticated: true, userId: "u1", username: "gwen", role: "user" };
    if (url.startsWith("/api/inbox/folders")) return { folders: [] };
    if (url.startsWith("/api/sendas")) return { aliases: [] };
    return {};
  });
});

describe("sign without encrypt on a client-custody account", () => {
  it("sends one signed delivery to every recipient without resolving keys", async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/read"]}>
        <App />
      </MemoryRouter>
    );
    await user.click(await screen.findByRole("button", { name: "New Email" }));
    await user.type(screen.getByLabelText("To recipients"), "a@b.test{Enter}");
    await user.type(screen.getByLabelText("Bcc recipients"), "hidden@b.test{Enter}");
    await user.type(screen.getByPlaceholderText("Subject"), "signed only");
    await user.click(screen.getByLabelText("Sign"));
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(sendClientEncryptedMail).toHaveBeenCalledTimes(1));
    expect(resolveRecipientKeys).not.toHaveBeenCalled();
    expect(buildEncryptedDeliveries).not.toHaveBeenCalled();
    const req = sendClientEncryptedMail.mock.calls[0][0] as {
      deliveries: { recipients: string[]; ciphertext: string }[];
      sentCopy: string;
      sentCopyEncrypted: boolean;
    };
    expect(req.deliveries).toEqual([{ recipients: ["a@b.test", "hidden@b.test"], ciphertext: "signed-bytes" }]);
    expect(req.sentCopyEncrypted).toBe(true);
    expect(req.sentCopy).toBe("sent-ciphertext");
    expect(postJSON.mock.calls.some(([url]) => url === "/api/mail/send")).toBe(false);
  });
});
