import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { KyPulse } from "./KyPulse";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const base = {
  available: true, enabled: false, own: ["alice@example.com"],
  recipients: [{ address: "ops@x.test", confirmed: true }, { address: "pending@x.test", confirmed: false }],
};

it("turns KyPulse alerts on and lists own and extra addresses", async () => {
  const fetchMock = vi.fn<typeof fetch>(async (_url, init) =>
    init?.method === "PUT" ? json({ ...base, enabled: true }) : json(base));
  vi.stubGlobal("fetch", fetchMock);
  render(<KyPulse />);
  const box = await screen.findByRole("checkbox", { name: /Let KyPulse send alerts from my mailbox/ });
  expect((box as HTMLInputElement).checked).toBe(false);
  expect(screen.getByText("alice@example.com")).toBeTruthy();
  expect(screen.getByText("Confirmed")).toBeTruthy();
  expect(screen.getByText("Pending")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Resend" })).toHaveLength(1);
  fireEvent.click(box);
  await waitFor(() => expect((box as HTMLInputElement).checked).toBe(true));
  const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(put?.[0]).toBe("/api/settings/kypulse");
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ enabled: true });
});

it("adds, resends and removes an address", async () => {
  const added = { ...base, recipients: [...base.recipients, { address: "new@x.test", confirmed: false }] };
  const fetchMock = vi.fn<typeof fetch>(async (url, init) => {
    if (init?.method === "POST" && String(url).endsWith("/recipients")) return json(added);
    if (init?.method === "POST") return json(added);
    if (init?.method === "DELETE") return json({ ...base, recipients: [base.recipients[1]] });
    return json(base);
  });
  vi.stubGlobal("fetch", fetchMock);
  render(<KyPulse />);
  fireEvent.change(await screen.findByPlaceholderText("ops@example.com"), { target: { value: " new@x.test " } });
  fireEvent.click(screen.getByRole("button", { name: "Send confirmation link" }));
  expect(await screen.findByText("new@x.test")).toBeTruthy();
  expect(screen.getByRole("status").textContent).toBe("A confirmation link was sent to new@x.test.");
  const post = fetchMock.mock.calls.find(([url, init]) => init?.method === "POST" && String(url) === "/api/settings/kypulse/recipients");
  expect(JSON.parse(String(post?.[1]?.body))).toEqual({ address: "new@x.test" });

  fireEvent.click(screen.getAllByRole("button", { name: "Resend" })[0]);
  await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => String(url) === "/api/settings/kypulse/recipients/resend")).toBe(true));

  fireEvent.click(await screen.findByRole("button", { name: "Remove ops@x.test" }));
  await waitFor(() => expect(screen.queryByText("ops@x.test")).toBeNull());
  const del = fetchMock.mock.calls.find(([, init]) => init?.method === "DELETE");
  expect(del?.[0]).toBe("/api/settings/kypulse/recipients");
  expect(JSON.parse(String(del?.[1]?.body))).toEqual({ address: "ops@x.test" });
});

it("says when KyPulse is not paired", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json({ ...base, available: false })));
  render(<KyPulse />);
  expect(await screen.findByText(/Alert mail with KyPulse is not connected on this server/)).toBeTruthy();
  expect(screen.queryByRole("checkbox")).toBeNull();
});

it("shows the server's refusal", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async (_url, init) =>
    init?.method === "POST" ? json({ error: "your own addresses are always allowed", code: "own_address" }, 409) : json(base)));
  render(<KyPulse />);
  fireEvent.change(await screen.findByPlaceholderText("ops@example.com"), { target: { value: "alice@example.com" } });
  fireEvent.click(screen.getByRole("button", { name: "Send confirmation link" }));
  expect((await screen.findByRole("alert")).textContent).toContain("your own addresses are always allowed");
});
