// Global setup: signs the bootstrap admin in through the real sign-in form
// (proof of work and client-side credential derivation run in the browser),
// rotates the forced first-run password, seeds contacts through the app's own
// API and saves the session for the specs. One sign-in per run.
import { chromium, expect } from '@playwright/test';

export const PORT = 5391;
export const ADMIN = { user: 'admin', initial: 'BrowserInitial-123!', password: 'BrowserUpdated-456!' };
export const STORAGE_STATE = 'test-results/admin-state.json';

// Three share the "al" prefix for the To-field suggestions; the rest push the
// Contacts page past CONTACTS_PER_PAGE (20, pages/contacts/formState.ts).
export const CONTACTS = [
  ['Alice Able', 'alice@example.test'],
  ['Alan Ames', 'alan@example.test'],
  ['Albert Ash', 'albert@example.test'],
  ...Array.from({ length: 22 }, (_, i) => [`Person ${String(i + 1).padStart(2, '0')}`, `person${i + 1}@example.test`]),
];

export async function signIn(page, password) {
  await page.goto('/');
  await page.getByLabel('Username', { exact: true }).fill(ADMIN.user);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
}

export default async function setup(config) {
  const { baseURL } = config.projects[0].use;
  const browser = await chromium.launch();
  try {
    const page = await (await browser.newContext({ baseURL })).newPage();
    await signIn(page, ADMIN.initial);
    await expect(page.getByRole('heading', { name: 'Choose a new password' })).toBeVisible({ timeout: 30000 });
    await page.getByLabel('Current password').fill(ADMIN.initial);
    await page.getByLabel('New password').fill(ADMIN.password);
    await page.getByRole('button', { name: 'Update password' }).click();
    await expect(page.getByRole('heading', { name: 'Choose a new password' })).toHaveCount(0, { timeout: 30000 });

    // A fresh server requires a PGP key before mail opens, and the key needs a
    // mail account. Lower the policy to Optional (Server -> Security), as an
    // administrator would, so the setup step offers "Not now" and no mail
    // account or IMAP fixture is needed.
    await page.goto('/admin/server?tab=security');
    await page.locator('#policy_incoming').selectOption('off');
    await page.locator('#policy_key').selectOption('optional');
    await page.locator('#policy_confirm_password').fill(ADMIN.password);
    await page.getByRole('button', { name: 'Save encryption policy' }).click();
    await expect(page.getByRole('status').filter({ hasText: 'Saved.' })).toBeVisible({ timeout: 30000 });

    // "Not now" is remembered in localStorage, which the saved storage state carries.
    await page.goto('/read');
    const notNow = page.getByRole('button', { name: 'Not now' });
    const newEmail = page.getByRole('button', { name: 'New email' }).first();
    await expect(notNow.or(newEmail).first()).toBeVisible({ timeout: 30000 }).catch(async (e) => { await page.screenshot({ path: 'test-results/setup-fail.png' }); throw e; });
    if (await notNow.isVisible()) await notNow.click();
    await expect(newEmail).toBeVisible({ timeout: 30000 });

    const failed = await page.evaluate(async (contacts) => {
      const csrf = decodeURIComponent(document.cookie.match(/(?:^|; )csrf_token=([^;]*)/)?.[1] ?? '');
      const bad = [];
      for (const [fn, email] of contacts) {
        const r = await fetch('/api/contacts', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
          body: JSON.stringify({ fn, emails: [{ value: email }] }),
        });
        if (!r.ok) bad.push(`${email}: ${r.status} ${await r.text()}`);
      }
      return bad;
    }, CONTACTS);
    expect(failed).toEqual([]);
    await page.context().storageState({ path: STORAGE_STATE });
  } finally {
    await browser.close();
  }
}
