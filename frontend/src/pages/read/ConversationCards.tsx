import { useState } from "react";
import { getJSON, toErrorMessage } from "../../api/client";
import { processEmailHtml } from "../../lib/emailHtml";
import { EmailBodyFrame } from "./EmailBodyFrame";
import { formatReaderTime, senderName } from "./format";
import type { SentMember } from "./conversations";
import type { InboxEmail } from "./types";

type Props = {
  items: Array<InboxEmail | SentMember>;
  onOpen: (message: InboxEmail) => void;
};

/**
 * The collapsed messages of an open conversation, on one side of the open one.
 * A message in this folder opens in the reader, where its actions apply to it
 * alone; a reply of yours expands in place and stays in Sent.
 */
export function ConversationCards({ items, onOpen }: Props) {
  if (items.length === 0) return null;
  return (
    <ol className="conversation-cards">
      {items.map((m) => (
        <li key={"mailbox" in m ? `${m.mailbox}\n${m.messageId}` : m.messageId}>
          {"mailbox" in m ? (
            <SentCard message={m} />
          ) : (
            <button
              type="button"
              className={`conversation-card${m.status === "read" ? "" : " conversation-card-unread"}`}
              onClick={() => onOpen(m)}
              aria-label={`${m.status === "read" ? "" : "Unread. "}Open message from ${senderName(m.sender)}, ${formatReaderTime(m.atUtc)}`}
            >
              <CardHead from={senderName(m.sender)} at={m.atUtc} />
              {m.preview ? <span className="conversation-card-snippet">{m.preview}</span> : null}
            </button>
          )}
        </li>
      ))}
    </ol>
  );
}

function CardHead({ from, at }: { from: string; at: string }) {
  return (
    <span className="conversation-card-head">
      <span className="conversation-card-from">{from}</span>
      <time dateTime={at}>{formatReaderTime(at)}</time>
    </span>
  );
}

// A reply's body loads when it is first expanded. Remote content stays blocked.
function SentCard({ message }: { message: SentMember }) {
  const [body, setBody] = useState<{ body: string; bodyMode?: string } | null>(null);
  const [error, setError] = useState("");
  const load = () => {
    if (body || error) return;
    const query = `messageId=${encodeURIComponent(message.messageId)}&mailbox=${encodeURIComponent(message.mailbox)}`;
    getJSON<{ body?: string; bodyMode?: string }>(`/api/mail/body?${query}`).then(
      (data) => setBody({ body: data.body ?? "", bodyMode: data.bodyMode }),
      (e: unknown) => setError(toErrorMessage(e, "could not load this message"))
    );
  };
  return (
    <details className="conversation-card conversation-card-sent" onToggle={(e) => e.currentTarget.open && load()}>
      <summary aria-label={`Your reply, ${formatReaderTime(message.atUtc)}, in ${message.mailbox}`}>
        <CardHead from="me" at={message.atUtc} />
        <span className="conversation-card-snippet">In {message.mailbox}</span>
      </summary>
      {error ? <p className="notice notice-error">{error}</p> : null}
      {!body && !error ? <p className="conversation-card-snippet">Loading message…</p> : null}
      {body && body.bodyMode === "html" && body.body ? (
        <EmailBodyFrame className="email-reader-body-frame" html={processEmailHtml(body.body, false)} />
      ) : body ? (
        <pre className="email-reader-body-block">{body.body || `No readable body here. Open it in ${message.mailbox} to read it.`}</pre>
      ) : null}
    </details>
  );
}
