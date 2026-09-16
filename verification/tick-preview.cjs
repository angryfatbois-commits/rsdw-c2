const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

const capture = JSON.parse(fs.readFileSync(path.join(__dirname, 'tick-live-observations.json'), 'utf8'));
const fields = {tickRate:['rateHz','ticks/second'],tickP50Ms:['p50','milliseconds'],tickP95Ms:['p95','milliseconds'],tickP99Ms:['p99','milliseconds'],tickWindowSeconds:['windowSeconds','seconds'],tickSampleCount:['sampleCount','ticks']};
const samples = capture.observations.map(({payload}) => {
  const tick = payload.tick;
  const observed = new Date(tick.observedAtUnixMs).toISOString();
  const sample = {timestamp:new Date(tick.observedAtUnixMs + tick.lastSampleAgeSeconds * 1000).toISOString(),observedAt:{},status:{}};
  for (const [key,[field]] of Object.entries(fields)) {
    sample[key] = tick[field] ?? tick.executionMs[field];
    sample.observedAt[key] = observed;
    sample.status[key] = 'available';
  }
  return sample;
});
const latest = samples.at(-1);
const metrics = Object.fromEntries(Object.entries(fields).map(([key,[,unit]]) => [key,{value:latest[key],status:'available',observedAt:latest.observedAt[key],unit,source:'Recorded game API /api/metrics',reason:''}]));
const server = {id:'recorded-ticks',name:'Recorded empty-world server',maxPlayers:4,status:'online',metrics};
const staticFiles = {'/':'index.html','/app.js':'app.js','/style.css':'style.css','/styles.css':'styles.css'};
http.createServer((request,response) => {
  const url = new URL(request.url,'http://127.0.0.1');
  response.setHeader('Cache-Control','no-store');
  if (request.method !== 'GET') { response.writeHead(405,{'Content-Type':'application/json'}); response.end(JSON.stringify({error:'Recorded UI verification only. No live server actions.'})); return; }
  let body;
  if (url.pathname === '/api/bootstrap') body = {mode:'demo',cluster:'Recorded actual-server telemetry. UI verification only.',servers:[server]};
  else if (url.pathname === '/api/events') body = [];
  else if (url.pathname.endsWith('/logs')) body = {lines:['Recorded actual-server measurements. The game and kind are stopped.']};
  else if (url.pathname.endsWith('/telemetry')) {
    const seconds = {'60s':60,'5m':300,'1h':3600}[url.searchParams.get('range')] || 60;
    body = {server,metrics,healthChecks:[],metricDefinitions:[],samples:samples.filter(item => Date.parse(item.timestamp) >= Date.parse(latest.timestamp)-seconds*1000)};
  }
  if (body !== undefined) { response.writeHead(200,{'Content-Type':'application/json'}); response.end(JSON.stringify(body)); return; }
  const file = staticFiles[url.pathname];
  if (!file || !fs.existsSync(path.join(__dirname,'..','web',file))) { response.writeHead(404); response.end(); return; }
  response.writeHead(200,{'Content-Type':file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html'});
  response.end(fs.readFileSync(path.join(__dirname,'..','web',file)));
}).listen(18085,'127.0.0.1',() => console.log('Recorded telemetry UI at http://127.0.0.1:18085/#telemetry'));
