import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AppearancePanel } from "./AppearancePanel";
import { MailPanel } from "./MailPanel";

describe("Reading pane setting", () => {
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

  it("sits in Appearance, not Mail", () => {
    render(<AppearancePanel />);
    expect(screen.getByRole("group", { name: "Reading pane" })).toBeTruthy();
    cleanup();
    vi.stubGlobal("fetch", () => new Promise(() => {}));
    render(<MemoryRouter><MailPanel /></MemoryRouter>);
    expect(screen.getByRole("tab", { name: "Email settings" })).toBeTruthy();
    expect(screen.queryByRole("tab", { name: "Reading pane" })).toBeNull();
  });
});
