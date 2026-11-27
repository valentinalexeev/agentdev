#!/usr/bin/env node
// Permission mode in the console (#505): opens the agent drawer of the mock API (scripts/mock-api.mjs) in a Chrome with
// --remote-debugging-port and checks, as an agentdev-admins member:
//   - the Overview shows the mode and the Permission tab shows mode auto with the three rule lists, read-only
//     (no input, no form), the rule "<img src=x onerror=alert(1)>" as text (no <img>, no dialog)
//   - the Audit page tells the story of the "permission" entry with the old and new mode
// and as a member without agentdev-admins: no Permission tab, no mode.
// Usage: npm run build; node scripts/mock-api.mjs <port> &; chrome --headless=new --remote-debugging-port=<cdp> &
//        node scripts/permission-ui.mjs <port> <cdp>       (exit 0 = pass)
const [P, D] = process.argv.slice(2).map(Number)
const tabs = await (await fetch('http://127.0.0.1:'+D+'/json')).json()
const ws = new WebSocket(tabs.find(t=>t.type==='page').webSocketDebuggerUrl)
let id=0; const pend=new Map(); const alerts=[]
ws.onmessage = (m)=>{ const d=JSON.parse(m.data); const r=typeof d.id==='number'?pend.get(d.id):undefined; if(typeof r==='function'){pend.delete(d.id); r(d.result)} if(d.method==='Page.javascriptDialogOpening'){alerts.push(d.params.message); send('Page.handleJavaScriptDialog',{accept:true})} }
await new Promise(r=>ws.onopen=r)
function send(method, params={}){ return new Promise(r=>{ const i=++id; pend.set(i,r); ws.send(JSON.stringify({id:i,method,params})) }) }
const ev = async (e)=> (await send('Runtime.evaluate',{expression:e, returnByValue:true})).result.value
const wait = (ms)=>new Promise(r=>setTimeout(r,ms))
const open = async (hash)=>{ await ev("location.hash='"+hash+"'"); await send('Page.reload'); await wait(3000) }
await send('Page.enable')
await fetch('http://localhost:'+P+'/mock/admin')
await send('Page.navigate',{url:'http://localhost:'+P+'/'}); await wait(2500)
const user = {id_token:'x', access_token:'tok', token_type:'Bearer', scope:'openid', profile:{sub:'u1', preferred_username:'v'}, expires_at: Math.floor(Date.now()/1000)+3600}
await ev("sessionStorage.setItem('oidc.user:http://localhost:"+P+"/oidc/:agentdev', JSON.stringify("+JSON.stringify(user)+"))")

const fails = []
const check = (ok, what, got) => { if (!ok) fails.push(what+': '+JSON.stringify(got)) }

await open('agents/alice')
const overview = await ev(`(() => { const d=document.querySelector('dialog[open]'); return d ? d.innerText : '' })()`)
check(/Permission mode\s*auto/.test(overview), 'overview shows the mode', overview.slice(0, 400))
await ev(`document.getElementById('agent-tab-permission')?.click()`); await wait(500)
const perm = await ev(`(() => { const d=document.querySelector('dialog[open]'); const p=d && d.querySelector('[role=tabpanel]'); if(!p) return null;
  return { text: p.innerText, inputs: p.querySelectorAll('input,textarea,select,form,button').length, imgs: p.querySelectorAll('img').length } })()`)
check(perm && /Mode\s*auto/.test(perm.text), 'permission tab mode', perm)
check(perm && perm.text.includes('Install dependencies declared in the repository') && perm.text.includes('Deploy to production'), 'rule lists', perm)
check(perm && /Environment\s*none/.test(perm.text), 'empty list shows none', perm)
check(perm && perm.text.includes('<img src=x onerror=alert(1)>') && perm.imgs === 0, 'rule shown as text', perm)
check(perm && perm.inputs === 0, 'read-only (no inputs)', perm)

await open('audit')
const audit = await ev(`document.querySelector('main')?.innerText ?? document.body.innerText`)
check(/permission mode of\s*alice\s*changed/i.test(audit) && audit.includes('dontAsk → auto (1 allow, 1 soft deny, 0 environment rules)'), 'audit story', audit.slice(0, 600))
check(audit.includes('unknown → auto (default rules)') && audit.includes('first seen (no earlier entry)'), 'first seen story', audit.slice(0, 900))

await fetch('http://localhost:'+P+'/mock/member')
await open('agents/alice')
const member = await ev(`(() => { const d=document.querySelector('dialog[open]'); return d ? { tab: !!document.getElementById('agent-tab-permission'), text: d.innerText } : null })()`)
await fetch('http://localhost:'+P+'/mock/admin')
check(member && !member.tab && !member.text.includes('Permission mode'), 'member sees no permission', member && member.text.slice(0, 300))
check(alerts.length === 0, 'no dialogs', alerts)

console.log(fails.length ? 'FAIL\n'+fails.join('\n') : 'PASS: mode and rules shown read-only to admins, audit story, hidden from members')
process.exit(fails.length ? 1 : 0)
