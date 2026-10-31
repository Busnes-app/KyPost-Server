import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { KyCalendarStatus } from "./KyCalendarStatus";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const show = async (body: unknown) => {
  const fetchMock = vi.fn<typeof fetch>(async () => json(body));
  vi.stubGlobal("fetch", fetchMock);
  render(<KyCalendarStatus />);
  const status = await screen.findByRole("status");
  expect(fetchMock.mock.calls[0][0]).toBe("/api/admin/kycalendar");
  return status.textContent;
};

it("shows a KyIdentity pairing", async () => {
  expect(await show({ source: "kyidentity", connected: true, url: "https://cal.example.com", reason: "Connected" }))
    .toBe("Connected via KyIdentity to https://cal.example.com.");
  expect(screen.getByText(/unless they turn it off/)).toBeTruthy();
});

it("shows an environment pairing", async () => {
  expect(await show({ source: "env", connected: true, url: "https://cal.example.com" }))
    .toBe("Connected via environment settings to https://cal.example.com.");
});

it("shows why it is not connected, as text", async () => {
  expect(await show({ source: "kyidentity", connected: false, reason: "<b>KyIdentity</b> has no KyCalendar to pair with" }))
    .toBe("Not connected: <b>KyIdentity</b> has no KyCalendar to pair with.");
  expect(document.querySelector("b")).toBeNull();
});
