// Playwright helpers for the native-mail harness. Run inside the namespace:
//   scripts/dev/native-mail-harness.sh exec node scripts/dev/native-mail-harness/checks.mjs <step>
// KPH_PLAYWRIGHT names @playwright/test's index.mjs when it is not resolvable.
import { readFileSync, writeFileSync, existsSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { randomBytes } from 'node:crypto';
import { execFileSync } from 'node:child_process';

const H = '/mnt';
export const cfg = JSON.parse(readFileSync(join(H, 'run/harness.json'), 'utf8'));
export const ORIGIN = cfg.origin;
const pw = await import(process.env.KPH_PLAYWRIGHT || '@playwright/test');
const credFile = join(H, 'secrets/ui-creds.json');
export const creds = existsSync(credFile) ? JSON.parse(readFileSync(credFile, 'utf8')) : {};
export const saveCreds = () => writeFileSync(credFile, JSON.stringify(creds), { mode: 0o600 });
export const secretFile = (name) => readFileSync(join(H, 'secrets', name), 'utf8').trim();
export const newSecret = () => `Kph-${randomBytes(18).toString('base64url')}9!`;

export async function launch() {
  // HOME carries the NSS database that trusts the harness CA: no TLS errors are ignored.
  process.env.HOME = cfg.browserHome;
  const browser = await pw.chromium.launch({ env: { ...process.env, HOME: cfg.browserHome } });
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, colorScheme: 'light', locale: 'en-US' });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.log('PAGEERROR', e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.log('CONSOLE', m.text().slice(0, 200)); });
  return { browser, ctx, page };
}

export async function shot(page, name) {
  mkdirSync(cfg.shots, { recursive: true });
  await page.waitForLoadState('networkidle').catch(() => {});
  await page.evaluate(() => document.fonts.ready);
  await page.mouse.move(0, 0);
  await page.waitForTimeout(400);
  await page.screenshot({ path: join(cfg.shots, `${name}.png`), fullPage: true });
  console.log('shot', name);
}

export const text = async (page) => (await page.locator('body').innerText()).replace(/\s+/g, ' ');

export function harness(...args) {
  return execFileSync('bash', [join(import.meta.dirname, '..', 'native-mail-harness.sh'), ...args], { encoding: 'utf8' });
}

// Local KyPost sign-in (the bootstrap/recovery administrator).
export async function localLogin(page, user, password) {
  await page.goto(`${ORIGIN}/login`);
  await page.getByLabel('Username', { exact: true }).fill(user);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
}

export async function adminLogin(page) {
  if (!creds.admin) {
    await localLogin(page, 'admin', secretFile('kypost-bootstrap'));
    await page.getByLabel('New password', { exact: true }).fill((creds.admin = newSecret()));
    await page.locator('button.auth-submit').click();
    saveCreds();
    await page.waitForTimeout(3000);
  } else {
    await localLogin(page, 'admin', creds.admin);
    await page.waitForTimeout(2000);
  }
}

// Native user sign-in: KyPost → "Continue with single sign-on" → KyIdentity.
export async function ssoLogin(page, username = 'owner') {
  await page.goto(`${ORIGIN}/login`);
  await page.getByRole('link', { name: 'Continue with single sign-on' }).click();
  await page.locator('#username').fill(username);
  await page.locator('#password').fill(secretFile(username));
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL((u) => u.origin === ORIGIN && ['/read', '/setup/encryption'].includes(u.pathname));
  await page.waitForLoadState('networkidle');
  // Admin setup made the key Optional, so the setup page offers "Not now".
  const notNow = page.getByRole('button', { name: /Not now/ });
  if (await notNow.isVisible().catch(() => false)) await notNow.click();
  await page.waitForURL((u) => u.pathname === '/read');
}

// Waits for a subject in the inbox list, reloading while the daemon imports it.
export async function openFromInbox(page, subject) {
  for (let i = 0; i < 30; i++) {
    await page.goto(`${ORIGIN}/read`);
    const row = page.getByText(subject).first();
    if (await row.waitFor({ timeout: 4000 }).then(() => true, () => false)) { await row.click(); return; }
    await page.waitForTimeout(2000);
  }
  throw new Error(`"${subject}" never arrived in the inbox`);
}
