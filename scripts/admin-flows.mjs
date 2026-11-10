import {createServer} from 'node:http';
import assert from 'node:assert/strict';

export async function runAdminFlows(browser, base, password, out) {
 const upstream=createServer(async(req,res)=>{
  let raw='';for await(const chunk of req) raw+=chunk;
  if(req.url.endsWith('/models')) {res.setHeader('Content-Type','application/json');res.end(JSON.stringify({data:[{id:'mock-model'}]}));return;}
  const body=raw?JSON.parse(raw):{};
  if(body.stream) {
   res.setHeader('Content-Type','text/event-stream');
   if(body.messages?.at(-1)?.content === 'slow local test') {
    res.write('data: '+JSON.stringify({choices:[{delta:{content:'started'}}]})+'\n\n');
    const timer=setTimeout(()=>res.end('data: [DONE]\n\n'),5000);
    res.on('close',()=>clearTimeout(timer));return;
   }
   res.end('data: '+JSON.stringify({choices:[{delta:{content:'mock reply'}}]})+'\n\ndata: '+JSON.stringify({choices:[],usage:{prompt_tokens:10,completion_tokens:5}})+'\n\ndata: [DONE]\n\n');
  } else {res.setHeader('Content-Type','application/json');res.end(JSON.stringify({choices:[{message:{role:'assistant',content:'mock reply'}}],usage:{prompt_tokens:10,completion_tokens:5}}));}
 });
 await new Promise(r=>upstream.listen(0,'127.0.0.1',r));
 const upstreamURL=`http://127.0.0.1:${upstream.address().port}/v1`;
 try {
  const context=await browser.newContext({viewport:{width:1280,height:900}});
  const page=await context.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
  page.on('dialog',d=>d.accept());
  await page.goto(base+'/login.html');
  await page.locator('#pw').fill('incorrect-local-test-password');await page.locator('#btn').click();
  await page.waitForFunction(()=>document.querySelector('#err').textContent.length>0);
  await page.locator('#pw').fill(password);await page.locator('#btn').click();await page.waitForURL(base+'/');
  const token=await page.evaluate(()=>localStorage.getItem('lg_token'));
  async function admin(path,method='GET',body) {
   const res=await fetch(base+path,{method,headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});
   assert(res.ok,`admin ${path} status ${res.status}`);return res.json();
  }
  await page.goto(base+'/channels.html');await page.locator('#btnNew').click();
  await page.locator('#mSave').click();await page.waitForFunction(()=>document.querySelector('#mErr').textContent.length>0);
  await page.locator('#fName').fill('browser-mock');await page.locator('#fBase').fill(upstreamURL);await page.locator('#fKeys').fill('local-mock-only');
  await page.locator('#fNewModel').fill('mock-model');await page.locator('#btnAddModel').click();await page.locator('#mSave').click();
  await page.locator('#modal').waitFor({state:'hidden'});await page.getByText('browser-mock',{exact:true}).waitFor();
  let row=page.locator('#rows tr').filter({hasText:'browser-mock'});
  await row.getByRole('button',{name:'测试',exact:true}).click();
  await page.waitForFunction(()=>document.body.textContent.includes('可用') || document.body.textContent.includes('成功'));
  await row.getByRole('button',{name:'编辑',exact:true}).click();await page.locator('#fRemark').fill('edited in browser');await page.locator('#mSave').click();await page.locator('#modal').waitFor({state:'hidden'});
  assert.equal((await admin('/api/admin/channels'))[0].remark,'edited in browser');
  await page.goto(base+'/prices.html');await page.locator('#btnNew').click();
  await page.locator('#fModel').fill('mock-model');await page.locator('#fIn').fill('2');await page.locator('#fOut').fill('4');await page.locator('#fCurrency').selectOption('USD');
  await page.locator('#formUp button[type="submit"]').click();await page.locator('#formUp').waitFor({state:'hidden'});
  await page.goto(base+'/keys.html');await page.locator('#fName').fill('browser-key');await page.locator('#formNew button').click();await page.getByText('browser-key',{exact:true}).waitFor();
  row=page.locator('#rows tr').filter({hasText:'browser-key'});await row.getByRole('button',{name:'编辑',exact:true}).click();
  await page.locator('#eBudgetTok').fill('1000');await page.locator('#eBudgetPeriod').selectOption('monthly');await page.locator('#eSave').click();await page.locator('#editModal').waitFor({state:'hidden'});
  assert.equal((await admin('/api/admin/keys')).find(k=>k.name==='browser-key').budget_tokens,1000);
  await row.getByRole('button',{name:'趋势',exact:true}).click();await page.locator('#trendBars').waitFor();await page.locator('#tClose').click();
  await page.goto(base+'/settings.html');await page.locator('#rateVal').fill('0');await page.locator('#rSave').click();await page.waitForFunction(()=>document.querySelector('#rErr').textContent.length>0);
  await page.locator('#rateVal').fill('7.5');await page.locator('#rSave').click();
  await page.waitForFunction(()=>document.querySelector('#rErr').textContent==='');
  assert.equal((await admin('/api/admin/budget')).usd_cny_rate,7.5);
  await page.locator('#roBtn').click();await page.locator('#roTok').waitFor({state:'visible'});
  const readonly=await page.locator('#roTok').inputValue();
  const denied=await fetch(base+'/api/admin/keys',{method:'POST',headers:{Authorization:'Bearer '+readonly},body:JSON.stringify({name:'must-not-create'})});assert.equal(denied.status,401);
  await page.goto(base+'/playground.html');await page.waitForFunction(()=>document.querySelector('#chipName').textContent==='mock-model');
  await page.locator('#fInput').fill('local test');
  const responsePromise=page.waitForResponse(r=>r.url().endsWith('/api/admin/playground/chat'));
  await page.locator('#btnSend').click();const chatResponse=await responsePromise;
  assert(chatResponse.ok(),`playground response ${chatResponse.status()}: ${await chatResponse.text()}`);
  await page.locator('span.answer').filter({hasText:'mock reply'}).waitFor();
  await page.locator('#fInput').fill('slow local test');await page.locator('#btnSend').click();
  await page.locator('span.answer').filter({hasText:'started'}).waitFor();
  await page.locator('#btnStop').click();await page.locator('#btnSend').waitFor({state:'visible'});
  const logs=await admin('/api/admin/logs');assert(logs.items.some(l=>l.model==='mock-model' && l.prompt_tokens===10),'playground usage not recorded');
  for(const width of [320,390,1280]) {
   await page.setViewportSize({width,height:900});
   for(const path of ['/','/channels.html','/keys.html','/logs.html','/prices.html','/balances.html','/settings.html','/playground.html']) {
    await page.goto(base+path,{waitUntil:'networkidle'});
    assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),`overflow ${width} ${path}`);
    await page.screenshot({path:out+`/${path==='/'?'dashboard':path.slice(1,-5)}-flow-${width}.png`,fullPage:true});
   }
   await page.screenshot({path:out+`/playground-${width}.png`,fullPage:true});
  }
  await page.setViewportSize({width:1280,height:900});
  for(const [path,text] of [['/keys.html','browser-key'],['/prices.html','mock-model'],['/channels.html','browser-mock']]) {
   await page.goto(base+path);const row=page.locator('#rows tr').filter({hasText:text});await row.getByRole('button',{name:'删除',exact:true}).click();await row.waitFor({state:'hidden'});
  }
  assert.deepEqual(errors,[]);
  console.log(JSON.stringify({adminFlows:true,loginError:true,channelCRUD:true,channelTest:true,priceCreateDelete:true,keyCRUD:true,budgetValidation:true,readonlyWriteDenied:true,playgroundStream:true,playgroundCancel:true,pages:8,widths:[320,390,1280],jsErrors:0}));
  await context.close();
 } finally {await new Promise(r=>{upstream.close(r);upstream.closeAllConnections();});}
}
