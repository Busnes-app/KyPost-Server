import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MailDefaults } from "./MailDefaults";

const getMailDefaults = vi.fn();
const putMailDefaults = vi.fn();
vi.mock("../../api/mailDefaults", () => ({
  getMailDefaults: () => getMailDefaults(),
  putMailDefaults: (d: unknown) => putMailDefaults(d)
}));

afterEach(cleanup);
beforeEach(() => {
  getMailDefaults.mockReset();
  putMailDefaults.mockReset();
  getMailDefaults.mockResolvedValue({ host: "imap.home.test", port: 993, smtpHost: "", smtpPort: 0 });
  putMailDefaults.mockImplementation(async (d: unknown) => d);
});

describe("MailDefaults", () => {
  it("loads current defaults and saves edits", async () => {
    render(<MailDefaults />);
    await waitFor(() => expect(screen.getByDisplayValue("imap.home.test")).toBeTruthy());
    await userEvent.clear(screen.getByLabelText(/smtp host/i));
    await userEvent.type(screen.getByLabelText(/smtp host/i), "smtp.home.test");
    await userEvent.click(screen.getByRole("button", { name: /save defaults/i }));
    await waitFor(() =>
      expect(putMailDefaults).toHaveBeenCalledWith({ host: "imap.home.test", port: 993, smtpHost: "smtp.home.test", smtpPort: 587 })
    );
    expect(await screen.findByText(/defaults saved/i)).toBeTruthy();
  });

  it("surfaces a failed load instead of showing blanks", async () => {
    getMailDefaults.mockReset();
    getMailDefaults.mockRejectedValue(new Error("boom"));
    render(<MailDefaults />);
    expect(await screen.findByText(/failed to load current defaults/i)).toBeTruthy();
  });
});
