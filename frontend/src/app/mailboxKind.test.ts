import { describe, expect, it } from "vitest";
import { standardMailboxKind, standardMailboxPath } from "./mailboxKind";

const folder = (path: string, special?: string) => ({ path, deletable: false, special });

// The phone layout hides folder rows of a standard kind behind its quick nav.
// A kind the nav misses shows the folder twice ("Sent" and "Sent Items").
describe("standardMailboxKind", () => {
  it.each([
    ["Sent Items", undefined, "sent"],
    ["INBOX/Sent Messages", undefined, "sent"],
    ["Deleted Items", undefined, "trash"],
    ["Junk E-mail", undefined, "junk"],
    ["Draft", undefined, "drafts"],
    ["INBOX", undefined, "inbox"],
    ["Gesendet", "sent", "sent"],
    ["Archive", "archive", "custom"],
    ["Projects", undefined, "custom"]
  ])("%s (role %s) is %s", (path, special, kind) => {
    expect(standardMailboxKind(folder(path, special))).toBe(kind);
  });
});

describe("standardMailboxPath", () => {
  it("points the quick nav at the account's own folder, preferring a published role", () => {
    const folders = [folder("Sent"), folder("Sent Items", "sent"), folder("Projects")];
    expect(standardMailboxPath(folders, "sent", "Sent")).toBe("Sent Items");
    expect(standardMailboxPath([folder("Sent Items")], "sent", "Sent")).toBe("Sent Items");
    expect(standardMailboxPath([folder("Projects")], "trash", "Trash")).toBe("Trash");
  });
});
