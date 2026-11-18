import { useEffect, useState } from "react";
import { toErrorMessage } from "../../api/client";
import { getSendDelay, SEND_DELAY_CHOICES, setSendDelay } from "../../api/scheduled";
import { CheckIcon } from "../../pages/read/icons";

/** How long Send holds a message on the server before it goes, for every client. */
export function SendDelay() {
  const [seconds, setSeconds] = useState<number | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let live = true;
    getSendDelay()
      .then((s) => { if (live) setSeconds(s); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read your Send delay.")); });
    return () => { live = false; };
  }, []);

  function choose(value: number) {
    setBusy(true);
    setError("");
    setSendDelay(value)
      .then(setSeconds)
      .catch((e: unknown) => setError(toErrorMessage(e, "Could not save your Send delay.")))
      .finally(() => setBusy(false));
  }

  return (
    <fieldset className="config-section">
      <legend><h3>Send delay</h3></legend>
      <p>
        Send waits this long on the server before the message goes, with Cancel sending shown until then. Canceling
        brings it back to edit. Once it has gone it cannot be recalled.
      </p>
      {error ? <p className="notice notice-error" role="alert">{error}</p> : null}
      {seconds === null && !error ? <p>Loading…</p> : null}
      {seconds !== null ? (
        <div className="appearance-modes">
          {SEND_DELAY_CHOICES.map((value) => (
            <label key={value} className="appearance-mode">
              <input
                type="radio"
                name="kypost-send-delay"
                value={value}
                checked={seconds === value}
                disabled={busy}
                onChange={() => choose(value)}
              />
              {seconds === value ? <CheckIcon /> : null}
              <span>{value === 0 ? "Off" : `${value} seconds`}</span>
            </label>
          ))}
        </div>
      ) : null}
    </fieldset>
  );
}
