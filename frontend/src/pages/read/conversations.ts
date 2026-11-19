import { senderName } from "./format";
import type { InboxEmail } from "./types";

// Conversation grouping comes from GET /api/inbox/conversations (docs/API.md), so
// every client groups the same way; this file only folds it into the list.

/** A reply of yours, listed where it is (Sent) and shown inline, never moved. */
export type SentMember = InboxEmail & { mailbox: string };

export type Conversation = { id: string; messageIds: string[]; sent?: SentMember[] };

/**
 * One list row. A lone message has members [lead]. A conversation row stands at
 * the place of its first member in the sorted view; members are every loaded
 * member in this folder, oldest first, and are what list actions act on.
 */
export type ListRow = { lead: InboxEmail; members: InboxEmail[]; conversation?: Conversation };

export function groupRows(rows: InboxEmail[], conversations: Conversation[], loaded: Map<string, InboxEmail>): ListRow[] {
  const byMessage = new Map<string, Conversation>();
  conversations.forEach((c) => c.messageIds.forEach((id) => byMessage.set(id, c)));
  const placed = new Set<string>();
  const out: ListRow[] = [];
  for (const lead of rows) {
    const conversation = byMessage.get(lead.messageId);
    if (!conversation) {
      out.push({ lead, members: [lead] });
    } else if (!placed.has(conversation.id)) {
      placed.add(conversation.id);
      const members = conversation.messageIds.flatMap((id) => loaded.get(id) ?? []);
      out.push({ lead, members, conversation });
    }
  }
  return out;
}

export const rowIds = (row: ListRow) => row.members.map((m) => m.messageId);

/** The row's message count: this folder's members plus your replies in Sent. */
export const rowCount = (row: ListRow) => row.members.length + (row.conversation?.sent?.length ?? 0);

/** Senders oldest first, your own replies as "me", at most three names. */
export function rowSenders(row: ListRow): string {
  const timeline = [...row.members, ...(row.conversation?.sent ?? [])].sort((a, b) => Date.parse(a.atUtc) - Date.parse(b.atUtc));
  const names = [...new Set(timeline.map((m) => ("mailbox" in m ? "me" : senderName(m.sender))))];
  return names.length > 3 ? `${names.slice(0, 3).join(", ")}…` : names.join(", ");
}

/** The newest message time in the conversation, replies included. */
export function rowNewest(row: ListRow): string {
  return [...row.members, ...(row.conversation?.sent ?? [])].reduce(
    (newest, m) => (Date.parse(m.atUtc) > Date.parse(newest) ? m.atUtc : newest),
    row.lead.atUtc
  );
}

/** Opening a conversation shows its newest unread message, else its newest. */
export function openTarget(row: ListRow): InboxEmail {
  const newestFirst = [...row.members].reverse();
  return newestFirst.find((m) => m.status !== "read") ?? newestFirst[0] ?? row.lead;
}

/** The conversation holding messageId, when it has more than that one message. */
export function conversationOf(messageId: string, conversations: Conversation[]): Conversation | undefined {
  return conversations.find((c) => c.messageIds.includes(messageId));
}
