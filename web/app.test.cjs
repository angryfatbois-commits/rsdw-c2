const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(`${__dirname}/app.js`, 'utf8');
const context = vm.createContext({});
vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.ui = {state, telemetry, eventsPage, maintenance, dashboard, metricValue, telemetryRows, usersPage, handleChange, openModal, submitModal, integrationsPage, integrationForm, editSettingsValues, editSettingsPatch, rebootsPage, rebootForm, zonedDate, PATTERN, eventTable};', context);
const {state, telemetry, eventsPage, maintenance} = context.ui;
state.capabilities = {dashboard:true, telemetry:true, events:true, maintenance:true, reboots:true, create:true, restart:true, update:true, logs:true, updateCheck:true};

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
assert.match(html, /Active players over the selected period\. Latest value 1/);
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
assert.match(html, /Endpoint unavailable\. Ask your cluster operator for the server address and game port\./);
state.servers[0].endpoint = 'game.example:7777';
assert.match(maintenance(), /game\.example:7777/);
assert.doesNotMatch(maintenance(), /Endpoint unavailable/);
delete state.servers[0].endpoint;
assert.match(html, /Recent changes/);
assert.match(html, /Audit trail/);
assert.match(html, /Image updated/);
assert.doesNotMatch(html, /Other world restarted|Backup then restart|Create window/);
for (const id of ['restart-server','update-image','check-update','maintenance-events']) assert.ok(html.includes(`data-testid="${id}"`));
state.displayTimezone = 'America/New_York';
state.reboots = [{id:'schedule-1', serverId:'world', serverName:'Test world', mode:'daily', dailyTimes:['05:00','17:00'], executionTimezone:'UTC', enabled:true, nextRun:'2026-09-16T12:00:00Z', lastResult:'awaiting_reconciliation', lastReason:'Waiting for telemetry'}];
html = context.ui.rebootsPage();
assert.match(html, /Scheduled reboots/);
assert.match(html, /Display timezone/);
assert.match(html, /Execution zone/);
assert.match(html, /\(America\/New_York\)/);
assert.match(html, /data-action="edit-reboot" data-id="schedule-1"/);
const rebootHTML = context.ui.rebootForm(state.reboots[0]);
assert.match(rebootHTML, /error-summary/);
assert.match(rebootHTML, /data-testid="reboot-server"/);
assert.match(rebootHTML, /data-reboot-field="daily"/);
assert.match(rebootHTML, /type="time"/);
assert.match(rebootHTML, /data-action="add-daily-time"/);
assert.match(rebootHTML, /data-action="remove-daily-time"/);
assert.match(rebootHTML, /acknowledgeDisconnect/);
state.reboots = [];
state.displayTimezone = '';
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
const {PATTERN, eventTable} = context.ui;
assert.doesNotThrow(() => new RegExp(PATTERN.dnsLabel, 'v'));
assert.doesNotThrow(() => new RegExp(PATTERN.eosId, 'v'));
html = context.ui.usersPage();
assert.match(html, /No player IDs saved yet|Renamed|&lt;Alice/);
state.users = [];
html = context.ui.usersPage();
assert.match(html, /No player IDs saved yet/);
assert.match(html, /class="empty"/);
state.users = [
  {id:'stable-one', name:'<Alice & "friends">', playerId:'0123456789abcdef0123456789abcdef'},
  {id:'stable-two', name:'<Alice & "friends">', playerId:'11111111111111111111111111111111'},
];
assert.match(eventTable([{id:'player-event', serverId:'world', category:'player', severity:'info', message:'A player joined'}], true), />Players</);
assert.doesNotMatch(eventTable([{id:'player-event', serverId:'world', category:'player', severity:'info', message:'A player joined'}], true), />player</);
assert.match(maintenance(), /Memory limit/);
assert.match(maintenance(), /Stable server ID/);
html = context.ui.dashboard();
assert.match(html, /Needs attention/);
assert.doesNotMatch(html, /stat-value amber[^"]*">0<\/div><div class="stat-label">Needs attention/);
assert.match(context.ui.integrationForm({serverIds:[], secretRef:{name:'s', key:'token'}, rules:{}}), /fieldset class="form-section"/);
console.log('UI rendering and saved-ID selection checks passed.');

state.capabilities = {integrations:true};
state.servers = [
  {id:'world-01', name:'PETZKO', worldName:'PC2-US-EAST-01'},
  {id:'world-02', name:'PETZKO', worldName:'PC2-US-EAST-02'},
  {id:'legacy', name:'Legacy world'},
];
for (const [id, label] of [['world-01','PC2-US-EAST-01'], ['world-02','PC2-US-EAST-02'], ['legacy','Legacy world'], ['missing','missing']]) {
  state.pendingRestarts = {[id]:{id:'restart-operation'}};
  assert.ok(context.ui.integrationsPage().includes(`Restart restart-operation for ${label} awaits a fresh observation.`));
}
state.servers[1].worldName = 'PC2-US-EAST-01';
state.pendingRestarts = {'world-02':{id:'restart-operation'}};
assert.ok(context.ui.integrationsPage().includes('Restart restart-operation for PC2-US-EAST-01 (world-02) awaits a fresh observation.'));
state.pendingRestarts = {};
state.alertRules = [];
state.integrations = [
  {id:'enabled-bot', name:'Alerts', enabled:true, secretRef:{name:'discord', key:'token'}, guildId:'1', channelId:'2', serverIds:[], rules:{}},
  {id:'disabled-bot', name:'Quiet', enabled:false, secretRef:{name:'discord', key:'token'}, guildId:'1', channelId:'2', serverIds:[], rules:{}},
];
html = context.ui.integrationsPage();
assert.match(html, /class="status enabled"/);
assert.match(html, /class="status disabled"/);
state.integrations = [];
assert.match(context.ui.integrationsPage(), /No Discord bots configured yet/);
state.servers = savedServers;

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
test('edit settings uses effective values and sends only changed fields', () => {
  const server = {id:'target', name:'PETZKO', worldName:'PC2-US-EAST-01', maxPlayers:8, memoryLimitMiB:3072, cpuLimitMillis:1250};
  const initial = context.ui.editSettingsValues(server);
  assert.deepEqual({...initial}, {name:'PETZKO', worldName:'PC2-US-EAST-01', maxPlayers:8, memoryLimitMiB:3072, cpuLimitMillis:1250});
  assert.equal(context.ui.editSettingsPatch(initial, {...initial}), null);
  assert.deepEqual({...context.ui.editSettingsPatch(initial, {...initial, name:'New creator', maxPlayers:'12'})}, {name:'New creator', maxPlayers:12, confirm:true});
  assert.deepEqual({...context.ui.editSettingsPatch(initial, {...initial, worldName:'New world', confirmWorldName:'true'})}, {worldName:'New world', confirmWorldName:true, confirm:true});
  assert.deepEqual({...context.ui.editSettingsValues({name:'Legacy creator', maxPlayers:4})}, {name:'Legacy creator', worldName:'Legacy creator', maxPlayers:4, memoryLimitMiB:2048, cpuLimitMillis:1000});
});
test('create uploads one save with settings and preserves authentication headers', async () => {
  const requests = [];
  const sandbox = vm.createContext({FormData, DOMException, sessionStorage:{getItem(){return 'admin-token';}}, fetch:async(path,options) => {
    requests.push({path,options});
    return {ok:true,status:201,headers:{get(){return 'session-csrf';}},text:async()=>'{}'};
  }});
  vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.ui = {state, api, createRequestBody};', sandbox);
  const {state, api, createRequestBody} = sandbox.ui;
  const content = Uint8Array.from([71,86,65,83,0,255]);
  const save = new File([content], 'World.sav');
  const body = createRequestBody({name:'World',ownerId:'0123456789abcdef0123456789abcdef',maxPlayers:4,save});
  assert.deepEqual([...body.keys()], ['request','save']);
  assert.deepEqual(JSON.parse(body.get('request')), {name:'World',ownerId:'0123456789abcdef0123456789abcdef',maxPlayers:4});
  assert.deepEqual(new Uint8Array(await body.get('save').arrayBuffer()), content);
  await api('/api/servers', {method:'POST',body});
  assert.equal(requests[0].options.headers.Authorization, 'Bearer admin-token');
  assert.equal(requests[0].options.headers['Content-Type'], undefined);
  state.authMode = 'oidc'; state.csrfToken = 'session-csrf';
  await api('/api/servers', {method:'POST',body});
  assert.equal(requests[1].options.headers['X-CSRF-Token'], 'session-csrf');
  assert.equal(requests[1].options.headers.Authorization, undefined);
  assert.equal(requests[1].options.credentials, 'same-origin');
  const empty = createRequestBody({name:'Empty world', save:new File([], '')});
  assert.deepEqual(JSON.parse(empty), {name:'Empty world'});
  await api('/api/servers', {method:'POST',body:empty});
  assert.equal(requests[2].options.headers['Content-Type'], 'application/json');
  for (const name of ['-World.sav', '--help.sav']) {
    assert.throws(() => createRequestBody({name:'World',save:new File([content], name)}), /plain filename/);
  }
  for (const [file, error] of [[{name:'world.zip',size:4},/Select a .sav/],[{name:'../world.sav',size:4},/plain filename/],[{name:'world.sav',size:0},/must not be empty/],[{name:'world.sav',size:32*1024*1024+1},/at most 32 MiB/]]) {
    assert.throws(()=>createRequestBody({name:'World',save:file}), error);
  }
});

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
  Object.assign(ui.state, {servers:[{id:'world'}], logs:'secret log', events:[{message:'secret event'}], users:[{id:'saved', name:'Alice', playerId:'0123456789abcdef0123456789abcdef'}], integrations:[{name:'private bot'}], deliveries:[{id:'private delivery'}], alertRules:[{kind:'server_down'}], pendingRestarts:{world:{id:'operation'}}, modalIntegrationId:'private bot', telemetry:{secret:true}, selectedEventId:'secret', modalAction:'edit-user', modalUserId:'saved', modalServerId:'world'});
  element('#modal-body').innerHTML = 'secret settings';
  const logs = ui.loadLogs();
  const events = ui.api('/api/events');
  const users = ui.api('/api/users');
  const integrations = ui.api('/api/integrations');
  const logRejected = assert.rejects(logs, {name:'AbortError'});
  const eventsRejected = assert.rejects(events, {name:'AbortError'});
  const usersRejected = assert.rejects(users, {name:'AbortError'});
  const integrationsRejected = assert.rejects(integrations, {name:'AbortError'});
  ui.applyAuth({mode:'oidc',authenticated:true,subject:'reader',role:'viewer',csrfToken:'viewer-session',required:true,capabilities:{dashboard:true,telemetry:true}});
  for (const request of requests) request.resolve({status:200,ok:true,headers:{get:()=> 'admin-session'},text:async()=>JSON.stringify({lines:['late secret'],events:[{message:'late secret'}]})});
  await Promise.all([logRejected, eventsRejected, usersRejected, integrationsRejected]);
  assert.equal(ui.state.logs, '');
  assert.equal(ui.state.events.length, 0);
  assert.equal(ui.state.users.length, 0);
  assert.equal(ui.state.integrations.length, 0);
  assert.equal(ui.state.deliveries.length, 0);
  assert.equal(ui.state.alertRules.length, 0);
  assert.equal(Object.keys(ui.state.pendingRestarts).length, 0);
  assert.equal(ui.state.modalIntegrationId, '');
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

function rosterResponse(id = 'a', players = [{name:'Alice', characterName:'Mage'}], count = 1) {
  return {server:{id, name:`World ${id}`, maxPlayers:4}, playerRoster:{status:'available', freshForMs:45000, players}, metrics:{players:{value:count, status:'available', observedAt:new Date().toISOString()}}, samples:[]};
}

test('connected players show labeled escaped fields, preserve duplicates, and distinguish empty from unavailable', () => {
  const sandbox = vm.createContext({});
  vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.ui = {state, telemetry};', sandbox);
  const {state, telemetry} = sandbox.ui;
  Object.assign(state, {servers:[{id:'a', name:'World A', maxPlayers:4}], serverId:'a', capabilities:{telemetry:true}});
  state.telemetry = rosterResponse('a', [{name:'<Alice>',characterName:'<script>bad()</script>'}, {name:'<Alice>'}, {characterName:'Mage'}, {name:' \t',characterName:'\n'}], 4);
  let html = telemetry();
  assert.equal((html.match(/<dt>Name<\/dt>/g) || []).length, 4);
  assert.equal((html.match(/<dt>Character name<\/dt>/g) || []).length, 4);
  assert.equal((html.match(/&lt;Alice&gt;/g) || []).length, 2);
  assert.match(html, /&lt;script&gt;bad\(\)&lt;\/script&gt;/);
  assert.doesNotMatch(html, /<script>|<Alice>/);
  assert.match(html, /Name unavailable/);
  assert.match(html, /Character name unavailable/);
  assert.match(html, /4 \/ 4/);
  assert.match(html, /Player count/);
  assert.match(html, /export-telemetry/);
  for (const [players, count, message] of [
    [[], 0, /No players are connected/],
    [[], 4, /reports 4 connected players, but the roster returned no entries/],
    [null, 0, /Connected players are unavailable/],
    [undefined, 4, /Connected players are unavailable/],
    ['broken', 4, /Connected players are unavailable/],
  ]) {
    state.telemetry = rosterResponse('a', [], count);
    state.telemetry.playerRoster.players = players;
    html = telemetry();
    assert.match(html, message);
    assert.doesNotMatch(html, /<dt>Name<\/dt>/);
    if (count !== 0 || !Array.isArray(players)) assert.doesNotMatch(html, /No players are connected/);
  }
  for (const status of ['stale','error','unavailable']) {
    state.telemetry = rosterResponse();
    state.telemetry.metrics.players.status = status;
    html = telemetry();
    assert.doesNotMatch(html, /Alice|Mage|<dt>Name<\/dt>|No players are connected/);
    assert.match(html, status === 'stale' ? /Connected players are stale/ : /Connected players are unavailable/);
  }
  for (const observedAt of [new Date(Date.now()-46000).toISOString(), null, 'invalid']) {
    state.telemetry = rosterResponse();
    state.telemetry.metrics.players.observedAt = observedAt;
    assert.doesNotMatch(telemetry(), /Alice|Mage|<dt>Name<\/dt>/);
    assert.match(telemetry(), /Connected players are stale/);
  }
  state.telemetry = rosterResponse('other');
  assert.doesNotMatch(telemetry(), /Alice|Mage|<dt>Name<\/dt>/);
  state.telemetry = rosterResponse('a', [null, {name:42, characterName:{}}], 2);
  html = telemetry();
  assert.match(html, /Name unavailable/);
  assert.match(html, /Character name unavailable/);
});

function refreshFixture(admin = false) {
  const elements = new Map();
  const element = (selector) => {
    if (!elements.has(selector)) elements.set(selector, {innerHTML:'', textContent:'', value:'', hidden:false, open:false, close(){this.open=false;}, setAttribute(){}, classList:{remove(){}, toggle(){}}});
    return elements.get(selector);
  };
  const requests = [];
  const timers = new Map();
  let timerID = 0;
  const clock = {now:Date.now()};
  let monotonicNow = clock.now;
  const sandbox = vm.createContext({
    DOMException, AbortController, URLSearchParams, HTMLInputElement:class {},
    Date:class extends Date {static now(){return clock.now;}},
    performance:{now:()=>monotonicNow},
    clearTimeout:(id)=>timers.delete(id), setTimeout:(fn,delay)=>{timers.set(++timerID,{fn,delay});return timerID;},
    document:{querySelector:element, querySelectorAll:()=>[], activeElement:null},
    sessionStorage:{getItem:()=>'', removeItem(){}},
    window:{scrollTo(){}}, location:{hash:'#dashboard'},
    fetch:(path,options)=>new Promise((resolve,reject)=>requests.push({path,options,resolve,reject})),
  });
  vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.ui = {state, applyAuth, refresh, handleChange, logout, navigate, render};', sandbox);
  const ui = sandbox.ui;
  const auth = {mode:'oidc', authenticated:true, subject:'test', role:admin ? 'admin' : 'viewer', csrfToken:'session', capabilities:{dashboard:true,telemetry:true,logs:admin}};
  ui.applyAuth(auth);
  Object.assign(ui.state, {page:'telemetry', loaded:true, serverId:'a', servers:[{id:'a',name:'World A',maxPlayers:4},{id:'b',name:'World B',maxPlayers:4}]});
  const reply = (path, body, status=200) => {
    const request = requests.find((request)=>request.path === path && !request.done);
    assert.ok(request, `No pending request for ${path}`);
    request.done = true;
    request.resolve({status,ok:status===200,headers:{get:()=>null},text:async()=>JSON.stringify(body)});
    return request;
  };
  const flush = () => new Promise(setImmediate);
  const discover = async () => {
    reply('/api/auth',auth);
    await flush();
    reply('/api/bootstrap',{servers:[{id:'a',name:'World A',maxPlayers:4},{id:'b',name:'World B',maxPlayers:4}]});
    await flush();
  };
  return {ui, element, requests, reply, flush, discover, clock, timers, sandbox, advanceMonotonic:(delta)=>{monotonicNow += delta;}};
}

test('switching servers clears rendered names immediately and aborts before auth discovery', async () => {
  const f = refreshFixture();
  f.ui.state.telemetry = rosterResponse();
  f.ui.render();
  const first = f.ui.refresh();
  await f.discover();
  const oldRequest = f.requests.at(-1);
  f.ui.handleChange({target:{id:'server-filter',value:'b'}});
  assert.equal(oldRequest.options.signal.aborted,true);
  assert.equal(f.ui.state.telemetry,null);
  assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage/);
  f.reply('/api/servers/a/telemetry?range=60s',rosterResponse());
  await first;
  assert.equal(f.ui.state.telemetry,null);
  assert.equal(f.ui.state.refreshing,true);
  await f.discover();
  f.reply('/api/servers/b/telemetry?range=60s',rosterResponse('b',[{name:'Bob',characterName:'Warrior'}]));
  await f.flush();
  assert.match(f.element('#content').innerHTML,/Bob/);
  assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage/);
});

test('inventory reconciliation clears old names before replacement telemetry settles', async () => {
  const f = refreshFixture();
  f.ui.state.telemetry = rosterResponse();
  f.ui.render();
  const pending = f.ui.refresh();
  f.reply('/api/auth',{mode:'oidc',authenticated:true,subject:'test',role:'viewer',csrfToken:'session',capabilities:{dashboard:true,telemetry:true}});
  await f.flush();
  f.reply('/api/bootstrap',{servers:[{id:'b',name:'World B',maxPlayers:4}],cluster:'test',mode:'demo'});
  await f.flush();
  assert.equal(f.ui.state.serverId,'');
  assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage/);
  f.reply('/api/servers/b/telemetry?range=60s',rosterResponse('b',[{name:'Bob',characterName:'Warrior'}]));
  await pending;
  assert.match(f.element('#content').innerHTML,/Bob/);
});

test('accepted telemetry renders before an unrelated slow request settles', async () => {
  const f = refreshFixture(true);
  const pending = f.ui.refresh();
  await f.discover();
  f.reply('/api/servers/a/telemetry?range=60s',rosterResponse());
  await f.flush();
  assert.match(f.element('#content').innerHTML,/Alice|Mage/);
  f.reply('/api/servers/a/logs?tail=100',{error:'Logs unavailable'},500);
  await pending;
  assert.match(f.element('#content').innerHTML,/Alice|Mage/);
});

test('A to B to A rejects the first A generation and late errors', async () => {
  const f = refreshFixture();
  const oldA = f.ui.refresh();
  await f.discover();
  const firstRequest = f.requests.at(-1);
  f.ui.handleChange({target:{id:'server-filter',value:'b'}});
  await f.discover();
  const bRequest = f.requests.at(-1);
  f.ui.handleChange({target:{id:'server-filter',value:'a'}});
  await f.discover();
  const latestRequest = f.requests.at(-1);
  const response = (body) => ({status:200,ok:true,headers:{get:()=>null},text:async()=>JSON.stringify(body)});
  latestRequest.resolve(response(rosterResponse('a',[{name:'Current A',characterName:'Current mage'}])));
  await f.flush();
  const currentHTML = f.element('#content').innerHTML;
  firstRequest.resolve(response(rosterResponse('a',[{name:'Old A',characterName:'Old mage'}])));
  bRequest.reject(new Error('Old B error'));
  await oldA;
  await f.flush();
  assert.equal(f.element('#content').innerHTML,currentHTML);
  assert.equal(f.element('#error-banner').hidden,true);
  assert.equal(f.ui.state.telemetry.playerRoster.players[0].name,'Current A');
});

test('range and page changes, response identity, failures, and logout cannot expose late names', async () => {
  for (const change of ['range','page','response identity','failure','logout','session']) {
    const f = refreshFixture();
    f.ui.state.telemetry = rosterResponse();
    f.ui.render();
    const pending = f.ui.refresh();
    await f.discover();
    if (change === 'range') {
      f.ui.handleChange({target:{id:'telemetry-range',value:'5m'}});
      assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage/);
    } else if (change === 'page') {
      f.ui.navigate();
    } else if (change === 'logout') {
      const logout = f.ui.logout();
      f.reply('/api/auth/logout',{});
      await logout;
    } else if (change === 'session') {
      f.ui.applyAuth({mode:'oidc',authenticated:true,subject:'new',role:'viewer',csrfToken:'new',capabilities:{dashboard:true,telemetry:true}});
    }
    f.reply('/api/servers/a/telemetry?range=60s', change === 'failure' ? {error:'Collection unavailable'} : rosterResponse(change === 'response identity' ? 'b' : 'a'), change === 'failure' ? 500 : 200);
    await pending;
    assert.equal(f.ui.state.telemetry,null,change);
    assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage|<dt>Name<\/dt>/,change);
    if (change === 'response identity' || change === 'failure') assert.equal(f.element('#error-banner').hidden,false);
  }
});

test('paused telemetry expires names locally without another request', () => {
  const f = refreshFixture();
  f.ui.state.telemetry = rosterResponse();
  f.ui.state.telemetry.metrics.players.observedAt = new Date(f.clock.now).toISOString();
  f.ui.state.paused = true;
  f.ui.render();
  assert.match(f.element('#content').innerHTML,/Alice/);
  const timer = [...f.timers.values()][0];
  assert.ok(timer.delay > 0 && timer.delay <= 45001);
  f.clock.now += 45001;
  timer.fn();
  assert.match(f.element('#content').innerHTML,/Connected players are stale/);
  assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage/);
  assert.equal(f.requests.length,0);
});

test('accepted roster freshness ignores browser wall-clock skew', async () => {
  const f = refreshFixture();
  const pending = f.ui.refresh();
  await f.discover();
  f.reply('/api/servers/a/telemetry?range=60s',rosterResponse());
  await pending;
  assert.match(f.element('#content').innerHTML,/Alice/);
  f.clock.now += 10 * 60 * 1000;
  f.advanceMonotonic(1000);
  f.ui.render();
  assert.match(f.element('#content').innerHTML,/Alice/);
  f.advanceMonotonic(45000);
  f.ui.render();
  assert.match(f.element('#content').innerHTML,/Connected players are stale/);
  assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage/);
});

test('repeated telemetry for one observation cannot renew roster freshness', async () => {
  const f = refreshFixture();
  const response = rosterResponse();
  const first = f.ui.refresh();
  await f.discover();
  f.reply('/api/servers/a/telemetry?range=60s',response);
  await first;
  const firstDeadline = f.ui.state.rosterDeadline;
  f.advanceMonotonic(1000);
  const second = f.ui.refresh();
  await f.discover();
  f.reply('/api/servers/a/telemetry?range=60s',response);
  await second;
  assert.equal(f.ui.state.rosterDeadline,firstDeadline);
  f.advanceMonotonic(45000);
  f.ui.render();
  assert.match(f.element('#content').innerHTML,/Connected players are stale/);
  assert.doesNotMatch(f.element('#content').innerHTML,/Alice|Mage/);
});

test('an independent log failure keeps successfully refreshed telemetry', async () => {
  const f = refreshFixture(true);
  const pending = f.ui.refresh();
  await f.discover();
  f.reply('/api/servers/a/telemetry?range=60s',rosterResponse());
  f.reply('/api/servers/a/logs?tail=100',{error:'Logs unavailable'},500);
  await pending;
  assert.match(f.element('#content').innerHTML,/Alice|Mage/);
  assert.match(f.element('#content').innerHTML,/1 \/ 4/);
  assert.equal(f.element('#error-banner').textContent,'Logs unavailable');
});
