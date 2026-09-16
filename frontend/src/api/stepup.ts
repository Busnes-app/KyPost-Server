import { deleteJSON, getJSON, HttpError, postJSON } from "./client";

/**
 * Action-bound re-authentication for sessions signed in through KySignOn.
 *
 * Such a session has no account password to re-enter. When a sensitive
 * request is refused with `sso_step_up_required`, the server has minted a
 * challenge bound to exactly that request; the user proves a fresh login to
 * KySignOn in a popup, and the same request is sent again carrying the
 * challenge in the `X-Kypost-Step-Up` header. The grant is spent once.
 */

export const STEP_UP_HEADER = "X-Kypost-Step-Up";

/** The challenge a refusal carries, or null when the refusal is something else. */
export function stepUpChallenge(error: unknown): string | null {
  if (!(error instanceof HttpError) || error.status !== 403) return null;
  const body = error.body;
  if (typeof body !== "object" || body === null) return null;
  const { error: code, challenge } = body as { error?: unknown; challenge?: unknown };
  return code === "sso_step_up_required" && typeof challenge === "string" && challenge ? challenge : null;
}

/**
 * Runs the request; if the server asks for a KySignOn confirmation, obtains
 * one and runs it again with the grant. `run` must send the identical
 * request both times, because the grant is bound to it.
 */
export async function withSSOStepUp<T>(run: (headers: Record<string, string>) => Promise<T>): Promise<T> {
  try {
    return await run({});
  } catch (error) {
    const challenge = stepUpChallenge(error);
    if (!challenge) throw error;
    await confirmSSOAction(challenge);
    return run({ [STEP_UP_HEADER]: challenge });
  }
}

/** How long the user has to finish the sign-in; matches the server's challenge life. */
const CONFIRMATION_WINDOW_MS = 5 * 60_000;

/**
 * Walks the user through proving one action to KySignOn. Opens the popup
 * synchronously from the click so popup blockers allow it, detaches it from
 * this window before navigating, then polls until the server marks the
 * challenge verified. Cancelling, closing the popup or timing out drops the
 * challenge on the server.
 */
export async function confirmSSOAction(challenge: string): Promise<void> {
  const endpoint = `/api/auth/oidc/step-up/${encodeURIComponent(challenge)}`;
  let popup: Window | null = null;
  let cancelled = false;
  let verified = false;

  const dialog = document.createElement("dialog");
  dialog.className = "sec-card";
  const title = document.createElement("h2");
  title.id = `sso-confirm-${challenge}`;
  title.textContent = "Confirm it is you";
  dialog.setAttribute("aria-labelledby", title.id);
  const explanation = document.createElement("p");
  explanation.textContent = "Sign in again with KySignOn to authorize only the action you just requested.";
  const proceed = document.createElement("button");
  proceed.type = "button";
  proceed.textContent = "Continue to KySignOn";
  const cancel = document.createElement("button");
  cancel.type = "button";
  cancel.className = "button secondary";
  cancel.textContent = "Cancel";
  dialog.append(title, explanation, proceed, cancel);
  document.body.append(dialog);

  try {
    await new Promise<void>((resolve, reject) => {
      const stop = () => {
        cancelled = true;
        reject(new Error("confirmation cancelled"));
      };
      dialog.addEventListener("cancel", (event) => {
        event.preventDefault();
        stop();
      });
      cancel.onclick = stop;
      proceed.onclick = () => {
        popup = window.open("about:blank", "_blank", "popup,width=600,height=750");
        if (!popup) {
          reject(new Error("allow the KySignOn sign-in window to confirm this action"));
          return;
        }
        // Detached: the sign-in window must not be able to reach this one.
        popup.opener = null;
        proceed.disabled = true;
        explanation.textContent = "Complete the sign-in in the KySignOn window. You can cancel here at any time.";
        resolve();
      };
      dialog.showModal();
    });
    cancel.onclick = () => {
      cancelled = true;
    };

    const { authorizeUrl } = await postJSON<{ authorizeUrl: string }>("/api/auth/oidc/step-up", { challenge });
    const target = new URL(authorizeUrl);
    if (target.protocol !== "https:" && !(target.protocol === "http:" && ["localhost", "127.0.0.1", "[::1]"].includes(target.hostname))) {
      throw new Error("the sign-in URL is not secure");
    }
    const signIn = popup as Window | null;
    if (!signIn || cancelled) throw new Error("confirmation cancelled");
    signIn.location.href = target.href;

    const deadline = Date.now() + CONFIRMATION_WINDOW_MS;
    while (!cancelled && Date.now() < deadline) {
      const status = await getJSON<{ verified: boolean }>(endpoint);
      if (status.verified) {
        verified = true;
        return;
      }
      if (signIn.closed) break;
      await new Promise((resolve) => setTimeout(resolve, 1000));
    }
    throw new Error("confirmation cancelled or expired");
  } finally {
    (popup as Window | null)?.close();
    dialog.close();
    dialog.remove();
    if (!verified) await deleteJSON(endpoint).catch(() => undefined);
  }
}
