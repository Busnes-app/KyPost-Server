import { createContext, useContext } from "react";

export type Role = "admin" | "user";

/** First sign-in encryption setup, as GET /api/auth/me reports it. */
export type EncryptionSetup = {
  /** Mail is refused until setup is complete; no "Not now". */
  required: boolean;
  /** The step is offered once, with "Not now". */
  offered: boolean;
  hasKey: boolean;
  hasRecovery: boolean;
  incomingEnabled: boolean;
  keyPolicy: "required" | "optional";
  incomingPolicy: "required" | "on" | "off";
};

export type AuthState = {
  authenticated: boolean;
  userId?: string;
  username?: string;
  role?: Role;
  mustChangePassword?: boolean;
  // False for an SSO-only account. Its PGP key is sealed under a separate
  // encryption passphrase, and key changes are confirmed with KySignOn.
  // Absent (older server) means a password account.
  hasPassword?: boolean;
  // True for a session signed in through KySignOn. It has no password to
  // re-enter, so sensitive actions are confirmed with KySignOn instead.
  ssoSession?: boolean;
  ssoSub?: string;
  ssoUsername?: string;
  ssoEmail?: string;
  ssoLinkedAt?: number;
  // The subject outlives the credential: revocation keeps ssoSub so directory
  // sync can still address the account. Read this, not ssoSub, to decide
  // whether the link can sign anyone in.
  ssoLinkRevoked?: boolean;
  // Absent on older servers: no setup step.
  encryptionSetup?: EncryptionSetup;
};

export const AuthContext = createContext<AuthState>({ authenticated: false });

export function useAuth(): AuthState {
  return useContext(AuthContext);
}
