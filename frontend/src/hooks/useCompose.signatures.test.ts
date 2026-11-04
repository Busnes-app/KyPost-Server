import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { createElement } from "react";
import { ComposeWindow } from "../components/ComposeWindow";
import type { SignatureSettings } from "../api/signatures";

// Compose inserts the account's signature for the From address: on a new
// message, on replies and forwards when the account says so, never into a
// saved draft, and it follows a From change unless the user edited it.

vi.mock("../api/client", () => ({
  HttpError: class HttpError extends Error {},
  postJSON: vi.fn(),
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback),
  selectedMailboxId: () => "",
  mailboxHeaders: () => ({})
}));
vi.mock("../api/pgp", () => ({
  checkPGPRecipients: async () => ({ results: [] }),
  createSealedPickup: vi.fn(),
  getPGPDiscoverySettings: async () => null,
  resolveRecipientKeys: vi.fn(),
  sendClientEncryptedMail: vi.fn()
}));
vi.mock("../api/sendas", () => ({ listSendAsAliases: async () => [] }));
vi.mock("../lib/pgpSession", () => ({
  accountAddress: () => "alice@example.com",
  isClientProtected: () => false,
  needsUnlock: () => false,
  pgpCustody: () => "none",
  pgpSessionState: () => ({ bootstrap: null }),
  subscribePGPSession: () => () => {}
}));
vi.mock("../lib/keyVault", () => ({ isUnlocked: () => false }));
vi.mock("../lib/pgpClient", () => ({
  buildEncryptedDeliveries: vi.fn(),
  buildEncryptedDraft: vi.fn(),
  buildEncryptedSentCopy: vi.fn(),
  buildSignedDelivery: vi.fn(),
  sealToSelf: vi.fn(),
  encryptedAttachmentBudget: () => 1 << 20,
  OUTER_PLACEHOLDER_SUBJECT: "[Encrypted] Email Sent by KyPost"
}));

const signatures: { current: SignatureSettings } = {
  current: {
    signatures: [
      { id: "main", name: "Main", text: "Alice <CEO>" },
      { id: "sales", name: "Sales", text: "Alice\nSales team" }
    ],
    defaultId: "main",
    byAddress: { "sales@example.com": "sales" },
    onReply: true,
    onForward: false
  }
};
const getSignatures = vi.fn(async () => signatures.current);
vi.mock("../api/signatures", () => ({ getSignatures: () => getSignatures() }));

import { useCompose, type Compose } from "./useCompose";

const mailboxes = [{ id: "u1", kind: "primary" as const, addresses: ["alice@example.com", "sales@example.com"] }];
const MAIN = "<p><br></p><p>Alice &lt;CEO&gt;</p>";
const SALES = "<p><br></p><p>Alice</p><p>Sales team</p>";
const REPLY = "<p><br /></p><p>On Monday, Bob wrote:</p><blockquote>hi</blockquote>";
// The same body as the editor writes it back.
const REPLY_SHOWN = "<p><br></p><p>On Monday, Bob wrote:</p><blockquote>hi</blockquote>";

beforeEach(() => {
  signatures.current = { ...signatures.current, onReply: true, onForward: false };
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.sessionStorage.clear();
});

// The real window and editor, so the body read back is what Quill serialised.
function compose() {
  const result: { current: Compose } = { current: undefined as unknown as Compose };
  function Harness() {
    result.current = useCompose({ userId: "u1", mailboxes });
    return createElement(ComposeWindow, { compose: result.current, mailboxes });
  }
  render(createElement(Harness));
  return result;
}

function body(): string {
  return document.querySelector(".ql-editor")?.innerHTML ?? "";
}

async function setBody(html: string) {
  const editor = document.querySelector(".ql-editor") as HTMLElement;
  await act(async () => {
    editor.innerHTML = html;
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

describe("compose signatures", () => {
  it("adds the default signature, escaped, below the first line of a new message", async () => {
    const result = compose();
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(body()).toBe(`<p><br></p>${MAIN}`));
    expect(result.current.composeFrom).toBe("alice@example.com");
  });

  it("switches to the From address's own signature, and leaves an edited one alone", async () => {
    const result = compose();
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(body()).toBe(`<p><br></p>${MAIN}`));
    act(() => result.current.setComposeFrom("sales@example.com"));
    await waitFor(() => expect(body()).toBe(`<p><br></p>${SALES}`));

    await setBody("<p>Hi</p><p><br></p><p>Alice, VP Sales</p>");
    act(() => result.current.setComposeFrom("alice@example.com"));
    act(() => result.current.setComposeFrom("sales@example.com"));
    await act(async () => {});
    expect(body()).toBe("<p>Hi</p><p><br></p><p>Alice, VP Sales</p>");
  });

  it("puts a reply's signature above the quote, and leaves forwards alone when switched off", async () => {
    const result = compose();
    act(() => result.current.openDraftInCompose({ intent: "reply", body: REPLY }));
    await waitFor(() =>
      expect(body()).toBe(`<p><br></p>${MAIN}<p><br></p><p>On Monday, Bob wrote:</p><blockquote>hi</blockquote>`)
    );

    act(() => result.current.closeComposeWindow());
    act(() => result.current.openDraftInCompose({ intent: "forward", body: REPLY }));
    await waitFor(() => expect(getSignatures).toHaveBeenCalledTimes(2));
    await act(async () => {});
    expect(body()).toBe(REPLY_SHOWN);
  });

  it("leaves replies alone when switched off", async () => {
    signatures.current = { ...signatures.current, onReply: false };
    const result = compose();
    act(() => result.current.openDraftInCompose({ intent: "reply", body: REPLY }));
    await waitFor(() => expect(getSignatures).toHaveBeenCalledTimes(1));
    await act(async () => {});
    expect(body()).toBe(REPLY_SHOWN);
  });

  it("does not let a draft that finished loading late replace an open window", async () => {
    const result = compose();
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(body()).toContain("Alice"));
    act(() => result.current.openDraftInCompose({ body: "<p>late draft</p>" }));
    await act(async () => {});
    expect(body()).not.toContain("late draft");
  });

  it("never adds one to a saved draft", async () => {
    const result = compose();
    act(() => result.current.openDraftInCompose({ body: "<p>my draft</p>" }));
    await act(async () => {});
    expect(getSignatures).not.toHaveBeenCalled();
    expect(body()).toBe("<p>my draft</p>");
  });

  it("does not autosave a window holding nothing but its signature", async () => {
    const result = compose();
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(body()).toContain("Alice"));
    // Real time: the autosave debounce is 1 s.
    await act(() => new Promise((resolve) => setTimeout(resolve, 1200)));
    expect(window.sessionStorage.length).toBe(0);
  });

  it("composes without a signature when they cannot be loaded", async () => {
    getSignatures.mockRejectedValueOnce(new Error("offline"));
    const result = compose();
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(getSignatures).toHaveBeenCalledTimes(1));
    await act(async () => {});
    expect(body()).toBe("<p><br></p>");
  });
});

// Leaving a window with content asks Save draft / Discard / Keep writing.
describe("leaving a compose window", () => {
  it("asks before closing one with content, and closes a blank one at once", async () => {
    const result = compose();
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(body()).toContain("Alice"));
    // Only the signature: nothing to lose.
    act(() => (document.querySelector(".compose-close") as HTMLButtonElement).click());
    expect(result.current.composeOpen).toBe(false);

    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(body()).toContain("Alice"));
    act(() => result.current.setComposeSubject("Quarterly numbers"));
    const dialog = document.querySelector("dialog.compose-backdrop") as HTMLDialogElement;
    act(() => {
      dialog.dispatchEvent(new Event("cancel", { cancelable: true }));
    });
    const ask = document.querySelector('[role="alertdialog"]') as HTMLElement;
    expect(ask?.textContent).toContain("Leave this message?");
    expect(result.current.composeOpen).toBe(true);

    const button = (name: string) =>
      [...document.querySelectorAll('[role="alertdialog"] button')].find((b) => b.textContent === name) as HTMLButtonElement;
    act(() => button("Keep writing").click());
    expect(result.current.composeOpen).toBe(true);
    expect(document.querySelector('[role="alertdialog"]')).toBeNull();

    // A click on the backdrop asks too; Discard then closes.
    act(() => dialog.click());
    expect(document.querySelector('[role="alertdialog"]')).not.toBeNull();
    act(() => button("Discard").click());
    expect(result.current.composeOpen).toBe(false);
  });

  it("keeps the editor read-only while Save draft saves before closing", async () => {
    const { postJSON } = await import("../api/client");
    let finish: (value: unknown) => void = () => {};
    vi.mocked(postJSON).mockReturnValue(new Promise((resolve) => (finish = resolve)));
    const result = compose();
    act(() => result.current.openComposeWindow());
    await waitFor(() => expect(body()).toContain("Alice"));
    act(() => {
      result.current.setComposeTo({ tokens: [], draft: "bob@example.com" });
      result.current.setComposeSubject("Quarterly numbers");
    });
    act(() => (document.querySelector(".compose-close") as HTMLButtonElement).click());
    const save = [...document.querySelectorAll('[role="alertdialog"] button')].find((b) => b.textContent === "Save draft") as HTMLButtonElement;
    act(() => save.click());
    const editor = document.querySelector(".ql-editor") as HTMLElement;
    await waitFor(() => expect(editor.getAttribute("contenteditable")).toBe("false"));
    await act(async () => finish({ ok: true }));
    await waitFor(() => expect(result.current.composeOpen).toBe(false));
  });
});
