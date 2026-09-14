/**
 * Parses the MIME entity that comes out of a PGP/MIME decryption, in the
 * browser, the same way the server's pgpmail.ParseContent does it.
 *
 * A PGP/MIME payload decrypts to a complete MIME entity — headers, boundaries,
 * encoded parts — not to display text. For a client-protected account the server
 * never sees any of it, so it can neither report `bodyMode` nor extract the
 * display part, and everything downstream worked from a string that still had
 * "Content-Type: text/html" and a boundary marker in it.
 *
 * Sniffing that string was wrong in both directions: "<user@example.com>" read
 * as a tag and deleted the address, and a tag allowlist read "use <br> to break
 * a line" as markup and deleted that. The Content-Type is in the bytes already.
 *
 * Mirrors the server implementation (backend/internal/pgpmail/mime.go) rather
 * than inventing a second set of rules: same first-part-wins selection, same
 * text/rfc822-headers skip, same depth cap. That parser renders nothing now
 * that server-side decryption is retired, but it is still the reference this
 * one is checked against: change one, change both, and add the case to
 * testdata/mime-corpus.json, which both test suites execute.
 */

/** Which MIME part the body was taken from. Matches the server's wire values. */
export type BodyMode = "html" | "plain";

/** One non-body part of a decrypted entity, decoded to its bytes. */
export type MimeAttachment = {
  name: string;
  mimeType: string;
  bytes: Uint8Array;
  /** Content-ID without its angle brackets, for `cid:` references in the body. */
  contentId?: string;
};

/**
 * Headers carried inside the encrypted entity (protected-headers v1). Present
 * only when the entity's own header block names them; an entity built by
 * another client may protect Subject alone, or nothing.
 */
export type ProtectedHeaders = {
  subject?: string;
  to?: string;
  cc?: string;
  bcc?: string;
};

export type MimeContent = {
  body: string;
  mode: BodyMode;
  attachments: MimeAttachment[];
  /** Parts dropped by MimeLimits rather than decoded. The reader says so. */
  attachmentsOmitted: number;
  protectedHeaders: ProtectedHeaders;
};

/**
 * Depth cap on nested multiparts, matching maxContentDepth server-side. A
 * message can nest these arbitrarily; a decrypted payload is attacker-supplied
 * by definition, so the walk must terminate on its own.
 */
const MAX_DEPTH = 8;

export type MimeLimits = {
  /** Attachments kept per message; the rest are counted, not decoded. */
  maxAttachments: number;
  /** Decoded attachment bytes kept per message, estimated before decoding. */
  maxAttachmentBytes: number;
};

/**
 * Defaults mirror mailmsg.MaxInboundMessageBytes (25 MiB): a decrypted entity
 * cannot legitimately carry more than the message that delivered it. The count
 * cap has no server twin because the server never opens these; it exists so a
 * hostile message cannot cost one allocation per part. Both are applied to
 * attachments only, never to body selection, so the shared corpus is unaffected.
 */
export const DEFAULT_MIME_LIMITS: MimeLimits = {
  maxAttachments: 200,
  maxAttachmentBytes: 25 * 1024 * 1024
};

/** Splits a raw MIME entity into its header block and its body. */
function splitHeaders(raw: string): { headers: Map<string, string>; body: string } {
  const headers = new Map<string, string>();
  // Accept both CRLF and bare LF: real mail uses CRLF, but a decrypted payload
  // has already been through a library that may have normalized it.
  const match = raw.match(/\r?\n\r?\n/);
  const headerBlock = match ? raw.slice(0, match.index) : raw;
  const body = match ? raw.slice((match.index ?? 0) + match[0].length) : "";

  // Unfold continuation lines (a header value may wrap onto following lines
  // that begin with whitespace) before splitting on ":".
  const unfolded = headerBlock.replace(/\r?\n[ \t]+/g, " ");
  for (const line of unfolded.split(/\r?\n/)) {
    const colon = line.indexOf(":");
    if (colon <= 0) continue;
    const name = line.slice(0, colon).trim().toLowerCase();
    // FIRST occurrence wins, matching textproto.MIMEHeader.Get on the Go side.
    // A Map.set here let the LAST duplicate win, so a part carrying two
    // Content-Type headers classified differently in the two parsers — one
    // signature, two readings.
    if (headers.has(name)) continue;
    headers.set(name, line.slice(colon + 1).trim());
  }
  return { headers, body };
}

/**
 * Pulls a parameter (boundary, filename, charset) out of a Content-Type value.
 *
 * RFC 2231 forms are honoured because Go's mime.ParseMediaType decodes them: a
 * part named only by `name*=utf-8''a.txt`, or by the `name*0`/`name*1`
 * continuation pair, reads as an attachment server-side. Matching only the
 * literal `name=` made those parts unnamed browser-side and therefore body
 * candidates — the two parsers then picked different bodies for one signed
 * message.
 */
function contentTypeParam(value: string, name: string): string {
  // RFC 2231 extended value: name*=charset'language'percent-encoded. Checked
  // before the plain form because a sender that emits both (this client does,
  // so old readers still see a name) means the extended one, and Go's
  // mime.ParseMediaType resolves the pair the same way.
  const extended = new RegExp(`;\\s*${name}\\*\\s*=\\s*"?([^;"]*)"?`, "i").exec(value);
  if (extended) return decodeRFC2231(extended[1]);

  const quoted = new RegExp(`;\\s*${name}\\s*=\\s*"([^"]*)"`, "i").exec(value);
  if (quoted) return quoted[1];
  const bare = new RegExp(`;\\s*${name}\\s*=\\s*([^;\\s]+)`, "i").exec(value);
  if (bare) return bare[1];

  // RFC 2231 continuations: name*0=…; name*1=… (each optionally *-encoded).
  let joined = "";
  for (let i = 0; ; i += 1) {
    const seg = new RegExp(`;\\s*${name}\\*${i}(\\*)?\\s*=\\s*(?:"([^"]*)"|([^;\\s]+))`, "i").exec(value);
    if (!seg) break;
    const raw = seg[2] ?? seg[3] ?? "";
    joined += seg[1] ? decodeRFC2231(raw) : raw;
  }
  return joined;
}

/** Strips an RFC 2231 charset'language' prefix and percent-decodes the rest. */
function decodeRFC2231(raw: string): string {
  const parts = raw.split("'");
  const encoded = parts.length >= 3 ? parts.slice(2).join("'") : raw;
  try {
    return decodeURIComponent(encoded);
  } catch {
    // A malformed escape must not throw the whole parse away; the value is
    // only ever used to decide whether a part is named.
    return encoded;
  }
}

/** The media type with parameters stripped, lowercased. */
function mediaType(value: string): string {
  return value.split(";")[0].trim().toLowerCase();
}

/**
 * The render mode a Content-Type asks for. Anything that is not explicitly
 * text/html renders as plain — the conservative direction, because showing
 * markup as text is ugly while showing text as markup deletes it.
 */
function modeFor(contentType: string): BodyMode {
  return mediaType(contentType) === "text/html" ? "html" : "plain";
}

/**
 * Reverses Content-Transfer-Encoding to the part's bytes. Returns null when
 * the encoding is malformed, so the caller can keep the raw text instead.
 *
 * The default branch re-encodes the text as UTF-8: a decrypted entity arrives
 * as text, so an unencoded part has already been through a UTF-8 decode and
 * binary bytes cannot be recovered exactly. Every client that sends binary
 * inside PGP/MIME uses base64, as this one does.
 */
function decodePartBytes(body: string, encoding: string): Uint8Array | null {
  switch (encoding.trim().toLowerCase()) {
    case "base64":
      try {
        return Uint8Array.from(atob(body.replace(/\s+/g, "")), (c) => c.charCodeAt(0));
      } catch {
        return null;
      }
    case "quoted-printable": {
      // Whitespace at the end of an encoded line is transport padding (RFC 2045
      // 6.7 rule 3) and is dropped before the soft line breaks are joined —
      // mime/quotedprintable does the same, and keeping it here made the two
      // decoders return different bytes for one signed part.
      const qp = body.replace(/[ \t]+(?=\r?\n|$)/g, "").replace(/=\r?\n/g, "");
      const bytes: number[] = [];
      for (let i = 0; i < qp.length; ) {
        if (
          qp[i] === "=" &&
          i + 2 < qp.length &&
          /^[0-9A-Fa-f]{2}$/.test(qp.slice(i + 1, i + 3))
        ) {
          bytes.push(parseInt(qp.slice(i + 1, i + 3), 16));
          i += 3;
        } else {
          bytes.push(qp.charCodeAt(i) & 0xff);
          i += 1;
        }
      }
      return Uint8Array.from(bytes);
    }
    default:
      return new TextEncoder().encode(body);
  }
}

/**
 * Decoded size of a part before decoding it, so a hostile message is refused
 * before the allocation it was trying to cause rather than after.
 */
function estimateDecodedBytes(body: string, encoding: string): number {
  return encoding.trim().toLowerCase() === "base64" ? Math.ceil((body.length * 3) / 4) : body.length;
}

/** Reverses Content-Transfer-Encoding so the body is readable text. */
function decodePart(body: string, encoding: string, charset?: string): string {
  const enc = encoding.trim().toLowerCase();
  if (enc !== "base64" && enc !== "quoted-printable") {
    return body;
  }
  // Malformed base64 from a hostile or truncated message: show the raw text
  // rather than throwing away the part.
  const bytes = decodePartBytes(body, enc);
  if (!bytes) return body;
  // ponytail: respect charset when available — base64/quoted-printable bytes are
  // in that charset, and ignoring it produces mojibake (e.g. =C3=A9 → Â). Fall
  // back to utf-8; if that charset is unsupported, try utf-8 before giving up.
  const label = (charset || "utf-8").trim().toLowerCase() || "utf-8";
  try {
    return new TextDecoder(label).decode(bytes);
  } catch {
    try {
      return new TextDecoder("utf-8").decode(bytes);
    } catch {
      return body;
    }
  }
}

function charsetFromContentType(value: string): string | undefined {
  return contentTypeParam(value, "charset") || undefined;
}

/**
 * Splits a multipart body on its boundary, discarding preamble and epilogue.
 *
 * RFC 2046 5.1.1 defines the delimiter as `CRLF--boundary`, so it is only
 * significant at the START OF A LINE, and Go's multipart.Reader enforces that.
 * Splitting on the bare token instead also matched a copy of it inside a part's
 * own text — and since the sender chooses the boundary, they could embed it
 * mid-sentence to truncate what a client-protected reader saw while a
 * server-protected reader saw the message whole, both under one signature.
 *
 * The opening delimiter legitimately begins the body with no preceding newline
 * (Go special-cases this at total==0), so normalise that case rather than
 * loosening the match. Both CRLF and bare LF are accepted because Go's reader
 * accepts whichever it sees first. See testdata/mime-corpus.json, which both
 * this suite and backend/internal/pgpmail execute.
 */
function splitParts(body: string, boundary: string): string[] {
  const opening = `--${boundary}`;
  const escaped = opening.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const anchored = body.startsWith(opening) ? `\n${body}` : body;
  // The delimiter line must END after the boundary too: RFC 2046 5.1.1 allows
  // only transport padding (LWSP) before the line break, or "--" for the close
  // delimiter. Anchoring only the start matched "--boundary_X", which Go's
  // reader treats as ordinary content — so a sender could split the browser's
  // view of a signed message where the server saw none.
  const segments = anchored.split(new RegExp(`\\r?\\n${escaped}[ \\t]*(?=\\r?\\n|--)`));
  // segments[0] is the preamble. A segment beginning "--" is the close
  // delimiter, and everything after it is the epilogue — so STOP there rather
  // than skipping it and continuing. Merely filtering it out meant a
  // lookalike close delimiter ("--B--junk") hid the parts before it from Go,
  // which errors on the malformed line, while the browser carried on and
  // rendered a later part. Truncating makes both yield no body.
  const parts: string[] = [];
  for (const segment of segments.slice(1)) {
    if (segment.startsWith("--")) break;
    parts.push(segment);
  }
  return parts;
}

/** Accumulates one walk: the first body wins, every other part is an attachment. */
type Collector = {
  body: { body: string; mode: BodyMode } | null;
  attachments: MimeAttachment[];
  attachmentsOmitted: number;
  attachmentBytes: number;
  limits: MimeLimits;
};

/**
 * Walks a multipart body. The first part that qualifies as the display body
 * becomes `out.body`; other recognised parts become attachments, exactly as
 * pgpmail.parseMultipart files them. The walk continues past the body so
 * attachments after it are found.
 */
function walkMultipart(body: string, boundary: string, depth: number, out: Collector): void {
  if (depth >= MAX_DEPTH) return;

  for (const segment of splitParts(body, boundary)) {
    // A part begins with CRLF after the delimiter; splitHeaders tolerates it.
    const { headers, body: partBody } = splitHeaders(segment.replace(/^\r?\n/, ""));
    const contentType = headers.get("content-type") ?? "";
    const type = mediaType(contentType);

    if (type.startsWith("multipart/")) {
      const nestedBoundary = contentTypeParam(contentType, "boundary");
      if (nestedBoundary) {
        walkMultipart(partBody, nestedBoundary, depth + 1, out);
      }
      continue;
    }

    // The protected-headers legacy-display part. It repeats Subject/From for
    // clients that cannot read protected headers; rendering it as the body
    // shows the user a header dump. Skipped server-side for the same reason.
    if (type === "text/rfc822-headers") continue;

    const encoding = headers.get("content-transfer-encoding") ?? "";
    const disposition = headers.get("content-disposition") ?? "";
    const filename = contentTypeParam(contentType, "name") || contentTypeParam(disposition, "filename");

    // An unnamed text part is a body candidate: the first wins, the rest are
    // dropped rather than misfiled as attachments. A named one is a genuine
    // text attachment (note.txt) and falls through.
    if (!filename && (type === "text/plain" || type === "text/html" || type === "")) {
      if (!out.body) {
        out.body = {
          body: decodePart(partBody, encoding, charsetFromContentType(contentType)),
          mode: modeFor(contentType)
        };
      }
      continue;
    }

    // Refused before decoding: the estimate is what the allocation would be.
    const estimated = estimateDecodedBytes(partBody, encoding);
    if (
      out.attachments.length >= out.limits.maxAttachments ||
      out.attachmentBytes + estimated > out.limits.maxAttachmentBytes
    ) {
      out.attachmentsOmitted += 1;
      continue;
    }
    const bytes = decodePartBytes(partBody, encoding);
    if (!bytes) {
      out.attachmentsOmitted += 1;
      continue;
    }
    out.attachmentBytes += bytes.length;
    const contentId = (headers.get("content-id") ?? "").trim().replace(/^<|>$/g, "");
    out.attachments.push({
      name: filename || "attachment",
      mimeType: type || "application/octet-stream",
      bytes,
      ...(contentId ? { contentId } : {})
    });
  }
}

/**
 * Extracts the display body and its render mode from a decrypted MIME entity.
 *
 * Returns null when the input is not MIME at all — an inline-PGP message
 * decrypts to bare text with no headers, and that must be shown as-is rather
 * than mangled by a parser looking for structure that was never there.
 */
export function parseMimeContent(raw: string, limits: MimeLimits = DEFAULT_MIME_LIMITS): MimeContent | null {
  const { headers, body } = splitHeaders(raw);
  const contentType = headers.get("content-type") ?? "";
  if (!contentType) {
    // No Content-Type means no MIME entity. Do not guess a mode here; the
    // caller falls back to its own last-resort check for this case alone.
    return null;
  }
  const protectedHeaders = readProtectedHeaders(headers);

  const type = mediaType(contentType);
  if (type.startsWith("multipart/")) {
    const boundary = contentTypeParam(contentType, "boundary");
    if (!boundary) {
      return { body, mode: "plain", attachments: [], attachmentsOmitted: 0, protectedHeaders };
    }
    const out: Collector = { body: null, attachments: [], attachmentsOmitted: 0, attachmentBytes: 0, limits };
    walkMultipart(body, boundary, 0, out);
    // A multipart with no usable display part still renders as something rather
    // than as nothing: better an empty body than the raw boundaries.
    return {
      body: out.body?.body ?? "",
      mode: out.body?.mode ?? "plain",
      attachments: out.attachments,
      attachmentsOmitted: out.attachmentsOmitted,
      protectedHeaders
    };
  }

  return {
    body: decodePart(
      body,
      headers.get("content-transfer-encoding") ?? "",
      charsetFromContentType(contentType)
    ),
    mode: modeFor(contentType),
    attachments: [],
    attachmentsOmitted: 0,
    protectedHeaders
  };
}

/**
 * The protected headers an entity carries, decoded from RFC 2047 where a
 * sender encoded them. Mirrors pgpmail.ExtractProtectedSubject for Subject
 * and extends it to the address headers a draft needs back.
 */
function readProtectedHeaders(headers: Map<string, string>): ProtectedHeaders {
  const out: ProtectedHeaders = {};
  for (const name of ["subject", "to", "cc", "bcc"] as const) {
    const value = headers.get(name);
    if (value !== undefined) out[name] = decodeRFC2047(value);
  }
  return out;
}

/**
 * Decodes RFC 2047 encoded-words. Only B and Q encodings exist; anything
 * else, or a malformed word, is left as written rather than dropped.
 */
export function decodeRFC2047(value: string): string {
  return value.replace(/=\?([^?]+)\?([bBqQ])\?([^?]*)\?=/g, (word, charset: string, enc: string, text: string) => {
    try {
      const bytes =
        enc.toUpperCase() === "B"
          ? Uint8Array.from(atob(text), (c) => c.charCodeAt(0))
          : (decodePartBytes(text.replace(/_/g, " "), "quoted-printable") ?? new Uint8Array());
      return new TextDecoder(charset.toLowerCase()).decode(bytes);
    } catch {
      return word;
    }
  });
}
