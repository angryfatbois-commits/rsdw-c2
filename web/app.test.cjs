const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(`${__dirname}/app.js`, 'utf8');
const context = vm.createContext({});
vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.ui = {state, telemetry, eventsPage, maintenance, dashboard, metricValue, telemetryRows, usersPage, handleChange, openModal, submitModal};', context);
const {state, telemetry, eventsPage, maintenance} = context.ui;
state.capabilities = {dashboard:true, telemetry:true, events:true, maintenance:true, create:true, restart:true, update:true, logs:true, updateCheck:true};

state.servers = [{id:'world', name:'Test world', status:'online', players:2, maxPlayers:4, metricsAvailable:false}];
state.telemetry = {samples:[], healthChecks:[], metricDefinitions:[{metric:'tick_rate', description:'Ticks <per> second'}]};
let html = telemetry();
assert.match(html, /Tick rate \(TPS\) samples are not available yet/);
assert.match(html, /Inbound \(bytes\/s\) samples are not available yet/);
assert.match(html, /Ticks &lt;per&gt; second/);
assert.match(html, /data-testid="log-output"/);
assert.doesNotMatch(html, /class="chart-line"/);

state.telemetry.samples = [
  {timestamp:'2026-09-16T12:00:00Z', tickRate:60, players:2, inboundBytesPerSecond:100, outboundBytesPerSecond:200},
  {timestamp:'2026-09-16T12:00:01Z', tickRate:59, players:3, inboundBytesPerSecond:150, outboundBytesPerSecond:250},
];
html = telemetry();
assert.match(html, /Inbound \(bytes\/s\) over the selected period. Latest value 150/);
assert.match(html, /Outbound \(bytes\/s\), latest value 250/);
assert.match(html, /class="chart-line secondary"/);
assert.doesNotMatch(html, /NaN|Infinity|Previous period/);

state.telemetry.metrics = {
  players:{value:0,status:'available',source:'Game API',observedAt:'2026-09-16T12:00:00Z'},
  cpuCores:{value:0.25,status:'available',source:'Metrics API'},
  cpuPercent:{value:25,status:'available',source:'Metrics API'},
  memoryUsedBytes:{value:1048576,status:'available',source:'Metrics API'},
  memoryLimitBytes:{value:2097152,status:'available',source:'Pod'},
  tickRate:{value:null,status:'unsupported',reason:'The game API does not expose tick rate.'},
  networkBytesPerSecond:{value:999,status:'stale',reason:'Source timed out'},
  diskUsedBytes:{value:null,status:'error',reason:'Disk <probe> failed'},
};
state.telemetry.samples = [
  {timestamp:'2026-09-16T12:00:00Z',players:0,cpuPercent:25,tickRate:null,observedAt:{cpuPercent:'2026-09-16T11:59:59Z'}},
  {timestamp:'2026-09-16T12:00:15Z',players:null,cpuPercent:null,tickRate:null},
  {timestamp:'2026-09-16T12:00:30Z',players:1,cpuPercent:20,tickRate:null},
];
html = telemetry();
assert.match(html, /0 \/ 4/);
assert.match(html, /25%/);
assert.match(html, /1 MB \/ 2 MB/);
assert.match(html, /The game API does not expose tick rate/);
assert.match(html, /Collection failed/);
assert.match(html, /Stale/);
assert.match(html, /Disk &lt;probe&gt; failed/);
assert.doesNotMatch(html, /999|60 TPS/);
assert.match(html, /value="1h"/);
assert.match(html, /View Active players data/);
assert.equal(context.ui.metricValue({metrics:state.telemetry.metrics}, 'networkBytesPerSecond'), null);
const rows = context.ui.telemetryRows();
assert.equal(rows.find((row) => row.metric === 'players').value, 0);
assert.equal(rows.find((row) => row.metric === 'tickRate').value, '');
assert.equal(rows.find((row) => row.metric === 'cpuPercent').observedAt, '2026-09-16T11:59:59Z');
assert.match(html, /M365\.00,170\.00\s+M695\.00,39\.57/);
state.telemetry.samples = [{timestamp:'2026-09-16T12:00:30Z',outboundBytesPerSecond:50, observedAt:{outboundBytesPerSecond:'2026-09-16T12:00:29Z'}}];
html = telemetry();
assert.match(html, /class="chart-point secondary"/);
assert.match(html, /Outbound \(bytes\/s\), latest value 50/);
state.telemetry.samples = [
  {timestamp:'2026-09-16T12:00:15Z',players:9999,observedAt:{players:'2026-09-16T11:00:00Z'}},
  {timestamp:'2026-09-16T12:00:30Z',players:1},
];
html = telemetry();
assert.doesNotMatch(html, /9,999|9999/);
assert.match(html, /data-testid="chart-data-players"/);
state.telemetry.metrics.cpuCores.value = 0.00535;
state.telemetry.samples = [{timestamp:'2026-09-16T12:00:30Z',cpuCores:0.00535}];
html = telemetry();
assert.match(html, /0\.00535 cores/);
assert.match(html, /CPU cores over the selected period\. Latest value 0\.00535/);
assert.match(html, />0\.00615<\/text>/);

for (const [key, value] of Object.entries({tickRate:29.7,tickP50Ms:0.4,tickP95Ms:1.2,tickP99Ms:3.6,tickWindowSeconds:10,tickSampleCount:297})) {
  state.telemetry.metrics[key] = {value,status:'available',unit:key.endsWith('Ms')?'milliseconds':'',source:'game API /api/metrics',observedAt:'2026-09-16T12:00:30Z'};
}
state.telemetry.samples = [{timestamp:'2026-09-16T12:00:30Z',tickRate:29.7,tickP50Ms:0.4,tickP95Ms:1.2,tickP99Ms:3.6,tickWindowSeconds:10,tickSampleCount:297}];
html = telemetry();
assert.match(html, /29\.7 TPS/);
assert.match(html, /1\.2 ms/);
assert.match(html, /3\.6 ms/);
assert.match(html, /UDomGameEngine::Tick/);
assert.match(html, /data-testid="chart-data-tickP95Ms"/);
assert.match(html, /p99 tick duration \(ms\), latest value 3\.6/);
assert.equal(context.ui.telemetryRows().find((row) => row.metric === 'tickP95Ms').value, 1.2);
state.telemetry.metrics.tickP95Ms = {value:null,status:'unsupported',reason:'Game build has no verified tick hook'};
state.telemetry.samples = [{timestamp:'2026-09-16T12:00:45Z',tickP95Ms:null,tickP99Ms:null}];
html = telemetry();
assert.match(html, /Game build has no verified tick hook/);
assert.equal(context.ui.telemetryRows().find((row) => row.metric === 'tickP95Ms').value, '');

state.events = [
  {id:'one', serverId:'world', category:'update', severity:'success', message:'Image updated', details:'<script>bad()</script>'},
  {id:'two', serverId:'other', category:'system', severity:'warning', message:'Other world restarted'},
];
state.selectedEventId = 'one';
html = eventsPage();
assert.match(html, /data-id="one" data-testid="event-row" aria-pressed="true"/);
assert.match(html, /&lt;script&gt;bad\(\)&lt;\/script&gt;/);
assert.match(html, /data-testid="copy-event"/);
html = maintenance();
assert.match(html, /Recent changes/);
assert.match(html, /Audit trail/);
assert.match(html, /Image updated/);
assert.doesNotMatch(html, /Other world restarted|Backup then restart|Create window/);
for (const id of ['restart-server','update-image','check-update','maintenance-events']) assert.ok(html.includes(`data-testid="${id}"`));
vm.runInContext('download = (...args) => { this.exportResult = args; }; notice = () => {}; this.exportCSV = exportCSV;', context);
context.exportCSV('events.csv', [{message:'=SUM(1,2)', details:'He said "hello"'}], ['message','details']);
assert.equal(context.exportResult[0], 'events.csv');
assert.equal(context.exportResult[1], '"message","details"\r\n"\'=SUM(1,2)","He said ""hello"""');
assert.equal(context.exportResult[2], 'text/csv;charset=utf-8');
const savedServers = state.servers;
state.servers = [];
state.users = [];
html = context.ui.usersPage();
assert.match(html, /No player IDs saved yet/);
assert.match(html, /data-testid="add-user"/);
state.users = [
  {id:'stable-one', name:'<Alice & "friends">', playerId:'0123456789abcdef0123456789abcdef'},
  {id:'stable-two', name:'<Alice & "friends">', playerId:'11111111111111111111111111111111'},
];
html = context.ui.usersPage();
assert.match(html, /&lt;Alice &amp; &quot;friends&quot;&gt;/);
assert.doesNotMatch(html, /<Alice/);
assert.match(html, /data-action="edit-user" data-id="stable-one"/);
assert.match(html, /data-action="delete-user" data-id="stable-two"/);
assert.match(html, /0123456789abcdef0123456789abcdef/);
assert.match(html, /11111111111111111111111111111111/);
const ownerField = {value:'manual-value'};
context.document = {querySelector(selector) {
  assert.equal(selector, '[data-testid="server-owner"]');
  return ownerField;
}};
context.ui.handleChange({target:{id:'saved-user', value:'stable-one'}});
assert.equal(ownerField.value, '0123456789abcdef0123456789abcdef');
context.ui.handleChange({target:{id:'saved-user', value:'stable-two'}});
assert.equal(ownerField.value, '11111111111111111111111111111111');
ownerField.value = 'abcdef0123456789abcdef0123456789';
context.ui.handleChange({target:{id:'saved-user', value:''}});
assert.equal(ownerField.value, 'abcdef0123456789abcdef0123456789');
context.ui.handleChange({target:{id:'saved-user', value:'deleted-id'}});
assert.equal(ownerField.value, 'abcdef0123456789abcdef0123456789');
assert.equal(state.users[0].playerId, '0123456789abcdef0123456789abcdef');
state.servers = savedServers;
console.log('UI rendering and saved-ID selection checks passed.');

state.capabilities = {dashboard:true, telemetry:true};
for (const action of ['add-user', 'edit-user', 'delete-user']) {
  state.modalAction = '';
  context.ui.openModal(action, 'stable-one');
  assert.equal(state.modalAction, '');
  state.modalAction = action;
  context.ui.submitModal({preventDefault(){}});
}
state.modalAction = '';
context.ui.handleChange({target:{id:'saved-user', value:'stable-one'}});
assert.equal(ownerField.value, 'abcdef0123456789abcdef0123456789');
state.events = [{message:'SECRET EVENT', details:'SECRET DETAILS'}];
state.logs = 'SECRET LOG';
for (const render of [context.ui.dashboard, telemetry, eventsPage, maintenance, context.ui.usersPage]) {
  html = render();
  assert.doesNotMatch(html, /Alice|0123456789abcdef|data-action="(?:add-user|edit-user|delete-user)"/);
  assert.doesNotMatch(html, /SECRET|Add server|Create your first server|Server lifecycle|Server logs|Fleet activity|View all events|Check update|Updates available|data-testid="(?:restart-server|update-image|check-update|log-output)"/);
}
html = telemetry();
assert.match(html, /data-testid="export-telemetry"/);
assert.match(html, /data-testid="telemetry-range"/);
assert.match(html, /Test world/);
state.servers = [];
assert.doesNotMatch(context.ui.dashboard(), /data-action="add-server"/);
console.log('Viewer capability rendering checks passed.');

const test = require('node:test');
test('identity changes clear protected data and reject late API and log responses', async () => {
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) elements.set(selector, {innerHTML:'', textContent:'', value:'', hidden:false, open:false, close(){this.open=false;}, setAttribute(){}, classList:{remove(){}, add(){}, toggle(){}}});
    return elements.get(selector);
  };
  const requests = [];
  let storedToken = '';
  const sandbox = vm.createContext({
    DOMException, AbortController, clearTimeout, setTimeout,
    document:{querySelector:element}, sessionStorage:{getItem(){return storedToken;}, removeItem(){storedToken='';}},
    fetch: (path, options) => new Promise((resolve) => requests.push({path, options, resolve})),
  });
  vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.authUI = {state, api, loadLogs, applyAuth, logout, discoverAuth, refresh};', sandbox);
  const ui = sandbox.authUI;
  ui.applyAuth({mode:'oidc',authenticated:true,subject:'operator',role:'admin',csrfToken:'admin-session',required:true,capabilities:{dashboard:true,telemetry:true,logs:true,events:true}});
  Object.assign(ui.state, {servers:[{id:'world'}], logs:'secret log', events:[{message:'secret event'}], users:[{id:'saved', name:'Alice', playerId:'0123456789abcdef0123456789abcdef'}], telemetry:{secret:true}, selectedEventId:'secret', modalAction:'edit-user', modalUserId:'saved', modalServerId:'world'});
  element('#modal-body').innerHTML = 'secret settings';
  const logs = ui.loadLogs();
  const events = ui.api('/api/events');
  const users = ui.api('/api/users');
  const logRejected = assert.rejects(logs, {name:'AbortError'});
  const eventsRejected = assert.rejects(events, {name:'AbortError'});
  const usersRejected = assert.rejects(users, {name:'AbortError'});
  ui.applyAuth({mode:'oidc',authenticated:true,subject:'reader',role:'viewer',csrfToken:'viewer-session',required:true,capabilities:{dashboard:true,telemetry:true}});
  for (const request of requests) request.resolve({status:200,ok:true,headers:{get:()=> 'admin-session'},text:async()=>JSON.stringify({lines:['late secret'],events:[{message:'late secret'}]})});
  await Promise.all([logRejected, eventsRejected, usersRejected]);
  assert.equal(ui.state.logs, '');
  assert.equal(ui.state.events.length, 0);
  assert.equal(ui.state.users.length, 0);
  assert.equal(ui.state.modalUserId, '');
  assert.equal(ui.state.servers.length, 0);
  assert.equal(ui.state.telemetry, null);
  assert.equal(ui.state.modalAction, '');
  assert.equal(element('#modal-body').innerHTML, '');
  assert.equal(element('#session-role').textContent, 'Viewer');
  assert.equal(ui.state.capabilities.logs, undefined);
  const responseChanged = ui.api('/api/bootstrap');
  const changedRejected = assert.rejects(responseChanged, {name:'AbortError'});
  requests.at(-1).resolve({status:200,ok:true,headers:{get:()=> 'different-session'},text:async()=>'{"servers":[{"name":"secret"}]}'});
  await changedRejected;
  assert.equal(ui.state.authRequired, true);
  assert.equal(ui.state.servers.length, 0);
  assert.equal(element('#login-dialog').open, false);
  ui.applyAuth({mode:'oidc',authenticated:true,subject:'reader',role:'viewer',csrfToken:'expired-session',required:true,capabilities:{dashboard:true,telemetry:true}});
  const logout = ui.logout();
  const requestCount = requests.length;
  await ui.refresh();
  assert.equal(requests.length, requestCount);
  assert.equal(requests.at(-1).options.headers['X-CSRF-Token'], 'expired-session');
  assert.equal(ui.state.authRequired, true);
  requests.at(-1).resolve({status:401,ok:false,headers:{get:()=>null},text:async()=>'{"error":"authentication required"}'});
  await logout;
  assert.equal(ui.state.logoutCSRF, '');
  assert.equal(element('#session-controls').hidden, true);
  assert.equal(element('#toast').hidden, true);

  ui.state.authMode = '';
  storedToken = 'old-admin-token';
  const discovery = ui.discoverAuth();
  assert.equal(requests.at(-1).options.headers.Authorization, 'Bearer old-admin-token');
  requests.at(-1).resolve({status:200,ok:true,headers:{get:()=>null},text:async()=>JSON.stringify({mode:'oidc',authenticated:false})});
  await new Promise(setImmediate);
  assert.equal(storedToken, '');
  assert.equal(requests.at(-1).options.headers.Authorization, undefined);
  requests.at(-1).resolve({status:200,ok:true,headers:{get:()=>null},text:async()=>JSON.stringify({mode:'oidc',authenticated:true,role:'viewer'})});
  assert.equal((await discovery).authenticated, true);
});

test('saved IDs refresh only for admins and late results cannot survive a session change', async () => {
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) elements.set(selector, {innerHTML:'', textContent:'', value:'', hidden:false, close(){}, setAttribute(){}, classList:{remove(){}, toggle(){}}});
    return elements.get(selector);
  };
  let auth = {mode:'oidc', authenticated:true, subject:'admin', role:'admin', csrfToken:'admin-session', capabilities:{dashboard:true,create:true}};
  let resolveUsers;
  const paths = [];
  const response = (body) => ({status:200, ok:true, headers:{get:()=>null}, text:async()=>JSON.stringify(body)});
  const sandbox = vm.createContext({
    DOMException, AbortController, URLSearchParams, clearTimeout, setTimeout,
    document:{querySelector:element}, sessionStorage:{getItem:()=>'', removeItem(){}},
    fetch: async (path) => {
      paths.push(path);
      if (path === '/api/auth') return response(auth);
      if (path === '/api/bootstrap') return response({servers:[], cluster:'test', mode:'demo'});
      if (path === '/api/users') return new Promise((resolve) => { resolveUsers = resolve; });
      throw new Error(`Unexpected request ${path}`);
    },
  });
  vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nrender = () => {}; connection = () => {}; this.ui = {state, applyAuth, refresh, logout};', sandbox);
  const ui = sandbox.ui;
  const refresh = ui.refresh();
  await new Promise(setImmediate);
  assert.equal(paths.at(-1), '/api/users');
  auth = {mode:'oidc', authenticated:true, subject:'viewer', role:'viewer', csrfToken:'viewer-session', capabilities:{dashboard:true,telemetry:true}};
  ui.applyAuth(auth);
  resolveUsers(response({users:[{id:'private', name:'Private', playerId:'0123456789abcdef0123456789abcdef'}]}));
  await refresh;
  assert.equal(ui.state.users.length, 0);
  paths.length = 0;
  await ui.refresh();
  assert.deepEqual(paths, ['/api/auth', '/api/bootstrap']);
  assert.equal(ui.state.users.length, 0);
});
