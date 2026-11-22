// Launches a disposable KyPost for the browser suite: the prebuilt server binary
// (frontend/.browser/server) serving the prebuilt frontend/dist, with an owned
// temporary data directory removed on exit. Builds nothing itself.
import { mkdirSync, openSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn, spawnSync } from 'node:child_process';
import { ADMIN, PORT } from './setup.mjs';

const bin = fileURLToPath(new URL('../.browser/server', import.meta.url));
const dir = await mkdtemp(join(tmpdir(), 'kypost-browser-'));
const env = {
  PATH: process.env.PATH,
  CONFIG_DIR: join(dir, 'config'), STATE_DIR: join(dir, 'state'),
  LOG_DIR: join(dir, 'logs'), SECRET_DIR: join(dir, 'secrets'),
  FRONTEND_DIR: fileURLToPath(new URL('../dist', import.meta.url)),
  WEB_PORT: String(PORT),
};
const remove = () => rm(dir, { recursive: true, force: true });

// First-run admin exactly as the container entrypoint seeds it (MUST_CHANGE_PASSWORD=true).
const seeded = spawnSync(bin, ['--mode', 'bootstrap-admin'], {
  env: { ...env, BOOTSTRAP_ADMIN_USER: ADMIN.user, BOOTSTRAP_ADMIN_PASS: ADMIN.initial }, stdio: 'inherit',
});
if (seeded.status !== 0) { console.error(seeded.error?.message ?? 'bootstrap-admin failed'); await remove(); process.exit(1); }

// Default mode (all): API plus daemon, as the container runs it. Its JSON log
// goes to test-results/server.log rather than flooding the reporter.
mkdirSync('test-results', { recursive: true });
const log = openSync('test-results/server.log', 'w');
const server = spawn(bin, [], { cwd: dir, env, stdio: ['ignore', log, log] });
for (const signal of ['SIGTERM', 'SIGINT']) process.on(signal, () => server.kill('SIGTERM'));
server.on('error', async (error) => { console.error(error.message); await remove(); process.exit(1); });
server.on('exit', async (code) => { await remove(); process.exit(code ?? 1); });
