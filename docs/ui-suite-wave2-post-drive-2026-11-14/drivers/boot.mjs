// Shared boot for the w2-post-drive capture drivers (same steps as wave 1's kypost-kydrive-links.mjs):
// a disposable KyPost server, the kypost-demo-IMAP fixture, real admin bootstrap, a real everyday user
// "charlie" with IMAP/SMTP pointed at the fixture. KyPost is paired with KyDrive by the environment
// (https://drive.example.test, throwaway secret; nothing listens there), so the real server sends the
// paired CSP and COOP. Push relays are disabled.
import { mkdir, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { homedir } from 'node:os';
const lib = await import(join(homedir(), 'git/ky-redesign/captures/drivers/lib.mjs'));
export const { NOW, DESKTOP, MOBILE, secret, settle } = lib;
export const DRIVE = 'https://drive.example.test';
export const VIEWS = [['desktop', lib.DESKTOP], ['phone', lib.MOBILE]];

export async function boot({ binary, frontend, fixtureDir, port }) {
  const { chromium } = await lib.pw('KyCalendar/web');
  const [PORT, IMAP, SMTP, HTTPS] = [port, port + 2, port + 3, port + 4].map(String);
  const ORIGIN = `http://localhost:${PORT}`;
  const dir = await lib.tempDir('post-w2');
  const d = (...p) => join(dir, ...p);
  for (const p of ['config', 'state', 'logs', 'secrets', 'keys', 'public']) await mkdir(d(p), { recursive: true });
  await writeFile(d('secrets', 'kydrive'), secret() + secret(), { mode: 0o600 });
  const pws = { admin: secret(), user: secret() };
  const fixture = lib.startServer(process.execPath, ['src/index.js'], {
    cwd: fixtureDir,
    env: { PATH: process.env.PATH, BIND_ADDRESS: '127.0.0.1', IMAP_PORT: IMAP, SMTP_PORT: SMTP, HTTPS_PORT: HTTPS,
      ALLOWED_CLIENT_IPS: '127.0.0.1,::1', TLS_KEY_DIR: d('keys'), TLS_PUBLISH_DIR: d('public') },
  });
  for (let i = 0; i < 80 && !(await readdir(d('public'))).length; i++) await new Promise((r) => setTimeout(r, 250));
  const srv = lib.startServer(binary, [], {
    cwd: dir,
    env: { PATH: process.env.PATH, HOME: dir, CONFIG_DIR: d('config'), STATE_DIR: d('state'), LOG_DIR: d('logs'), SECRET_DIR: d('secrets'),
      FRONTEND_DIR: frontend, WEB_PORT: PORT, SSL_CERT_DIR: d('public'), SANDBOX_PRIVATE_HOSTS: 'localhost', KYPOST_DISABLE_AI_SORTING: 'true',
      PUSH_RELAY_URL: '', APNS_RELAY_URL: '', KYPOST_KYDRIVE_URL: DRIVE, KYPOST_KYDRIVE_SECRET_FILE: d('secrets', 'kydrive') },
  });
  const stop = async () => { await srv?.stop(); await fixture?.stop(); await rm(dir, { recursive: true, force: true }); };
  try {
    await lib.waitReady(`${ORIGIN}/api/health`, 90000);
    const firstRun = d('config', 'first-run-password.txt');
    for (let i = 0; i < 40 && !existsSync(firstRun); i++) await new Promise((r) => setTimeout(r, 250));
    const bootstrap = (await readFile(firstRun, 'utf8')).match(/^password: (.+)$/m)[1];
    await rm(firstRun);
    const browser = await chromium.launch();
    const context = (scheme, viewport = DESKTOP) => browser.newContext({ viewport, colorScheme: scheme, timezoneId: 'America/New_York', locale: 'en-US' });
    const login = async (page, user, password) => {
      await page.goto(`${ORIGIN}/login`);
      await page.getByLabel('Username', { exact: true }).fill(user);
      await page.getByLabel('Password', { exact: true }).fill(password);
      await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    };
    const changePassword = async (page, next) => {
      await page.getByLabel('New password', { exact: true }).fill(next);
      await page.locator('button.auth-submit').click();
      await page.waitForURL('**/read', { timeout: 60000 });
    };
    {
      const ctx = await context('light');
      const page = await ctx.newPage();
      await login(page, 'admin', bootstrap);
      await changePassword(page, pws.admin);
      await page.goto(`${ORIGIN}/admin/server`);
      await page.getByRole('tab', { name: 'Manage Users' }).click();
      await page.getByLabel('Your account password').fill(pws.admin);
      const create = page.locator('form.users-create-card');
      await create.getByLabel('Username').fill('charlie');
      const temp = secret();
      await create.getByLabel('Temporary Password').fill(temp);
      await create.getByRole('button', { name: 'Create User' }).click();
      await page.locator('.users-list-card').getByText('charlie', { exact: true }).first().waitFor();
      await ctx.clearCookies();
      await login(page, 'charlie', temp);
      await changePassword(page, pws.user);
      await page.goto(`${ORIGIN}/settings/mail`);
      const card = page.locator('.config-card').first();
      const field = (label) => card.locator('label', { has: page.locator(`div:text-is("${label}")`) }).locator('input');
      await field('Host').fill('localhost');
      await field('Port').fill(IMAP);
      await field('Username').fill('charlie@kypost-demo.local');
      await field('Password or App Password').fill(secret());
      await field('Mailbox').fill('INBOX');
      await field('SMTP Host (optional)').fill('localhost');
      await field('SMTP Port (optional)').fill(SMTP);
      await card.getByRole('button', { name: 'Save', exact: true }).click();
      await card.getByText('IMAP configuration saved.').waitFor({ timeout: 30000 });
      await ctx.close();
    }
    // A signed-in page for charlie in the given theme, with route(ctx) applied first.
    const open = async (scheme, viewport, route = async () => {}, { signIn = true } = {}) => {
      const ctx = await context(scheme, viewport);
      await route(ctx);
      const page = await ctx.newPage();
      await page.clock.setFixedTime(NOW);
      if (signIn) {
        await login(page, 'charlie', pws.user);
        await page.waitForURL('**/read', { timeout: 60000 });
        await page.goto(`${ORIGIN}/settings/appearance`);
        await page.getByRole('group', { name: 'Busnes' }).getByRole('radio', { name: scheme === 'dark' ? 'Dark' : 'Light' }).check();
        await settle(page);
      }
      return { ctx, page };
    };
    return { ORIGIN, browser, open, login, pws, stop: async () => { await browser.close(); await stop(); } };
  } catch (e) {
    await stop();
    throw e;
  }
}

export function shooter(OUT, results) {
  return async (page, file) => {
    await page.screenshot({ path: join(OUT, `${file}.png`) });
    results.push({ file, overflow: await page.evaluate(() => document.documentElement.scrollWidth - innerWidth) });
    console.log(file);
  };
}
