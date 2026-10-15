import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AssignMailbox } from "./AssignMailbox";
import type { AdminConfirm, ManagedUser } from "../../api/users";

const api = {
  getUserIMAPConfig: vi.fn(),
  putUserIMAPConfig: vi.fn(),
  deleteUserIMAPConfig: vi.fn(),
  getUserCardDAVClient: vi.fn(),
  putUserCardDAVClient: vi.fn(),
  deleteUserCardDAVClient: vi.fn()
};
vi.mock("../../api/users", () => ({
  getUserIMAPConfig: (id: string) => api.getUserIMAPConfig(id),
  putUserIMAPConfig: (c: AdminConfirm, id: string, i: unknown) => api.putUserIMAPConfig(c, id, i),
  deleteUserIMAPConfig: (c: AdminConfirm, id: string) => api.deleteUserIMAPConfig(c, id),
  getUserCardDAVClient: (id: string) => api.getUserCardDAVClient(id),
  putUserCardDAVClient: (c: AdminConfirm, id: string, i: unknown) => api.putUserCardDAVClient(c, id, i),
  deleteUserCardDAVClient: (c: AdminConfirm, id: string) => api.deleteUserCardDAVClient(c, id)
}));

const user: ManagedUser = {
  id: "u1", username: "gwen", role: "user", active: true, mustChangePassword: false, createdAt: "", updatedAt: ""
};

const confirm: AdminConfirm = { ssoSession: false, accountPassword: "admin-pw" };
const view = <AssignMailbox user={user} unlocked takeConfirm={() => confirm} onClose={() => undefined} />;

afterEach(cleanup);
beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.getUserIMAPConfig.mockResolvedValue({ configured: true, host: "imap.example.test", port: 993, username: "gwen", mailbox: "INBOX", managed: false });
  api.getUserCardDAVClient.mockResolvedValue({ configured: false });
  api.putUserIMAPConfig.mockResolvedValue({ configured: true, managed: true });
  api.putUserCardDAVClient.mockResolvedValue({ configured: true, managed: false });
});

describe("AssignMailbox", () => {
  it("loads the user's current mailbox and saves with the lock flag", async () => {
    render(view);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.test")).toBeTruthy());
    await userEvent.click(screen.getByLabelText(/lock mail settings/i));
    await userEvent.click(screen.getByRole("button", { name: /save mailbox/i }));
    await waitFor(() =>
      expect(api.putUserIMAPConfig).toHaveBeenCalledWith(
        confirm,
        "u1",
        expect.objectContaining({ host: "imap.example.test", username: "gwen", password: "", managed: true })
      )
    );
  });

  it("disables changes until the admin can confirm them", async () => {
    render(<AssignMailbox user={user} unlocked={false} takeConfirm={() => confirm} onClose={() => undefined} />);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.test")).toBeTruthy());
    expect((screen.getByRole("button", { name: /save mailbox/i }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("does not wipe unsaved CardDAV edits when saving the mailbox form", async () => {
    render(view);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.test")).toBeTruthy());
    await userEvent.type(screen.getByLabelText(/carddav server url/i), "https://c.example.test/dav/");
    await userEvent.click(screen.getByRole("button", { name: /save mailbox/i }));
    await waitFor(() => expect(api.putUserIMAPConfig).toHaveBeenCalled());
    expect(screen.getByDisplayValue("https://c.example.test/dav/")).toBeTruthy();
  });

  it("saves a CardDAV client for the user", async () => {
    render(view);
    await waitFor(() => expect(screen.getByDisplayValue("imap.example.test")).toBeTruthy());
    await userEvent.type(screen.getByLabelText(/carddav server url/i), "https://c.example.test/dav/");
    await userEvent.type(screen.getByLabelText(/carddav username/i), "gwen");
    await userEvent.type(screen.getByLabelText(/carddav password/i), "pw");
    await userEvent.click(screen.getByRole("button", { name: /save contacts sync/i }));
    await waitFor(() =>
      expect(api.putUserCardDAVClient).toHaveBeenCalledWith(
        confirm,
        "u1",
        expect.objectContaining({ serverUrl: "https://c.example.test/dav/", username: "gwen", password: "pw", managed: false })
      )
    );
  });
});
