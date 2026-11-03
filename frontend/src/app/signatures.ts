// Choosing a signature for a compose window and placing it in the editor body.
// Pure — no React — so the insertion and swap rules are testable on their own.
//
// A signature is plain text (api/signatures.ts). It becomes editor markup only
// here, by escaping, so nothing typed into a signature can turn into live HTML
// in the compose editor, which runs in the app document.

import type { SignatureSettings } from "../api/signatures";

export type ComposeKind = "new" | "reply" | "forward";

/** The signature text for a message from `from` ("" is the account address), or "" for none. */
export function signatureTextFor(settings: SignatureSettings | null, from: string, kind: ComposeKind): string {
  if (!settings) return "";
  if (kind === "reply" && !settings.onReply) return "";
  if (kind === "forward" && !settings.onForward) return "";
  const key = from.trim().toLowerCase();
  const byAddress = settings.byAddress ?? {};
  const id = key && Object.prototype.hasOwnProperty.call(byAddress, key) ? byAddress[key] : settings.defaultId;
  if (!id) return "";
  return (settings.signatures ?? []).find((s) => s.id === id)?.text ?? "";
}

// Only what an HTML text node needs, and exactly what the browser writes back
// out of one: the block must read the same after Quill re-serialises it, or a
// later swap could not find it.
function escapeText(value: string): string {
  return value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
}

const BLANK = "<p><br></p>";

/**
 * The editor block for a signature: a blank separator line, then one paragraph
 * per line, in the form Quill serialises. "" for an empty signature.
 */
export function signatureBlock(text: string): string {
  if (!text.trim()) return "";
  return BLANK + text.split("\n").map((line) => (line ? `<p>${escapeText(line)}</p>` : BLANK)).join("");
}

const LEADING_BLANK = /^<p><br\s*\/?><\/p>/;

/**
 * Places `block` below the line the user writes on: after a reply's or
 * forward's leading blank line, so it sits above the quoted message with a
 * blank line between them, or at the end of a new message.
 */
export function insertSignature(body: string, block: string): string {
  if (!block) return body;
  const lead = LEADING_BLANK.exec(body);
  if (lead) {
    const rest = body.slice(lead[0].length);
    return lead[0] + block + (rest ? BLANK : "") + rest;
  }
  if (!body.trim()) return BLANK + block;
  return body + block;
}

/**
 * Moves to another From address's signature: replaces the inserted block (or
 * removes it), or inserts one where there was none. A body the user has edited
 * the old block out of, or changed it in, is left alone.
 */
export function swapSignature(body: string, oldBlock: string, newBlock: string): string {
  if (oldBlock === newBlock) return body;
  if (!oldBlock) return insertSignature(body, newBlock);
  if (!body.includes(oldBlock)) return body;
  return body.replace(oldBlock, () => newBlock);
}
