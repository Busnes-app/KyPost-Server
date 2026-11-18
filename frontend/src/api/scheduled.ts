import { getJSON, mailboxHeaders, postJSON, putJSON } from "./client";

/** One scheduled message as GET /api/scheduled lists it; delayed marks a Send delay hold. */
export type ScheduledMessage = { id: string; status: string; reason: string; sendAt: string; subject: string; to: string[]; delayed: boolean };

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
    to: Array.isArray(r.to) ? r.to.filter((v): v is string => typeof v === "string") : [],
    delayed: r.delayed === true
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

/** The Send delay choices in seconds, as the server accepts them; 0 is Off. */
export const SEND_DELAY_CHOICES = [0, 10, 20, 30, 60, 90] as const;

function readSendDelay(data: { seconds?: unknown }): number {
  if (typeof data.seconds !== "number" || !(SEND_DELAY_CHOICES as readonly number[]).includes(data.seconds)) {
    throw new Error("The Send delay setting could not be read. Reload and try again.");
  }
  return data.seconds;
}

/** The account's Send delay in seconds (GET|PUT /api/mail/send-delay). */
export async function getSendDelay(): Promise<number> {
  return readSendDelay(await getJSON<{ seconds?: unknown }>("/api/mail/send-delay"));
}

export async function setSendDelay(seconds: number): Promise<number> {
  return readSendDelay(await putJSON<{ seconds?: unknown }>("/api/mail/send-delay", { seconds }));
}
