import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { KyMessagesStatus } from "./KyMessagesStatus";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const show = async (body: unknown) => {
  const fetchMock = vi.fn<typeof fetch>(async () => json(body));
  vi.stubGlobal("fetch", fetchMock);
  render(<KyMessagesStatus />);
  const status = await screen.findByRole("status");
  expect(fetchMock.mock.calls[0][0]).toBe("/api/admin/kymessage");
  return status.textContent;
};

it("shows a KyIdentity pairing", async () => {
  expect(await show({ source: "kyidentity", connected: true, url: "https://message.example.com", reason: "Connected" }))
    .toBe("Connected via KyIdentity to https://message.example.com.");
  expect(screen.getByText(/Chat beside senders and contacts/)).toBeTruthy();
});

it("shows an environment pairing", async () => {
  expect(await show({ source: "env", connected: true, url: "https://message.example.com" }))
    .toBe("Connected via environment settings to https://message.example.com.");
});

it("shows why it is not connected, as text", async () => {
  expect(await show({ source: "kyidentity", connected: false, reason: "<b>KyIdentity</b> has no KyMessages to pair with" }))
    .toBe("Not connected: <b>KyIdentity</b> has no KyMessages to pair with.");
  expect(document.querySelector("b")).toBeNull();
});
