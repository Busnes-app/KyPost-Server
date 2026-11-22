// The compose To/Cc fields as a Base UI Combobox in a real browser, inside the
// compose <dialog>: combobox/option semantics, arrows, Enter, Tab, Escape,
// comma and Backspace, and no injected <style>.
import { test, expect } from '@playwright/test';

async function openCompose(page) {
  await page.goto('/read');
  await page.getByRole('button', { name: 'New email' }).first().click();
  const compose = page.locator('dialog.compose-backdrop');
  await expect(compose).toBeVisible();
  return compose;
}

const chips = (compose) => compose.locator('.compose-token-field').first().locator('.compose-token-pill-label');

test('To: suggestions are options; arrows, Enter and Tab choose; focus stays in the box', async ({ page }) => {
  const styles = [];
  await page.exposeBinding('__style', (_s, t) => styles.push(t));
  await page.addInitScript(() => addEventListener('load', () => new MutationObserver((rs) => {
    for (const r of rs) for (const n of r.addedNodes) if (n.nodeName === 'STYLE') window.__style(n.textContent ?? '');
  }).observe(document, { childList: true, subtree: true })));
  const compose = await openCompose(page);
  const to = compose.getByRole('combobox', { name: 'To recipients', exact: true });
  await expect(to).toHaveAttribute('aria-expanded', 'false');
  await to.fill('al');
  const options = compose.getByRole('option');
  await expect(options).toHaveCount(3);
  await expect(to).toHaveAttribute('aria-expanded', 'true');
  await expect(to).toHaveAttribute('aria-activedescendant', await options.nth(0).getAttribute('id'));

  // The list sits inside the dialog, on top, and inside the window.
  const first = options.nth(0);
  expect(await first.evaluate((el) => el.closest('dialog')?.className)).toBe('compose-backdrop');
  expect(await first.evaluate((el) => { const r = el.getBoundingClientRect(); return el.contains(document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2)); })).toBe(true);
  const box = await compose.locator('.recipient-autocomplete').boundingBox();
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(page.viewportSize().width);

  await page.keyboard.press('ArrowDown');
  await expect(to).toBeFocused();
  await expect(to).toHaveAttribute('aria-activedescendant', await options.nth(1).getAttribute('id'));
  await page.keyboard.press('Enter');
  await expect(chips(compose)).toHaveText(['Alan Ames <alan@example.test>']);
  await expect(to).toHaveValue('');
  await expect(to).toBeFocused();
  await expect(compose.getByRole('listbox')).toHaveCount(0);

  // Tab accepts the highlighted one and moves on to Cc.
  await to.pressSequentially('al');
  await expect(options).toHaveCount(3);
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Tab');
  await expect(chips(compose)).toHaveText(['Alan Ames <alan@example.test>', 'Albert Ash <albert@example.test>']);
  await expect(compose.getByRole('combobox', { name: 'Cc recipients', exact: true })).toBeFocused();
  expect(styles).toEqual([]);
});

test('To: Escape closes the list, then the dialog; comma commits; Backspace removes a chip', async ({ page }) => {
  const compose = await openCompose(page);
  const to = compose.getByRole('combobox', { name: 'To recipients', exact: true });
  await to.pressSequentially('zed@example.test,');
  await to.pressSequentially('bad,');
  await expect(compose.getByText('"bad" doesn\'t look like a valid email address.')).toBeVisible();
  await to.fill('');
  await to.pressSequentially('al');
  await expect(compose.getByRole('option')).toHaveCount(3);
  await to.pressSequentially('i');
  await expect(compose.getByRole('option')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(compose.getByRole('listbox')).toHaveCount(0);
  await expect(compose).toBeVisible();
  await expect(to).toHaveValue('ali');
  await expect(chips(compose)).toHaveText(['zed@example.test']);

  await to.fill('');
  await page.keyboard.press('Backspace');
  await expect(chips(compose)).toHaveCount(0);
  await to.pressSequentially('a@b.test;c@d.test;');
  await page.keyboard.press('ArrowLeft');
  await expect(compose.locator('.compose-token-pill').nth(1)).toBeFocused();
  await page.keyboard.press('Delete');
  await expect(chips(compose)).toHaveText(['a@b.test']);
  // Base UI moves focus to the neighbouring chip; Right returns to the box.
  await expect(compose.locator('.compose-token-pill').nth(0)).toBeFocused();
  await page.keyboard.press('ArrowRight');
  await expect(to).toBeFocused();

  // With the list closed, Escape belongs to the dialog and the chips survive it.
  await page.keyboard.press('Escape');
  await expect(compose).toBeHidden();
});
