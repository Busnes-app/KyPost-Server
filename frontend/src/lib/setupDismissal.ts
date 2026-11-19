// "Not now" on an optional encryption setup offer, remembered per browser.
const key = (userId: string) => `kypost-encryption-setup-dismissed:${userId}`;

export function setupDismissed(userId: string): boolean {
  try {
    return localStorage.getItem(key(userId)) === "1";
  } catch {
    return false;
  }
}

export function dismissSetup(userId: string): void {
  try {
    localStorage.setItem(key(userId), "1");
  } catch {
    // Storage blocked: the offer comes back next sign-in, which is harmless.
  }
}
