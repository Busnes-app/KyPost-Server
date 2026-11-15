// The compose deep link (docs/SUITE-INTEGRATIONS.md "J"):
// /compose?to=<addresses, ≤100>&subject=<≤255 bytes>&body=<≤2000 bytes>.
// Every value is untrusted text: an invalid one is dropped silently, nothing
// is sent or saved. The link waits in sessionStorage (this tab only) across
// the sign-in, so it survives the password form and the single sign-on trip.

import { isPlausibleEmail } from "./recipients";

export type ComposeLink = { to: string[]; subject: string; body: string };

const KEY = "kypost.compose-link.v1";
/** A link not used within this long after it was opened is dropped. */
const TTL_MS = 10 * 60_000;
const bytes = (s: string) => new TextEncoder().encode(s).length;
const CONTROL = /[\u0000-\u0008\u000b-\u001f\u007f]/;

/** Reads a deep link's query; null when it carries nothing usable. */
export function parseComposeLink(search: string): ComposeLink | null {
  const q = new URLSearchParams(search);
  const to: string[] = [];
  const rawTo = q.get("to") ?? "";
  const parts = rawTo.split(",").map((a) => a.trim()).filter(Boolean);
  if (parts.length <= 100) {
    for (const a of parts) {
      if (a.length <= 254 && isPlausibleEmail(a) && !/[<>"'(),;:\\[\]]/.test(a) && !to.some((t) => t.toLowerCase() === a.toLowerCase())) to.push(a);
    }
  }
  let subject = q.get("subject") ?? "";
  if (bytes(subject) > 255 || CONTROL.test(subject) || subject.includes("\n") || subject.includes("\t")) subject = "";
  let body = (q.get("body") ?? "").replace(/\r\n?/g, "\n");
  if (bytes(body) > 2000 || CONTROL.test(body)) body = "";
  return to.length || subject || body ? { to, subject, body } : null;
}

const escapeHtml = (s: string) => s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);

/** Plain text as compose HTML: escaped paragraphs, http(s) URLs as links. */
export function bodyHtml(text: string): string {
  if (!text) return "";
  return text.split("\n").map((line) => {
    const html = line.split(/(https?:\/\/[^\s<>"']+)/g).map((part, i) => {
      if (i % 2 === 0) return escapeHtml(part);
      try {
        const u = new URL(part);
        if ((u.protocol === "https:" || u.protocol === "http:") && !u.username && !u.password) return `<a href="${escapeHtml(u.href)}">${escapeHtml(part)}</a>`;
      } catch { /* not a URL: plain text */ }
      return escapeHtml(part);
    }).join("");
    return `<p>${html || "<br>"}</p>`;
  }).join("");
}

/** Keeps a parsed link for after sign-in. */
export function holdComposeLink(link: ComposeLink | null) {
  try {
    if (link) window.sessionStorage.setItem(KEY, JSON.stringify({ ...link, at: Date.now() }));
  } catch { /* storage unavailable: the link is just not kept */ }
}

/** Takes the held link, once; a stale or malformed one reads as none. */
export function takeComposeLink(): ComposeLink | null {
  try {
    const raw = window.sessionStorage.getItem(KEY);
    window.sessionStorage.removeItem(KEY);
    const v = raw ? (JSON.parse(raw) as Record<string, unknown>) : null;
    if (!v || typeof v.at !== "number" || Date.now() - v.at > TTL_MS || Date.now() < v.at) return null;
    // Re-check through the same parser: storage is not a trust boundary we own alone.
    const q = new URLSearchParams();
    if (Array.isArray(v.to)) q.set("to", v.to.filter((a) => typeof a === "string").join(","));
    if (typeof v.subject === "string") q.set("subject", v.subject);
    if (typeof v.body === "string") q.set("body", v.body);
    return parseComposeLink(q.toString());
  } catch {
    return null;
  }
}
