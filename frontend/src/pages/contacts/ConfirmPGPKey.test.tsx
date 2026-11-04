import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ConfirmPGPKey } from "./ConfirmPGPKey";

const postJSON = vi.fn();

vi.mock("../../api/client", () => ({
  HttpError: class HttpError extends Error {},
  getJSON: vi.fn(),
  putJSON: vi.fn(),
  deleteJSON: vi.fn(),
  postFormData: vi.fn(),
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback)
}));

vi.mock("../../api/auth", () => ({
  deriveCredential: async () => ({ authSecret: "derived-account-credential" }),
  credentialFields: (c: { authSecret: string }) => ({ authSecret: c.authSecret })
}));

const bob = { uid: "bob-1", rev: 1, createdAt: "", updatedAt: "", fn: "Bob", pgpKeySource: "autocrypt", pgpKeyFingerprint: "aaaabbbbccccdddd" };

afterEach(cleanup);
beforeEach(() => {
  postJSON.mockReset();
  postJSON.mockResolvedValue({ ...bob, pgpKeyVerified: true });
});

describe("ConfirmPGPKey", () => {
  it("shows the pinned fingerprint and confirms exactly it with the account password", async () => {
    const onConfirmed = vi.fn();
    render(<ConfirmPGPKey contact={bob} onConfirmed={onConfirmed} />);
    expect(screen.getByText("AAAA BBBB CCCC DDDD")).toBeTruthy();
    const button = screen.getByRole("button", { name: /confirm key/i }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    await userEvent.type(screen.getByLabelText(/account password/i), "account-password");
    await userEvent.click(button);
    await waitFor(() => expect(onConfirmed).toHaveBeenCalled());
    expect(postJSON).toHaveBeenCalledWith("/api/contacts/bob-1/pgp-key/confirm", {
      fingerprint: "aaaabbbbccccdddd",
      authSecret: "derived-account-credential"
    });
    expect((screen.getByLabelText(/account password/i) as HTMLInputElement).value).toBe("");
  });

  it("says so when the server refuses, and does not report success", async () => {
    postJSON.mockRejectedValue(new Error("the fingerprint does not match this contact's current key"));
    const onConfirmed = vi.fn();
    render(<ConfirmPGPKey contact={bob} onConfirmed={onConfirmed} />);
    await userEvent.type(screen.getByLabelText(/account password/i), "account-password");
    await userEvent.click(screen.getByRole("button", { name: /confirm key/i }));
    expect(await screen.findByText(/could not confirm the key: the fingerprint does not match/i)).toBeTruthy();
    expect(onConfirmed).not.toHaveBeenCalled();
  });
});
