// KyPost invitation card: updates, cancellation, Join meeting. Boot as kypost-440-signatures.mjs.
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
const dir = await tempDir('post-invite');
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
  const open = async (page, subject) => {
    await page.goto(`${ORIGIN}/read`);
    await page.locator('.inbox-unit', { hasText: subject }).first().click();
    await page.locator('.invite-card').waitFor({ timeout: 60000 });
    await settle(page, 800);
  };
  for (const scheme of ['light', 'dark']) {
    for (const [size, viewport] of [['desktop', DESKTOP], ['phone', MOBILE]]) {
      const ctx = await context(scheme, viewport);
      const page = await ctx.newPage();
      await page.clock.setFixedTime(NOW);
      await login(page, USER, pws.alex);
      await page.waitForURL('**/read', { timeout: 60000 });
      await page.goto(`${ORIGIN}/settings/appearance`);
      await page.getByRole('group', { name: 'Busnes' }).getByRole('radio', { name: scheme === 'dark' ? 'Dark' : 'Light' }).check();
      await settle(page);
      // Answer the first revision through the real fallback (KyCalendar unpaired).
      await open(page, /^(?![\s\S]*Updated)[\s\S]*Invitation: Q4 planning review/);
      await page.locator('.invite-card').getByRole('button', { name: 'Accept' }).click();
      await page.locator('.invite-card').getByRole('button', { name: 'Send answer' }).click();
      await page.locator('.invite-card').getByText(/Your answer was sent|ordinary mail/).waitFor({ timeout: 30000 });
      await open(page, 'Updated invitation: Q4 planning review');
      results.push({ scheme, size, view: 'updated', text: (await page.locator('.invite-card').innerText()).replace(/\s+/g, ' '),
        join: await page.locator('.invite-card a').evaluateAll((as) => as.map((a) => [a.href, a.target, a.rel])),
        overflow: await page.evaluate(() => document.documentElement.scrollWidth - innerWidth) });
      await shot(page, `${PREFIX}-1-updated-${size}-${scheme}`);
      await open(page, 'Cancelled: Q4 planning review');
      results.push({ scheme, size, view: 'cancelled', text: (await page.locator('.invite-card').innerText()).replace(/\s+/g, ' '),
        buttons: await page.locator('.invite-card button').count(),
        overflow: await page.evaluate(() => document.documentElement.scrollWidth - innerWidth) });
      await shot(page, `${PREFIX}-2-cancelled-${size}-${scheme}`);
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
