import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EmailServer } from "./EmailServer";

const getJSON = vi.fn();
const postJSON = vi.fn();
const deleteJSON = vi.fn();

vi.mock("../../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  deleteJSON: (url: string) => deleteJSON(url),
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback)
}));

const getMailDefaults = vi.fn();
vi.mock("../../api/mailDefaults", () => ({ getMailDefaults: () => getMailDefaults() }));

afterEach(cleanup);

beforeEach(() => {
  getJSON.mockReset();
  postJSON.mockReset();
  deleteJSON.mockReset();
  getJSON.mockResolvedValue({ configured: true, host: "imap.example.com", port: 993, username: "gwen", mailbox: "INBOX" });
  postJSON.mockResolvedValue({ ok: true });
  getMailDefaults.mockReset();
  getMailDefaults.mockResolvedValue({ host: "", port: 0, smtpHost: "", smtpPort: 0 });
});

describe("EmailServer", () => {
  it("loads the stored settings on mount without being told to", async () => {
    render(<EmailServer />);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.com")).toBeTruthy());
  });

  it("saves the form on its own, with no parent involvement", async () => {
    render(<EmailServer />);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.com")).toBeTruthy());
    await userEvent.type(screen.getByLabelText(/password/i), "app-password");
    await userEvent.click(screen.getByRole("button", { name: /save email settings/i }));

    // Pin the actual save request, not just "postJSON ran" — that would
    // still pass if Save posted to the wrong endpoint or an empty body.
    await waitFor(() =>
      expect(postJSON).toHaveBeenCalledWith(
        "/api/imap/config",
        expect.objectContaining({
          host: "imap.example.com",
          port: 993,
          username: "gwen",
          password: "app-password",
          mailbox: "INBOX"
        })
      )
    );
  });

  it("prefills host and ports from instance defaults when nothing is configured", async () => {
    getJSON.mockResolvedValue({ configured: false });
    getMailDefaults.mockResolvedValue({ host: "imap.home.test", port: 993, smtpHost: "smtp.home.test", smtpPort: 465 });
    render(<EmailServer />);
    await waitFor(() => expect(screen.getByDisplayValue("imap.home.test")).toBeTruthy());
    expect(screen.getByDisplayValue("smtp.home.test")).toBeTruthy();
    expect(screen.getByDisplayValue("465")).toBeTruthy();
    // Username stays blank: defaults never carry an identity.
    expect((screen.getByLabelText(/^username/i) as HTMLInputElement).value).toBe("");
  });

  it("does not override a stored config with defaults", async () => {
    getMailDefaults.mockResolvedValue({ host: "imap.home.test", port: 993, smtpHost: "", smtpPort: 0 });
    render(<EmailServer />);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.com")).toBeTruthy());
    expect(screen.queryByDisplayValue("imap.home.test")).toBeNull();
  });

  it("renders read-only with a notice when managed by an admin", async () => {
    getJSON.mockResolvedValue({ configured: true, managed: true, host: "imap.example.com", port: 993, username: "gwen", mailbox: "INBOX" });
    render(<EmailServer />);
    await waitFor(() => expect(screen.getByText(/managed by your administrator/i)).toBeTruthy());
    expect((screen.getByDisplayValue("imap.example.com") as HTMLInputElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: /save email settings/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /delete stored email settings/i })).toBeNull();
    // Testing the connection stays available; it only reads stored credentials.
    const testButton = screen.getByRole("button", { name: /test email settings/i });
    expect(testButton).toBeTruthy();

    await userEvent.click(testButton);
    await waitFor(() =>
      expect(postJSON).toHaveBeenCalledWith("/api/imap/test", { mailbox: "INBOX" })
    );
    const [, body] = postJSON.mock.calls[postJSON.mock.calls.length - 1];
    expect(body).not.toHaveProperty("password");
  });

  it("tests the full form when the user has typed a new password", async () => {
    render(<EmailServer />);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.com")).toBeTruthy());
    await userEvent.type(screen.getByLabelText(/password/i), "app-password");
    await userEvent.click(screen.getByRole("button", { name: /test email settings/i }));

    await waitFor(() =>
      expect(postJSON).toHaveBeenCalledWith(
        "/api/imap/test",
        expect.objectContaining({
          host: "imap.example.com",
          port: 993,
          username: "gwen",
          password: "app-password",
          mailbox: "INBOX"
        })
      )
    );
  });
});
