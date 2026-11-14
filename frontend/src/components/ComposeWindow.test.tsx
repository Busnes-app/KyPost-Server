import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ComposeWindow } from "./ComposeWindow";
import type { Compose } from "../hooks/useCompose";

afterEach(cleanup);

// Only the fields the window reads matter; anything else is a no-op.
const fake = (fields: Record<string, unknown>) => new Proxy(fields, {
  get: (target, key: string) => (key in target ? target[key] : key.endsWith("Ref") ? { current: null } : () => {})
}) as unknown as Compose;

describe("ComposeWindow New meeting", () => {
  const compose = fake({
    composeOpen: true, composeSending: false, composeSavingDraft: false, composeFromChoices: [], sendAsOptions: [],
    composeTo: { tokens: [{ email: "bob@example.net", isCustom: true }], draft: "" },
    composeCc: { tokens: [{ email: "carol@example.org", isCustom: true }], draft: "" },
    composeBcc: { tokens: [{ email: "hidden@example.org", isCustom: true }], draft: "" },
    composeSubject: "Budget", composeAttachments: [], composeRecipientTiers: {},
    drive: { driveStatus: { available: false }, driveOffer: [], driveUploads: [], driveOutside: null }
  });

  it("is absent without KyCalendar and opens the editor with subject, To and Cc (never Bcc)", async () => {
    HTMLDialogElement.prototype.showModal ??= function (this: HTMLDialogElement) { this.open = true; };
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const { unmount } = render(<ComposeWindow compose={compose} mailboxes={[]} />);
    expect(screen.queryByRole("button", { name: "New meeting", hidden: true })).toBeNull();
    unmount();
    render(<ComposeWindow compose={compose} mailboxes={[]} calendarOrigin="https://cal.example" />);
    await userEvent.click(screen.getByRole("button", { name: "New meeting", hidden: true }));
    expect(open).toHaveBeenCalledWith("https://cal.example/new?title=Budget&attendees=bob%40example.net%2Ccarol%40example.org", "_blank", "noopener,noreferrer");
    open.mockRestore();
  });
});
