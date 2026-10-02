import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { IncomingEncryption } from "./IncomingEncryption";
import { getIncomingEncryption, setIncomingEncryption, type PGPIdentity } from "../../../api/pgp";

const auth = vi.hoisted(() => ({ ssoSession: false }));
vi.mock("../../../auth", () => ({ useAuth: () => auth }));
vi.mock("../../../api/pgp", () => ({ getIncomingEncryption: vi.fn(), setIncomingEncryption: vi.fn() }));
const identity: PGPIdentity = { pgpRevision: 4, fingerprint: "fingerprint", keyId: "key", publicKey: "public", source: "generated", createdAt: "now" };
afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks(); auth.ssoSession = false;
  vi.mocked(getIncomingEncryption).mockResolvedValue({ enabled: false, pending: false });
  vi.mocked(setIncomingEncryption).mockResolvedValue(undefined);
});
describe("incoming encryption opt-in", () => {
  it("requires a backup acknowledgment and password before saving", async () => {
    render(<IncomingEncryption identity={identity} clientProtected />);
    const enabled = await screen.findByLabelText("Encrypt incoming mail after classification");
    await waitFor(() => expect((enabled as HTMLInputElement).disabled).toBe(false));
    fireEvent.click(enabled);
    const save = screen.getByRole("button", { name: "Save incoming encryption" });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByLabelText(/I saved a private-key/));
    fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account credential" } });
    fireEvent.click(save);
    await waitFor(() => expect(setIncomingEncryption).toHaveBeenCalledWith(true, "account credential", identity, true));
    await screen.findByText("Incoming encryption enabled.");
    expect((screen.getByLabelText("Account password") as HTMLInputElement).value).toBe("");
  });
  it("lets SSO accounts confirm without an account password", async () => {
    auth.ssoSession = true;
    render(<IncomingEncryption identity={identity} clientProtected />);
    const enabled = await screen.findByLabelText("Encrypt incoming mail after classification");
    await waitFor(() => expect((enabled as HTMLInputElement).disabled).toBe(false));
    fireEvent.click(enabled);
    fireEvent.click(screen.getByLabelText(/I saved a private-key/));
    expect(screen.queryByLabelText("Account password")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Save incoming encryption" }));
    await waitFor(() => expect(setIncomingEncryption).toHaveBeenCalledWith(true, "", identity, true));
  });
  it("requires a client-protected key and reports pending recovery", async () => {
    vi.mocked(getIncomingEncryption).mockResolvedValue({ enabled: false, pending: true });
    render(<IncomingEncryption identity={null} clientProtected={false} />);
    await screen.findByText(/An incoming replacement is pending/);
    expect((screen.getByLabelText("Encrypt incoming mail after classification") as HTMLInputElement).disabled).toBe(true);
  });
});
