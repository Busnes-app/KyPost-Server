import { beforeEach, describe, expect, it, vi } from "vitest";
import { listNativeDevices } from "./devices";

const getJSON = vi.fn();
vi.mock("./client", () => ({ getJSON: (path: string) => getJSON(path) }));

beforeEach(() => {
  getJSON.mockReset();
});

describe("listNativeDevices", () => {
  // The three enrollment fields are the whole reason this module exists:
  // NotificationsPage's private copy of the type predates them, so a device
  // that HAS published a key read as one that had not.
  it("passes the enrollment fields through", async () => {
    getJSON.mockResolvedValue({
      devices: [
        {
          deviceId: "d1",
          platform: "android",
          pushToken: "tok",
          enrollmentPublicKey: "BASE64KEY",
          enrollmentKeyAt: "2026-08-05T10:00:00Z",
          enrollmentEnvelopeVersions: [3, 2, 4],
          encryptionEnrolled: true
        }
      ]
    });

    const { devices } = await listNativeDevices();

    expect(getJSON).toHaveBeenCalledWith("/api/notifications/native/devices");
    expect(devices[0].enrollmentPublicKey).toBe("BASE64KEY");
    expect(devices[0].enrollmentEnvelopeVersions).toEqual([2, 3, 4]);
    expect(devices[0].encryptionEnrolled).toBe(true);
  });

  it("reads a device that has published nothing as unenrolled", async () => {
    getJSON.mockResolvedValue({
      devices: [{ deviceId: "d2", platform: "ios", pushToken: "tok", encryptionEnrolled: false }]
    });

    const { devices } = await listNativeDevices();

    expect(devices[0].enrollmentPublicKey).toBeUndefined();
    // No claim is no capability; it is not read as the retired v2.
    expect(devices[0].enrollmentEnvelopeVersions).toEqual([]);
    expect(devices[0].encryptionEnrolled).toBe(false);
  });
});

it.each([[2, 2], [1], [65536], [2.5], ["3"], false, Array.from({ length: 17 }, (_, i) => i + 2)])("rejects malformed enrollment versions %j", async value => {
  getJSON.mockResolvedValue({ devices: [{ deviceId: "d", enrollmentEnvelopeVersions: value }] });
  await expect(listNativeDevices()).rejects.toThrow(/Invalid device enrollment versions/);
});


it("reads OpenPGP codecs independently of envelope support and defaults old devices to v4", async () => {
  getJSON.mockResolvedValue({ devices: [{ deviceId: "old" }, { deviceId: "new", openpgp: ["rfc9980", "v6", "v4", "v6"] }] });
  const { devices } = await listNativeDevices();
  expect(devices.map(device => device.openpgp)).toEqual([["v4"], ["v4", "v6", "rfc9980"]]);
});
it.each([[], ["v5"], [null], "v6", [4], false])("rejects malformed OpenPGP capability %j", async openpgp => {
  getJSON.mockResolvedValue({ devices: [{ deviceId: "bad", openpgp }] });
  await expect(listNativeDevices()).rejects.toThrow(/Invalid device OpenPGP/);
});
