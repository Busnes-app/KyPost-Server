// Chat from mail (docs/KYMESSAGES.md): which addresses are people in the
// organisation, and the KyMessages (Element) link to chat with one of them.
// KyPost's server resolves the person and checks the link; this file checks
// the link again before opening it.

import type { SuiteApp } from "../components/AppRail";
import { HttpError, postJSON } from "./client";

const KNOWN_FOR_MS = 5 * 60_000;
const BATCH = 100;

/** The https origin of Element's suite entry; "" hides Chat. */
export function chatOrigin(apps: SuiteApp[]): string {
  const url = apps.find((a) => a.id === "kymessage-chat")?.url ?? "";
  try {
    const u = new URL(url);
    return u.protocol === "https:" && !u.username && !u.password ? u.origin : "";
  } catch {
    return "";
  }
}

const known = new Map<string, { inside: boolean; until: number }>();
let queued: { address: string; done: (inside: boolean) => void }[] = [];

async function flush() {
  const batch = queued;
  queued = [];
  const addresses = [...new Set(batch.map((q) => q.address))];
  const inside = new Set<string>();
  let ok = true;
  for (let i = 0; i < addresses.length; i += BATCH) {
    try {
      const r = await postJSON<{ inside?: unknown }>("/api/kymessage/people", { addresses: addresses.slice(i, i + BATCH) });
      if (Array.isArray(r.inside)) for (const a of r.inside) if (typeof a === "string") inside.add(a.toLowerCase());
    } catch {
      ok = false; // a failure is "no Chat" and is not remembered
    }
  }
  const until = Date.now() + KNOWN_FOR_MS;
  for (const a of addresses) if (ok || inside.has(a)) known.set(a, { inside: inside.has(a), until });
  for (const q of batch) q.done(inside.has(q.address));
}

/** Whether address is a person in the organisation; lookups in one tick share a request. */
export function isChatPerson(address: string): Promise<boolean> {
  const a = address.trim().toLowerCase();
  const hit = known.get(a);
  if (hit && hit.until > Date.now()) return Promise.resolve(hit.inside);
  if (!a.includes("@")) return Promise.resolve(false);
  return new Promise((done) => {
    if (queued.length === 0) setTimeout(() => void flush(), 0);
    queued.push({ address: a, done });
  });
}

/** Test hook: forget every remembered answer. */
export function forgetChatPeople() {
  known.clear();
}

const refusals: Record<string, string> = {
  no_target: "This person isn't on KyMessages yet.",
  no_user: "You have no KyMessages account yet.",
  rate_limited: "Too many chat requests. Try again shortly.",
  kymessage_off: "Chat is not available for your account."
};

/**
 * Opens a chat with address in a new tab. The tab opens inside the click, so
 * no popup blocker stops it, and is pointed at the answer once it comes.
 * Returns "" on success, else the message to show.
 */
export async function openChat(address: string, origin: string): Promise<string> {
  const tab = window.open("", "_blank");
  if (tab) tab.opener = null;
  try {
    const { url } = await postJSON<{ url?: unknown }>("/api/kymessage/chat", { address });
    if (typeof url !== "string" || !onOrigin(url, origin)) throw new Error("bad answer");
    if (tab) tab.location.replace(url);
    else window.open(url, "_blank", "noopener,noreferrer");
    return "";
  } catch (e) {
    tab?.close();
    const code = e instanceof HttpError ? (e.body as { code?: unknown } | undefined)?.code : undefined;
    return (typeof code === "string" && refusals[code]) || "Could not open KyMessages. Try again.";
  }
}

function onOrigin(raw: string, origin: string): boolean {
  try {
    const u = new URL(raw);
    return u.protocol === "https:" && u.origin === origin && !u.username && !u.password;
  } catch {
    return false;
  }
}
