import { type ReactNode } from "react";
import { Tabs } from "@base-ui/react/tabs";
import { useSearchParams } from "react-router";

export type PanelTab = {
  /** Stable id, used as the `?tab=` value — retired routes redirect straight to one. */
  id: string;
  label: string;
  body: ReactNode;
};

/** Falls back to the first tab for anything unrecognised, so a bad link still renders. */
export function resolvePanelTab(raw: string | null, tabs: ReadonlyArray<{ id: string }>): string {
  const value = (raw ?? "").trim();
  return tabs.some((tab) => tab.id === value) ? value : (tabs[0]?.id ?? "");
}

/**
 * The tab strip a panel uses once it holds more sections than fit one scroll.
 *
 * The active tab lives in the URL, matching Security: prose elsewhere can link
 * to a specific section, a reload keeps you where you were, and the retired
 * routes have somewhere exact to point.
 */
export function PanelTabs({ tabs, ariaLabel }: { tabs: ReadonlyArray<PanelTab>; ariaLabel: string }) {
  const [searchParams, setSearchParams] = useSearchParams();
  const active = resolvePanelTab(searchParams.get("tab"), tabs);

  function select(id: string) {
    const next = new URLSearchParams(searchParams);
    next.set("tab", id);
    // Replace, not push: switching tabs is not navigation, and pushing would
    // mean Back walks every tab visited instead of leaving the panel.
    setSearchParams(next, { replace: true });
  }

  // Base UI Tabs owns the keyboard: arrows move focus along the strip,
  // Enter or Space opens a tab (manual activation), and each panel is
  // labelled by its tab. Only the open panel is mounted, as before.
  return (
    <Tabs.Root value={active} onValueChange={(id) => select(String(id))}>
      <Tabs.List className="config-tabs" aria-label={ariaLabel}>
        {tabs.map((tab) => (
          <Tabs.Tab key={tab.id} value={tab.id} className={`config-tab${active === tab.id ? " active" : ""}`}>
            {tab.label}
          </Tabs.Tab>
        ))}
      </Tabs.List>
      {tabs.map((tab) => (
        <Tabs.Panel key={tab.id} value={tab.id}>
          {tab.body}
        </Tabs.Panel>
      ))}
    </Tabs.Root>
  );
}
