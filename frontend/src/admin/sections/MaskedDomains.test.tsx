import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MaskedDomains } from "./MaskedDomains";

const getJSON = vi.fn();
const putJSON = vi.fn();

vi.mock("../../api/client", () => ({
  HttpError: class HttpError extends Error { status = 0; },
  getJSON: (url: string) => getJSON(url),
  putJSON: (url: string, body: unknown) => putJSON(url, body),
  toErrorMessage: (err: unknown, fallback: string) => (err instanceof Error ? err.message : fallback),
}));
vi.mock("../../api/users", () => ({
  confirmedAdmin: (_: unknown, body: unknown, send: (b: unknown, h?: unknown) => Promise<unknown>) => send(body),
}));
vi.mock("../../auth", () => ({ useAuth: () => ({ ssoSession: true }) }));

afterEach(() => {
  cleanup();
  getJSON.mockReset();
  putJSON.mockReset();
});

const domain = (name: string) => ({ domain: name, retired: false, configured: true, established: true, recordName: `_kypost-mail.${name}`, recordValue: `kypost-mail-verify=${"a".repeat(64)}`, expiresAt: 0, verifiedUntil: 0 });

it("is off until a domain is ticked, and saves the chosen domains", async () => {
  getJSON.mockImplementation(async (url: string) => url === "/api/admin/masked-domains"
    ? { domains: [] }
    : { receivingEnabled: false, issuer: "https://idp.example", founding: "example.test", domains: [domain("example.test"), domain("masked.test")] });
  putJSON.mockResolvedValue({ domains: ["masked.test"] });
  render(<MaskedDomains />);
  const box = (await screen.findByRole("checkbox", { name: /masked\.test/ })) as HTMLInputElement;
  expect(box.checked).toBe(false);
  expect(screen.getByText(/keeps the feature off/)).toBeTruthy();
  fireEvent.click(box);
  fireEvent.click(screen.getByRole("button", { name: "Save masked domains" }));
  expect((await screen.findByRole("status")).textContent).toMatch(/Users can make masked addresses/);
  expect(putJSON).toHaveBeenCalledWith("/api/admin/masked-domains", { domains: ["masked.test"] });
});
