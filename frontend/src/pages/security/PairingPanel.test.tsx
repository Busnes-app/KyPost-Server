import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PairingPanel } from "./PairingPanel";
import { mintPairing } from "../../api/devices";

const auth = vi.hoisted(() => ({ ssoSession: false }));
vi.mock("../../auth", () => ({ useAuth: () => auth }));
vi.mock("../../api/devices", () => ({ mintPairing: vi.fn() }));
vi.mock("qrcode", () => ({ default: { toDataURL: vi.fn().mockResolvedValue("data:image/png;base64,") } }));

const live = {
  subscriberId: "sub-1",
  serverBaseUrl: "https://mail.example",
  registerEndpoint: "https://mail.example/api/notifications/native/register",
  pairingToken: "token",
  pairingTtlSeconds: 90,
  configured: true
};

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  auth.ssoSession = false;
  vi.mocked(mintPairing).mockResolvedValue({ ...live, pairingExpiresAt: new Date(Date.now() + 90_000).toISOString() });
});

describe("pairing step-up", () => {
  it("mints nothing until the account password is confirmed", async () => {
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    const show = screen.getByRole("button", { name: "Show pairing code" });
    expect(mintPairing).not.toHaveBeenCalled();
    expect((show as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account credential" } });
    fireEvent.click(show);
    await waitFor(() => expect(mintPairing).toHaveBeenCalledWith({ ssoSession: false, password: "account credential" }));
    await screen.findByAltText("Native mobile pairing QR code");
  });

  it("lets SSO sessions confirm with KySignOn instead of a password", async () => {
    auth.ssoSession = true;
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    expect(screen.queryByLabelText("Account password")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await waitFor(() => expect(mintPairing).toHaveBeenCalledWith({ ssoSession: true, password: "" }));
  });

  it("shows the refusal and stays on the confirmation form", async () => {
    vi.mocked(mintPairing).mockRejectedValue(new Error("invalid credentials"));
    const onStatus = vi.fn();
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={onStatus} />);
    fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await waitFor(() => expect(onStatus).toHaveBeenCalledWith("Could not show a pairing code: invalid credentials"));
    expect(screen.getByRole("button", { name: "Show pairing code" })).toBeTruthy();
    expect(screen.queryByAltText("Native mobile pairing QR code")).toBeNull();
  });
});
