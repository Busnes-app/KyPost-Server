import { getJSON, mailboxHeaders, postJSON } from "./client";

/** One scheduled message as GET /api/scheduled lists it. */
export type ScheduledMessage = { id: string; status: string; reason: string; sendAt: string; subject: string; to: string[] };

function readScheduled(raw: unknown): ScheduledMessage {
  const r = (raw ?? {}) as Record<string, unknown>;
  const str = (v: unknown) => (typeof v === "string" ? v : "");
  if (typeof r.id !== "string" || typeof r.status !== "string" || typeof r.sendAt !== "string") {
    throw new Error("The scheduled list could not be read. Reload and try again.");
  }
  return {
    id: r.id,
    status: r.status,
    reason: str(r.reason),
    sendAt: r.sendAt,
    subject: str(r.subject),
    to: Array.isArray(r.to) ? r.to.filter((v): v is string => typeof v === "string") : []
  };
}

export async function listScheduled(mailbox: string): Promise<ScheduledMessage[]> {
  const data = await getJSON<{ scheduled?: unknown }>("/api/scheduled", mailboxHeaders(mailbox));
  if (!Array.isArray(data.scheduled)) throw new Error("The scheduled list could not be read. Reload and try again.");
  return data.scheduled.map(readScheduled);
}

/** Returns a waiting message to Drafts; the server answers 409 once it is sending. */
export function cancelScheduled(mailbox: string, id: string): Promise<{ ok: boolean; warning?: string }> {
  return postJSON(`/api/scheduled/${encodeURIComponent(id)}/cancel`, {}, mailboxHeaders(mailbox));
}
