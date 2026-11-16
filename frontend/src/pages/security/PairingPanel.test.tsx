import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PairingPanel } from "./PairingPanel";
import { decidePendingDevice, listPendingDevices, mintPairing } from "../../api/devices";
import { HttpError } from "../../api/client";

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

const stale = () => new HttpError("invalid credentials", 403, null);

describe("pairing step-up", () => {
  it("uses the page's step-up without asking again", async () => {
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    expect(screen.queryByLabelText("Account password")).toBeNull();
    expect(mintPairing).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await waitFor(() => expect(mintPairing).toHaveBeenCalledWith(undefined));
    await screen.findByAltText("Native mobile pairing QR code");
  });

  it("asks for the password once the page's step-up is refused", async () => {
    vi.mocked(mintPairing).mockRejectedValueOnce(stale());
    const onStatus = vi.fn();
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={onStatus} />);
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    const password = await screen.findByLabelText("Account password");
    expect(onStatus).toHaveBeenCalledWith("Your confirmation has expired. Confirm it is you to show a pairing code.");
    const show = screen.getByRole("button", { name: "Show pairing code" });
    expect((show as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(password, { target: { value: "account credential" } });
    fireEvent.click(show);
    await waitFor(() => expect(mintPairing).toHaveBeenLastCalledWith({ ssoSession: false, accountPassword: "account credential" }));
    await screen.findByAltText("Native mobile pairing QR code");
  });

  it("lets SSO sessions confirm with KySignOn once the page's step-up is refused", async () => {
    auth.ssoSession = true;
    vi.mocked(mintPairing).mockRejectedValueOnce(stale());
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await screen.findByText("You will confirm with KySignOn.");
    expect(screen.queryByLabelText("Account password")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await waitFor(() => expect(mintPairing).toHaveBeenLastCalledWith({ ssoSession: true, accountPassword: "" }));
  });

  it("shows other refusals and stays on the form", async () => {
    vi.mocked(mintPairing).mockRejectedValue(new Error("server down"));
    const onStatus = vi.fn();
    render(<PairingPanel onDevicesMayHaveChanged={() => {}} onStatus={onStatus} />);
    fireEvent.click(screen.getByRole("button", { name: "Show pairing code" }));
    await waitFor(() => expect(onStatus).toHaveBeenCalledWith("Could not show a pairing code: server down"));
    expect(screen.getByRole("button", { name: "Show pairing code" })).toBeTruthy();
    expect(screen.queryByLabelText("Account password")).toBeNull();
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
