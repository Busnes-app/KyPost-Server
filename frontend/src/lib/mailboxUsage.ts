// Mailbox storage against its quota. Pure, so the thresholds are testable.

export type UsageLevel = "ok" | "warning" | "critical";

/** 80% warns, 95% is critical: a full mailbox refuses new mail (senders retry). */
export function usageLevel(used: number, quota: number): UsageLevel {
  if (quota <= 0 || used >= quota * 0.95) return "critical";
  return used >= quota * 0.8 ? "warning" : "ok";
}

/** Binary units, matching the server's 5 GiB = 5 × 2^30 bytes. */
export function formatStorage(bytes: number): string {
  for (const [unit, size] of [["GiB", 2 ** 30], ["MiB", 2 ** 20], ["KiB", 2 ** 10]] as const) {
    if (bytes >= size) return `${(bytes / size).toFixed(1).replace(/\.0$/, "")} ${unit}`;
  }
  return `${bytes} B`;
}

export function usageText(used: number, quota: number): string {
  const percent = quota > 0 ? Math.floor((used * 100) / quota) : 100;
  return `${formatStorage(used)} of ${formatStorage(quota)} used (${percent}%)`;
}

/** The sentence a warning or critical level shows; empty when ok. */
export function usageNotice(used: number, quota: number): string {
  switch (usageLevel(used, quota)) {
    case "critical":
      return used >= quota
        ? "This mailbox is full. New mail is refused for now and senders retry for a few days: delete or export mail you no longer need to receive it."
        : "This mailbox is almost full. Once it is, new mail is refused for now and senders retry for a few days: delete or export mail you no longer need.";
    case "warning":
      return "This mailbox is over 80% full. Delete or export mail you no longer need before it fills.";
    default:
      return "";
  }
}
