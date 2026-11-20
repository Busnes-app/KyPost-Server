// Browser walk-through for the native-mail harness (Playwright Chromium,
// 1440x900, light theme, real sign-in). Steps, in order:
//   admin-setup  bootstrap admin: verify the domain, relay, sending domain, policy
//   check3       plus addressing: mail to owner+tag arrives; the reply is From owner+tag
//   check4       masked addresses: enable, create, mail in, reply From, pause/refuse, resume, delete
// The provision phase of native-mail-harness.sh runs between admin-setup and check3.
import { writeFileSync, readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { launch, adminLogin, ssoLogin, openFromInbox, shot, text, harness, cfg, creds, ORIGIN } from './ui.mjs';

const step = process.argv[2];
const assert = (ok, what) => { if (!ok) throw new Error(`check failed: ${what}`); console.log('ok:', what); };
const domain = cfg.domain;

async function confirmPassword(page, scope) {
  await scope.getByLabel(/^(Account password|Your account password)$/).first().fill(creds.admin);
}

async function adminSetup() {
  const { browser, page } = await launch();
  await adminLogin(page);
  await page.goto(`${ORIGIN}/admin/server?tab=mail-domain`);
  const panel = page.locator('main');
  await confirmPassword(page, panel);
  await panel.getByRole('button', { name: `Verify ${domain}` }).click();
  await panel.getByText('Established').first().waitFor({ timeout: 30000 });
  await shot(page, '01-admin-domain-established');

  await confirmPassword(page, panel);
  await panel.getByLabel('Relay host').fill(cfg.relay.host);
  await panel.getByLabel('Relay port').fill(String(cfg.relay.port));
  await panel.getByLabel('Relay username').fill(cfg.relay.username);
  await panel.getByLabel('Relay password').fill('x-any-password');
  const box = panel.locator('label.config-checkbox', { hasText: domain }).locator('input[type=checkbox]');
  if (!(await box.isChecked())) await box.check();
  page.once('dialog', (d) => d.accept());
  await panel.getByRole('button', { name: 'Save outgoing relay' }).click();
  await page.waitForTimeout(2500);
  console.log((await text(page)).match(/2\. Configure outgoing delivery.{0,600}/)?.[0]);
  await confirmPassword(page, panel);
  await panel.getByRole('button', { name: 'Check saved relay' }).click();
  await page.waitForTimeout(4000);
  await shot(page, '02-admin-relay-checked');

  await page.goto(`${ORIGIN}/admin/server?tab=security`);
  await page.locator('#policy_key').selectOption('optional');
  await page.locator('#policy_incoming').selectOption('off');
  await page.locator('#policy_confirm_password').fill(creds.admin);
  await page.locator('#policy_confirm_password').press('Enter');
  await page.waitForTimeout(2000);
  await browser.close();
}


// Evidence the screenshots cannot show: SMTP replies and the relayed headers.
const evidenceFile = join(cfg.shots, 'evidence.json');
const evidence = existsSync(evidenceFile) ? JSON.parse(readFileSync(evidenceFile, 'utf8')) : {};
const record = (k, v) => { evidence[k] = v; writeFileSync(evidenceFile, JSON.stringify(evidence, null, 2) + '\n'); };

// Delivers over SMTP+STARTTLS to mx.harness.test:25; returns the RCPT reply line.
function smtpIn(to, subject, body) {
  let out = '';
  try { out = harness('send', 'friend@outside.test', to, subject, body); } catch (e) { out = String(e.stdout || '') + String(e.stderr || ''); }
  const rcpt = out.split('\n').find((l) => /^< \d{3} /.test(l) && out.indexOf(l) > out.indexOf(`RCPT TO:<${to}>`)) || '';
  return { rcpt: rcpt.slice(2).trim(), queued: /^< 250 2\.0\.0 OK: queued/m.test(out) };
}

// Headers of the relay's (fixture SMTP) accepted message with this subject.
async function relayed(subject) {
  for (let i = 0; i < 40; i++) {
    const out = harness('relay-sent');
    const block = out.split(/^\* \d+ FETCH/m).find((b) => b.includes(`Subject: ${subject}`));
    if (block) return block.trim().split(/\r?\n/).filter((l) => /^(From|To|Subject):/.test(l));
    await new Promise((r) => setTimeout(r, 1500));
  }
  throw new Error(`relay never received "${subject}"`);
}

async function reply(page, subject, expectFrom, name) {
  await openFromInbox(page, subject);
  await page.getByRole('button', { name: 'Reply', exact: true }).first().click();
  const c = page.locator('.compose-backdrop');
  await c.waitFor();
  const from = await c.locator('select').first().inputValue();
  assert(from === expectFrom, `reply From preselects ${expectFrom} (got ${from})`);
  await c.locator('.ql-editor').first().click();
  await page.keyboard.press('Control+Home'); // above the quoted message
  await page.keyboard.type(`Reply from ${expectFrom}, sent through the harness relay.`);
  await shot(page, name);
  await c.getByRole('button', { name: 'Send', exact: true }).click();
  await c.waitFor({ state: 'detached', timeout: 30000 }).catch(() => {});
  const headers = await relayed(`Re: ${subject}`);
  assert(headers.some((h) => h.toLowerCase() === `from: ${expectFrom}` || h.toLowerCase().includes(`<${expectFrom}>`)), `relay got From ${expectFrom}: ${headers.join(' | ')}`);
  return headers;
}

async function check3() {
  const subject = 'Plus addressing check';
  const to = `owner+news@${domain}`;
  const smtp = smtpIn(to, subject, 'Sent to the plus address owner+news.');
  assert(smtp.queued, `${to} accepted by the MX (${smtp.rcpt})`);
  const { browser, page } = await launch();
  await ssoLogin(page);
  await openFromInbox(page, subject);
  await page.getByText('Sent to the plus address').first().waitFor();
  await shot(page, '03-1-plus-arrived');
  const headers = await reply(page, subject, to, '03-2-plus-reply-from');
  await page.goto(`${ORIGIN}/read?folder=Sent`);
  await page.getByRole('link', { name: 'Sent', exact: true }).first().click().catch(() => {});
  await page.getByText(`Re: ${subject}`).first().waitFor({ timeout: 30000 });
  await shot(page, '03-3-plus-reply-in-sent');
  record('check3', { smtpIn: smtp, relayedHeaders: headers });
  await browser.close();
}

async function check4() {
  const ev = {};
  { // Admin enables masked addresses on the domain.
    const { browser, page } = await launch();
    await adminLogin(page);
    await page.goto(`${ORIGIN}/admin/server?tab=mail-addresses`);
    const form = page.locator('form', { has: page.getByRole('heading', { name: 'Masked addresses' }) });
    const box = form.locator('label.security-check', { hasText: domain }).locator('input');
    if (!(await box.isChecked())) await box.check();
    await form.locator('#masked_confirm_password').fill(creds.admin);
    await form.getByRole('button', { name: 'Save masked domains' }).click();
    await form.getByRole('status').waitFor({ timeout: 15000 });
    ev.adminNotice = await form.getByRole('status').innerText();
    await form.scrollIntoViewIfNeeded();
    await shot(page, '04-1-admin-masked-domain-enabled');
    await browser.close();
  }
  const { browser, page } = await launch();
  await ssoLogin(page);
  const open = async () => { await page.goto(`${ORIGIN}/settings/mail?tab=addresses`); await page.getByRole('heading', { name: 'Masked addresses' }).waitFor(); };
  await open();
  await page.getByLabel('Note (optional)').fill('example-shop');
  await page.getByRole('button', { name: 'Create address' }).click();
  const row = page.locator('.config-status-card strong').first();
  await row.waitFor({ timeout: 15000 });
  const masked = (await row.innerText()).trim();
  assert(masked.endsWith(`@${domain}`) && !masked.startsWith('owner'), `masked address created: ${masked}`);
  ev.masked = masked;
  await shot(page, '04-2-masked-created');

  const subject = 'Masked address check';
  ev.active = smtpIn(masked, subject, 'Sent to the masked address.');
  assert(ev.active.queued, `active masked address accepted (${ev.active.rcpt})`);
  await openFromInbox(page, subject);
  await page.getByText('Sent to the masked address').first().waitFor();
  await shot(page, '04-3-masked-arrived');
  ev.relayedHeaders = await reply(page, subject, masked, '04-4-masked-reply-from');

  await open();
  await page.getByRole('button', { name: `Pause ${masked}` }).click();
  await page.getByRole('button', { name: `Resume ${masked}` }).waitFor();
  await shot(page, '04-5-masked-paused');
  ev.paused = smtpIn(masked, 'Masked while paused', 'Must be refused.');
  ev.unknown = smtpIn(`no-such-address@${domain}`, 'Unknown address', 'Must be refused.');
  const answer = (r) => r.rcpt.replace(/\(msg ID = \w+\)/, '').trim();
  assert(!ev.paused.queued && answer(ev.paused) === answer(ev.unknown) && answer(ev.paused).startsWith('550 '),
    `paused address refused exactly like an unknown one (${answer(ev.paused)})`);

  await page.getByRole('button', { name: `Resume ${masked}` }).click();
  await page.getByRole('button', { name: `Pause ${masked}` }).waitFor();
  ev.resumed = smtpIn(masked, 'Masked after resume', 'Accepted again.');
  assert(ev.resumed.queued, `resumed address accepted (${ev.resumed.rcpt})`);
  await openFromInbox(page, 'Masked after resume');
  await shot(page, '04-6-masked-resumed-arrived');

  await open();
  page.once('dialog', (d) => d.accept());
  await page.getByRole('button', { name: `Delete ${masked}` }).click();
  await page.getByText('Deleted addresses (1)').waitFor();
  await page.getByText('Deleted addresses (1)').click();
  await shot(page, '04-7-masked-deleted');
  ev.deleted = smtpIn(masked, 'Masked after delete', 'Must be refused.');
  assert(!ev.deleted.queued && answer(ev.deleted) === answer(ev.unknown), `deleted address refused exactly like an unknown one (${answer(ev.deleted)})`);
  record('check4', ev);
  await browser.close();
}

const steps = { 'admin-setup': adminSetup, check3, check4 };
if (!steps[step]) throw new Error(`unknown step ${step}; one of ${Object.keys(steps).join(', ')}`);
await steps[step]();
