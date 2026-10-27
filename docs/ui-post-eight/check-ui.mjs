// Real-browser behavior and geometry checks on the owned synthetic fixture only.
import {readFile,writeFile} from 'node:fs/promises';
import {execFileSync} from 'node:child_process';
import {chromium,expect} from '/home/yoshi/git/busnes.app/ky-ui/node_modules/@playwright/test/index.mjs';
const creds=JSON.parse(await readFile(process.argv[2],'utf8'));const results=[];
const browser=await chromium.launch();
const source_commit=execFileSync('git',['rev-parse','HEAD'],{encoding:'utf8'}).trim();
async function contained(locator,page){const b=await locator.boundingBox();const v=page.viewportSize();expect(b.x).toBeGreaterThanOrEqual(0);expect(b.y).toBeGreaterThanOrEqual(0);expect(b.x+b.width).toBeLessThanOrEqual(v.width+1);expect(b.y+b.height).toBeLessThanOrEqual(v.height+1);return b;}
try{for(const [device,width,height]of [['desktop',1280,900],['mobile',390,844]])for(const theme of ['light','dark']){
 const ctx=await browser.newContext({viewport:{width,height},deviceScaleFactor:1,colorScheme:theme,locale:'en-US',timezoneId:'America/New_York'});
 try{const page=await ctx.newPage();await page.goto(creds.origin+'/login');await expect(page.getByText('Security check verified.',{exact:true})).toBeVisible();
 const contrast=await page.evaluate(()=>{
  const c=document.createElement('canvas');c.width=c.height=1;const ctx=c.getContext('2d');
  function rgba(s){ctx.clearRect(0,0,1,1);ctx.fillStyle=s;ctx.fillRect(0,0,1,1);return [...ctx.getImageData(0,0,1,1).data].map((v,i)=>i===3?v/255:v);}
  function composite(a,b){return a.slice(0,3).map((v,i)=>v*a[3]+b[i]*(1-a[3]));}
  function lum(a){return a.slice(0,3).map(x=>{x/=255;return x<=.04045?x/12.92:((x+.055)/1.055)**2.4;}).reduce((s,v,i)=>s+v*[.2126,.7152,.0722][i],0);}
  function ratio(a,b){const aa=lum(a),bb=lum(b);return (Math.max(aa,bb)+.05)/(Math.min(aa,bb)+.05);}
  const card=getComputedStyle(document.querySelector('.auth-card'));const bg=rgba(card.backgroundColor);const label=getComputedStyle(document.querySelector('.auth-label'));const input=getComputedStyle(document.querySelector('.auth-input'));const ibg=composite(rgba(input.backgroundColor),bg);const success=getComputedStyle(document.querySelector('.auth-pow-done'));
  return {label:ratio(composite(rgba(label.color),bg),bg),border:ratio(composite(rgba(input.borderColor),ibg),ibg),success:ratio(composite(rgba(success.color),bg),bg),opacity:card.opacity,labelFont:label.fontFamily};
 });expect(contrast.opacity).toBe('1');expect(contrast.label).toBeGreaterThanOrEqual(4.5);expect(contrast.border).toBeGreaterThanOrEqual(3);expect(contrast.success).toBeGreaterThanOrEqual(4.5);
 await page.getByLabel('Username',{exact:true}).fill(creds.user);await page.getByLabel('Password',{exact:true}).fill(creds.password);await page.getByRole('button',{name:'Sign in',exact:true}).click();await page.waitForURL('**/read');
 if(device==='mobile'){for(const cell of await page.locator('.inbox-row-meta').all()){const c=await cell.boundingBox(),date=await cell.locator('> span:nth-child(2)').boundingBox();expect(date.x+date.width).toBeLessThanOrEqual(c.x+c.width+1);}}
 await page.getByRole('checkbox',{name:'Select email Re: Sandbox rollout plan'}).check({force:true});
 const selectAll=page.getByRole('checkbox',{name:'Select all emails in page',exact:true});
 await expect(selectAll).toHaveJSProperty('indeterminate',true);
 for(const target of await page.locator('.inbox-select-hit').all()){const box=await target.boundingBox();expect(box.width).toBeGreaterThanOrEqual(40);expect(box.height).toBeGreaterThanOrEqual(40);}
 const bulk=page.locator('.inbox-action-bar');
 expect(await bulk.evaluate(e=>e.scrollWidth-e.clientWidth)).toBe(0);
 for(const b of await bulk.getByRole('button').all())await contained(b,page);
 await page.getByRole('checkbox',{name:'Select email Re: Sandbox rollout plan'}).uncheck({force:true});
 await expect(selectAll).toHaveJSProperty('indeterminate',false);
 const subject=page.getByRole('button',{name:'Re: Sandbox rollout plan',exact:true});await subject.click();const dialog=page.getByRole('dialog',{name:'Re: Sandbox rollout plan',exact:true});await expect(dialog.getByRole('button',{name:'Reply',exact:true})).toBeEnabled();
 const replyFont=await dialog.getByRole('button',{name:'Reply',exact:true}).evaluate(e=>getComputedStyle(e).fontSize);
 expect(await dialog.getByRole('button',{name:'More actions',exact:true}).evaluate(e=>getComputedStyle(e).fontSize)).toBe(replyFont);
 const more=dialog.getByRole('button',{name:'More actions',exact:true});const menu=dialog.locator('.email-reader-tools');await expect(menu).toBeHidden();await expect(dialog.getByPlaceholder('Add keyword')).toBeHidden();
 const text=dialog.locator('pre[key],.email-reader-body-block').first();expect(await text.evaluate(e=>getComputedStyle(e).fontFamily)).toContain('Space Grotesk');
 await more.focus();await page.keyboard.press('Space');await expect(menu).toBeVisible();const readerMenu=await contained(menu,page);await page.keyboard.press('Escape');await expect(menu).toBeHidden();await expect(more).toBeFocused();
 await more.click();const remote=menu.getByRole('button',{name:'Show Remote Content',exact:true});await remote.focus();await page.keyboard.press('Enter');await expect(menu.getByRole('button',{name:'Hide Remote Content',exact:true})).toBeFocused();await expect(menu).toBeVisible();await page.keyboard.press('Escape');await expect(more).toBeFocused();
 await more.click();await menu.getByRole('button',{name:'Hide Remote Content',exact:true}).click();await page.keyboard.press('Escape');
 await more.click();await menu.getByRole('button',{name:'Mark as unread',exact:true}).click();await expect(menu).toBeHidden();await more.click();await expect(menu.getByRole('button',{name:'Mark as Read',exact:true})).toBeEnabled();await menu.getByRole('button',{name:'Mark as Read',exact:true}).click();await expect(menu).toBeHidden();
 await dialog.locator('summary').click();await expect(dialog.getByPlaceholder('Add keyword')).toBeVisible();await dialog.getByRole('button',{name:'View raw email',exact:true}).click();expect(await dialog.locator('.email-reader-raw').evaluate(e=>getComputedStyle(e).fontFamily)).toContain('IBM Plex Mono');
 await page.keyboard.press('Escape');await expect(dialog).not.toBeVisible();await expect(subject).toBeFocused();
 await page.getByRole('button',{name:'New Email',exact:true}).first().click();const compose=page.getByRole('dialog',{name:'New message',exact:true});await expect(compose.locator('.ql-editor')).toBeVisible();const editor=await compose.locator('.compose-editor-html').boundingBox(),footer=await compose.locator('.compose-footer').boundingBox(),toolbar=await compose.locator('.ql-toolbar').boundingBox();expect(editor.y+editor.height).toBeLessThanOrEqual(footer.y+1);expect(Math.abs(editor.x-toolbar.x)).toBeLessThanOrEqual(1);expect(Math.abs(editor.width-toolbar.width)).toBeLessThanOrEqual(1);expect(await compose.locator('.ql-picker').evaluate(e=>getComputedStyle(e).fontFamily)).toContain('Space Grotesk');expect(toolbar.height).toBeLessThanOrEqual(62);expect(await compose.locator('.ql-toolbar button svg').evaluateAll(nodes=>Math.max(...nodes.map(n=>n.getBoundingClientRect().height)))).toBeLessThanOrEqual(20);await compose.getByRole('button',{name:'Close',exact:true}).click();
 await page.goto(creds.origin+'/contacts');await expect(page.getByRole('button',{name:'New Contact',exact:true})).toBeVisible();expect((await page.getByRole('button',{name:'New Contact',exact:true}).boundingBox()).height).toBeGreaterThanOrEqual(40);const cm=page.locator('.contacts-utility-menu');await expect(cm).toBeHidden();await page.getByRole('button',{name:'More actions',exact:true}).click();await expect(cm.getByRole('link',{name:'Export vCard',exact:true})).toBeVisible();const contactMenu=await contained(cm,page);await page.keyboard.press('Escape');await expect(page.getByRole('button',{name:'More actions',exact:true})).toBeFocused();
 await page.getByRole('button',{name:'New Contact',exact:true}).click();const form=page.getByRole('dialog',{name:'Add Contact',exact:true});await form.getByRole('button',{name:'+ Add email',exact:true}).click();await expect(form.locator('input[type=email]').first()).toBeVisible();await expect(form.locator('input[type=file]')).toHaveCount(0);await expect(form.locator('.contact-identity-details')).not.toHaveAttribute('open');await form.getByRole('button',{name:'Close',exact:true}).click();await expect(page.getByRole('button',{name:'New Contact',exact:true})).toBeFocused();
 await page.goto(creds.origin+'/settings/appearance');await expect(page.getByRole('heading',{name:'Appearance',exact:true})).toBeVisible();if(device==='mobile'){const options=page.locator('.ky-appearance-option');const a=await options.nth(0).boundingBox(),b=await options.nth(1).boundingBox();expect(Math.abs(a.y-b.y)).toBeLessThanOrEqual(1);expect(b.x).toBeGreaterThan(a.x);}
 expect(await page.evaluate(()=>document.documentElement.scrollWidth-innerWidth)).toBe(0);
 results.push({device,theme,contrast,readerMenu,contactMenu,compose:{editor,footer,toolbar},status:'pass'});
 console.log(device,theme,'pass');
 }finally{await ctx.close();}
}}finally{await browser.close();}
await writeFile(`docs/ui-post-eight/checks/${process.argv[3]??'r2'}-browser.json`,JSON.stringify({source_commit,browser:browser.version(),dpr:1,results,limits:'Four focused synthetic cases; not complete roles/states/all-theme/assistive-tech acceptance.'},null,2)+'\n');
