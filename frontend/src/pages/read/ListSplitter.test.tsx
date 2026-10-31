import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ListSplitter, SPLIT_AXES } from "./ListSplitter";

const WIDTH_KEY = SPLIT_AXES.right.key;

afterEach(() => {
  cleanup();
  localStorage.clear();
});

function Layout({ pane = "right" }: { pane?: "right" | "below" }) {
  return (
    <div data-testid="layout">
      <ListSplitter pane={pane} />
    </div>
  );
}

describe("ListSplitter", () => {
  it("starts from the saved width and saves keyboard resizes within bounds", async () => {
    localStorage.setItem(WIDTH_KEY, "40");
    const user = userEvent.setup();
    render(<Layout />);
    const handle = screen.getByRole("separator", { name: "Resize message list" });
    expect(handle.getAttribute("aria-valuenow")).toBe("40");
    expect(screen.getByTestId("layout").getAttribute("style")).toContain("--read-list-size: 40%");

    handle.focus();
    await user.keyboard("{ArrowRight}");
    expect(handle.getAttribute("aria-valuenow")).toBe("42");
    expect(localStorage.getItem(WIDTH_KEY)).toBe("42");

    await user.keyboard("{End}{ArrowRight}");
    expect(handle.getAttribute("aria-valuenow")).toBe("65");
  });

  it("ignores a saved width it cannot use", () => {
    localStorage.setItem(WIDTH_KEY, "wide");
    render(<Layout />);
    expect(screen.getByRole("separator").getAttribute("aria-valuenow")).toBe("52");
  });

  it("resizes the list's height with the up and down keys when the pane is below", async () => {
    const user = userEvent.setup();
    render(<Layout pane="below" />);
    const handle = screen.getByRole("separator", { name: "Resize message list" });
    expect(handle.getAttribute("aria-orientation")).toBe("horizontal");
    expect(handle.getAttribute("aria-valuenow")).toBe("45");
    handle.focus();
    await user.keyboard("{ArrowDown}");
    expect(localStorage.getItem(SPLIT_AXES.below.key)).toBe("47");
    expect(localStorage.getItem(WIDTH_KEY)).toBeNull();
  });
});
