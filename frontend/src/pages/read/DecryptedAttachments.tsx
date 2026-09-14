import { useEffect, useState } from "react";
import { formatBytes } from "../../app/compose";
import type { MimeAttachment } from "../../lib/mimeContent";

/**
 * Raster types safe to inline through <img>. SVG is excluded on purpose: it is
 * a document, and as a download it is opened by whatever the user chooses,
 * never rendered by this app.
 */
const INLINE_IMAGE_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp"]);

/** cid → data: URL for the images a decrypted body may reference inline. */
export function inlineImageMap(attachments: MimeAttachment[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const a of attachments) {
    if (!a.contentId || !INLINE_IMAGE_TYPES.has(a.mimeType)) continue;
    let binary = "";
    for (let i = 0; i < a.bytes.length; i += 0x8000) {
      binary += String.fromCharCode(...a.bytes.subarray(i, i + 0x8000));
    }
    out.set(a.contentId, `data:${a.mimeType};base64,${btoa(binary)}`);
  }
  return out;
}

/**
 * Download links for attachments decoded in this browser.
 *
 * Every blob is typed application/octet-stream and every link carries
 * `download`, so a click saves the file and nothing the sender attached is
 * ever navigated to in this origin. A text/html attachment opened as a blob
 * URL would otherwise run as this app.
 */
export function DecryptedAttachments({ attachments, omitted }: { attachments: MimeAttachment[]; omitted: number }) {
  const [urls, setUrls] = useState<string[]>([]);
  useEffect(() => {
    const created = attachments.map((a) => URL.createObjectURL(new Blob([a.bytes as BlobPart], { type: "application/octet-stream" })));
    setUrls(created);
    return () => {
      created.forEach((u) => URL.revokeObjectURL(u));
    };
  }, [attachments]);

  if (attachments.length === 0 && omitted === 0) return null;
  return (
    <div className="email-attachments">
      <strong>Attachments:</strong>
      <div className="email-attachment-list">
        {attachments.map((a, i) => (
          <a key={`${a.name}-${i}`} className="email-attachment-link" href={urls[i]} download={a.name}>
            📎 {a.name} <span className="email-attachment-size">({formatBytes(a.bytes.length)})</span>
          </a>
        ))}
      </div>
      {omitted > 0 ? (
        <span className="email-attachments-status email-attachments-error">
          {" "}
          {omitted} attachment{omitted === 1 ? "" : "s"} not shown: over the size or count limit.
        </span>
      ) : null}
    </div>
  );
}
