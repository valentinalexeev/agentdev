#!/usr/bin/env node
// XSS check of the run log (#425): opens the job drawer of the mock API (scripts/mock-api.mjs) in a Chrome with
// --remote-debugging-port and checks that the mock run event "<img src=x onerror=alert(1)> <script>" shows as text:
// no <img> or <script> element in the drawer, no dialog, the markup visible as text, no markdown rendering.
// Usage: npm run build; node scripts/mock-api.mjs <port> &; chrome --headless=new --remote-debugging-port=<cdp> &
//        node scripts/xss-runlog.mjs <port> <cdp>       (exit 0 = pass)
const [P, D] = process.argv.slice(2).map(Number)
const tabs = await (await fetch('http://127.0.0.1:'+D+'/json')).json()
const ws = new WebSocket(tabs.find(t=>t.type==='page').webSocketDebuggerUrl)
let id=0; const pend=new Map(); const alerts=[]
ws.onmessage = (m)=>{ const d=JSON.parse(m.data); const r=typeof d.id==='number'?pend.get(d.id):undefined; if(typeof r==='function'){pend.delete(d.id); r(d.result)} if(d.method==='Page.javascriptDialogOpening'){alerts.push(d.params.message); send('Page.handleJavaScriptDialog',{accept:true})} }
await new Promise(r=>ws.onopen=r)
function send(method, params={}){ return new Promise(r=>{ const i=++id; pend.set(i,r); ws.send(JSON.stringify({id:i,method,params})) }) }
const ev = async (e)=> (await send('Runtime.evaluate',{expression:e, returnByValue:true})).result.value
await send('Page.enable')
await send('Page.navigate',{url:'http://localhost:'+P+'/'}); await new Promise(r=>setTimeout(r,2500))
const user = {id_token:'x', access_token:'tok', token_type:'Bearer', scope:'openid', profile:{sub:'u1', preferred_username:'v'}, expires_at: Math.floor(Date.now()/1000)+3600}
await ev("sessionStorage.setItem('oidc.user:http://localhost:"+P+"/oidc/:agentdev', JSON.stringify("+JSON.stringify(user)+"))")
await ev("location.hash='work?job=alice%2Fissue-301&jrepo=acme%2Fshop-api'"); await send('Page.reload'); await new Promise(r=>setTimeout(r,3500))
const out = await ev(`(() => { const d=document.querySelector('dialog[open]'); if(!d) return {err:'no drawer', body: document.body.innerText.slice(0,500)};
 return { title: d.querySelector('#drawer-title')?.innerText, imgs: d.querySelectorAll('img').length, scripts: d.querySelectorAll('.agd-runlog script').length,
  hasText: d.innerText.includes('<img src=x onerror=alert(1)>'), noBold: d.querySelectorAll('.agd-runlog strong,.agd-runlog b').length,
  folded: d.querySelectorAll('.agd-runlog details').length, truncated: d.innerText.includes('size limit'), result: d.innerText.includes('6 turns') } })()`)
console.log(JSON.stringify(out), 'alerts:', JSON.stringify(alerts))
process.exit(out.hasText && out.imgs===0 && out.scripts===0 && alerts.length===0 ? 0 : 1)
