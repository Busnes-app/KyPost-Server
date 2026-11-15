import { useEffect, useState } from "react";
import { toErrorMessage } from "../api/client";
import { getOutOfOffice, putOutOfOffice, type OutOfOfficeView } from "../api/outOfOffice";

const when = (iso: string) => new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });

/** "Out of office is on" above the inbox, with a one-click Turn off. */
export function OutOfOfficeBar() {
  const [state, setState] = useState<OutOfOfficeView | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    getOutOfOffice()
      .then((s) => { if (live && s) setState(s); })
      .catch(() => undefined);
    return () => { live = false; };
  }, []);

  if (!state?.enabled) return null;

  const turnOff = () => {
    setBusy(true);
    setError("");
    putOutOfOffice({ ...state, enabled: false })
      .then(setState)
      .catch((e: unknown) => setError(toErrorMessage(e, "Could not turn off your automatic reply.")))
      .finally(() => setBusy(false));
  };

  const future = state.start && !state.active && new Date(state.start) > new Date();
  const text = future
    ? `Out of office is on from ${when(state.start)}${state.end ? ` until ${when(state.end)}` : ""}.`
    : `Out of office is on${state.end ? ` until ${when(state.end)}` : ""}.`;
  return (
    <div className="notice notice-success ooo-bar" role="status">
      <span>{text} Automatic replies are being sent.</span>
      <button type="button" onClick={turnOff} disabled={busy}>{busy ? "Turning off..." : "Turn off"}</button>
      {error ? <span role="alert">{error}</span> : null}
    </div>
  );
}
