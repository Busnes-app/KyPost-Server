import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Filters } from "./Filters";

vi.mock("../../api/client", () => ({
  getJSON: async () => ({ configured: [], imap: [] }),
  toErrorMessage: (err: unknown, fallback: string) => (err instanceof Error ? err.message : fallback),
}));
vi.mock("../../app/mailboxes", () => ({ mailboxName: () => "", useMailboxes: () => ({ mailboxes: [], selected: "" }) }));
vi.mock("../../api/rules", () => ({
  listRules: async () => [{
    id: "r1", name: "Receipts", enabled: true, order: 0, scope: {}, rev: 1, createdAt: "", updatedAt: "", guiEditable: true,
    match: { op: "allof", conditions: [{ field: "to", comparator: "is", value: "receipts", part: "detail" }] },
    actions: [{ type: "move", value: "Receipts" }],
  }],
}));

afterEach(cleanup);

it("edits the plus-address part of an address condition", async () => {
  render(<Filters />);
  expect(await screen.findByText(/to tag after \+ is "receipts"/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  const part = screen.getByRole("combobox", { name: "Address part" }) as HTMLSelectElement;
  expect(part.value).toBe("detail");
  expect([...screen.getAllByRole("option")].some((o) => o.textContent === "exists" && o.closest("select") !== part)).toBe(false);
  fireEvent.change(screen.getByDisplayValue("to"), { target: { value: "subject" } });
  expect(screen.queryByRole("combobox", { name: "Address part" })).toBeNull();
});
