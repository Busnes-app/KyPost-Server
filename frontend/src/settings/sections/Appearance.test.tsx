import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { THEME_CATALOG } from "../../ky-ui/themes";
import { applyStoredTheme, THEME_STORAGE_KEY } from "../../theme";
import { Appearance } from "./Appearance";

const root = document.documentElement;
const radio = (name: RegExp) => screen.getByRole("radio", { name }) as HTMLInputElement;

describe("Appearance", () => {
  beforeEach(() => { localStorage.clear(); root.removeAttribute("style"); delete root.dataset.theme; });
  afterEach(cleanup);

  it("offers Match system and every catalog theme as native radios without inline styles", () => {
    render(<Appearance />);
    expect(screen.getAllByRole("radio")).toHaveLength(THEME_CATALOG.length + 1);
    expect(radio(/^Match system/).checked).toBe(true);
    expect(document.querySelector(".ky-appearance [style]")).toBeNull();
  });

  it("shows an earlier saved name as its theme and leaves the saved value alone", () => {
    localStorage.setItem(THEME_STORAGE_KEY, "Current");
    applyStoredTheme();
    render(<Appearance />);
    expect(radio(/^Dark Matter/).checked).toBe(true);
    expect(root.style.getPropertyValue("--bg")).toBe("#1a1a1e");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("Current");
  });

  it("saves names, applies base palettes exactly and returns to Busnes and the OS", () => {
    render(<Appearance />);
    fireEvent.click(radio(/^Nord Slate/));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("Nord Slate");
    expect(root.style.getPropertyValue("--line-strong")).toBe("#5e81ac");
    expect(root.dataset.theme).toBeUndefined();
    fireEvent.click(radio(/^Patina Ky \(base\)/));
    expect(root.style.getPropertyValue("--ink-strong")).toBe("#f1f5f9");
    fireEvent.click(radio(/^Patina Ky\s*Dark$/));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("Patina Ky");
    expect(root.style.getPropertyValue("--ink-strong")).toBe("#e2e8f0");
    fireEvent.click(radio(/^Busnes Light/));
    expect(root.dataset.theme).toBe("busnes-light");
    expect(root.style.getPropertyValue("--line-strong")).toBe("");
    fireEvent.click(radio(/^Match system/));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("system");
    expect(root.dataset.theme).toMatch(/^busnes-(light|dark)$/);
  });
});
