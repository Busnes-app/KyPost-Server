import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router";
import { PanelTabs, resolvePanelTab } from "./PanelTabs";

afterEach(cleanup);

const TABS = [
  { id: "one", label: "First", body: <p>first body</p> },
  { id: "two", label: "Second", body: <p>second body</p> },
  { id: "three", label: "Third", body: <p>third body</p> }
];

function Location() {
  const location = useLocation();
  return <p data-testid="location">{location.search}</p>;
}

function renderTabs(initial = "/settings/mail") {
  return render(
    <MemoryRouter initialEntries={[initial]}>
      <PanelTabs tabs={TABS} ariaLabel="Test sections" />
      <Location />
    </MemoryRouter>
  );
}

describe("resolvePanelTab", () => {
  it("opens a real tab", () => {
    expect(resolvePanelTab("two", TABS)).toBe("two");
  });

  it("falls back to the first tab for anything unrecognised, so a bad link still renders", () => {
    expect(resolvePanelTab(null, TABS)).toBe("one");
    expect(resolvePanelTab("", TABS)).toBe("one");
    expect(resolvePanelTab("nope", TABS)).toBe("one");
    expect(resolvePanelTab("../../etc/passwd", TABS)).toBe("one");
  });
});

describe("PanelTabs", () => {
  it("renders only the active tab's body", async () => {
    renderTabs();
    expect(screen.getByText("first body")).toBeTruthy();
    expect(screen.queryByText("second body")).toBeNull();

    await userEvent.click(screen.getByRole("tab", { name: "Second" }));

    expect(screen.getByText("second body")).toBeTruthy();
    expect(screen.queryByText("first body")).toBeNull();
  });

  it("opens the tab named in the URL, which is what the retired routes point at", () => {
    renderTabs("/settings/mail?tab=two");
    expect(screen.getByText("second body")).toBeTruthy();
  });

  it("marks exactly one tab selected, so assistive tech is never told two are", async () => {
    renderTabs();
    const selected = () => screen.getAllByRole("tab").filter((t) => t.getAttribute("aria-selected") === "true");
    expect(selected()).toHaveLength(1);

    await userEvent.click(screen.getByRole("tab", { name: "Second" }));
    expect(selected()).toHaveLength(1);
  });

  it("moves focus with the arrows and opens a tab only on Enter or Space", async () => {
    const user = userEvent.setup();
    renderTabs();
    const tab = (name: string) => screen.getByRole("tab", { name });
    await user.click(tab("First"));
    await user.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(tab("Second"));
    expect(tab("First").getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText("first body")).toBeTruthy();
    await user.keyboard("{Enter}");
    expect(tab("Second").getAttribute("aria-selected")).toBe("true");
    expect(screen.getByTestId("location").textContent).toBe("?tab=two");
    // Left from the first wraps to the last; End and Home jump.
    await user.keyboard("{ArrowLeft}{ArrowLeft}");
    expect(document.activeElement).toBe(tab("Third"));
    await user.keyboard("{Home}");
    expect(document.activeElement).toBe(tab("First"));
    await user.keyboard("{End} ");
    expect(tab("Third").getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText("third body")).toBeTruthy();
    // Only the selected tab is a Tab stop.
    expect(screen.getAllByRole("tab").map((t) => t.tabIndex)).toEqual([-1, -1, 0]);
  });

  it("labels the open panel by its tab, which controls it", async () => {
    renderTabs("/settings/mail?tab=two");
    const panel = screen.getByRole("tabpanel", { name: "Second" });
    expect(screen.getByRole("tab", { name: "Second" }).getAttribute("aria-controls")).toBe(panel.id);
    expect(screen.getAllByRole("tabpanel")).toHaveLength(1);
  });
});

