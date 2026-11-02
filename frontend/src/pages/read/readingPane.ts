import { useSyncExternalStore } from "react";

// Where an open message shows on wide screens; saved per browser. Narrower screens
// always use the pop-up reader.
export type ReadingPane = "right" | "below" | "off";

export const READING_PANE_STORAGE_KEY = "kypost-reading-pane";

export const READING_PANE_CHOICES: ReadonlyArray<{ value: ReadingPane; label: string }> = [
  { value: "right", label: "Right of list" },
  { value: "below", label: "Below list" },
  { value: "off", label: "Off" }
];

export function readReadingPane(): ReadingPane {
  try {
    const saved = window.localStorage.getItem(READING_PANE_STORAGE_KEY);
    return saved === "below" || saved === "off" ? saved : "right";
  } catch {
    return "right";
  }
}

// `storage` fires only in other tabs, so a save also tells this tab.
const CHANGE_EVENT = "kypost-reading-pane-change";

export function saveReadingPane(pane: ReadingPane) {
  try {
    window.localStorage.setItem(READING_PANE_STORAGE_KEY, pane);
  } catch {
    // Storage off: the choice is not kept, and an open inbox keeps its layout.
  }
  window.dispatchEvent(new Event(CHANGE_EVENT));
}

function subscribe(onChange: () => void) {
  const onStorage = (event: StorageEvent) => {
    if (event.key === READING_PANE_STORAGE_KEY || event.key === null) onChange();
  };
  window.addEventListener(CHANGE_EVENT, onChange);
  window.addEventListener("storage", onStorage);
  return () => {
    window.removeEventListener(CHANGE_EVENT, onChange);
    window.removeEventListener("storage", onStorage);
  };
}

/** The saved choice, following changes from Settings in this tab and others. */
export function useReadingPane(): ReadingPane {
  return useSyncExternalStore(subscribe, readReadingPane);
}
