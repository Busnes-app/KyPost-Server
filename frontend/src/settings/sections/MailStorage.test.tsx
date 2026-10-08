import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MailStorage } from "./MailStorage";

const GiB = 2 ** 30;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const box = (id: string, address: string, usedBytes?: number) => ({ id, kind: "primary", addresses: [{ address, kind: "primary" }], usedBytes, quotaBytes: 5 * GiB });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

function serve(body: unknown, status = 200) {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json(body, status)));
  render(<MailStorage />);
}

it("shows each mailbox's usage, warning at 80% and 95%", async () => {
  serve({ mailboxes: [box("a", "one@example.com", GiB), box("b", "two@example.com", 4 * GiB), box("c", "three@example.com", Math.round(4.9 * GiB)), box("d", "four@example.com")] });
  expect(await screen.findByText("1 GiB of 5 GiB used (20%)")).toBeTruthy();
  expect(screen.getByText("4 GiB of 5 GiB used (80%)")).toBeTruthy();
  expect(screen.getByText("4.9 GiB of 5 GiB used (98%)")).toBeTruthy();
  expect(screen.getByText("four@example.com: usage unavailable right now.")).toBeTruthy();
  const notes = screen.getAllByRole("note");
  expect(notes.map(n => [n.textContent?.slice(0, 40), n.className])).toEqual([
    ["This mailbox is over 80% full. Delete or", "notice notice-warning"],
    ["This mailbox is almost full. Once it is,", "notice notice-error"],
  ]);
  const meter = screen.getByRole("meter", { name: "two@example.com" });
  expect(meter.getAttribute("value")).toBe(String(4 * GiB));
});

it("explains mailboxes KyPost does not host, and read failures", async () => {
  serve({ mailboxes: [] });
  expect(await screen.findByText(/Storage is shown for mailboxes KyPost hosts/)).toBeTruthy();
  cleanup();
  serve("down", 503);
  expect((await screen.findByRole("alert")).textContent).not.toBe("");
});
