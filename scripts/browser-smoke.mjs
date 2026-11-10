// Optional development dependency: PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs
// node scripts/browser-smoke.mjs /path/to/litegate /path/to/output-directory
import {spawn} from 'node:child_process';
import {randomBytes} from 'node:crypto';
import {mkdtemp, mkdir, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join, resolve} from 'node:path';
import assert from 'node:assert/strict';
import {runAdminFlows} from './admin-flows.mjs';
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const work = await mkdtemp(join(tmpdir(), 'litegate-browser-'));
const out = resolve(process.argv[3] || '/tmp/litegate-browser-results');
await mkdir(out, {recursive: true});
const password = randomBytes(32).toString('hex');
const base = 'http://127.0.0.1:18083';
const server = spawn(resolve(process.argv[2]), ['-addr','127.0.0.1:18083','-db',join(work,'test.db')], {
 env:{...process.env,LITEGATE_ADMIN_PASSWORD:password}, stdio:'ignore',
});
let browser;
try {
 let healthy = false;
 for(let i=0;i<100;i++) {
  try {const r=await fetch(base+'/healthz?deep=1'); if(r.ok){healthy=true;break;}} catch {}
  await new Promise(r=>setTimeout(r,100));
 }
 assert(healthy,'server health check failed');
 const login=await fetch(base+'/api/admin/login',{method:'POST',body:JSON.stringify({password})});
 assert(login.ok,'login failed');
 const {token}=await login.json();
 const dashboard=await fetch(base+'/api/admin/dashboard',{headers:{Authorization:'Bearer '+token}}).then(r=>r.json());
 assert(dashboard.channels===0 && dashboard.keys>0,'fresh database fixture must include default key');
 browser=await chromium.launch({executablePath:process.env.CHROMIUM || '/usr/bin/chromium',headless:true,args:['--no-sandbox','--disable-dev-shm-usage']});
 for(const width of [320,390,1280]) {
  const context=await browser.newContext({viewport:{width,height:900},timezoneId:'Asia/Shanghai'});
  await context.addInitScript(t=>localStorage.setItem('lg_token',t),token);
  const page=await context.newPage();const errors=[];
  page.on('pageerror',e=>errors.push(e.message));
  await page.goto(base,{waitUntil:'networkidle'});
  assert(await page.locator('#quickStart').isVisible(),'default key hid onboarding');
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'horizontal overflow');
  if(width<900) {
   await page.getByRole('button',{name:'打开菜单'}).click();
   assert.equal(await page.locator('.topbar-icon').getAttribute('aria-expanded'),'true');
   assert(await page.locator('.mnav-close').evaluate(el=>el===document.activeElement),'focus not moved to drawer');
   await page.keyboard.press('Shift+Tab');
   assert(await page.locator('.mnav-drawer').evaluate(el=>el.contains(document.activeElement)),'focus escaped drawer');
   await page.keyboard.press('Escape');
   assert.equal(await page.locator('.topbar-icon').getAttribute('aria-expanded'),'false');
   assert(await page.locator('.topbar-icon').evaluate(el=>el===document.activeElement),'focus not restored');
  }
  await page.screenshot({path:join(out,`dashboard-${width}.png`),fullPage:true});
  // Deterministic chart fixture verifies currency series and UTC in a non-UTC browser.
  const date=new Date().toISOString().slice(0,10);
  await page.evaluate(date=>{
   trendData={bucket:'hour',series:[{name:'mixed',points:[{t:date+' 00:00',tokens:30,cache:0,tps:2.5,cost:0,cost_by_currency:{USD:1,CNY:7}}]}]};
   renderTrend();
  },date);
  const chart=await page.evaluate(()=>({cost:trendXY('cost'),usage:trendXY('tokens'),speed:trendXY('tps')}));
  assert.deepEqual(chart.cost.names.sort(),['mixed (CNY)','mixed (USD)']);
  assert.equal(chart.usage.vals[0][0],30);
  assert.equal(chart.speed.vals[0][0],2.5);
  assert.equal(chart.cost.xs[0],Date.parse(date+'T00:00:00Z')/1000);
  await page.screenshot({path:join(out,`currency-fixture-${width}.png`),fullPage:true});
  const hostileNames = await page.evaluate(date=>{
   window.__chartXSS = false;
   const names = ['__proto__', 'constructor', '<img src=x onerror="window.__chartXSS=true">'];
   trendData={bucket:'hour',series:names.map(name=>({name,points:[{t:date+' 00:00',tokens:30,cache:0,tps:2.5,cost:1,cost_by_currency:{USD:1}}]}))};
   renderTrend();
   return {names:trendXY('tokens').names, vals:trendXY('tokens').vals, injected:document.querySelectorAll('#modelChart img, #costChart img').length, executed:window.__chartXSS};
  },date);
  assert.equal(hostileNames.names.length,3,'prototype-named series was lost');
  assert(hostileNames.vals.every(v=>v[0]===30));
  assert.equal(hostileNames.injected,0);assert.equal(hostileNames.executed,false);

  assert.deepEqual(errors,[],'JavaScript errors');
  console.log(JSON.stringify({width,health:true,login:true,onboarding:true,drawer:width<900,utc:true,currency:true,jsErrors:errors.length}));
  await context.close();
 }
 if(process.env.ADMIN_FLOWS === "1") await runAdminFlows(browser,base,password,out);
} finally {
 if(browser) await browser.close();
 server.kill('SIGTERM');
 await new Promise(r=>{server.once('exit',r);setTimeout(r,2000).unref();});
 await rm(work,{recursive:true,force:true});
}
