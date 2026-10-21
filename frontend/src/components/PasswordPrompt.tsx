import { useCallback, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useDialogOpen } from "../hooks/useDialogOpen";

type Pending = { message: string; resolve: (password: string | null) => void };

/**
 * An in-page replacement for `window.prompt` when the answer is the account
 * password: the browser prompt shows it in clear text, cannot be filled by a
 * password manager, and sits outside the page's own dialogs.
 *
 * `ask` resolves to the password, or null when the user cancels. Render
 * `dialog` once in the calling component. Paragraphs in `message` are split on
 * blank lines, as the old prompt strings were written.
 */
export function usePasswordPrompt(): [ask: (message: string) => Promise<string | null>, dialog: ReactNode] {
  const [pending, setPending] = useState<Pending | null>(null);
  const ask = useCallback(
    (message: string) => new Promise<string | null>((resolve) => setPending({ message, resolve })),
    []
  );
  const finish = (password: string | null) => {
    pending?.resolve(password);
    setPending(null);
  };
  return [ask, <PasswordPromptDialog key={pending?.message ?? ""} pending={pending} onDone={finish} />];
}

function PasswordPromptDialog({ pending, onDone }: { pending: Pending | null; onDone: (password: string | null) => void }) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);
  const inputId = useId();
  // Keyed by message in usePasswordPrompt, so a typed password never outlives its prompt.
  const [password, setPassword] = useState("");
  useDialogOpen(dialogRef, pending);

  function submit(event: FormEvent) {
    event.preventDefault();
    onDone(password);
  }

  return (
    <dialog
      ref={dialogRef}
      className="rules-help-backdrop"
      aria-label="Enter your account password"
      onCancel={(event) => {
        event.preventDefault();
        onDone(null);
      }}
    >
      {pending ? (
        <div className="rules-help-window rules-help-compact">
          <div className="rules-help-head">
            <h3>Enter your account password</h3>
          </div>
          <div className="rules-help-body">
            {pending.message.split("\n\n").map((paragraph) => (
              <p key={paragraph} className="contacts-muted">{paragraph}</p>
            ))}
            <form onSubmit={submit}>
              <label htmlFor={inputId} style={{ display: "block", marginBottom: 4 }}>
                Account password
              </label>
              <input
                id={inputId}
                type="password"
                autoFocus
                autoComplete="current-password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                style={{ width: "100%" }}
              />
              <div style={{ display: "flex", gap: 8, marginTop: 16, justifyContent: "flex-end" }}>
                <button type="button" className="contacts-action" onClick={() => onDone(null)}>
                  Cancel
                </button>
                <button type="submit" disabled={password.length === 0}>
                  Continue
                </button>
              </div>
            </form>
          </div>
        </div>
      ) : null}
    </dialog>
  );
}
