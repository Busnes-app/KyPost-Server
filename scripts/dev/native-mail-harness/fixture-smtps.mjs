// Boots the kypost-demo-IMAP fixture unchanged, then adds the one listener the
// domain relay needs and the fixture lacks: SMTP over implicit TLS (KyPost's
// relay refuses STARTTLS). It reuses the fixture's own SMTP session, store and
// TLS material in the same process, so relayed mail lands in the
// authenticated persona's "Sent Items", readable over the fixture's IMAPS.
// Usage: node fixture-smtps.mjs FIXTURE_CHECKOUT   (env as for the fixture, plus SMTPS_PORT)
import tls from 'node:tls';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const root = process.argv[2];
const mod = (p) => import(pathToFileURL(join(root, 'src', p)).href);
await mod('index.js');
const { createSmtpSession } = await mod('smtp.js');
const { ensureCert } = await mod('tls.js');
const env = process.env;
const { key, cert } = ensureCert({ keyDir: env.TLS_KEY_DIR, publishDir: env.TLS_PUBLISH_DIR, hostnames: [], ips: [], log: () => {} });
const allowed = new Set(env.ALLOWED_CLIENT_IPS.split(','));
const log = (...a) => console.log(new Date().toISOString(), 'smtps', ...a);
const server = tls.createServer({ key, cert, minVersion: 'TLSv1.2' }, (socket) => {
  socket.on('error', (e) => log('socket error', e.message));
  createSmtpSession(socket, { log, secureContext: null, secure: true, allowLogin: (u, p) => Boolean(u && p) });
});
server.prependListener('connection', (s) => { if (!allowed.has(s.remoteAddress?.replace(/^::ffff:/, ''))) s.destroy(); });
server.listen(Number(env.SMTPS_PORT), env.BIND_ADDRESS, () => log(`listening on ${env.BIND_ADDRESS}:${env.SMTPS_PORT}`));
