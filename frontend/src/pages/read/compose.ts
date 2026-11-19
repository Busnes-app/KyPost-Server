// Building reply/forward bodies and reply-all recipient lists from a message.
//
// Quoting routes through processEmailHtml, NOT sanitizeEmailHtml: it must use
// the same pipeline the read view does (link blocking, img -> "[Image
// Blocked]", remote-content-blocking sanitize). sanitizeEmailHtml alone does
// not strip <img>, so quoting a message whose images the user chose not to
// unblock would fire its tracking pixels the moment they pressed Reply.

import { processEmailHtml } from "../../lib/emailHtml";
import { displayBody } from "./body";
import { firstAddressFromText, listAddressesFromText, plusBase } from "../../lib/addressText";
import type { DecryptedView, InboxEmail } from "./types";
import { formatTimestamp } from "./format";

export function ensureSubjectPrefix(subject: string | undefined, prefix: "Re:" | "Fwd:"): string {
  const base = (subject ?? "").trim();
  if (base === "") {
    return prefix;
  }
  const lowerPrefix = prefix.toLowerCase();
  if (base.toLowerCase().startsWith(lowerPrefix)) {
    return base;
  }
  return `${prefix} ${base}`;
}

export function escapeHtml(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

// decrypted is the locally-decrypted view when there is one. Without it, quoting
// a client-protected account's mail quotes the envelope — an armored blob or
// nothing at all — and pairs the plaintext with the envelope's render mode. See
// read/body.ts.
export function buildReplyBody(email: InboxEmail, decrypted?: DecryptedView): string {
  const time = formatTimestamp(email.atUtc);
  const sender = email.sender || "-";
  const subject = email.subject || "(no subject)";
  const quoted = displayBody(email, decrypted);
  const body = quoted.body;
  const isHtml = quoted.mode === "html" && Boolean(body);
  // processEmailHtml, not sanitizeEmailHtml: quoting must go through the same
  // pipeline the read view uses (link blocking + img -> "[Image Blocked]" +
  // remote-content-blocking sanitize). sanitizeEmailHtml alone does not strip
  // <img>, so quoting a message the user chose not to unblock used to fire its
  // tracking pixels the moment they pressed Reply.
  const rendered = isHtml ? processEmailHtml(body, false) : `<pre style=\"white-space: pre-wrap; margin: 0;\">${escapeHtml(body)}</pre>`;
  return [
    "<p><br /></p>",
    `<p>On ${escapeHtml(time)}, ${escapeHtml(sender)} wrote:</p>`,
    "<blockquote style=\"margin: 0 0 0 0.8rem; padding-left: 0.8rem; border-left: 3px solid var(--line, #c2c7d0);\">",
    `<p><strong>Subject:</strong> ${escapeHtml(subject)}</p>`,
    rendered,
    "</blockquote>"
  ].join("");
}

export function buildForwardBody(email: InboxEmail, decrypted?: DecryptedView): string {
  const time = formatTimestamp(email.atUtc);
  const sender = email.sender || "-";
  const sentTo = email.sentTo || "-";
  const subject = email.subject || "(no subject)";
  const quoted = displayBody(email, decrypted);
  const body = quoted.body;
  const isHtml = quoted.mode === "html" && Boolean(body);
  // processEmailHtml, not sanitizeEmailHtml: quoting must go through the same
  // pipeline the read view uses (link blocking + img -> "[Image Blocked]" +
  // remote-content-blocking sanitize). sanitizeEmailHtml alone does not strip
  // <img>, so quoting a message the user chose not to unblock used to fire its
  // tracking pixels the moment they pressed Reply.
  const rendered = isHtml ? processEmailHtml(body, false) : `<pre style=\"white-space: pre-wrap; margin: 0;\">${escapeHtml(body)}</pre>`;
  return [
    "<p><br /></p>",
    "<p>---------- Forwarded message ----------</p>",
    `<p><strong>From:</strong> ${escapeHtml(sender)}</p>`,
    `<p><strong>Date:</strong> ${escapeHtml(time)}</p>`,
    `<p><strong>Subject:</strong> ${escapeHtml(subject)}</p>`,
    `<p><strong>To:</strong> ${escapeHtml(sentTo)}</p>`,
    rendered
  ].join("");
}

/**
 * The address a reply sends from, so a reply to a masked or plus address
 * never reveals the primary. Native mail carries `delivered`, the receipt's
 * envelope recipients (trusted, unlike headers, and set for Bcc): the To/Cc
 * address naming one keeps its tag, else the delivered own address; null when
 * none is the user's, so they must choose. Without a receipt (external IMAP,
 * Sent) the own To/Cc address is used, and "" keeps the account default.
 */
export function replyFromAddress(email: InboxEmail, own: string[], delivered?: string[]): string | null {
  const mine = new Set(own.map((a) => a.toLowerCase()));
  const isMine = (a: string) => mine.has(a) || mine.has(plusBase(a));
  const headers = [...listAddressesFromText(email.sentTo ?? ""), ...listAddressesFromText(email.cc ?? "")].map((a) => a.toLowerCase());
  if (delivered) {
    const got = new Set(delivered.map((a) => a.toLowerCase()));
    const tagged = headers.find((a) => (got.has(a) || got.has(plusBase(a))) && isMine(a));
    return tagged ?? [...got].find((a) => mine.has(a)) ?? null;
  }
  return headers.find(isMine) ?? "";
}

export function buildReplyAllRecipients(email: InboxEmail): { to: string; cc: string } {
  const sender = firstAddressFromText(email.sender || "");
  const senderKey = sender.toLowerCase();
  const recipients = [
    ...listAddressesFromText(email.sentTo || ""),
    ...listAddressesFromText(email.cc || "")
  ];
  const cc: string[] = [];
  const seen = new Set<string>();
  for (const recipient of recipients) {
    const key = recipient.toLowerCase();
    if (!recipient || key === senderKey || seen.has(key)) {
      continue;
    }
    seen.add(key);
    cc.push(recipient);
  }
  return { to: sender, cc: cc.join(", ") };
}

