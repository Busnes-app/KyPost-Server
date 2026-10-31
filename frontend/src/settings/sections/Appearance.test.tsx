import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { THEME_PAIRS } from "../../ky-ui/themes";
import { applyStoredTheme, THEME_STORAGE_KEY } from "../../theme";
import { Appearance } from "./Appearance";

const root = document.documentElement;
const mode = (pair: string, label: string) =>
  within(screen.getByRole("group", { name: pair })).getByRole("radio", { name: label }) as HTMLInputElement;

describe("Appearance", () => {
  beforeEach(() => { localStorage.clear(); root.removeAttribute("style"); delete root.dataset.theme; });
  afterEach(cleanup);

  it("offers Match system, Light and Dark per pair as one radio group without inline styles", () => {
    render(<Appearance />);
    expect(screen.getAllByRole("radio")).toHaveLength(THEME_PAIRS.length * 3);
    expect(new Set(screen.getAllByRole("radio").map((r) => (r as HTMLInputElement).name)).size).toBe(1);
    expect(mode("Busnes", "Match system").checked).toBe(true);
    expect(document.querySelector(".ky-appearance [style]")).toBeNull();
  });

  it("marks only the selected segment with a check", () => {
    render(<Appearance />);
    fireEvent.click(mode("Ocean", "Dark"));
    expect(document.querySelectorAll(".appearance-mode svg")).toHaveLength(1);
    expect(mode("Ocean", "Dark").closest("label")?.querySelector("svg")).not.toBeNull();
  });

  it("shows an earlier saved name as its half and leaves the saved value alone", () => {
    localStorage.setItem(THEME_STORAGE_KEY, "Current");
    applyStoredTheme();
    render(<Appearance />);
    expect(mode("Matter", "Dark").checked).toBe(true);
    expect(root.style.getPropertyValue("--bg")).toBe("#1d1b19");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("Current");
  });

  it("falls back to Busnes following the OS for a removed palette", () => {
    localStorage.setItem(THEME_STORAGE_KEY, "Cyber Punk");
    applyStoredTheme();
    render(<Appearance />);
    expect(mode("Busnes", "Match system").checked).toBe(true);
    expect(root.dataset.theme).toMatch(/^busnes-(light|dark)$/);
  });

  it("saves half names and pair IDs, applies base palettes exactly and returns to Busnes and the OS", () => {
    render(<Appearance />);
    fireEvent.click(mode("Nord", "Dark"));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("Nord Dark");
    expect(root.style.getPropertyValue("--line-strong")).toBe("#5e81ac");
    expect(root.dataset.theme).toBeUndefined();
    fireEvent.click(mode("Ky", "Match system"));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("ky-pair");
    expect(root.style.getPropertyValue("--ink-strong")).toMatch(/^#(0f172a|e2e8f0)$/);
    fireEvent.click(mode("Busnes", "Light"));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("Busnes Light");
    expect(root.dataset.theme).toBe("busnes-light");
    expect(root.style.getPropertyValue("--line-strong")).toBe("");
    fireEvent.click(mode("Busnes", "Match system"));
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("system");
    expect(root.dataset.theme).toMatch(/^busnes-(light|dark)$/);
  });
});
