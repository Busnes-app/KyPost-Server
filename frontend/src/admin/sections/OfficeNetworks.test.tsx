import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { OfficeNetworks } from "./OfficeNetworks";

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
});

it("shows the stored networks one per line and saves an edited list", async () => {
  getJSON.mockResolvedValue({ networks: "203.0.113.0/24", environment: "192.0.2.0/24", exempted: 41 });
  putJSON.mockResolvedValue({ networks: "203.0.113.0/24,198.51.100.7/32", environment: "192.0.2.0/24", exempted: 0 });
  const { container } = render(<OfficeNetworks />);
  const box = (await screen.findByDisplayValue("203.0.113.0/24")) as HTMLTextAreaElement;
  expect(getJSON).toHaveBeenCalledWith("/api/admin/rate-limit-networks");
  expect(container.textContent).toMatch(/41 since/);
  expect(container.textContent).toMatch(/192\.0\.2\.0\/24/);
  expect(container.textContent).toMatch(/never add guest Wi-Fi, a VPN/);

  fireEvent.change(box, { target: { value: "203.0.113.0/24\n 198.51.100.7 \n" } });
  fireEvent.click(screen.getByRole("button", { name: "Save networks" }));
  expect((await screen.findByRole("status")).textContent).toMatch(/Saved/);
  expect(putJSON).toHaveBeenCalledWith("/api/admin/rate-limit-networks", { networks: "203.0.113.0/24,198.51.100.7" });
  expect(box.value).toBe("203.0.113.0/24\n198.51.100.7/32");
});

it("shows why the server refused a list and keeps what was typed", async () => {
  getJSON.mockResolvedValue({ networks: "", environment: "", exempted: 0 });
  putJSON.mockRejectedValue(new Error('request failed: 400 - "10.0.0.0/8" is too broad; the widest IPv4 range allowed is /16'));
  const { container } = render(<OfficeNetworks />);
  await screen.findByText(/0 since/);
  const box = screen.getByPlaceholderText("203.0.113.0/24") as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "10.0.0.0/8" } });
  fireEvent.click(screen.getByRole("button", { name: "Save networks" }));
  expect((await screen.findByRole("alert")).textContent).toMatch(/too broad; the widest IPv4 range allowed is \/16/);
  expect(box.value).toBe("10.0.0.0/8");
  expect(container.textContent).not.toMatch(/Also set by the server environment/);
});
