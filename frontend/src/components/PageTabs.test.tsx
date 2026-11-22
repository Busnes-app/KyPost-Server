import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageTabs } from "./PageTabs";

afterEach(cleanup);

it("is page navigation, not tabs: a labelled nav whose current page is aria-current", async () => {
  const onSelect = vi.fn();
  render(<PageTabs totalPages={3} currentPage={2} onSelect={onSelect} classPrefix="contacts" ariaLabel="Contact pages" />);
  const nav = screen.getByRole("navigation", { name: "Contact pages" });
  expect(screen.queryByRole("tab")).toBeNull();
  expect([...nav.querySelectorAll("button")].map((b) => [b.textContent, b.getAttribute("aria-current")])).toEqual([["1", null], ["2", "page"], ["3", null]]);
  expect(screen.getByRole("button", { name: "2" }).className).toBe("contacts-page-tab active");
  await userEvent.click(screen.getByRole("button", { name: "3" }));
  expect(onSelect).toHaveBeenCalledWith(3);
});

it("renders nothing for a single page", () => {
  const { container } = render(<PageTabs totalPages={1} currentPage={1} onSelect={() => {}} classPrefix="inbox" ariaLabel="Email pages" />);
  expect(container.innerHTML).toBe("");
});
