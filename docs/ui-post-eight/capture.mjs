// Staged KyPost screenshots through real sign-in and a private synthetic mail fixture.
// Adapted from the archived 2026-10-26 council capture driver; no API/auth/CSP mocks.
import {createHash} from 'node:crypto';
import {readFile,writeFile,mkdir} from 'node:fs/promises';
import {execFileSync} from 'node:child_process';
import path from 'node:path';
const {chromium}=await import(process.env.KY_PLAYWRIGHT??'/home/yoshi/git/busnes.app/ky-ui/node_modules/@playwright/test/index.mjs');
const credentials=JSON.parse(await readFile(process.argv[2],'utf8'));
const ORIGIN=credentials.origin;
const [stage='00',phase='before']=process.argv.slice(3);
const requested=process.argv[5]?.split(',');
if(!/^\d{2}$/.test(stage)||!['before','after'].includes(phase))throw Error('Expected two-digit stage and before|after');
const OUT=path.resolve('docs/ui-post-eight/screenshots');
const sha=data=>createHash('sha256').update(data).digest('hex');
const UNREAD = ['(no body)', 'Re: Sandbox rollout plan', 'Welcome to the KyPost sandbox'];
async function resetFlags(p) {
  if (!p.url().startsWith(ORIGIN)) await p.goto(ORIGIN + '/read');
  await p.evaluate(async (unread) => {
    const csrf = document.cookie.match(/(?:^|; )csrf_token=([^;]*)/)?.[1] ?? '';
    const res = await fetch('/api/inbox?limit=500&bodies=0', { credentials: 'include' });
    const body = await res.json();
    const all = Object.values(body.byTab ?? {}).flat();
    const items = all.filter((x) => x.atUtc < '2026-10-01');
    const ids = (want) => items.filter((x) => want(x.subject)).map((x) => x.messageId);
    const drip = all.filter((x) => x.atUtc >= '2026-10-01').map((x) => x.messageId);
    for (const [action, messageIds] of [['delete', drip], ['unread', ids((s) => unread.includes(s))], ['read', ids((s) => !unread.includes(s))]]) {
      if (!messageIds.length) continue;
      const r = await fetch('/api/inbox/actions', { method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': decodeURIComponent(csrf) },
        body: JSON.stringify({ action, messageIds, mailbox: 'INBOX' }) });
      if (!r.ok) throw new Error(`${action} ${r.status} ${await r.text()}`);
    }
  }, UNREAD);
}
const inbox = async (p) => {
  await resetFlags(p);
  await p.goto(ORIGIN + '/read');
  await p.getByText('Welcome to the KyPost sandbox', { exact: false }).first().waitFor({ timeout: 30000 });
  await p.waitForTimeout(600);
};
const SURFACES = {
  inbox,
  'inbox-selection': async (p) => {
    await inbox(p);
    await p.getByRole('checkbox', { name: 'Select email Re: Sandbox rollout plan' }).check({ force: true });
    await p.getByRole('checkbox', { name: /Select email Your weekly digest/ }).check({ force: true });
    await p.waitForTimeout(300);
  },
  reader: async (p) => {
    await inbox(p);
    await p.getByRole('button', { name: 'Re: Sandbox rollout plan', exact: true }).first().click();
    await p.locator('dialog[open]').waitFor();
    if (!(await p.locator('dialog[open]').getByText('Alice Demo').count())) throw new Error('reader fixture drift: not the Alice Demo message');
    await p.waitForTimeout(1200);
  },
  contacts: async (p) => {
    await p.goto(ORIGIN + '/contacts');
    await p.getByText('Priya Natarajan', { exact: true }).first().waitFor({ timeout: 15000 });
    await p.waitForTimeout(400);
  },
  'contact-editor': async (p) => {
    await SURFACES.contacts(p);
    await p.getByRole('button', { name: 'New Contact', exact: true }).click();
    await p.locator('dialog[open]').waitFor();
    await p.waitForTimeout(400);
  },
  appearance: async (p) => {
    await p.goto(ORIGIN + '/settings/appearance');
    await p.getByRole('heading', { name: 'Appearance', exact: true }).waitFor();
    await p.waitForTimeout(400);
  },
  compose: async (p) => {
    await inbox(p);
    await p.getByRole('button', { name: 'New Email', exact: true }).first().click();
    await p.getByRole('heading', { name: 'New message', exact: true }).waitFor();
    await p.waitForTimeout(800);
  },
};

SURFACES.login=async p=>{await p.goto(ORIGIN+'/login');await p.getByRole('heading',{name:'Sign in',exact:true}).waitFor();await p.getByText('Security check verified.',{exact:true}).waitFor();};
await mkdir(OUT,{recursive:true});const shots=[];const browser=await chromium.launch();
const source_commit=execFileSync('git',['rev-parse','HEAD'],{encoding:'utf8'}).trim();
const sourceFiles=execFileSync('git',['ls-files','frontend/src','frontend/public','frontend/package.json','frontend/package-lock.json'],{encoding:'utf8'}).trim().split('\n');
const source_files={};for(const f of sourceFiles)source_files[f]=sha(await readFile(f));
try{for(const [device,width,height]of [['desktop',1280,900],['mobile',390,844]])for(const theme of ['light','dark'])for(const [surface,action]of Object.entries(SURFACES).filter(([name])=>!requested||requested.includes(name))){
 const ctx=await browser.newContext({viewport:{width,height},colorScheme:theme,locale:'en-US',timezoneId:'America/New_York',deviceScaleFactor:1});
 try{const page=await ctx.newPage();await page.goto(ORIGIN);
 if(surface!=='login'){await page.getByLabel('Username',{exact:true}).fill(credentials.user);await page.getByLabel('Password',{exact:true}).fill(credentials.password);await page.getByRole('button',{name:'Sign in',exact:true}).click();await page.waitForURL('**/read',{timeout:30000});}
 await action(page);await page.evaluate(async()=>{await document.fonts.ready;await Promise.allSettled(document.getAnimations().filter(a=>a.effect?.getComputedTiming().iterations!==Infinity).map(a=>a.finished));});await page.mouse.move(0,0);
 const file=`kypost-${stage}-${phase}-${surface}-${device}-${theme}.png`;const data=await page.screenshot({path:path.join(OUT,file)});
 const facts=await page.evaluate(()=>({overflow:document.documentElement.scrollWidth-document.documentElement.clientWidth,h1:[...document.querySelectorAll('h1')].map(x=>x.innerText),route:location.pathname,savedChoice:localStorage.getItem('kypost-theme'),fonts:[...document.fonts].filter(x=>x.status==='loaded').map(x=>x.family)}));
 const cohort={inbox:'mail-list','inbox-selection':'mail-list',reader:'mail-read',compose:'compose',contacts:'contacts-list','contact-editor':'contact-editor',appearance:'settings',login:'auth'}[surface];
 shots.push({file:'screenshots/'+file,sha256:sha(data),bytes:data.length,width,height,viewport:{width,height},surface,device,theme,cohort,state:surface==='inbox'||surface==='inbox-selection'?'four synthetic messages':surface==='compose'||surface==='contact-editor'?'open unsubmitted form':surface==='contacts'?'synthetic populated contacts':'initial view',...facts});console.log(file,JSON.stringify({overflow:facts.overflow,route:facts.route}));
 }finally{await ctx.close();}
}}finally{await browser.close();}
const fixtureRoot=path.dirname(process.argv[2]);
await writeFile(`docs/ui-post-eight/manifest-${stage}-${phase}.json`,JSON.stringify({product:'KyPost',source_commit,source_files,captured_at:new Date().toISOString(),driver:'Playwright; Chromium '+browser.version()+'; DPR1; normal service workers',browser:browser.version(),capture_driver_sha256:sha(await readFile(new URL(import.meta.url))),server_sha256:sha(await readFile(path.join(fixtureRoot,'server'))),locale:'en-US',timezone:'America/New_York',theme_source:'OS light/dark; no saved choice',fixture:'Private copy of historical synthetic council data; everyday alex; real PoW sign-in; own loopback TLS kypost-demo-IMAP; four seeded messages and populated contacts; no Ollama; no mocks, auth/CSRF/CSP bypass, send or submitted contact edit. Reader changes restored through native inbox actions. Demo drip set86400s.',shots},null,2)+'\n');
