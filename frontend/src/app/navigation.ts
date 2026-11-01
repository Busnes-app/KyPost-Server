// The Settings page's section tabs (inside the content, not the sidebar). Data,
// not behaviour — kept out of App.tsx so adding a panel is a one-line edit in an
// obvious place. The footer's gear opens a menu listing them in this order.
//
// ponytail: version is no longer a frontend constant — About and Admin > Server
// both read the backend's appversion.String() (via GET /api/status `version`),
// the same LHS the self-update check uses. backend/internal/appversion/VERSION
// is the only version source; package.json has none.

export type SettingsTab = { to: string; label: string };

/** The suite's personal sections, first and in the suite's order. Account is sign-in security, behind re-authentication. */
const PERSONAL_TABS: ReadonlyArray<SettingsTab> = [
  { to: "/settings/account", label: "Account" },
  { to: "/settings/apps", label: "Phones & apps" },
  { to: "/settings/appearance", label: "Appearance" }
];

/** Where a bare /settings lands: the first tab without Account's password check. */
export const SETTINGS_LANDING = "/settings/apps";

/** KyPost's own panels anyone can reach. */
const MAIL_TABS: ReadonlyArray<SettingsTab> = [
  { to: "/settings/mail", label: "Mail" },
  { to: "/settings/notifications", label: "Notifications" },
  // Per-user, not Admin: everything on it is each account's own TUNING.md,
  // auto-apply preference and decision log, all behind withAuth endpoints.
  { to: "/settings/automation", label: "Email Labels" }
];

const STATUS_TAB: SettingsTab = { to: "/settings/status", label: "Status" };

const ADMIN_TABS: ReadonlyArray<SettingsTab> = [
  { to: "/admin/server", label: "Server" },
  { to: "/admin/diagnostics", label: "Diagnostics" }
];

/** An admin loses Status because Diagnostics is the same page without the trimming. */
export function visibleSettingsTabs(isAdmin: boolean): ReadonlyArray<SettingsTab> {
  return [...PERSONAL_TABS, ...MAIL_TABS, ...(isAdmin ? ADMIN_TABS : [STATUS_TAB])];
}
