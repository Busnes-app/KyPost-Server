import type { InboxFolder } from "./types";

/** The folders the phone layout's quick nav stands in for. */
export type StandardMailboxKind = "inbox" | "drafts" | "junk" | "sent" | "trash";

// Leaf names per kind, used when the server publishes no special-use role.
// Mirrors folderNameAliases in backend/internal/adapters/imap/special_folders.go.
const NAME_ALIASES: Record<StandardMailboxKind, string[]> = {
  inbox: ["inbox"],
  drafts: ["drafts", "draft"],
  junk: ["junk", "spam", "junk e-mail", "junk email", "bulk mail"],
  sent: ["sent", "sent items", "sent messages", "sent mail"],
  trash: ["trash", "deleted items", "deleted messages", "bin"]
};

/** The last segment of a folder path, without an INBOX/ prefix. */
export function mailboxLabel(path: string): string {
  const clean = path.trim();
  if (!clean) return "";
  const parts = clean.replace(/^INBOX[/.]/i, "").split(/[/.]/).filter(Boolean);
  return parts[parts.length - 1] ?? clean;
}

/** A folder's standard kind, from its RFC 6154 role first and its name second. */
export function standardMailboxKind(folder: InboxFolder): StandardMailboxKind | "custom" {
  if (folder.special) {
    return folder.special in NAME_ALIASES ? (folder.special as StandardMailboxKind) : "custom";
  }
  const leaf = mailboxLabel(folder.path).trim().toLowerCase();
  const kind = (Object.keys(NAME_ALIASES) as StandardMailboxKind[]).find((k) => NAME_ALIASES[k].includes(leaf));
  return kind ?? "custom";
}

/** The account's folder for kind (a published role wins), or fallback when it has none. */
export function standardMailboxPath(folders: InboxFolder[], kind: StandardMailboxKind, fallback: string): string {
  const folder = folders.find((f) => f.special === kind) ?? folders.find((f) => standardMailboxKind(f) === kind);
  return folder?.path ?? fallback;
}
