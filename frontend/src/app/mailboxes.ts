import { createContext, useContext } from "react";

// One of the caller's mailboxes from GET /api/mailboxes. Addresses may be
// empty when none is active; the label then falls back to the ID.
export type UserMailbox = { id: string; kind: "primary" | "extra"; addresses: string[] };

export type MailboxState = { mailboxes: UserMailbox[]; selected: string };
export const MailboxContext = createContext<MailboxState>({ mailboxes: [], selected: "" });
export const useMailboxes = () => useContext(MailboxContext);
export const mailboxName = (m: UserMailbox) => m.addresses[0] ?? m.id;

const extraID = /^mbx-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
// Lowercase bare dot-atom, the only form the ledger stores (as api/nativeMail.ts).
const atom = "[a-z0-9!#$%&'*+/=?^_`{|}~-]+";
const addressPattern = new RegExp(`^${atom}(\\.${atom})*@[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`);

function invalid(): never {
  throw new Error("Invalid mailbox list.");
}

function record(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) invalid();
  return Object.fromEntries(Object.entries(value));
}

/** Validates GET /api/mailboxes: the user's primary first, then extras; addresses primary first. */
export function readMailboxes(value: unknown, userId: string): UserMailbox[] {
  const { mailboxes } = record(value);
  if (!Array.isArray(mailboxes) || mailboxes.length > 1000) invalid();
  const ids = new Set<string>(), seen = new Set<string>();
  return mailboxes.map((item: unknown, index) => {
    const entry = record(item);
    const primary = index === 0, { id } = entry;
    if (typeof id !== "string" || entry.kind !== (primary ? "primary" : "extra") || (primary ? id !== userId : !extraID.test(id)) || ids.has(id)) invalid();
    ids.add(id);
    if (!Array.isArray(entry.addresses) || entry.addresses.length > 1000) invalid();
    const addresses = entry.addresses.map((a: unknown) => {
      const { address, kind } = record(a);
      if (typeof address !== "string" || address.length > 254 || !addressPattern.test(address) ||
          (kind !== "primary" && kind !== "alias") || seen.has(address)) invalid();
      seen.add(address);
      return { address, kind };
    });
    if (addresses.filter((a) => a.kind === "primary").length > 1) invalid();
    // The server lists active addresses in ledger order; the primary leads here.
    addresses.sort((a, b) => Number(b.kind === "primary") - Number(a.kind === "primary"));
    return { id, kind: primary ? "primary" : "extra", addresses: addresses.map((a) => a.address) };
  });
}

// The selection lasts for the tab, per user: sessionStorage, like the compose autosave.
const storageKey = (userId: string) => `kypost-mailbox:${userId}`;

export function loadMailboxSelection(userId: string): string {
  const id = window.sessionStorage.getItem(storageKey(userId)) ?? "";
  return extraID.test(id) ? id : "";
}

export function saveMailboxSelection(userId: string, id: string) {
  if (id) {
    window.sessionStorage.setItem(storageKey(userId), id);
  } else {
    window.sessionStorage.removeItem(storageKey(userId));
  }
}
