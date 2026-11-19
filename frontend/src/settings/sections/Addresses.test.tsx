import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Addresses } from "./Addresses";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

const masked = { address: "quiet.otter.4821@example.test", note: "used for: shop", state: "active", createdAt: 1 };
const base = { plusAddressing: true, maskedDomains: ["example.test"], masked: [masked, { address: "old.fox.1234@example.test", note: "", state: "deleted", createdAt: 1 }], maskedLimit: 100, maskedPerDay: 20 };

it("switches plus addressing and lists masked addresses", async () => {
  const fetchMock = vi.fn<typeof fetch>(async (_url, init) => json(init?.method === "PUT" ? { ...base, plusAddressing: false } : base));
  vi.stubGlobal("fetch", fetchMock);
  render(<Addresses />);
  const box = await screen.findByRole("checkbox", { name: /Accept mail sent to plus addresses/ });
  expect((box as HTMLInputElement).checked).toBe(true);
  expect(screen.getByText("quiet.otter.4821@example.test")).toBeTruthy();
  expect(screen.getByText("Deleted addresses (1)")).toBeTruthy();
  expect(screen.getByText("1 of 100 in use; up to 20 new ones a day.")).toBeTruthy();
  fireEvent.click(box);
  await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === "/api/addresses/plus" && init?.method === "PUT" && init.body === JSON.stringify({ enabled: false }))).toBe(true));
});

it("creates, pauses and deletes a masked address", async () => {
  const fetchMock = vi.fn<typeof fetch>(async (url, init) => {
    if (init?.method === "POST") return json({ ...masked, address: "brave.lynx.2000@example.test", expiresAt: 99 });
    if (init?.method === "PUT" || init?.method === "DELETE") return json(masked);
    return json(base);
  });
  vi.stubGlobal("fetch", fetchMock);
  vi.spyOn(window, "confirm").mockReturnValue(true);
  render(<Addresses />);
  fireEvent.change(await screen.findByPlaceholderText("used for: example-shop"), { target: { value: " newsletter " } });
  fireEvent.change(screen.getByRole("combobox"), { target: { value: "7d" } });
  fireEvent.click(screen.getByRole("button", { name: "Create address" }));
  expect((await screen.findByRole("status")).textContent).toBe("Created brave.lynx.2000@example.test.");
  const post = fetchMock.mock.calls.find(([, init]) => init?.method === "POST");
  expect(post?.[0]).toBe("/api/addresses/masked");
  expect(JSON.parse(String(post?.[1]?.body))).toEqual({ domain: "example.test", note: "newsletter", expires: "7d" });

  fireEvent.click(screen.getByRole("button", { name: `Pause ${masked.address}` }));
  await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => url === `/api/addresses/masked/${encodeURIComponent(masked.address)}` && init?.method === "PUT" && init.body === JSON.stringify({ state: "paused" }))).toBe(true));
  fireEvent.click(await screen.findByRole("button", { name: `Delete ${masked.address}` }));
  await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === "DELETE")).toBe(true));
  expect(window.confirm).toHaveBeenCalledWith(expect.stringMatching(/never given to anyone again/));
});

it("explains when no masked domain is set and when the mailbox is not hosted", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json({ ...base, maskedDomains: [], masked: [] })));
  render(<Addresses />);
  expect(await screen.findByText(/administrator has not set up masked addresses/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Create address" })).toBeNull();
  cleanup();
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => new Response("plus addressing and masked addresses need a hosted mailbox", { status: 404 })));
  render(<Addresses />);
  expect(await screen.findByText(/available for mailboxes hosted on this server/)).toBeTruthy();
});
