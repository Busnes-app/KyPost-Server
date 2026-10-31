// The Settings page's section list (inside the content, not the sidebar). Data,
// not behaviour — kept out of App.tsx so adding a panel is a one-line edit in an
// obvious place. Appearance is not here: it is its own sidebar item.
//
// ponytail: version is no longer a frontend constant — About and Admin > Server
// both read the backend's appversion.String() (via GET /api/status `version`),
// the same LHS the self-update check uses. backend/internal/appversion/VERSION
// is the only version source; package.json has none.

export type SettingsNavItem = { to: string; label: string };

export type SettingsNavGroup = {
  /** Rendered only when more than one group is visible. */
  heading?: string;
  items: ReadonlyArray<SettingsNavItem>;
};

/** Panels anyone can reach. Status is the trimmed health view. */
const CONFIG_ITEMS: ReadonlyArray<SettingsNavItem> = [
  { to: "/settings/mail", label: "Mail" },
  { to: "/settings/security", label: "Security" },
  { to: "/settings/notifications", label: "Notifications" },
  // Config, not Admin: everything on it is per-user — each account's own
  // TUNING.md, auto-apply preference and decision log, all behind withAuth
  // endpoints. Nothing on the panel is role-gated.
  { to: "/settings/automation", label: "Email Labels" }
];

const STATUS_ITEM: SettingsNavItem = { to: "/settings/status", label: "Status" };

const ADMIN_ITEMS: ReadonlyArray<SettingsNavItem> = [
  { to: "/admin/server", label: "Server" },
  { to: "/admin/diagnostics", label: "Diagnostics" }
];

/**
 * A non-admin sees one flat list: a heading would be labelling the only group
 * there is. An admin sees Config and Admin, and loses Status because
 * Diagnostics is the same page without the trimming.
 */
export function visibleSettingsGroups(isAdmin: boolean): ReadonlyArray<SettingsNavGroup> {
  if (!isAdmin) {
    return [{ items: [...CONFIG_ITEMS, STATUS_ITEM] }];
  }
  return [
    { heading: "Config", items: CONFIG_ITEMS },
    { heading: "Admin", items: ADMIN_ITEMS }
  ];
}
