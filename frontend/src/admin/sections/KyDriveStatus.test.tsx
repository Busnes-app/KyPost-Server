import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { KyDriveStatus } from "./KyDriveStatus";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const show = async (body: unknown) => {
  const fetchMock = vi.fn<typeof fetch>(async () => json(body));
  vi.stubGlobal("fetch", fetchMock);
  render(<KyDriveStatus />);
  const status = await screen.findByRole("status");
  expect(fetchMock.mock.calls[0][0]).toBe("/api/admin/kydrive");
  return status.textContent;
};

it("shows a KyIdentity pairing", async () => {
  expect(await show({ source: "kyidentity", connected: true, url: "https://drive.example.com", reason: "Connected" }))
    .toBe("Connected via KyIdentity to https://drive.example.com.");
  expect(screen.getByText(/Let KyPost save and share my files/)).toBeTruthy();
});

it("shows an environment pairing", async () => {
  expect(await show({ source: "env", connected: true, url: "https://drive.example.com" }))
    .toBe("Connected via environment settings to https://drive.example.com.");
});

it("shows why it is not connected, as text", async () => {
  expect(await show({ source: "kyidentity", connected: false, reason: "<b>KyIdentity</b> has no KyDrive to pair with" }))
    .toBe("Not connected: <b>KyIdentity</b> has no KyDrive to pair with.");
  expect(document.querySelector("b")).toBeNull();
});
