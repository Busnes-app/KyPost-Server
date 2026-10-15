// The credential half of the auth API: fetching the derivation parameters and
// turning a typed password into the auth secret the server actually receives.
//
// Kept in one place so no page re-implements the handshake. Every caller that
// needs to prove the account password goes through deriveCredential and sends
// what it returns — see lib/authSecret.ts for why the password itself must not
// be transmitted.

import { getJSON, postJSON } from "./client";
import { withSSOStepUp } from "./stepup";
import { deriveAuthSecret, type LoginParams } from "../lib/authSecret";

/**
 * Fetches this username's derivation parameters.
 *
 * Unauthenticated and deliberately uninformative: the response is the same
 * shape whether or not the account exists (the server returns a stable
 * synthetic salt otherwise), so this cannot be used to enumerate accounts.
 */
export async function getLoginParams(username: string): Promise<LoginParams> {
  // An empty username asks the server to answer for the authenticated caller,
  // which is what the re-authentication flows want: they already hold a session,
  // and echoing their own username back would let a caller pass someone else's.
  const q = username ? `?username=${encodeURIComponent(username)}` : "";
  return getJSON<LoginParams>(`/api/auth/login-params${q}`);
}

/** The derived credential for an account password; the password itself stays here. */
export type Credential = { authSecret: string };

/**
 * The credential fields to put on the wire: only the auth secret, under
 * `authSecret` or `<prefix>AuthSecret`. The server accepts nothing else.
 */
export function credentialFields(credential: Credential, prefix = ""): Record<string, string> {
  return { [prefix ? `${prefix}AuthSecret` : "authSecret"]: credential.authSecret };
}

/** Derives the credential for username/password. */
export async function deriveCredential(username: string, password: string): Promise<Credential> {
  return { authSecret: await deriveAuthSecret(password, await getLoginParams(username)) };
}

/**
 * Derives the credential for a NEW password, against a caller-supplied salt.
 *
 * The salt is chosen client-side for a password change because the server
 * cannot mint one for a credential it will never see: it has to be recorded in
 * the same request that sets the credential, or the two disagree and the account
 * is locked out.
 */
export async function deriveNewCredential(
  password: string,
  salt: string,
  iterations: number
): Promise<string> {
  return deriveAuthSecret(password, { salt, iterations });
}

/**
 * Re-proves the account credential and the second factor for the session that
 * already exists. Throws on any failure; the caller shows the message.
 *
 * `code` is a TOTP code, or a one-time recovery code in its place, and is
 * ignored by the server for an account without TOTP.
 *
 * This authorises nothing on its own. It answers "is the person at this
 * keyboard the account owner, right now", which is what a screen full of key
 * material wants to know before it draws itself; the operations behind that
 * screen re-verify for themselves regardless.
 */
export async function reauthenticate(password: string, code: string): Promise<void> {
  const credential = await deriveCredential("", password);
  await postJSON("/api/auth/step-up", { ...credentialFields(credential), code });
}

/** The same gate for a KySignOn session: a fresh sign-in there, bound to this request. */
export async function reauthenticateWithSSO(): Promise<void> {
  await withSSOStepUp((headers) => postJSON("/api/auth/step-up", {}, headers));
}
