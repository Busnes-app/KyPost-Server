// Before/after evidence for the recipient field and the three tab strips:
// <widget>-<project>.png and measurements-<project>.json in $SHOTS_DIR.
// Measurement selectors are main's class names, kept stable across the Base UI change.
import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { test, expect } from '@playwright/test';
import { ADMIN } from './setup.mjs';

const SHOTS_DIR = process.env.SHOTS_DIR || 'test-results/shots';

const TARGETS = {
  tokenField: '.compose-token-field',
  chip: '.compose-token-pill',
  chipRemove: '.compose-token-pill-remove',
  tokenInput: '.compose-token-input',
  suggestionRow: '.recipient-autocomplete-row',
  suggestionList: '.recipient-autocomplete',
  organisationHeader: '.recipient-autocomplete-group',
  fieldError: '.compose-field-error',
  configTab: '.config-tab',
  configTabs: '.config-tabs',
  secTab: '.sec-tab',
  secTabs: '.sec-tabs',
  contactsPageTab: '.contacts-page-tab',
};

// Runs in every document before the app: notes <style> elements inserted after
// the load event and CSP violations, reported to the test through a binding.
function watchPage() {
  const report = (kind, detail) => window.__kpReport?.({ kind, path: location.pathname + location.search, ...detail });
  document.addEventListener('securitypolicyviolation', (e) =>
    report('csp', { directive: e.violatedDirective, blocked: e.blockedURI, sample: e.sample }));
  addEventListener('load', () => {
    report('loadStyles', { count: document.querySelectorAll('style').length });
    new MutationObserver((records) => {
      for (const r of records) for (const n of r.addedNodes) {
        if (n.nodeName === 'STYLE') report('styleAdded', { parent: n.parentNode?.nodeName, text: (n.textContent ?? '').slice(0, 200) });
      }
    }).observe(document, { childList: true, subtree: true });
  });
}

async function measure(page, keys) {
  return page.evaluate(({ targets, keys }) => {
    const round = (n) => Math.round(n * 10) / 10;
    return Object.fromEntries(keys.map((key) => {
      const el = document.querySelector(targets[key]);
      if (!el) return [key, null];
      const r = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      return [key, { selector: targets[key], x: round(r.left), y: round(r.top), width: round(r.width), height: round(r.height), fontSize: cs.fontSize, lineHeight: cs.lineHeight }];
    }));
  }, { targets: TARGETS, keys });
}

// Which build is on screen: the entry script, and ARIA that only the Base UI
// build has (combobox/option roles on the recipient field; tab panels named by
// their tab and roving tabindex on the strips; pagination nav on contacts).
// A stale frontend/dist shows up here instead of as identical screenshots.
async function dom(page) {
  return page.evaluate(() => {
    const attrs = (sel, names) => [...document.querySelectorAll(sel)].map((el) => Object.fromEntries(names.map((n) => [n, el.getAttribute(n)])));
    return {
      entryScript: document.querySelector('script[type="module"][src*="/assets/index-"]')?.getAttribute('src') ?? null,
      recipientInput: attrs('.compose-token-input', ['role', 'aria-expanded', 'aria-controls', 'aria-activedescendant'])[0] ?? null,
      options: document.querySelectorAll('[role="option"]').length,
      tabs: attrs('[role="tab"]', ['tabindex', 'aria-selected', 'aria-controls']),
      tabpanels: attrs('[role="tabpanel"]', ['aria-labelledby']),
      contactPages: attrs('nav[aria-label="Contact pages"] button', ['aria-current']),
    };
  });
}

// Viewport clip around the union of the matched elements, padded and kept on screen.
async function shot(page, name, selectors, { pad = 12, below = 0 } = {}) {
  const box = await page.evaluate(({ selectors, pad, below }) => {
    const rects = selectors.flatMap((s) => [...document.querySelectorAll(s)]).map((el) => el.getBoundingClientRect()).filter((r) => r.width && r.height);
    const x = Math.max(0, Math.min(...rects.map((r) => r.left)) - pad);
    const y = Math.max(0, Math.min(...rects.map((r) => r.top)) - pad);
    const right = Math.min(innerWidth, Math.max(...rects.map((r) => r.right)) + pad);
    const bottom = Math.min(innerHeight, Math.max(...rects.map((r) => r.bottom)) + pad + below);
    return { x, y, width: right - x, height: bottom - y };
  }, { selectors, pad, below });
  await page.mouse.move(0, 0);
  await page.screenshot({ path: join(SHOTS_DIR, name), clip: box, animations: 'disabled', caret: 'hide' });
}

test('capture recipient field and tab strips', async ({ page, context }, testInfo) => {
  const project = testInfo.project.name;
  const events = [];
  const consoleCsp = [];
  await context.exposeBinding('__kpReport', (_source, event) => { events.push(event); });
  await context.addInitScript(watchPage);
  page.on('console', (m) => { if (/Content Security Policy|violates the following/i.test(m.text())) consoleCsp.push(m.text()); });
  await mkdir(SHOTS_DIR, { recursive: true });
  const out = { project, viewport: page.viewportSize(), colorScheme: testInfo.project.use.colorScheme, widgets: {} };

  await test.step('recipients', async () => {
    const response = await page.goto('/read');
    out.cspHeader = response.headers()['content-security-policy'] ?? null;
    await page.getByRole('button', { name: 'New email' }).first().click();
    const compose = page.locator('.compose-window');
    await expect(compose).toBeVisible();
    const to = compose.getByLabel('To recipients', { exact: true });
    await to.fill('al');
    await compose.locator('.recipient-autocomplete-row', { hasText: 'Alice Able' }).click();
    await to.fill('zed@example.test');
    await to.press(',');
    await expect(compose.locator('.compose-token-pill')).toHaveCount(2);
    await to.fill('al');
    await expect(compose.locator('.recipient-autocomplete-row')).toHaveCount(3);
    await expect(compose.getByLabel('Cc recipients', { exact: true })).toHaveValue('');
    out.widgets.recipients = await measure(page, ['tokenField', 'chip', 'chipRemove', 'tokenInput', 'suggestionRow', 'suggestionList', 'organisationHeader']);
    out.widgets['recipients'].dom = await dom(page);
    await shot(page, `recipients-${project}.png`, ['.compose-topbar', '.compose-token-field', '.recipient-autocomplete']);
  });

  await test.step('recipients-error', async () => {
    const to = page.locator('.compose-window').getByLabel('To recipients', { exact: true });
    await to.fill('not-an-address');
    await expect(page.locator('.recipient-autocomplete-row')).toHaveCount(0); // search settled
    await to.press(',');
    const error = page.locator('.compose-field-error');
    await expect(error).toBeVisible();
    // On main the still-open suggestion list overlaps the error; note it, then
    // dismiss the list (Escape, as a user would) so the error shows.
    const errorCovered = await error.evaluate((el) => {
      const r = el.getBoundingClientRect();
      return !el.contains(document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2));
    });
    const listOpen = await page.locator('.recipient-autocomplete').isVisible();
    if (listOpen) await to.press('Escape');
    await expect(page.locator('.recipient-autocomplete')).toBeHidden();
    await expect(page.locator('.compose-window')).toBeVisible();
    out.widgets['recipients-error'] = { listOpenAfterComma: listOpen, errorCoveredByList: errorCovered, ...await measure(page, ['tokenField', 'fieldError']) };
    await shot(page, `recipients-error-${project}.png`, ['.compose-topbar', '.compose-token-field-wrap', '.recipient-autocomplete']);
  });

  await test.step('panel-tabs', async () => {
    await page.goto('/settings/mail');
    const strip = page.locator('.config-tabs');
    await expect(strip).toBeVisible();
    await strip.evaluate((el) => el.scrollIntoView({ block: 'center' }));
    out.widgets['panel-tabs'] = await measure(page, ['configTab', 'configTabs']);
    out.widgets['panel-tabs'].dom = await dom(page);
    await shot(page, `panel-tabs-${project}.png`, ['.config-tabs'], { below: 260 });
  });

  await test.step('security-tabs', async () => {
    await page.goto('/settings/account');
    const gate = page.locator('form.sec-inline-form');
    await gate.getByLabel('Account password').fill(ADMIN.password);
    await gate.getByRole('button', { name: 'Continue' }).click();
    const strip = page.locator('.sec-tabs');
    await expect(strip).toBeVisible({ timeout: 30000 });
    await strip.evaluate((el) => el.scrollIntoView({ block: 'center' }));
    out.widgets['security-tabs'] = await measure(page, ['secTab', 'secTabs']);
    out.widgets['security-tabs'].dom = await dom(page);
    await shot(page, `security-tabs-${project}.png`, ['.sec-tabs'], { below: 40 });
  });

  await test.step('page-tabs', async () => {
    await page.goto('/contacts');
    const tabs = page.locator('.contacts-page-tabs');
    await expect(tabs.locator('.contacts-page-tab')).toHaveCount(2);
    await tabs.evaluate((el) => el.scrollIntoView({ block: 'center' }));
    out.widgets['page-tabs'] = await measure(page, ['contactsPageTab']);
    out.widgets['page-tabs'].dom = await dom(page);
    await shot(page, `page-tabs-${project}.png`, ['.contacts-page-tabs', '.contacts-count'], { below: 120 });
  });

  out.styles = {
    loadCounts: events.filter((e) => e.kind === 'loadStyles').map(({ path, count }) => ({ path, count })),
    addedAfterLoad: events.filter((e) => e.kind === 'styleAdded'),
  };
  out.csp = { events: events.filter((e) => e.kind === 'csp'), console: consoleCsp };
  await writeFile(join(SHOTS_DIR, `measurements-${project}.json`), JSON.stringify(out, null, 2) + '\n');
});
