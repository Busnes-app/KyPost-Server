// Display formatting for the read view. Pure — no React, no I/O — so it is
// unit-testable without rendering the page.

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function formatTimestamp(value: string): string {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

export function formatInboxListTime(value: string): string {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;

  const now = new Date();
  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const emailStart = new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const diffDays = Math.floor((todayStart.getTime() - emailStart.getTime()) / 86_400_000);

  if (diffDays === 0) {
    return date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }
  if (diffDays === 1) {
    return "Yesterday";
  }
  if (diffDays > 1 && diffDays <= 6) {
    return date.toLocaleDateString([], { weekday: "short" });
  }
  return date.toLocaleDateString([], date.getFullYear() === now.getFullYear()
    ? { month: "short", day: "numeric" }
    : { month: "short", day: "numeric", year: "numeric" });
}

/** The reader's sent time beside the sender: "Aug 10, 2026, 9:02 AM". */
export function formatReaderTime(value: string): string {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return value || "";
  return date.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
}

/** The display name from `Name <address>`, else the bare address. */
export function senderName(sender: string): string {
  const named = sender.match(/^\s*"?([^"<]*?)"?\s*<[^>]*>\s*$/)?.[1]?.trim();
  return named || sender.replace(/[<>]/g, "").trim() || "-";
}

export function formatUpdatedLabel(lastLoadedAt: Date | null, now: number): string {
  if (!lastLoadedAt) return "Not updated yet";
  const elapsedMs = now - lastLoadedAt.getTime();
  if (elapsedMs < 3 * 60 * 1000) {
    return "Updated just now";
  }
  return `Updated ${lastLoadedAt.toLocaleTimeString([], {
    hour: "numeric",
    minute: "2-digit"
  })}`;
}

