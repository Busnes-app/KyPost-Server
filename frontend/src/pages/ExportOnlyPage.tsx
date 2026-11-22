import { AuthContext, type AuthState } from "../auth";
import { postJSON } from "../api/client";
import { AppMark } from "./read/icons";
import { MailExport } from "../settings/sections/MailExport";

/**
 * The whole app for an export-only session (hosted mode, while the account's organisation is
 * scheduled for deletion): the server answers only the export routes, so nothing else is shown.
 */
export function ExportOnlyPage({ auth }: { auth: AuthState }) {
  async function signOut() {
    try {
      await postJSON<{ ok: boolean }>("/api/auth/logout", {});
    } finally {
      window.location.assign("/login");
    }
  }
  return (
    <main className="ky-auth">
      <div className="ky-auth-theme" />
      <div className="ky-auth-body">
        <div className="ky-auth-brand">
          <AppMark size={56} />
          <h1>KyPost</h1>
        </div>
        <div className="ky-auth-card">
          <div role="status">
            <h2>This account is being deleted</h2>
            <p>Download a copy of your mail now. When the deletion completes, it cannot be recovered. Nothing else in KyPost is available.</p>
          </div>
          {/* MailExport reads the session as signed in; the server admits only the export. */}
          <AuthContext.Provider value={{ ...auth, authenticated: true }}>
            <MailExport />
          </AuthContext.Provider>
          <button type="button" className="button secondary" onClick={() => void signOut()}>Sign out</button>
        </div>
      </div>
    </main>
  );
}
