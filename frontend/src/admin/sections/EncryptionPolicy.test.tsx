import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { EncryptionPolicy } from "./EncryptionPolicy";

const getJSON = vi.fn();
const putJSON = vi.fn();

vi.mock("../../api/client", () => ({
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
  vi.restoreAllMocks();
});

it("does not offer Required beside a generic provider and says why", async () => {
  getJSON.mockResolvedValue({ key: "optional", incoming: "on", genericProvider: true, issuer: "https://auth.example.test" });
  const { container } = render(<EncryptionPolicy />);
  await screen.findByDisplayValue("Optional");
  const required = screen.getAllByRole("option", { name: "Required" }) as HTMLOptionElement[];
  expect(required).toHaveLength(2);
  expect(required.every((o) => o.disabled)).toBe(true);
  expect(container.textContent).toMatch(/Needs KySignOn: users signing in through auth\.example\.test couldn't create a key and would be locked out\./);
});

it("Required incoming mail also requires the key and warns before saving", async () => {
  getJSON.mockResolvedValue({ key: "optional", incoming: "on", genericProvider: false });
  putJSON.mockResolvedValue({ key: "required", incoming: "required", genericProvider: false });
  const confirm = vi.spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
  const { container } = render(<EncryptionPolicy />);
  const incoming = (await screen.findByLabelText("Encrypt incoming mail")) as HTMLSelectElement;
  fireEvent.change(incoming, { target: { value: "required" } });
  expect((screen.getByLabelText("Encryption key") as HTMLSelectElement).value).toBe("required");
  expect(container.textContent).toMatch(/also sets Encryption key to Required/);
  expect(container.textContent).toMatch(/doesn't sweep existing mail/);

  fireEvent.click(screen.getByRole("button", { name: "Save encryption policy" }));
  expect(confirm.mock.calls[0][0]).toMatch(/search, Sieve body rules and AI labels.*only headers/);
  expect(putJSON).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: "Save encryption policy" }));
  await screen.findByRole("status");
  expect(putJSON).toHaveBeenCalledWith("/api/admin/encryption-policy", { key: "required", incoming: "required" });
});

it("shows the server's refusal", async () => {
  getJSON.mockResolvedValue({ key: "optional", incoming: "on", genericProvider: false });
  putJSON.mockRejectedValue(new Error("request failed: 409 - Needs KySignOn: users signing in through idp couldn't create a key"));
  render(<EncryptionPolicy />);
  fireEvent.change(await screen.findByLabelText("Encryption key"), { target: { value: "required" } });
  fireEvent.click(screen.getByRole("button", { name: "Save encryption policy" }));
  expect((await screen.findByRole("alert")).textContent).toMatch(/Needs KySignOn/);
});
