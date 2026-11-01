import { type ReactNode, useEffect, useRef } from "react";
import { Link } from "react-router";
import type { SettingsTab } from "../app/navigation";
import { initMenuButton } from "../ky-ui/menu";

/**
 * ky-ui's Settings menu (README "Settings menu"): the gear opens a menu of the
 * Settings tabs instead of a page. ky-ui's menu.js owns open/close, focus and keys;
 * React only renders the markup, so `hidden` and `aria-expanded` are left to it.
 */
export function SettingsMenu({ id, tabs, pathname, onSettings, icon }: {
  id: string;
  tabs: ReadonlyArray<SettingsTab>;
  pathname: string;
  onSettings: boolean;
  icon: ReactNode;
}) {
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => initMenuButton(button.current!), []);
  return (
    <div className="ky-menu">
      <button
        ref={button}
        type="button"
        className="ky-icon-button"
        aria-label="Settings"
        title="Settings"
        aria-haspopup="menu"
        aria-expanded="false"
        aria-controls={id}
        aria-current={onSettings ? "page" : undefined}
      >
        {icon}
      </button>
      <div className="ky-menu-list" id={id} role="menu" aria-label="Settings" hidden>
        {tabs.map(({ to, label }) => (
          <Link key={to} role="menuitem" tabIndex={-1} to={to} aria-current={to === pathname ? "page" : undefined}>
            {label}
          </Link>
        ))}
      </div>
    </div>
  );
}
