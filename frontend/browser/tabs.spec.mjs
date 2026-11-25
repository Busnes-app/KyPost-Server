// Settings and Security tab strips on Base UI Tabs (manual activation), and the
// list page buttons as pagination.
import { test, expect } from '@playwright/test';
import { ADMIN } from './setup.mjs';

test('Mail tabs: arrows move focus, Enter opens, the panel names its tab', async ({ page }) => {
  await page.goto('/settings/mail');
  const list = page.getByRole('tablist', { name: 'Mail sections' });
  const first = list.getByRole('tab', { name: 'Email settings' });
  await expect(first).toHaveAttribute('aria-selected', 'true');
  await first.focus();
  await page.keyboard.press('ArrowRight');
  const pane = list.getByRole('tab', { name: 'Conversations' });
  await expect(pane).toBeFocused();
  await expect(first).toHaveAttribute('aria-selected', 'true');
  await page.keyboard.press('Enter');
  await expect(pane).toHaveAttribute('aria-selected', 'true');
  await expect(page).toHaveURL(/[?&]tab=conversations/);
  await expect(page.getByRole('tabpanel')).toHaveAttribute('aria-labelledby', await pane.getAttribute('id'));
  await page.keyboard.press('ArrowLeft');
  await page.keyboard.press('ArrowLeft');
  await expect(list.getByRole('tab', { name: 'Mailbox rules' })).toBeFocused();
  await page.keyboard.press('Space');
  await expect(page.getByRole('tabpanel', { name: 'Mailbox rules' })).toBeVisible();
});

test('Security tabs: arrows and Space, behind the page password', async ({ page }) => {
  await page.goto('/settings/account');
  const gate = page.locator('form.sec-inline-form');
  await gate.getByLabel('Account password').fill(ADMIN.password);
  await gate.getByRole('button', { name: 'Continue' }).click();
  const list = page.getByRole('tablist', { name: 'Security sections' });
  const signIn = list.getByRole('tab', { name: 'Sign-in' });
  await expect(signIn).toHaveAttribute('aria-selected', 'true', { timeout: 30000 });
  await signIn.focus();
  await page.keyboard.press('End');
  await expect(list.getByRole('tab', { name: 'Export Mail' })).toBeFocused();
  await page.keyboard.press('ArrowRight');
  await expect(signIn).toBeFocused();
  await page.keyboard.press('Home');
  await expect(signIn).toBeFocused();
  await page.keyboard.press('ArrowLeft');
  await expect(list.getByRole('tab', { name: 'Export Mail' })).toBeFocused();
  await page.keyboard.press('ArrowRight');
  await expect(signIn).toBeFocused();
  await page.keyboard.press('ArrowRight');
  await page.keyboard.press('Space');
  const devices = list.getByRole('tab', { name: 'Devices' });
  await expect(devices).toHaveAttribute('aria-selected', 'true');
  await expect(page.getByRole('tabpanel')).toHaveAttribute('aria-labelledby', await devices.getAttribute('id'));
  // The strip is one Tab stop: only the selected tab is in the tab order.
  await expect(list.getByRole('tab').and(page.locator('[tabindex="0"]'))).toHaveCount(1);
  await page.keyboard.press('Shift+Tab');
  await expect(list.locator(':focus')).toHaveCount(0);
});

test('Contact page buttons are pagination: a nav with the current page marked', async ({ page }) => {
  await page.goto('/contacts');
  const nav = page.getByRole('navigation', { name: 'Contact pages' });
  await expect(nav.getByRole('button')).toHaveCount(2);
  await expect(page.getByRole('tab')).toHaveCount(0);
  await expect(nav.getByRole('button', { name: '1' })).toHaveAttribute('aria-current', 'page');
  await nav.getByRole('button', { name: '1' }).focus();
  await page.keyboard.press('Tab');
  await expect(nav.getByRole('button', { name: '2' })).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(nav.getByRole('button', { name: '2' })).toHaveAttribute('aria-current', 'page');
});
