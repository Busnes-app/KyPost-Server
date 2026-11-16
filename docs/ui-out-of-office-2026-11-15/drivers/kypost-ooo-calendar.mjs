// KyPost out-of-office KyCalendar boxes (worker f-ooo, KyPost-Server feat/out-of-office-kycalendar).
// A scratch server cannot be paired with KyCalendar for an SSO-linked user, so every
// /api/mail/out-of-office answer has only calendarAvailable rewritten to true (no other mock).
// Settings › Mail › Out of office only; the inbox bar and admin switch are unchanged.
// Same boot as
// kypost-440-signatures.mjs: a disposable KyPost server, real sign-in, the kypost-demo-IMAP fixture.
// Before (origin/main frontend): Settings › Mail has no Out of office tab, the inbox has no bar,
// Admin › Application has no switch. After: the admin allows outside replies, the user saves a
// reply through the real UI and server; the tab, the inbox bar and the switch.
// Desktop 1440×900 and phone 390×844, Busnes Light and Dark.
// Usage: node kypost-ooo.mjs <binary> <frontend dist> <fixture worktree> <out dir> <prefix>
import { mkdir, readFile, readdir, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { NOW, DESKTOP, MOBILE, secret, tempDir, pw, waitReady, startServer, settle } from './lib.mjs';

const { chromium } = await pw('KyCalendar/web');
const PORT = '5961', IMAP = '5963', SMTP = '5964', HTTPS = '5965';
const ORIGIN = `http://localhost:${PORT}`;
const [binary, frontend, fixtureDir, OUT] = process.argv.slice(2, 6).map((p) => resolve(p ?? ''));
const PREFIX = process.argv[6];
if (!PREFIX) throw new Error('usage: kypost-ooo.mjs <binary> <dist> <fixture> <out> <prefix>');
const AFTER = PREFIX.includes('after');
await mkdir(OUT, { recursive: true });
const results = [];
const shot = async (page, file, opts = {}) => {
  await page.screenshot({ path: join(OUT, `${file}.png`), ...opts });
  results.push({ file, overflow: await page.evaluate(() => document.documentElement.scrollWidth - innerWidth) });
  console.log(file);
};
const dir = await tempDir('post-ooo');
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
  const context = async (scheme, viewport = DESKTOP) => {
    const ctx = await browser.newContext({ viewport, colorScheme: scheme, timezoneId: 'America/New_York', locale: 'en-US' });
    await ctx.route('**/api/mail/out-of-office', async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      await route.fulfill({ response: res, json: { ...body, calendarAvailable: true } });
    });
    return ctx;
  };
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
    {
      await page.goto(`${ORIGIN}/admin/server`);
      await page.getByRole('checkbox', { name: 'Allow out-of-office replies to outside senders' }).check();
      await page.getByRole('button', { name: 'Save Configuration' }).click();
      await page.getByText('Configuration saved.').waitFor({ timeout: 30000 });
    }
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

    {
      await page.goto(`${ORIGIN}/settings/mail?tab=out-of-office`);
      await page.getByRole('checkbox', { name: 'Send automatic replies' }).check();
      await page.getByLabel('Message for people in your organisation').fill('I am away until Monday 30 November.\nFor anything urgent, ask Sam Lee in Operations.');
      await page.getByLabel('People outside your organisation').selectOption('contacts');
      await page.getByLabel('Message for people outside your organisation').fill('Thank you for your message. I am away until 30 November and will reply when I am back.');
      await page.getByLabel('Start (optional)').fill('2026-11-23T00:00');
      await page.getByLabel('End (optional)').fill('2026-11-30T00:00');
      if (AFTER) {
        await page.getByRole('checkbox', { name: /Show me as away in KyCalendar/ }).check();
        await page.getByRole('checkbox', { name: /Answer new invitations Tentative/ }).check();
      }
      await page.getByRole('button', { name: 'Save automatic reply' }).click();
      await page.getByText('Automatic reply saved.').waitFor({ timeout: 30000 });
    }
    await ctx.close();
  }

  for (const scheme of ['light', 'dark']) {
    for (const [size, viewport] of [['desktop', DESKTOP], ['phone', MOBILE]]) {
      const tag = `${size}-${scheme}`;
      const ctx = await context(scheme, viewport);
      const page = await ctx.newPage();
      await page.clock.setFixedTime(NOW);
      await login(page, USER, pws.alex);
      await page.waitForURL('**/read', { timeout: 60000 });
      await page.goto(`${ORIGIN}/settings/appearance`);
      await page.getByRole('group', { name: 'Busnes' }).getByRole('radio', { name: scheme === 'dark' ? 'Dark' : 'Light' }).check();
      await settle(page);
      await page.goto(`${ORIGIN}/settings/mail?tab=out-of-office`);
      await page.getByRole('button', { name: 'Save automatic reply' }).waitFor({ timeout: 30000 });
      await settle(page, 1000);
      await shot(page, `${PREFIX}-settings-${tag}`, { fullPage: true });
      await ctx.close();
    }
  }
  await browser.close();
} finally {
  await srv?.stop();
  await fixture?.stop();
  await rm(dir, { recursive: true, force: true });
}
for (const r of results) console.log(JSON.stringify(r));
