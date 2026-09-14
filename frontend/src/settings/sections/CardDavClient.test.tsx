import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { CardDavClient } from "./CardDavClient";

const getCardDAVClientConfig = vi.fn();
const saveCardDAVClientConfig = vi.fn();
const deleteCardDAVClientConfig = vi.fn();
const syncCardDAVClient = vi.fn();

vi.mock("../../api/contacts", () => ({
  getCardDAVClientConfig: () => getCardDAVClientConfig(),
  saveCardDAVClientConfig: (i: unknown) => saveCardDAVClientConfig(i),
  deleteCardDAVClientConfig: () => deleteCardDAVClientConfig(),
  syncCardDAVClient: () => syncCardDAVClient()
}));

afterEach(cleanup);
beforeEach(() => {
  getCardDAVClientConfig.mockReset();
});

describe("CardDavClient", () => {
  it("renders editable when not managed", async () => {
    getCardDAVClientConfig.mockResolvedValue({ configured: true, serverUrl: "https://c.example.test/dav/", username: "gwen" });
    render(<CardDavClient />);
    await waitFor(() => expect(screen.getByDisplayValue("https://c.example.test/dav/")).toBeTruthy());
    expect(screen.getByRole("button", { name: /save carddav client/i })).toBeTruthy();
  });

  it("renders read-only with a notice when managed, but keeps Sync Now", async () => {
    getCardDAVClientConfig.mockResolvedValue({ configured: true, managed: true, serverUrl: "https://c.example.test/dav/", username: "gwen" });
    render(<CardDavClient />);
    await waitFor(() => expect(screen.getByText(/managed by your administrator/i)).toBeTruthy());
    expect((screen.getByDisplayValue("https://c.example.test/dav/") as HTMLInputElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: /save carddav client/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /delete stored configuration/i })).toBeNull();
    expect(screen.getByRole("button", { name: /sync now/i })).toBeTruthy();
  });
});
