const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(`${__dirname}/app.js`, 'utf8');
const context = vm.createContext({});
vm.runInContext(source.slice(0, source.indexOf("$('#refresh').innerHTML")) + '\nthis.ui = {state, telemetry, eventsPage, maintenance};', context);
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
