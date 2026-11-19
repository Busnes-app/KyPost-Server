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

const incoming = vi.hoisted(() => ({ identity: undefined as unknown }));
vi.mock("./IncomingEncryption", () => ({ IncomingEncryption: (props: { identity: unknown }) => { incoming.identity = props.identity; return null; } }));

import { MailKeys } from "./MailKeys";
import type { PGPIdentity } from "../../../api/pgp";
import type { PGPSessionState } from "../../../lib/pgpSession";

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

describe("MailKeys incoming encryption", () => {
  // Storing a recovery copy moves the server's revision and reloads only the
  // session snapshot; enabling incoming encryption must not send the old one.
  it("sends the session's current PGP revision, not the identity's stale one", () => {
    const identity: PGPIdentity = { pgpRevision: 1, fingerprint: "fp", keyId: "key", publicKey: "public", source: "generated", createdAt: "now" };
    const session: PGPSessionState = { loaded: true, unlocked: true, error: "", bootstrap: { pgpRevision: 3, hasIdentity: true, protection: "client" } as PGPSessionState["bootstrap"] };
    render(<MemoryRouter><MailKeys pgpIdentity={identity} pgpSession={session} /></MemoryRouter>);
    expect(incoming.identity).toMatchObject({ fingerprint: "fp", pgpRevision: 3 });
  });
});
