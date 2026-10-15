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
});
