import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { RecipientAutocomplete } from "./RecipientAutocomplete";

afterEach(cleanup);

it("puts organisation people under one Organisation heading, as text", () => {
  const onSelect = vi.fn();
  render(<RecipientAutocomplete query="al" loading={false} searched activeIndex={0} onHoverIndex={() => {}} onSelect={onSelect} results={[
    { key: "c1", name: "Alice (mine)", email: "alice@example.com", organisation: false },
    { key: "org:a", name: "<b>Albert</b>", email: "albert@example.com", organisation: true },
    { key: "org:b", name: "", email: "alma@example.com", organisation: true }
  ]} />);
  expect(screen.getAllByText("Organisation")).toHaveLength(1);
  expect(document.querySelector(".recipient-autocomplete b")).toBeNull();
  const rows = document.querySelectorAll(".recipient-autocomplete-row");
  expect([...rows].map((r) => r.textContent)).toEqual(["Alice (mine)alice@example.com", "<b>Albert</b>albert@example.com", "alma@example.com"]);
  fireEvent.click(rows[2]);
  expect(onSelect).toHaveBeenCalledWith({ key: "org:b", name: "", email: "alma@example.com", organisation: true });
});

it("shows organisation people's status in words, and asks only for them", async () => {
  const sent: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    sent.push(JSON.parse(String(init?.body)));
    return new Response(JSON.stringify({ people: [
      { address: "albert@example.com", state: "in_a_meeting", until: "2026-11-16T15:00:00Z" },
      { address: "alma@example.com", state: "away" }
    ] }), { status: 200, headers: { "Content-Type": "application/json" } });
  });
  render(<RecipientAutocomplete query="al" loading={false} searched activeIndex={0} onHoverIndex={() => {}} onSelect={() => {}} results={[
    { key: "c1", name: "Alice (mine)", email: "alice@example.com", organisation: false },
    { key: "org:a", name: "Albert", email: "albert@example.com", organisation: true },
    { key: "org:b", name: "", email: "alma@example.com", organisation: true }
  ]} />);
  expect(await screen.findByText(/^In a meeting until /)).toBeTruthy();
  const away = screen.getByText("Away");
  expect(away.getAttribute("data-state")).toBe("away");
  expect(away.querySelector(".recipient-status-dot")?.getAttribute("aria-hidden")).toBe("true");
  expect(sent).toEqual([{ addresses: ["albert@example.com", "alma@example.com"] }]);
  vi.restoreAllMocks();
});
