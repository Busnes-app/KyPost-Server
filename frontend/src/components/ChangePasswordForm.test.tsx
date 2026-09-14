import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ChangePasswordForm } from "./ChangePasswordForm";

const postJSON = vi.fn();
let recoverySlots: string[] | undefined;
let protection = "client";
let mustChangePassword = false;
const onSuccess = vi.fn();
const TEST_LEDE = "This account needs a new password before you can go any further.";

vi.mock("../api/client", () => ({
  toErrorMessage: (e: unknown, fallback: string) => e instanceof Error ? e.message : fallback,
  postJSON: (url: string, body: unknown) => postJSON(url, body)
}));

vi.mock("../api/auth", () => ({
  deriveNewCredential: async () => "new-secret",
  deriveCredential: async () => ({ authSecret: "old-secret" }),
  credentialFields: (c: { authSecret: string }, prefix: string) => ({ [`${prefix}AuthSecret`]: c.authSecret })
}));

vi.mock("../lib/authSecret", () => ({
  defaultIterations: () => 600000,
  newLoginSalt: () => "salt"
}));

vi.mock("../api/pgp", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api/pgp")>(),
  getPasswordSnapshot: async () => ({ pgpRevision: 7, protection, wrappedPrivateKey: "", mustChangePassword })
}));

vi.mock("../lib/pgpSession", async (importOriginal) => ({
  subscribePGPSession: (fn: (s: unknown) => void) => {
    fn({ loaded: true, error: "", bootstrap: { protection, envelopeSlots: recoverySlots } });
    return () => {};
  },
  rewrappedEnvelopeFor: async (...args: [string, string]) => mustChangePassword
    ? (await importOriginal<typeof import("../lib/pgpSession")>()).rewrappedEnvelopeFor(...args)
    : { expectedRevision: 7, rewrappedPgpKey: "sealed" },
  loadPGPSession: async () => undefined
}));

afterEach(cleanup);

beforeEach(() => {
  recoverySlots = [];
  protection = "client";
  mustChangePassword = false;
  postJSON.mockReset();
  onSuccess.mockReset();
  postJSON.mockResolvedValue({ ok: true });
});

describe("ChangePasswordForm", () => {
  it("rejects a new password under the minimum length without calling the server", async () => {
    render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);
    await userEvent.type(screen.getByLabelText("Current password"), "currentpassword");
    await userEvent.type(screen.getByLabelText("New password"), "short");
    await userEvent.click(screen.getByRole("button", { name: /update password/i }));

    expect(postJSON).not.toHaveBeenCalled();
    expect((await screen.findByRole("status")).textContent).toContain("at least 14 characters");
  });

  it("calls onSuccess after the credential commits, and does not navigate itself", async () => {
    render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);
    await userEvent.type(screen.getByLabelText("Current password"), "currentpassword");
    await userEvent.type(screen.getByLabelText("New password"), "a-long-enough-password");
    await userEvent.click(screen.getByRole("button", { name: /update password/i }));

    await waitFor(() => expect(postJSON).toHaveBeenCalledWith("/api/auth/password", expect.anything()));
    expect(onSuccess).toHaveBeenCalledOnce();
    expect(postJSON).toHaveBeenCalledWith("/api/auth/password", expect.objectContaining({ expectedRevision: 7, rewrappedPgpKey: "sealed" }));
  });

  it("uses the password carried in from sign-in when the current-password field is left blank", async () => {
    render(
      <ChangePasswordForm username="gwen" initialCurrentPassword="from-signin" lede={TEST_LEDE} onSuccess={onSuccess} />
    );
    await userEvent.type(screen.getByLabelText("New password"), "a-long-enough-password");
    await userEvent.click(screen.getByRole("button", { name: /update password/i }));

    await waitFor(() => expect(postJSON).toHaveBeenCalled());
  });

  it("renders exactly the lede the caller passes, never a string of its own", () => {
    render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);

    expect(screen.getByText(TEST_LEDE)).toBeTruthy();
  });
});


describe("PGP recovery warning", () => {
  it.each([[], undefined])("warns when no server copy is confirmed: %s", (slots) => {
    recoverySlots = slots;
    render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);
    expect(screen.getByText(/No server recovery copy is confirmed/)).toBeTruthy();
  });
  it("does not claim a missing copy when a recovery slot exists", () => {
    recoverySlots = ["password", "recovery"];
    render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);
    expect(screen.queryByText(/No server recovery copy is confirmed/)).toBeNull();
  });
  it("does not warn about a PGP backup for a keyless account", () => {
    protection = "";
    render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);
    expect(screen.queryByText(/No server recovery copy is confirmed/)).toBeNull();
  });
});


it("shows stale-write failure without retrying or reporting success", async () => {
  postJSON.mockRejectedValue(new Error("PGP state changed; reload before preparing the update again"));
  render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);
  await userEvent.type(screen.getByLabelText("Current password"), "currentpassword");
  await userEvent.type(screen.getByLabelText("New password"), "a-long-enough-password");
  await userEvent.click(screen.getByRole("button", { name: /update password/i }));
  expect((await screen.findByRole("status")).textContent).toContain("PGP state changed; reload");
  expect(postJSON).toHaveBeenCalledTimes(1);
  expect(onSuccess).not.toHaveBeenCalled();
});


it("warns on a resumed forced change without a prefilled password and preserves the envelope", async () => {
  mustChangePassword = true;
  render(<ChangePasswordForm username="gwen" lede={TEST_LEDE} onSuccess={onSuccess} />);
  expect(await screen.findByText(/This change will not re-encrypt your existing PGP key/)).toBeTruthy();
  await userEvent.type(screen.getByLabelText("Current password"), "currentpassword");
  await userEvent.type(screen.getByLabelText("New password"), "a-long-enough-password");
  await userEvent.click(screen.getByRole("button", { name: /update password/i }));
  await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce());
  const body = postJSON.mock.calls[0][1];
  expect(body.expectedRevision).toBe(7);
  expect(body).not.toHaveProperty("rewrappedPgpKey");
});
