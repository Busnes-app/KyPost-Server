// TLS front for the harness's KyIdentity, which serves plain HTTP behind a
// proxy in production. Usage: node tls-front.mjs LISTEN_IP:PORT UPSTREAM_IP:PORT CERT KEY
// KyIdentity trusts X-Forwarded-For only from TRUSTED_PROXY_CIDRS (127.0.0.1).
import https from 'node:https';
import http from 'node:http';
import { readFileSync } from 'node:fs';

const [listen, upstream, cert, key] = process.argv.slice(2);
const [lhost, lport] = listen.split(':');
const [uhost, uport] = upstream.split(':');
https.createServer({ cert: readFileSync(cert), key: readFileSync(key), minVersion: 'TLSv1.2' }, (req, res) => {
  const headers = { ...req.headers, 'x-forwarded-for': req.socket.remoteAddress, 'x-forwarded-proto': 'https' };
  const up = http.request({ host: uhost, port: uport, method: req.method, path: req.url, headers }, (r) => {
    res.writeHead(r.statusCode, r.rawHeaders);
    r.pipe(res);
  });
  up.on('error', () => { res.writeHead(502); res.end(); });
  req.pipe(up);
}).listen(Number(lport), lhost, () => console.log(`tls-front ${listen} -> ${upstream}`));
