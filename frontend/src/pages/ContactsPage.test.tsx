import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ContactsPage } from "./ContactsPage";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const fetchMock = vi.fn<typeof fetch>();

beforeEach(() => {
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation(async (url, init) => {
    if (url === "/api/contacts" && init?.method === "POST") {
      return new Response("invalid PGP key: this is a private key; paste the public key block instead\n", {
        status: 400,
        headers: { "Content-Type": "text/plain; charset=utf-8" }
      });
    }
    if (String(url).startsWith("/api/contacts")) return json({ contacts: [] });
    if (String(url).startsWith("/api/groups")) return json({ groups: [] });
    return json({});
  });
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); });

describe("ContactsPage", () => {
  it("shows a refused key inside the open form, where the user can see it", async () => {
    render(<ContactsPage />);
    await userEvent.click(await screen.findByRole("button", { name: "New Contact" }));
    const form = screen.getByRole("heading", { name: "Add Contact" }).closest("form")!;
    await userEvent.type(within(form).getByLabelText("Full Name"), "Bob");
    await userEvent.click(within(form).getByRole("button", { name: "Add Contact" }));
    expect((await within(form).findByRole("alert")).textContent).toContain("private key");
  });

  it("keeps Close in the header and Cancel beside the primary in the footer", async () => {
    render(<ContactsPage />);
    await userEvent.click(await screen.findByRole("button", { name: "New Contact" }));
    const form = screen.getByRole("form", { name: "Add Contact" });
    const dialog = form.closest("dialog")!;
    const footerButtons = within(form.querySelector("footer")!).getAllByRole("button").map((b) => b.textContent);
    expect(footerButtons).toEqual(["Cancel", "Add Contact"]);
    await userEvent.click(within(form.querySelector("header")!).getByRole("button", { name: "Close" }));
    expect(dialog.open).toBe(false);
  });

  it("opens an optional group when the saved contact has data in it", async () => {
    fetchMock.mockImplementation(async (url) => {
      if (String(url).startsWith("/api/contacts")) {
        return json({ contacts: [{ uid: "c1", rev: 1, createdAt: "", updatedAt: "", fn: "Dana Ops", nickname: "Dee" }] });
      }
      if (String(url).startsWith("/api/groups")) return json({ groups: [] });
      return json({});
    });
    render(<ContactsPage />);
    await userEvent.click(await screen.findByRole("button", { name: "Edit" }));
    const form = screen.getByRole("form", { name: "Edit Contact" });
    const group = (summary: string) => within(form).getByText(summary).closest("details")!;
    expect(group("More name fields").open).toBe(true);
    expect(group("Addresses and links").open).toBe(false);
    // Every field stays in the form, open or not.
    expect((within(form).getByLabelText("Nickname") as HTMLInputElement).value).toBe("Dee");
  });

  it("opens a collapsed group whose field blocks submit", async () => {
    render(<ContactsPage />);
    await userEvent.click(await screen.findByRole("button", { name: "New Contact" }));
    const form = screen.getByRole("form", { name: "Add Contact" });
    const links = within(form).getByText("Addresses and links").closest("details")!;
    await userEvent.click(within(links).getByRole("button", { name: "+ Add website" }));
    await userEvent.type(within(links).getByPlaceholderText("https://..."), "not a url");
    expect(links.open).toBe(false);
    await userEvent.click(within(form).getByRole("button", { name: "Add Contact" }));
    expect(links.open).toBe(true);
  });
});
