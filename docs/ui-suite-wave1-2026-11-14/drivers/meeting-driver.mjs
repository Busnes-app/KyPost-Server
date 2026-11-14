// KyPost New meeting / Reply with meeting buttons. Boot as kypost-440-signatures.mjs.
// Usage: node invite-driver.mjs <binary> <frontend dist> <fixture worktree> <out dir> <prefix>
import { mkdir, readFile, readdir, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { homedir } from 'node:os';
const lib = await import(join(homedir(), 'git/ky-redesign/captures/drivers/lib.mjs'));
const { NOW, DESKTOP, MOBILE, secret, tempDir, pw, waitReady, startServer, settle } = lib;

const { chromium } = await pw('KyCalendar/web');
const PORT = '5871', IMAP = '5873', SMTP = '5874', HTTPS = '5875';
const ORIGIN = `http://localhost:${PORT}`;
const [binary, frontend, fixtureDir, OUT] = process.argv.slice(2, 6).map((p) => resolve(p ?? ''));
const PREFIX = process.argv[6];
await mkdir(OUT, { recursive: true });
const shot = async (page, file) => { await page.screenshot({ path: join(OUT, `${file}.png`) }); console.log(file); };
const dir = await tempDir('post-meeting');
const d = (...p) => join(dir, ...p);
for (const p of ['config', 'state', 'logs', 'secrets', 'keys', 'public']) await mkdir(d(p), { recursive: true });
const USER = 'alex';
const pws = { admin: secret(), alex: secret() };
const results = [];
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
    await ctx.close();
  }
  const SUITE = { apps: [
    { id: 'kypost', name: 'KyPost', url: 'https://mail.kypost-demo.local/' },
    { id: 'kycalendar', name: 'KyCalendar', url: 'https://calendar.kypost-demo.local/' },
  ] };
  {
    // One synthetic contact through the normal form.
    const ctx = await context('light');
    const page = await ctx.newPage();
    await login(page, USER, pws.alex);
    await page.waitForURL('**/read', { timeout: 60000 });
    await page.goto(`${ORIGIN}/contacts`);
    await page.getByRole('button', { name: 'New contact' }).click();
    const form = page.getByRole('form', { name: 'Add contact' });
    await form.getByLabel('Full Name').fill('Dana Ops');
    await form.getByRole('button', { name: '+ Add email' }).click();
    await form.getByPlaceholder('email@example.com').first().fill('dana@kypost-demo.local');
    await form.getByRole('button', { name: 'Add contact' }).click();
    await page.getByRole('button', { name: 'Actions for Dana Ops' }).waitFor({ timeout: 30000 });
    await ctx.close();
  }
  for (const scheme of ['light', 'dark']) {
    for (const [size, viewport] of [['desktop', DESKTOP], ['phone', MOBILE]]) {
      const ctx = await context(scheme, viewport);
      // The suite list is stubbed in the browser: no KyIdentity in this scratch setup.
      await ctx.route('**/api/suite/apps', (route) => route.fulfill({ json: SUITE }));
      await ctx.addInitScript(() => { window.__opened = []; window.open = (...a) => { window.__opened.push(a); return null; }; });
      const page = await ctx.newPage();
      await page.clock.setFixedTime(NOW);
      await login(page, USER, pws.alex);
      await page.waitForURL('**/read', { timeout: 60000 });
      await page.goto(`${ORIGIN}/settings/appearance`);
      await page.getByRole('group', { name: 'Busnes' }).getByRole('radio', { name: scheme === 'dark' ? 'Dark' : 'Light' }).check();
      await settle(page);
      const overflow = () => page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
      const press = async (name) => {
        const b = page.getByRole('button', { name, exact: true }).first();
        if (!(await b.count())) return null;
        await b.click();
        return page.evaluate(() => window.__opened.pop()?.join(' | ') ?? null);
      };

      await page.goto(`${ORIGIN}/read`);
      await page.locator('.inbox-unit', { hasText: 'Welcome to the KyPost sandbox' }).first().click();
      await page.locator('.email-reader-window').waitFor({ timeout: 60000 });
      await settle(page, 800);
      await shot(page, `${PREFIX}-1-reader-${size}-${scheme}`);
      results.push({ scheme, size, view: 'reader', opened: await press('Reply with meeting'), overflow: await overflow() });

      await page.goto(`${ORIGIN}/read`);
      await page.getByRole('button', { name: 'New email' }).first().click();
      await page.locator('.ql-editor').waitFor({ timeout: 30000 });
      await page.getByLabel('To recipients', { exact: true }).fill('bob@kypost-demo.local');
      await page.getByLabel('To recipients', { exact: true }).press('Enter');
      await page.getByLabel('Cc recipients', { exact: true }).fill('dana@kypost-demo.local');
      await page.getByLabel('Cc recipients', { exact: true }).press('Enter');
      await page.getByPlaceholder('Subject').fill('Budget review');
      await page.locator('.compose-actionbar').scrollIntoViewIfNeeded();
      await settle(page, 800);
      await shot(page, `${PREFIX}-2-compose-${size}-${scheme}`);
      results.push({ scheme, size, view: 'compose', opened: await press('New meeting'), overflow: await overflow() });
      await page.getByRole('button', { name: 'Close' }).first().click().catch(() => {});

      await page.goto(`${ORIGIN}/contacts`);
      await page.getByRole('button', { name: 'Actions for Dana Ops' }).click();
      await settle(page, 600);
      await shot(page, `${PREFIX}-3-contacts-${size}-${scheme}`);
      results.push({ scheme, size, view: 'contacts', opened: await press('New meeting'), overflow: await overflow() });
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
