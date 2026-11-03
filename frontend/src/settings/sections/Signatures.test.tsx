import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createElement } from "react";
import type { SignatureSettings } from "../../api/signatures";
import { MailboxContext } from "../../app/mailboxes";

const getSignatures = vi.fn<() => Promise<SignatureSettings>>();
const putSignatures = vi.fn(async (s: SignatureSettings) => s);
vi.mock("../../api/signatures", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/signatures")>()),
  getSignatures: () => getSignatures(),
  putSignatures: (s: SignatureSettings) => putSignatures(s)
}));
vi.mock("../../api/sendas", () => ({
  listSendAsAliases: async () => [
    { id: "a", userId: "u1", email: "alias@example.org", status: "verified", createdAt: "", expiresAt: "" },
    { id: "b", userId: "u1", email: "pending@example.org", status: "pending", createdAt: "", expiresAt: "" }
  ]
}));

import { Signatures } from "./Signatures";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderSection() {
  const mailboxes = [{ id: "u1", kind: "primary" as const, addresses: ["me@example.org"] }];
  return render(createElement(MailboxContext.Provider, { value: { mailboxes, selected: "" } }, createElement(Signatures)));
}

describe("Signatures settings", () => {
  it("creates a signature, makes it the default, and saves the whole set", async () => {
    getSignatures.mockResolvedValue({ signatures: [], defaultId: "", byAddress: {}, onReply: true, onForward: true });
    const user = userEvent.setup();
    renderSection();

    await user.click(await screen.findByRole("button", { name: "Add signature" }));
    await user.type(screen.getByRole("textbox", { name: "Signature" }), "Ada{Enter}<b>not bold</b>");
    await user.click(screen.getByRole("checkbox", { name: "Add to forwards" }));
    await user.selectOptions(screen.getByRole("combobox", { name: "alias@example.org" }), "None");
    await user.click(screen.getByRole("button", { name: "Save signatures" }));

    await screen.findByText("Signatures saved.");
    const saved = putSignatures.mock.calls[0][0];
    expect(saved.signatures).toHaveLength(1);
    expect(saved.signatures[0]).toMatchObject({ name: "My signature", text: "Ada\n<b>not bold</b>" });
    expect(saved.signatures[0].id).toMatch(/^[0-9a-f]{24}$/);
    expect(saved.defaultId).toBe(saved.signatures[0].id);
    expect(saved.onForward).toBe(false);
    expect(saved.byAddress).toEqual({ "alias@example.org": "" });
    // Only addresses this account can send from are offered.
    expect(screen.getByRole("combobox", { name: "me@example.org" })).toBeDefined();
    expect(screen.queryByRole("combobox", { name: "pending@example.org" })).toBeNull();
  });

  it("deleting a signature clears it as the default and from any address", async () => {
    getSignatures.mockResolvedValue({
      signatures: [{ id: "s1", name: "Work", text: "Ada" }],
      defaultId: "s1",
      byAddress: { "me@example.org": "s1" },
      onReply: true,
      onForward: true
    });
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const user = userEvent.setup();
    renderSection();

    await user.click(await screen.findByRole("button", { name: "Delete signature" }));
    await user.click(screen.getByRole("button", { name: "Save signatures" }));
    await waitFor(() => expect(putSignatures).toHaveBeenCalledTimes(1));
    expect(putSignatures.mock.calls[0][0]).toMatchObject({ signatures: [], defaultId: "", byAddress: {} });
  });

  it("refuses to save an unnamed signature", async () => {
    getSignatures.mockResolvedValue({
      signatures: [{ id: "s1", name: "Work", text: "Ada" }],
      defaultId: "",
      byAddress: {},
      onReply: true,
      onForward: true
    });
    const user = userEvent.setup();
    renderSection();
    await user.clear(await screen.findByRole("textbox", { name: "Name" }));
    await user.click(screen.getByRole("button", { name: "Save signatures" }));
    expect(await screen.findByText("Give every signature a name.")).toBeDefined();
    expect(putSignatures).not.toHaveBeenCalled();
  });
});
