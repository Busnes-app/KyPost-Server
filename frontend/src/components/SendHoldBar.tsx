import { useEffect, useEffectEvent, useState } from "react";
import type { HeldSend } from "../hooks/useCompose";

/**
 * "Sending in Ns · Cancel sending" while the server holds a sent message for
 * the Send delay. It goes when the hold ends: after that the message cannot be
 * recalled, so nothing may suggest it can. Mount one per hold (key it by id).
 */
export function SendHoldBar({ held, onCancel, onDismiss }: { held: HeldSend; onCancel: () => void; onDismiss: () => void }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 250);
    return () => window.clearInterval(timer);
  }, []);
  const expired = !held.late && !held.canceling && now >= held.endsAt;
  const dismiss = useEffectEvent(onDismiss);
  useEffect(() => {
    if (expired) dismiss();
  }, [expired]);

  return (
    <div className="send-hold-bar" role="status">
      {held.late ? (
        <>
          <span>Too late to cancel: this message is already being sent.</span>
          <button type="button" onClick={onDismiss}>Dismiss</button>
        </>
      ) : (
        <>
          <span>Sending in {Math.max(0, Math.ceil((held.endsAt - now) / 1000))}s</span>
          <span aria-hidden="true">·</span>
          <button type="button" className="btn-primary" onClick={onCancel} disabled={held.canceling}>Cancel sending</button>
          {held.error ? <span className="send-hold-error">{held.error}</span> : null}
        </>
      )}
    </div>
  );
}
