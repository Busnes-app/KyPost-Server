import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { READING_PANE_STORAGE_KEY } from "../../pages/read/readingPane";
import { ReadingPane } from "./ReadingPane";

afterEach(() => {
  cleanup();
  localStorage.clear();
});

describe("ReadingPane", () => {
  it("defaults to Right of list and saves the choice in this browser", async () => {
    const user = userEvent.setup();
    render(<ReadingPane />);
    const group = screen.getByRole("group", { name: "Reading pane" });
    expect(group).toBeDefined();
    expect((screen.getByRole("radio", { name: "Right of list" }) as HTMLInputElement).checked).toBe(true);

    await user.click(screen.getByRole("radio", { name: "Off" }));
    expect(localStorage.getItem(READING_PANE_STORAGE_KEY)).toBe("off");
    expect(screen.getByRole("radio", { name: "Off" }).closest("label")?.querySelector("svg")).not.toBeNull();
    expect(document.querySelectorAll(".appearance-mode svg")).toHaveLength(1);
  });

  it("reads an earlier choice and ignores one it does not know", () => {
    localStorage.setItem(READING_PANE_STORAGE_KEY, "below");
    render(<ReadingPane />);
    expect((screen.getByRole("radio", { name: "Below list" }) as HTMLInputElement).checked).toBe(true);
    cleanup();
    localStorage.setItem(READING_PANE_STORAGE_KEY, "sideways");
    render(<ReadingPane />);
    expect((screen.getByRole("radio", { name: "Right of list" }) as HTMLInputElement).checked).toBe(true);
  });
});
