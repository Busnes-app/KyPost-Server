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

export function saveReadingPane(pane: ReadingPane) {
  try {
    window.localStorage.setItem(READING_PANE_STORAGE_KEY, pane);
  } catch {
    // Storage off: the choice lasts until the page reloads.
  }
}
