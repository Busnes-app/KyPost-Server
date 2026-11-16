// KyPost Settings › Mail › KyPulse (worker f-pulse, KyPost-Server feat/kypulse-alert-mail).
// Adapted from ky-redesign captures/drivers/kypost-ooo.mjs: a disposable KyPost server, real
// sign-in, the kypost-demo-IMAP fixture as IMAP/SMTP sink. Nothing leaves the host.
// Before (origin/main frontend): Settings › Mail has no KyPulse tab. After: the KyPulse tab with
// the user's own address and one extra address added through the real UI (Pending).
// KyPulse is not paired here, so the section shows its "not connected" note.
// Desktop 1440×900 and phone 390×844, Busnes Light and Dark. Hold /tmp/ky-integ-test.lock.
// Usage: node capture.mjs <binary> <frontend dist> <fixture worktree> <out dir> <before|after>
import { mkdir, readFile, readdir, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { homedir } from 'node:os';
const { DESKTOP, MOBILE, secret, tempDir, pw, waitReady, startServer, settle } = await import(join(homedir(), 'git/ky-redesign/captures/drivers/lib.mjs'));

const { chromium } = await pw('KyCalendar/web');
const PORT = '5981', IMAP = '5983', SMTP = '5984', HTTPS = '5985';
const ORIGIN = `http://localhost:${PORT}`;
const [binary, frontend, fixtureDir, OUT] = process.argv.slice(2, 6).map((p) => resolve(p ?? ''));
const PREFIX = process.argv[6];
if (!PREFIX) throw new Error('usage: capture.mjs <binary> <dist> <fixture> <out> <prefix>');
const AFTER = PREFIX.includes('after');
await mkdir(OUT, { recursive: true });
const results = [];
const shot = async (page, file, opts = {}) => {
  await page.screenshot({ path: join(OUT, `${file}.png`), ...opts });
  results.push({ file, overflow: await page.evaluate(() => document.documentElement.scrollWidth - innerWidth) });
  console.log(file);
};
const dir = await tempDir('post-kypulse');
const d = (...p) => join(dir, ...p);
for (const p of ['config', 'state', 'logs', 'secrets', 'keys', 'public']) await mkdir(d(p), { recursive: true });
const USER = 'alex';
const pws = { admin: secret(), alex: secret() };
let fixture, srv;
try {
  fixture = startServer(process.execPath, ['src/index.js'], {
    cwd: fixtureDir,
    env: { PATH: process.env.PATH, BIND_ADDRESS: '127.0.0.1', IMAP_PORT: IMAP, SMTP_PORT: SMTP, HTTPS_PORT: HTTPS,
      ALLOWED_CLIENT_IPS: '127.0.0.1,::1', TLS_KEY_DIR: d('keys'), TLS_PUBLISH_DIR: d('public') },
  });
  for (let i = 0; i < 80 && !(await readdir(d('public'))).length; i++) await new Promise((r) => setTimeout(r, 250));
  srv = startServer(binary, [], {
    cwd: dir,
    env: { PATH: process.env.PATH, HOME: dir, CONFIG_DIR: d('config'), STATE_DIR: d('state'), LOG_DIR: d('logs'), SECRET_DIR: d('secrets'),
      FRONTEND_DIR: frontend, WEB_PORT: PORT, SSL_CERT_DIR: d('public'), SANDBOX_PRIVATE_HOSTS: 'localhost', KYPOST_DISABLE_AI_SORTING: 'true',
      PUSH_RELAY_URL: '', APNS_RELAY_URL: '' },
  });
  await waitReady(`${ORIGIN}/api/health`, 90000);
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
    await create.getByLabel('Username').fill(USER);
    const temp = secret();
    await create.getByLabel('Temporary Password').fill(temp);
    await create.getByRole('button', { name: 'Create User' }).click();
    await page.locator('.users-list-card').getByText(USER, { exact: true }).first().waitFor();
    await ctx.clearCookies();
    await login(page, USER, temp);
    await changePassword(page, pws.alex);
    await page.goto(`${ORIGIN}/settings/mail`);
    const card = page.locator('.config-card').first();
    const field = (label) => card.locator('label', { has: page.locator(`div:text-is("${label}")`) }).locator('input');
    await field('Host').fill('localhost');
    await field('Port').fill(IMAP);
    await field('Username').fill('alice@kypost-demo.local');
    await field('Password or App Password').fill(secret());
    await field('Mailbox').fill('INBOX');
    await field('SMTP Host (optional)').fill('localhost');
    await field('SMTP Port (optional)').fill(SMTP);
    await card.getByRole('button', { name: 'Save', exact: true }).click();
    await card.getByText('IMAP configuration saved.').waitFor({ timeout: 30000 });

    if (AFTER) {
      await page.goto(`${ORIGIN}/settings/mail?tab=kypulse`);
      await page.getByLabel('Add an address').fill('oncall@kypost-demo.local');
      await page.getByRole('button', { name: 'Send confirmation link' }).click();
      await page.getByText('A confirmation link was sent to').waitFor({ timeout: 30000 });
    }
    await ctx.close();
  }

  for (const scheme of ['light', 'dark']) {
    for (const [size, viewport] of [['desktop', DESKTOP], ['phone', MOBILE]]) {
      const tag = `${size}-${scheme}`;
      const open = async (user, password) => {
        const ctx = await context(scheme, viewport);
        const page = await ctx.newPage();
                await login(page, user, password);
        await page.waitForURL('**/read', { timeout: 60000 });
        await page.goto(`${ORIGIN}/settings/appearance`);
        await page.getByRole('group', { name: 'Busnes' }).getByRole('radio', { name: scheme === 'dark' ? 'Dark' : 'Light' }).check();
        await settle(page);
        return { ctx, page };
      };
      {
        const { ctx, page } = await open(USER, pws.alex);
        await page.goto(`${ORIGIN}/settings/mail${AFTER ? '?tab=kypulse' : ''}`);
        await settle(page, 1000);
        await shot(page, `${PREFIX}-settings-mail-${tag}`, { fullPage: true });
        await ctx.close();
      }
    }
  }
  await browser.close();
} finally {
  await srv?.stop();
  await fixture?.stop();
  await rm(dir, { recursive: true, force: true });
}
for (const r of results) console.log(JSON.stringify(r));
