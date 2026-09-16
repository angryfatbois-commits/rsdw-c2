const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(`${__dirname}/app.js`, 'utf8');
const context = vm.createContext({});
vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.ui = {state, telemetry, eventsPage, maintenance, dashboard, metricValue, telemetryRows};', context);
const {state, telemetry, eventsPage, maintenance} = context.ui;

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
console.log('UI rendering checks passed.');
