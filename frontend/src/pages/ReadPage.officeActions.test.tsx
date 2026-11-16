import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// Open and Edit in KyDrive appear for a viewable attachment of plain mail
// only: never for an encrypted message, never when KyDrive has no viewer.

const getJSON = vi.fn();
vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  getBytes: () => Promise.resolve(new Uint8Array()),
  mailboxHeaders: (id: string) => ({ "X-KyPost-Mailbox": id }),
  selectedMailboxId: () => "",
  postJSON: () => Promise.resolve({ ok: true, results: [], failed: [] }),
  toErrorMessage: (_e: unknown, fallback: string) => fallback,
  HttpError: class extends Error {}
}));

vi.mock("../lib/pgpSession", () => ({
  isClientProtected: () => false,
  needsUnlock: () => false,
  subscribePGPSession: () => () => {}
}));

const drive = vi.hoisted(() => ({ status: { available: true, viewer: true, appUrl: "https://drive.example/", origin: "https://drive.example" } as Record<string, unknown> }));
vi.mock("../api/kydrive", async (original) => ({ ...(await original<typeof import("../api/kydrive")>()), loadDriveStatus: () => Promise.resolve(drive.status) }));

import { ReadPage } from "./ReadPage";

afterEach(cleanup);

const base = { sender: "a@example.com", sentTo: "me@example.com", status: "read", atUtc: "2026-11-14T10:00:00Z", hasAttachments: true };
const plain = { ...base, messageId: "1", subject: "Plain plan" };
const sealed = { ...base, messageId: "2", subject: "Sealed plan", pgpEncrypted: true, pgpDecryptError: "locked" };
const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document";

beforeEach(() => {
  vi.clearAllMocks();
  drive.status = { available: true, viewer: true, appUrl: "https://drive.example/", origin: "https://drive.example" };
  getJSON.mockImplementation((url: string) => {
    if (url.startsWith("/api/inbox")) return Promise.resolve({ tabs: ["Primary"], byTab: { Primary: [plain, sealed] } });
    if (url.startsWith("/api/labels")) return Promise.resolve({ configured: [], imap: [] });
    if (url.startsWith("/api/mail/body")) return Promise.resolve({ body: "see attached", bodyMode: "plain" });
    if (url.startsWith("/api/mail/attachments")) {
      return Promise.resolve({ ok: true, attachments: [{ index: 0, name: "plan.docx", mimeType: docx, size: 3 }, { index: 1, name: "page.html", mimeType: "text/html", size: 3 }] });
    }
    return Promise.resolve({});
  });
});

async function open(subject: string) {
  render(<MemoryRouter initialEntries={["/"]}><ReadPage onOpenDraft={() => {}} /></MemoryRouter>);
  await userEvent.setup().click(await screen.findByText(subject));
  await screen.findByRole("button", { name: "Save plan.docx to KyDrive" });
}

it("offers Open and Edit for a viewable attachment of plain mail", async () => {
  await open("Plain plan");
  expect(screen.getByRole("button", { name: "Open plan.docx read-only" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Edit plan.docx in KyDrive" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Open page.html read-only" })).toBeNull();
});

it("never offers them on encrypted mail", async () => {
  await open("Sealed plan");
  expect(screen.queryByRole("button", { name: "Open plan.docx read-only" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Edit plan.docx in KyDrive" })).toBeNull();
});

it("hides them when KyDrive has no viewer", async () => {
  drive.status = { available: true, appUrl: "https://drive.example/", origin: "https://drive.example" };
  await open("Plain plan");
  expect(screen.queryByRole("button", { name: "Open plan.docx read-only" })).toBeNull();
});

it("hides everything when KyDrive is not paired", async () => {
  drive.status = { available: false };
  render(<MemoryRouter initialEntries={["/"]}><ReadPage onOpenDraft={() => {}} /></MemoryRouter>);
  await userEvent.setup().click(await screen.findByText("Plain plan"));
  await screen.findByText(/plan\.docx/);
  expect(screen.queryByRole("button", { name: /plan\.docx/ })).toBeNull();
});
