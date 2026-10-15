import { getJSON, postJSON, putJSON, deleteJSON } from "./client";
import { credentialFields, deriveCredential } from "./auth";
import { withSSOStepUp } from "./stepup";
import type { Role } from "../auth";

/** The acting admin's step-up: their account password (derived, never sent), unused for a KySignOn session. */
export type AdminConfirm = { ssoSession: boolean; accountPassword: string };

type Send<T> = (body: object, headers: Record<string, string>) => Promise<T>;

/**
 * Sends one user-management change with the actor's credential as
 * adminAuthSecret (the bodies carry a password of their own), or through a
 * KySignOn confirmation bound to the identical request.
 */
export async function confirmedAdmin<T>(confirm: AdminConfirm, body: object, send: Send<T>): Promise<T> {
  const credential = confirm.ssoSession ? {} : credentialFields(await deriveCredential("", confirm.accountPassword), "admin");
  const signed = { ...body, ...credential };
  return withSSOStepUp((headers) => send(signed, headers));
}

export type ManagedUser = {
  id: string;
  username: string;
  role: Role;
  active: boolean;
  mustChangePassword: boolean;
  createdAt: string;
  updatedAt: string;
  deactivatedAt?: string;
  totpEnabled?: boolean;
  /** Legacy plaintext-password hash: cannot sign in until an admin resets it. */
  needsPasswordReset?: boolean;
};

type UsersListResponse = {
  users: ManagedUser[];
};

export async function listUsers(): Promise<ManagedUser[]> {
  const res = await getJSON<UsersListResponse>("/api/users");
  return res.users ?? [];
}

export function createUser(confirm: AdminConfirm, username: string, password: string, role: Role): Promise<ManagedUser> {
  return confirmedAdmin(confirm, { username, password, role }, (b, h) => postJSON<ManagedUser>("/api/users", b, h));
}

export function setUserRole(confirm: AdminConfirm, id: string, role: Role): Promise<ManagedUser> {
  return confirmedAdmin(confirm, { role }, (b, h) => putJSON<ManagedUser>(`/api/users/${encodeURIComponent(id)}`, b, h));
}

/**
 * Resets an account's password to a temporary one.
 *
 * `pgpKeyInaccessible` reports whether this reset left a client-protected PGP
 * key unreadable under the NEW password. The key is wrapped under the password
 * the user chose and the server cannot rewrap what it cannot read.
 *
 * It is NOT destroyed, and the name matters: the server writes no envelope
 * field on a reset, and the wrapping salt is stored inside the envelope rather
 * than in the account's login salt, so the PREVIOUS password still opens it via
 * "Key won't unlock?". The field was once called `pgpKeyDestroyed`, which sent
 * users to generate a new identity — the one action that really is irreversible.
 */
export function resetUserPassword(
  confirm: AdminConfirm,
  id: string,
  password: string
): Promise<{ user: ManagedUser; pgpKeyInaccessible: boolean }> {
  return confirmedAdmin(confirm, { password }, (b, h) =>
    postJSON<{ user: ManagedUser; pgpKeyInaccessible: boolean }>(`/api/users/${encodeURIComponent(id)}/reset-password`, b, h)
  );
}

/** Deactivates, reactivates or clears MFA for one account. */
export function userAction(confirm: AdminConfirm, id: string, action: "deactivate" | "reactivate" | "clear-mfa"): Promise<ManagedUser> {
  return confirmedAdmin(confirm, {}, (b, h) => postJSON<ManagedUser>(`/api/users/${encodeURIComponent(id)}/${action}`, b, h));
}

export type UserIMAPStatus = {
  configured: boolean;
  host?: string;
  port?: number;
  username?: string;
  mailbox?: string;
  smtpHost?: string;
  smtpPort?: number;
  managed?: boolean;
};

export type UserIMAPInput = {
  host: string;
  port: number;
  username: string;
  password: string;
  mailbox: string;
  smtpHost: string;
  smtpPort: number;
  managed: boolean;
};

export type UserCardDAVStatus = {
  configured: boolean;
  serverUrl?: string;
  username?: string;
  addressBookPath?: string;
  managed?: boolean;
};

export type UserCardDAVInput = {
  serverUrl: string;
  username: string;
  password: string;
  addressBookPath: string;
  managed: boolean;
};

const userPath = (id: string, tail: string) => `/api/users/${encodeURIComponent(id)}/${tail}`;

export function getUserIMAPConfig(id: string): Promise<UserIMAPStatus> {
  return getJSON<UserIMAPStatus>(userPath(id, "imap-config"));
}
export function putUserIMAPConfig(confirm: AdminConfirm, id: string, input: UserIMAPInput): Promise<UserIMAPStatus> {
  return confirmedAdmin(confirm, input, (b, h) => putJSON<UserIMAPStatus>(userPath(id, "imap-config"), b, h));
}
export function deleteUserIMAPConfig(confirm: AdminConfirm, id: string): Promise<{ ok: boolean }> {
  return confirmedAdmin(confirm, {}, (b, h) => deleteJSON<{ ok: boolean }>(userPath(id, "imap-config"), b, h));
}
export function getUserCardDAVClient(id: string): Promise<UserCardDAVStatus> {
  return getJSON<UserCardDAVStatus>(userPath(id, "carddav-client"));
}
export function putUserCardDAVClient(confirm: AdminConfirm, id: string, input: UserCardDAVInput): Promise<UserCardDAVStatus> {
  return confirmedAdmin(confirm, input, (b, h) => putJSON<UserCardDAVStatus>(userPath(id, "carddav-client"), b, h));
}
export function deleteUserCardDAVClient(confirm: AdminConfirm, id: string): Promise<{ ok: boolean }> {
  return confirmedAdmin(confirm, {}, (b, h) => deleteJSON<{ ok: boolean }>(userPath(id, "carddav-client"), b, h));
}
