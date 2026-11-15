// w2-post-drive screens (KyPost-Server, suite wave 2 contracts I, J, K). Boot in boot.mjs.
// Scenarios: picker (Attach from KyDrive), deeplink (/compose?to=&subject=&body=), guest (Send a guest
// link instead). /api/kydrive/{status,upload-tickets,links,recipients,copies} and the KyDrive origin
// (https://drive.example.test: /pick stand-in, upload PUTs) are Playwright-routed; KyPost's own
// sign-in, step-up and pages are real. No CSP bypass. Desktop 1440x900, phone 390x844, Busnes Light/Dark.
// Usage: node w2-captures.mjs <binary> <frontend dist> <fixture dir> <out dir> <prefix> <scenario> <port>
import { mkdir } from 'node:fs/promises';
import { resolve } from 'node:path';
import { boot, shooter, settle, VIEWS, DRIVE } from './boot.mjs';

const [binary, frontend, fixtureDir, OUT] = process.argv.slice(2, 6).map((p) => resolve(p ?? ''));
const [PREFIX, SCENARIO, PORT] = process.argv.slice(6, 9);
if (!PORT) throw new Error('usage: w2-captures.mjs <binary> <dist> <fixture> <out> <prefix> <picker|deeplink|guest> <port>');
const AFTER = PREFIX.includes('after');
await mkdir(OUT, { recursive: true });
const results = [];
const shot = shooter(OUT, results);
const json = (route, status, body) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
const status = (extra = {}) => (r) => json(r, 200, { available: true, freeBytes: 40e9, maxFileBytes: 5e9, appUrl: `${DRIVE}/`, origin: DRIVE, ...extra });

const PICKER = `<!doctype html><meta charset="utf-8"><title>KyDrive picker (stand-in)</title>
<body style="font:16px system-ui;padding:24px"><p>Stand-in for KyDrive's /pick.</p><p id="o"></p><button id="go">Confirm</button>
<script>
const q = new URLSearchParams(location.search), mode = q.get('mode'), origin = q.get('origin');
document.getElementById('o').textContent = 'opener: ' + (window.opener ? 'present' : 'severed');
document.getElementById('go').onclick = () => {
  const files = mode === 'copy' ? [{ name: 'Budget 2027.xlsx', size: 48213, file_id: 'f-budget' }]
    : [{ name: 'Brand guidelines.pdf', size: 2621440, url: '${DRIVE}/l/#fixture-brand', expires: '2026-12-14T15:00:00Z' },
       { name: 'Logo pack.zip', size: 9437184, url: '${DRIVE}/l/#fixture-logo', expires: '2026-12-14T15:00:00Z' }];
  window.opener.postMessage({ type: 'kydrive.picked', files }, origin);
  window.close();
};
</script>`;

const env = await boot({ binary, frontend, fixtureDir, port: Number(PORT) });
const { ORIGIN, open, login, pws } = env;
const compose = async (page) => {
  await page.goto(`${ORIGIN}/read`);
  await page.getByRole('button', { name: 'New email' }).first().click();
  await page.locator('.ql-editor').waitFor({ timeout: 30000 });
  await settle(page, 600);
};
try {
  results.push({ coop: (await fetch(`${ORIGIN}/api/health`)).headers.get('cross-origin-opener-policy') });
  for (const scheme of ['light', 'dark']) {
    for (const [size, viewport] of VIEWS) {
      const tag = `${size}-${scheme}`;
      if (SCENARIO === 'picker') {
        const { ctx, page } = await open(scheme, viewport, async (c) => {
          await c.route('**/api/kydrive/status', status());
          await c.route('**/api/kydrive/copies', (r) => json(r, 200, { name: 'Budget 2027.xlsx', mimeType: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', size: 48213, dataBase64: Buffer.alloc(48213, 1).toString('base64') }));
          await c.route(`${DRIVE}/pick?*`, (r) => r.fulfill({ status: 200, contentType: 'text/html', body: PICKER }));
        });
        await compose(page);
        await page.getByPlaceholder('Subject').fill('Launch kit');
        await page.locator('.ql-editor').click();
        await page.keyboard.type('Everything for the launch: ');
        await page.locator('.compose-actionbar').scrollIntoViewIfNeeded();
        await settle(page, 600);
        await shot(page, `${PREFIX}-9-compose-actions-${tag}`);
        if (AFTER) {
          const menu = page.getByRole('button', { name: 'Attach from KyDrive' });
          await menu.click();
          await page.getByRole('menuitem', { name: 'Share a link' }).waitFor();
          await settle(page, 300);
          await shot(page, `${PREFIX}-10-compose-drive-menu-${tag}`);
          const pick = async (item) => {
            const [popup] = await Promise.all([page.waitForEvent('popup'), page.getByRole('menuitem', { name: item }).click()]);
            await popup.waitForLoadState();
            results.push({ tag, item, opener: await popup.locator('#o').textContent() });
            await popup.locator('#go').click();
          };
          await pick('Share a link');
          await page.locator('.ql-editor a', { hasText: 'Logo pack.zip' }).waitFor();
          await settle(page, 300);
          await shot(page, `${PREFIX}-11-compose-picked-links-${tag}`);
          await menu.click();
          await pick('Attach a copy');
          await page.locator('.compose-attachment-chip', { hasText: 'Budget 2027.xlsx' }).waitFor();
          await page.locator('.compose-attachments').scrollIntoViewIfNeeded();
          await settle(page, 300);
          await shot(page, `${PREFIX}-12-compose-picked-copy-${tag}`);
        }
        await ctx.close();
      }

      if (SCENARIO === 'deeplink') {
        const link = `${ORIGIN}/compose?` + new URLSearchParams({
          to: 'dana@partner.example,not an address,ann@kypost-demo.local',
          subject: 'Q3 board deck.pdf',
          body: `Here is the file: ${DRIVE}/l/#fixture-deck\n<b>not bold</b>`,
        });
        // Signed in: the link opens a prefilled draft.
        {
          const { ctx, page } = await open(scheme, viewport);
          await page.goto(link);
          await settle(page, 1500);
          if (AFTER) await page.locator('.ql-editor a').first().waitFor({ timeout: 30000 });
          await settle(page, 600);
          await shot(page, `${PREFIX}-13-deeplink-${tag}`);
          results.push({ tag, url: page.url(), body: AFTER ? await page.locator('.ql-editor').innerHTML() : '' });
          await ctx.close();
        }
        // Signed out: through the password sign-in, then the draft.
        if (AFTER) {
          const { ctx, page } = await open(scheme, viewport, undefined, { signIn: false });
          await page.goto(link);
          await page.waitForURL('**/login');
          await login(page, 'charlie', pws.user);
          await page.locator('.ql-editor a').first().waitFor({ timeout: 60000 });
          await settle(page, 600);
          await shot(page, `${PREFIX}-14-deeplink-after-signin-${tag}`);
          results.push({ tag, signin: page.url(), subject: await page.getByPlaceholder('Subject').inputValue() });
          await ctx.close();
        }
      }

      if (SCENARIO === 'guest') {
        let guestCalls = 0;
        const { ctx, page } = await open(scheme, viewport, async (c) => {
          await c.route('**/api/kydrive/status', status({ guestLinks: true, guestMaxDays: 30 }));
          await c.route('**/api/kydrive/upload-tickets', (r) => json(r, 201, { uploadUrl: `${DRIVE}/api/suite/upload/fixture`, expires: '2026-10-28T15:30:00Z', maxChunk: 64 << 20 }));
          await c.route('**/api/kydrive/recipients', (r) => json(r, 200, { outside: ['dana@partner.example'] }));
          await c.route('**/api/kydrive/links', (r) => {
            const body = r.request().postDataJSON();
            if (body.audience !== 'guest') return json(r, 201, { url: `${DRIVE}/l/#fixture-token`, expires: '2026-11-27T15:15:00Z' });
            // The first guest request has had no step-up yet; the real server would say so too.
            if (guestCalls++ === 0) return json(r, 403, { error: 'confirm it is you before sending guest links', code: 'step_up_required' });
            return json(r, 201, { url: `${DRIVE}/g/#fixture-guest`, expires: '2026-11-04T15:15:00Z' });
          });
          await c.route('**/api/mail/send', () => {}); // held: the rewritten body stays on screen
          await c.route(`${DRIVE}/**`, (r) => {
            if (r.request().method() === 'OPTIONS') return r.fulfill({ status: 204, headers: { 'Access-Control-Allow-Origin': ORIGIN, 'Access-Control-Allow-Methods': 'PUT', 'Access-Control-Allow-Headers': 'Content-Type, Content-Range' } });
            return r.fulfill({ status: 201, headers: { 'Access-Control-Allow-Origin': ORIGIN }, contentType: 'application/json', body: JSON.stringify({ file_id: 'f1', name: 'site-survey.mov', size: 19 << 20 }) });
          });
        });
        await compose(page);
        const to = page.getByLabel('To recipients', { exact: true });
        await to.fill('dana@partner.example');
        await to.press('Enter');
        await page.getByPlaceholder('Subject').fill('Site survey footage');
        await page.locator('.ql-editor').click();
        await page.keyboard.type('Here is the walkthrough from Tuesday: ');
        await page.locator('input[type="file"]').setInputFiles({ name: 'site-survey.mov', mimeType: 'video/quicktime', buffer: Buffer.alloc(19 << 20, 7) });
        await page.getByRole('button', { name: 'Upload to KyDrive and send a link' }).click();
        await page.locator('.ql-editor a').first().waitFor();
        await page.getByRole('button', { name: 'Send', exact: true }).click();
        await page.getByText('cannot open the KyDrive links').waitFor();
        await page.locator('.compose-drive').first().scrollIntoViewIfNeeded();
        await settle(page, 400);
        await shot(page, `${PREFIX}-15-outside-${tag}`);
        if (AFTER) {
          await page.getByLabel('Guest links expire after').selectOption('7');
          await page.getByRole('button', { name: 'Send a guest link instead' }).click();
          await page.getByText('confirm it is you before KyPost makes them').waitFor();
          await page.locator('.compose-drive-stepup input[type="password"]').fill(pws.user);
          await settle(page, 300);
          await shot(page, `${PREFIX}-16-guest-stepup-${tag}`);
          await page.getByRole('button', { name: 'Confirm and send' }).click();
          await page.locator('.ql-editor a[href*="/g/"]').waitFor({ timeout: 30000 });
          await settle(page, 400);
          await shot(page, `${PREFIX}-17-guest-sending-${tag}`);
          results.push({ tag, guestCalls, body: await page.locator('.ql-editor').innerHTML() });
        }
        await ctx.close();
      }
    }
  }
} finally {
  await env.stop();
}
for (const r of results) console.log(JSON.stringify(r));
