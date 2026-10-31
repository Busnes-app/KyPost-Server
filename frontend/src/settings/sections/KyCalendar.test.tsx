import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { KyCalendar } from "./KyCalendar";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("turns KyCalendar sending on", async () => {
  const fetchMock = vi.fn<typeof fetch>(async (_url, init) =>
    init?.method === "PUT" ? json({ available: true, enabled: true }) : json({ available: true, enabled: false }));
  vi.stubGlobal("fetch", fetchMock);
  render(<KyCalendar />);
  const box = await screen.findByRole("checkbox", { name: /Let KyCalendar send invitations from this mailbox/ });
  expect((box as HTMLInputElement).checked).toBe(false);
  fireEvent.click(box);
  await waitFor(() => expect((box as HTMLInputElement).checked).toBe(true));
  const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(put?.[0]).toBe("/api/settings/kycalendar");
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ enabled: true });
});

it("says when invitation mail is not set up on the server", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json({ available: false, enabled: false })));
  render(<KyCalendar />);
  expect(await screen.findByText("Invitation mail with KyCalendar is not connected on this server. Your administrator can see why under Server → KyCalendar. It lets KyCalendar send and receive meeting invitations through your mailbox; it does not sync your calendars.")).toBeTruthy();
  expect(screen.queryByRole("checkbox")).toBeNull();
});
