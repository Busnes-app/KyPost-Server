import { getJSON, postJSON, putJSON, deleteJSON } from "./client";
import type { Role } from "../auth";

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
};

type UsersListResponse = {
  users: ManagedUser[];
};

export async function listUsers(): Promise<ManagedUser[]> {
  const res = await getJSON<UsersListResponse>("/api/users");
  return res.users ?? [];
}

export function createUser(username: string, password: string, role: Role): Promise<ManagedUser> {
  return postJSON<ManagedUser>("/api/users", { username, password, role });
}

export function setUserRole(id: string, role: Role): Promise<ManagedUser> {
  return putJSON<ManagedUser>(`/api/users/${encodeURIComponent(id)}`, { role });
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
  id: string,
  password: string
): Promise<{ user: ManagedUser; pgpKeyInaccessible: boolean }> {
  return postJSON<{ user: ManagedUser; pgpKeyInaccessible: boolean }>(
    `/api/users/${encodeURIComponent(id)}/reset-password`,
    { password }
  );
}

export function deactivateUser(id: string): Promise<ManagedUser> {
  return postJSON<ManagedUser>(`/api/users/${encodeURIComponent(id)}/deactivate`, {});
}

export function reactivateUser(id: string): Promise<ManagedUser> {
  return postJSON<ManagedUser>(`/api/users/${encodeURIComponent(id)}/reactivate`, {});
}

export function clearUserMFA(id: string): Promise<ManagedUser> {
  return postJSON<ManagedUser>(`/api/users/${encodeURIComponent(id)}/clear-mfa`, {});
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
export function putUserIMAPConfig(id: string, input: UserIMAPInput): Promise<UserIMAPStatus> {
  return putJSON<UserIMAPStatus>(userPath(id, "imap-config"), input);
}
export function deleteUserIMAPConfig(id: string): Promise<{ ok: boolean }> {
  return deleteJSON<{ ok: boolean }>(userPath(id, "imap-config"));
}
export function getUserCardDAVClient(id: string): Promise<UserCardDAVStatus> {
  return getJSON<UserCardDAVStatus>(userPath(id, "carddav-client"));
}
export function putUserCardDAVClient(id: string, input: UserCardDAVInput): Promise<UserCardDAVStatus> {
  return putJSON<UserCardDAVStatus>(userPath(id, "carddav-client"), input);
}
export function deleteUserCardDAVClient(id: string): Promise<{ ok: boolean }> {
  return deleteJSON<{ ok: boolean }>(userPath(id, "carddav-client"));
}
