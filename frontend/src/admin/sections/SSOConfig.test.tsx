import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SSOConfig } from "./SSOConfig";

const putJSON = vi.fn();

vi.mock("../../api/client", () => ({
  getJSON: () =>
    Promise.resolve({ enabled: true, issuerUrl: "https://id.example", clientId: "kypost", autoProvision: true, allowInsecureIssuer: false }),
  putJSON: (url: string, body: unknown) => putJSON(url, body),
  toErrorMessage: (_: unknown, fallback: string) => fallback,
}));
vi.mock("../../api/users", () => ({
  confirmedAdmin: (_: unknown, body: unknown, send: (b: unknown, h?: unknown) => Promise<unknown>) => send(body),
}));
vi.mock("../../auth", () => ({ useAuth: () => ({ ssoSession: true }) }));

afterEach(cleanup);

it("saves the administrator group, empty by default", async () => {
  putJSON.mockImplementation((_: string, body: object) => Promise.resolve({ settings: body }));
  render(<SSOConfig />);

  const field = (await screen.findByLabelText(/administrator group/i)) as HTMLInputElement;
  expect(field.value).toBe("");
  fireEvent.change(field, { target: { value: "/kypost-admins" } });
  fireEvent.click(screen.getByRole("button", { name: /save sso settings/i }));

  await waitFor(() => expect(putJSON).toHaveBeenCalled());
  expect(putJSON.mock.calls[0][0]).toBe("/api/admin/sso");
  expect(putJSON.mock.calls[0][1]).toMatchObject({ adminGroup: "/kypost-admins" });
});
