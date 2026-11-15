import { useEffect, useState } from "react";
import { isChatPerson, openChat } from "../api/kymessage";

/**
 * "Chat" with the person at address on KyMessages (docs/KYMESSAGES.md).
 * Renders nothing unless Element is in the suite (origin) and KyPost's server
 * places the address in the organisation. A refusal goes to onMessage when
 * given, else beside the button.
 */
export function ChatButton({ address, origin, onMessage, className = "btn-ghost" }: { address: string; origin: string; onMessage?: (m: string) => void; className?: string }) {
  const [person, setPerson] = useState({ address: "", inside: false });
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState({ address: "", text: "" });
  useEffect(() => {
    if (!origin || !address) return;
    let live = true;
    void isChatPerson(address).then((inside) => {
      if (live) setPerson({ address, inside });
    });
    return () => {
      live = false;
    };
  }, [address, origin]);
  if (!origin || person.address !== address || !person.inside) return null;
  return (
    <>
      <button
        type="button"
        className={className}
        title="Chat on KyMessages"
        disabled={busy}
        onClick={() => {
          setBusy(true);
          setMessage({ address, text: "" });
          void openChat(address, origin).then((text) => {
            setBusy(false);
            if (onMessage) onMessage(text);
            else setMessage({ address, text });
          });
        }}
      >
        Chat
      </button>
      {message.address === address && message.text ? <span className="email-attachments-error" role="alert">{message.text}</span> : null}
    </>
  );
}
