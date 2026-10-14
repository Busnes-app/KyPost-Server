import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Devices } from "./Devices";
import { setDeviceApprover, setPushApproval } from "../../../api/devices";
import type { DeviceRow } from "../deviceJoin";

const auth = vi.hoisted(() => ({ ssoSession: false }));
vi.mock("../../../auth", () => ({ useAuth: () => auth }));
vi.mock("../../../api/devices", () => ({ setDeviceApprover: vi.fn(), setPushApproval: vi.fn() }));

const status = { totpEnabled: true, recoveryCodesRemaining: 10, pushMfaEnabled: false, approverDevices: [] };
const row: DeviceRow = {
  deviceId: "phone-1",
  name: "Phone",
  approver: false,
  canApprove: true,
  missingFromInventory: false,
  approvalUnavailable: false,
  mailAccess: "unsupported"
};

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  auth.ssoSession = false;
  vi.mocked(setDeviceApprover).mockResolvedValue({});
  vi.mocked(setPushApproval).mockResolvedValue({});
});

describe("sign-in approval step-up", () => {
  it("keeps approval controls locked until the password is entered, then sends it", async () => {
    render(<Devices status={status} deviceRows={[row]} />);
    const push = screen.getByLabelText("Let a paired device approve sign-ins with a tap") as HTMLInputElement;
    // A new device is not an approver; it can be made one while push is still off.
    const approver = screen.getByLabelText("Can approve sign-ins") as HTMLInputElement;
    expect(push.disabled).toBe(true);
    expect(approver.disabled).toBe(true);

    const password = screen.getByLabelText("Account password to change sign-in approval") as HTMLInputElement;
    fireEvent.change(password, { target: { value: "account credential" } });
    expect(approver.disabled).toBe(false);
    fireEvent.click(approver);
    await waitFor(() => expect(setDeviceApprover).toHaveBeenCalledWith({ ssoSession: false, password: "account credential" }, "phone-1", true));
    expect(password.value).toBe("");

    fireEvent.change(password, { target: { value: "account credential" } });
    fireEvent.click(push);
    await waitFor(() => expect(setPushApproval).toHaveBeenCalledWith({ ssoSession: false, password: "account credential" }, true));
  });

  it("confirms SSO sessions with KySignOn, not a password", async () => {
    auth.ssoSession = true;
    render(<Devices status={status} deviceRows={[row]} />);
    expect(screen.queryByLabelText("Account password to change sign-in approval")).toBeNull();
    fireEvent.click(screen.getByLabelText("Let a paired device approve sign-ins with a tap"));
    await waitFor(() => expect(setPushApproval).toHaveBeenCalledWith({ ssoSession: true, password: "" }, true));
  });
});
