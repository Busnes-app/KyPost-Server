// The Settings page's section tabs (inside the content, not the sidebar). Data,
// not behaviour — kept out of App.tsx so adding a panel is a one-line edit in an
// obvious place. The footer's gear button opens the first one.
//
// ponytail: version is no longer a frontend constant — About and Admin > Server
// both read the backend's appversion.String() (via GET /api/status `version`),
// the same LHS the self-update check uses. backend/internal/appversion/VERSION
// is the only version source; package.json has none.

export type SettingsTab = { to: string; label: string };

/** The suite's personal sections, first and in the suite's order. KyPost has no separate Account page. */
const PERSONAL_TABS: ReadonlyArray<SettingsTab> = [
  { to: "/settings/apps", label: "Phones & apps" },
  { to: "/settings/appearance", label: "Appearance" }
];

/** KyPost's own panels anyone can reach. */
const MAIL_TABS: ReadonlyArray<SettingsTab> = [
  { to: "/settings/mail", label: "Mail" },
  { to: "/settings/security", label: "Security" },
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
