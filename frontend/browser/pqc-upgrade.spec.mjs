// Real ordinary-user conversion, production CSP/API, no mocked transport.
import { test, expect } from '@playwright/test';
import { ADMIN } from './setup.mjs';
import { mkdir } from 'node:fs/promises';
import { readdirSync } from 'node:fs';
const engineChunk = '/assets/' + readdirSync(new URL('../dist/assets/', import.meta.url)).find(name => /^pgpEngineWasm-.*\.js$/.test(name));

test('P4 recovery-first upgrade retains v4 and commits both certificates', async ({ page }) => {
  test.skip(test.info().project.name !== 'light-1280', 'one ceremony on the shared test server');
  test.setTimeout(120000);
  await page.goto('/read');
  const created = await page.evaluate(async (password) => {
    const params = await (await fetch('/api/auth/login-params')).json();
    const enc = new TextEncoder();
    const key = await crypto.subtle.importKey('raw', enc.encode(password), 'PBKDF2', false, ['deriveBits']);
    const stretch = await crypto.subtle.deriveBits({ name:'PBKDF2', salt: Uint8Array.from(atob(params.salt), c => c.charCodeAt(0)), iterations:params.iterations, hash:'SHA-256' }, key, 256);
    const hkdf = await crypto.subtle.importKey('raw', stretch, 'HKDF', false, ['deriveBits']);
    const bits = await crypto.subtle.deriveBits({ name:'HKDF', hash:'SHA-256', salt:new Uint8Array(), info:enc.encode('kypost/auth/v1') }, hkdf, 256);
    const adminAuthSecret = Array.from(new Uint8Array(bits), n => n.toString(16).padStart(2,'0')).join('');
    const csrf = decodeURIComponent(document.cookie.match(/(?:^|; )csrf_token=([^;]*)/)?.[1] ?? '');
    const response = await fetch('/api/users', { method:'POST', headers:{'Content-Type':'application/json','X-CSRF-Token':csrf}, body:JSON.stringify({ username:'pqc-user', password:'PQC-Temporary-123!', role:'user', adminAuthSecret }) });
    return { status:response.status, body:await response.text() };
  }, ADMIN.password);
  expect(created.status, created.body).toBe(201);
  await page.context().clearCookies();
  await page.goto('/');
  await page.getByLabel('Username', {exact:true}).fill('pqc-user');
  await page.getByLabel('Password', {exact:true}).fill('PQC-Temporary-123!');
  await page.getByRole('button',{name:'Sign in',exact:true}).click();
  await expect(page.getByLabel('New password')).toBeVisible({timeout:30000});
  await page.getByLabel('Current password').fill('PQC-Temporary-123!');
  await page.getByLabel('New password').fill('PQC-Current-456!');
  await page.getByRole('button',{name:'Update password'}).click();
  await expect(page.getByLabel('New password')).toHaveCount(0,{timeout:30000});
  const configured = await page.evaluate(async () => {
    const csrf = decodeURIComponent(document.cookie.match(/(?:^|; )csrf_token=([^;]*)/)?.[1] ?? '');
    const r = await fetch('/api/imap/config', { method:'POST', headers:{'Content-Type':'application/json','X-CSRF-Token':csrf}, body:JSON.stringify({host:'127.0.0.1',port:993,username:'pqc@example.test',password:'fake-mail-password',mailbox:'INBOX',smtpHost:'127.0.0.1',smtpPort:587}) });
    return {status:r.status,body:await r.text()};
  });
  expect(configured.status,configured.body).toBe(200);
  await page.goto('/settings/security?tab=mail');
  await expect(page.getByRole('heading',{name:'Confirm it is you'})).toBeVisible();
  const gate = page.locator('section').filter({has:page.getByRole('heading',{name:'Confirm it is you'})});
  await gate.getByLabel('Account password', {exact:true}).fill('PQC-Current-456!');
  await gate.getByRole('button',{name:'Continue',exact:true}).click();
  await page.getByRole('button',{name:'Generate new identity',exact:true}).click();
  const prompt = page.getByRole('dialog',{name:'Enter your account password'});
  await prompt.getByLabel('Account password').fill('PQC-Current-456!');
  await prompt.getByRole('button',{name:'Continue'}).click();
  const prepare = page.getByRole('button',{name:'Prepare post-quantum upgrade'});
  await expect(prepare).toBeVisible({timeout:30000});
  const before = await page.evaluate(async () => (await fetch('/api/pgp/bootstrap')).json());
  await mkdir('test-results/p4', {recursive:true});
  await page.screenshot({path:'test-results/p4/before.png',fullPage:true});
  await prepare.click();
  await prompt.getByLabel('Account password').fill('PQC-Current-456!');
  const download = page.waitForEvent('download');
  await prompt.getByRole('button',{name:'Continue'}).click();
  await (await download).saveAs('test-results/p4/recovery.json');
  const complete = page.getByRole('button',{name:'Complete post-quantum upgrade'});
  await expect(complete).toBeDisabled();
  await page.getByLabel('I saved the new recovery file and its secret separately.').check();
  await complete.click();
  await prompt.getByLabel('Account password').fill('PQC-Current-456!');
  await prompt.getByRole('button',{name:'Continue'}).click();
  await expect(page.getByText('Post-quantum certificate installed.',{exact:false})).toBeVisible({timeout:30000});
  const after = await page.evaluate(async () => (await fetch('/api/pgp/bootstrap')).json());
  expect(after.fingerprint).toBe(before.fingerprint);
  expect(after.fingerprintV6).toMatch(/^[a-f0-9]{64}$/i);
  expect(after.keyring.version).toBe(2);
  expect(after.keyring.primaryFingerprints).toHaveLength(2);
  expect(after.v6Ready).toBe(true); // browser-only test account
  // Never put even a throwaway recovery secret in a review screenshot.
  await page.screenshot({path:'test-results/p4/after.png',fullPage:true,mask:[page.locator('.sec-fingerprint code')]});
  const contact = await page.evaluate(async chunk => {
    const { wasmEngine } = await import(chunk);
    const publicKey = (await wasmEngine.generate([{name:'Classical',email:'classical@example.test'}], 'v4')).armoredPublicKey;
    const csrf = decodeURIComponent(document.cookie.match(/(?:^|; )csrf_token=([^;]*)/)?.[1] ?? '');
    const response = await fetch('/api/contacts', {method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':csrf},body:JSON.stringify({fn:'Classical correspondent', emails:[{value:'classical@example.test'}], pgpKey:publicKey})});
    return {status:response.status,body:await response.text()};
  }, engineChunk);
  expect(contact.status,contact.body).toBe(200);
  await page.goto('/read');
  await page.getByRole('button',{name:'New email',exact:true}).click();
  await page.getByLabel('To recipients').fill('classical@example.test');
  await page.getByLabel('To recipients').press('Enter');
  await page.getByLabel('Encrypt',{exact:true}).check();
  await expect(page.getByText('Not quantum-safe: classical@example.test will receive classical encryption.',{exact:true})).toBeVisible();
  await page.screenshot({path:'test-results/p4/classical-compose.png',fullPage:true});

});
