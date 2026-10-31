import { useLayoutEffect, useRef, useState, type KeyboardEvent, type PointerEvent } from "react";

// Right of list: the list's share of the width; below list: its share of the height.
export const SPLIT_AXES = {
  right: { key: "kypost-read-list-width", initial: 52, min: 30, max: 65, orientation: "vertical", less: "ArrowLeft", more: "ArrowRight" },
  below: { key: "kypost-read-list-height", initial: 45, min: 20, max: 75, orientation: "horizontal", less: "ArrowUp", more: "ArrowDown" }
} as const;
const STEP = 2;

function saveSize(key: string, n: number) {
  try {
    window.localStorage.setItem(key, String(n));
  } catch {
    // Storage off: the size lasts for this page only.
  }
}

/** The handle between the message list and the reading pane (wide screens only). The
 *  list's share of the layout (the handle's parent) is a percentage, saved per browser;
 *  CSS keeps both sides above their minimum sizes. */
export function ListSplitter({ pane }: { pane: keyof typeof SPLIT_AXES }) {
  const handleRef = useRef<HTMLDivElement>(null);
  const axis = SPLIT_AXES[pane];
  const clamp = (n: number) => Math.min(axis.max, Math.max(axis.min, Math.round(n)));
  const [percent, setPercent] = useState(() => {
    try {
      const n = Number(window.localStorage.getItem(axis.key));
      return n ? clamp(n) : axis.initial;
    } catch {
      return axis.initial;
    }
  });

  useLayoutEffect(() => {
    handleRef.current?.parentElement?.style.setProperty("--read-list-size", `${percent}%`);
  }, [percent]);

  function resize(n: number) {
    setPercent(clamp(n));
    saveSize(axis.key, clamp(n));
  }

  function onPointerMove(event: PointerEvent<HTMLDivElement>) {
    if (!event.currentTarget.hasPointerCapture(event.pointerId)) return;
    const box = event.currentTarget.parentElement?.getBoundingClientRect();
    if (!box?.width || !box.height) return;
    resize(axis.orientation === "vertical" ? ((event.clientX - box.left) / box.width) * 100 : ((event.clientY - box.top) / box.height) * 100);
  }

  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    const next = { [axis.less]: percent - STEP, [axis.more]: percent + STEP, Home: axis.min, End: axis.max }[event.key];
    if (next === undefined) return;
    event.preventDefault();
    resize(next);
  }

  return (
    <div
      ref={handleRef}
      className="read-split"
      role="separator"
      aria-orientation={axis.orientation}
      aria-label="Resize message list"
      aria-valuemin={axis.min}
      aria-valuemax={axis.max}
      aria-valuenow={percent}
      tabIndex={0}
      onPointerDown={(event) => event.currentTarget.setPointerCapture(event.pointerId)}
      onPointerMove={onPointerMove}
      onKeyDown={onKeyDown}
    />
  );
}
