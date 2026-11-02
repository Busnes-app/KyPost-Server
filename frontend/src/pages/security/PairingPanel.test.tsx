import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PairingPanel } from "./PairingPanel";
import { decidePendingDevice, listPendingDevices, mintPairing } from "../../api/devices";

const auth = vi.hoisted(() => ({ ssoSession: false }));
vi.mock("../../auth", () => ({ useAuth: () => auth }));
vi.mock("../../api/devices", () => ({
  mintPairing: vi.fn(),
  listPendingDevices: vi.fn(),
  decidePendingDevice: vi.fn()
}));
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
  vi.mocked(listPendingDevices).mockResolvedValue([]);
  vi.mocked(decidePendingDevice).mockResolvedValue({ ok: true });
});

describe("pairing step-up", () => {
  it("mints nothing until the account password is confirmed", async () => {
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    const show = screen.getByRole("button", { name: "Show pairing code" });
    expect(mintPairing).not.toHaveBeenCalled();
    expect((show as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account credential" } });
    fireEvent.click(show);
    await waitFor(() => expect(mintPairing).toHaveBeenCalledWith({ ssoSession: false, accountPassword: "account credential" }));
    await screen.findByAltText("Native mobile pairing QR code");
  });

  it("lets SSO sessions confirm with KySignOn instead of a password", async () => {
    auth.ssoSession = true;
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    expect(screen.queryByLabelText("Account password")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await waitFor(() => expect(mintPairing).toHaveBeenCalledWith({ ssoSession: true, accountPassword: "" }));
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

describe("device approval", () => {
  const phone = {
    deviceId: "phone-1",
    deviceName: "Pixel 9",
    platform: "android",
    osVersion: "Android 16",
    appVersion: "KyPost for Android v0.6.0",
    requestIp: "203.0.113.7",
    approvalCode: "048213"
  };

  async function showCode() {
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account credential" } });
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await screen.findByAltText("Native mobile pairing QR code");
  }

  it("does not poll for pending devices before a code is shown", () => {
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    expect(listPendingDevices).not.toHaveBeenCalled();
  });

  it("shows who is asking and approves only on request", async () => {
    vi.mocked(listPendingDevices).mockResolvedValue([phone]);
    await showCode();
    await screen.findByText("Pixel 9 wants to pair");
    expect(screen.getByText("Android 16 · KyPost for Android v0.6.0")).toBeTruthy();
    expect(screen.getByText("From 203.0.113.7")).toBeTruthy();
    expect(screen.getByText("048213")).toBeTruthy();
    expect(decidePendingDevice).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(decidePendingDevice).toHaveBeenCalledWith("phone-1", "approve"));
  });

  it("denies", async () => {
    vi.mocked(listPendingDevices).mockResolvedValue([phone]);
    await showCode();
    fireEvent.click(await screen.findByRole("button", { name: "Deny" }));
    await waitFor(() => expect(decidePendingDevice).toHaveBeenCalledWith("phone-1", "deny"));
  });
});
