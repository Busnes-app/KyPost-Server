import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

// A sealed compose snapshot behind a locked vault is offered for restore, not
// forced: the user may dismiss the prompt and write something new, and a later
// unlock (here through Save Draft) must leave that typing alone. The snapshot
// itself survives the whole time.

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

const vault = vi.hoisted(() => ({ locked: true, custody: "client" as "client" | "other" | "unknown" }));
vi.mock("./lib/pgpSession", () => ({
  subscribePGPSession: () => () => {},
  loadPGPSession: async () => ({ bootstrap: null, unlocked: false }),
  clearPGPSession: () => {},
  isClientProtected: () => vault.custody === "client",
  pgpCustody: () => vault.custody,
  needsUnlock: () => vault.locked,
  accountAddress: () => "me@example.com",
  unlockPGPSession: async () => {}
}));

// A reversible stand-in for the seal, so the test can plant a snapshot.
vi.mock("./lib/pgpClient", () => ({
  sealToSelf: async (text: string) => "sealed:" + btoa(text),
  openSealedToSelf: async (armored: string) => atob(armored.slice("sealed:".length)),
  buildEncryptedDraft: async () => "ciphertext",
  buildEncryptedDeliveries: vi.fn(),
  buildEncryptedSentCopy: vi.fn(),
  encryptedAttachmentBudget: () => 1 << 20,
  decryptMessage: vi.fn(),
  verifySignedMessage: vi.fn(),
  OUTER_PLACEHOLDER_SUBJECT: "[Encrypted] Email Sent by KyPost"
}));

vi.mock("./components/PgpUnlockDialog", () => ({
  PgpUnlockDialog: (props: { open: boolean; onUnlocked: () => void; onCancel: () => void }) =>
    props.open ? (
      <div>
        <button type="button" onClick={() => { vault.locked = false; props.onUnlocked(); }}>Unlock key</button>
        <button type="button" onClick={props.onCancel}>Not now</button>
      </div>
    ) : null
}));

import { App } from "./App";

const KEY = "kypost-compose-draft:u1";

function plantSnapshot(subject: string) {
  const fields = { to: "a@b.test", cc: "", bcc: "", subject, body: "<p>saved</p>", attachmentNames: [] };
  window.sessionStorage.setItem(
    KEY,
    JSON.stringify({ version: 2, savedAt: new Date().toISOString(), sealed: "sealed:" + btoa(JSON.stringify(fields)) })
  );
}

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
});

beforeEach(() => {
  vi.clearAllMocks();
  vault.locked = true;
  vault.custody = "client";
  getJSON.mockImplementation(async (url: string) => {
    if (url === "/api/auth/me") return { authenticated: true, userId: "u1", username: "gwen", role: "user" };
    if (url.startsWith("/api/inbox/folders")) return { folders: [] };
    if (url.startsWith("/api/contacts/search")) return { contacts: [] };
    if (url.startsWith("/api/sendas")) return { aliases: [] };
    return {};
  });
});

async function openCompose() {
  const user = userEvent.setup();
  render(
    <MemoryRouter initialEntries={["/read"]}>
      <App />
    </MemoryRouter>
  );
  await user.click(await screen.findByRole("button", { name: "New Email" }));
  await screen.findByText("An unsent draft is waiting. Unlock your PGP key to restore it.");
  return user;
}

describe("save draft before the PGP bootstrap has answered", () => {
  it("refuses rather than posting the draft in cleartext", async () => {
    vault.custody = "unknown";
    vault.locked = false;
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/read"]}>
        <App />
      </MemoryRouter>
    );
    await user.click(await screen.findByRole("button", { name: "New Email" }));
    await user.type(screen.getByLabelText("To recipients"), "a@b.test{Enter}");
    await user.type(screen.getByPlaceholderText("Subject"), "secret subject");
    await user.click(screen.getByRole("button", { name: "Save Draft" }));

    await screen.findByText(/could not be confirmed/);
    const draftPosts = postJSON.mock.calls.filter(([url]) => url === "/api/mail/draft");
    expect(draftPosts).toEqual([]);
    expect(JSON.stringify(postJSON.mock.calls)).not.toContain("secret subject");
  });
});

describe("sealed compose snapshot behind a locked vault", () => {
  it("restores into a blank window once the key is unlocked", async () => {
    plantSnapshot("from the snapshot");
    const user = await openCompose();
    await user.click(screen.getByRole("button", { name: "Unlock key" }));
    await waitFor(() => {
      expect((screen.getByPlaceholderText("Subject") as HTMLInputElement).value).toBe("from the snapshot");
    });
  });

  it("survives the autosave timer firing on the blank window behind the prompt", async () => {
    plantSnapshot("from the snapshot");
    const user = await openCompose();
    // Real time: the 1 s autosave debounce fires while the prompt is still up.
    await new Promise((resolve) => setTimeout(resolve, 1200));
    expect(window.sessionStorage.getItem(KEY)).not.toBeNull();
    await user.click(screen.getByRole("button", { name: "Unlock key" }));
    await waitFor(() => {
      expect((screen.getByPlaceholderText("Subject") as HTMLInputElement).value).toBe("from the snapshot");
    });
  });

  it("never overwrites typing after the prompt was dismissed, and keeps the snapshot", async () => {
    plantSnapshot("from the snapshot");
    const user = await openCompose();
    await user.click(screen.getByRole("button", { name: "Not now" }));
    await user.type(screen.getByLabelText("To recipients"), "a@b.test{Enter}");
    await user.type(screen.getByPlaceholderText("Subject"), "typed after cancel");

    // Save Draft needs the key and reopens the prompt; unlocking there must
    // not restore the snapshot over what was just typed.
    await user.click(screen.getByRole("button", { name: "Save Draft" }));
    await user.click(await screen.findByRole("button", { name: "Unlock key" }));
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: "Unlock key" })).toBeNull();
    });
    expect((screen.getByPlaceholderText("Subject") as HTMLInputElement).value).toBe("typed after cancel");
    expect(window.sessionStorage.getItem(KEY)).not.toBeNull();
  });
});
