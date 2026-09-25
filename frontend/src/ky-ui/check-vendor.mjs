// Run from any checkout: node path/to/ky-ui/check-vendor.mjs
import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
const manifest = JSON.parse(await readFile(new URL('./VERSION', import.meta.url), 'utf8'));
for (const [file, digest] of Object.entries(manifest.files)) {
  if (!/^[a-z][a-z0-9.-]*$/.test(file)) throw new Error('Invalid vendor filename');
  const bytes = await readFile(new URL(file, import.meta.url));
  if (createHash('sha256').update(bytes).digest('hex') !== digest) {
    throw new Error(`ky-ui ${manifest.version}: ${file} differs from the recorded release; refresh from ky-ui`);
  }
}
console.log(`ky-ui ${manifest.version}: vendor files verified`);
