import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SendDelay } from "./SendDelay";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("starts Off and saves a chosen delay", async () => {
  const fetchMock = vi.fn<typeof fetch>(async (_url, init) =>
    init?.method === "PUT" ? json({ seconds: 30 }) : json({ seconds: 0 }));
  vi.stubGlobal("fetch", fetchMock);
  render(<SendDelay />);
  const off = await screen.findByRole("radio", { name: "Off" });
  expect((off as HTMLInputElement).checked).toBe(true);
  expect(screen.getAllByRole("radio").map((r) => r.parentElement?.textContent)).toEqual(["Off", "10 seconds", "20 seconds", "30 seconds", "60 seconds", "90 seconds"]);
  fireEvent.click(screen.getByRole("radio", { name: "30 seconds" }));
  await waitFor(() => expect((screen.getByRole("radio", { name: "30 seconds" }) as HTMLInputElement).checked).toBe(true));
  const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(put?.[0]).toBe("/api/mail/send-delay");
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ seconds: 30 });
  expect(document.body.textContent?.toLowerCase()).not.toContain("undo");
});

it("shows a read failure instead of a choice", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json({ seconds: 15 })));
  render(<SendDelay />);
  expect((await screen.findByRole("alert")).textContent).toContain("could not be read");
  expect(screen.queryByRole("radio")).toBeNull();
});
