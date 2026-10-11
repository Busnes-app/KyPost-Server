import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth", () => ({ useAuth: () => ({ userId: "u1" }) }));
vi.mock("../../../api/contacts", () => ({ listContacts: async () => [] }));
vi.mock("../../../api/pgp", async (importOriginal) => ({
  ...(await importOriginal<object>()),
  getPGPDiscoverySettings: async () => ({}),
  listDiscoverySuppressions: async () => ({ suppressions: [] })
}));

import { MailKeys } from "./MailKeys";

afterEach(cleanup);

describe("MailKeys key import", () => {
  // Enhanced (cloud) spellcheck uploads field contents; a pasted private key must not go.
  it("keeps the pasted private key out of spellcheck and autofill", async () => {
    render(<MemoryRouter><MailKeys /></MemoryRouter>);
    await userEvent.click(screen.getByRole("button", { name: "Import existing key" }));
    const key = screen.getByLabelText("Armored private key");
    expect(key.getAttribute("spellcheck")).toBe("false");
    expect(key.getAttribute("autocomplete")).toBe("off");
    expect(key.getAttribute("autocorrect")).toBe("off");
    expect(key.getAttribute("autocapitalize")).toBe("off");
  });
});
