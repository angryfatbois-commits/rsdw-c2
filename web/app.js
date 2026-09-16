'use strict';

const $ = (selector) => document.querySelector(selector);
const escapeHTML = (value) => String(value ?? '').replace(/[&<>"']/g, (char) => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[char]));
const icons = {
  dashboard: '<rect x="3" y="3" width="7" height="7"/><rect x="14" y="3" width="7" height="7"/><rect x="3" y="14" width="7" height="7"/><rect x="14" y="14" width="7" height="7"/>',
  telemetry: '<path d="M4 20V13m5 7V7m6 13V3m5 17V10"/>',
  events: '<rect x="4" y="4" width="16" height="17" rx="2"/><path d="M8 2v4m8-4v4M4 10h16m-12 4h8m-8 3h5"/>',
  maintenance: '<path d="m14 6 4 4m-2-7a6 6 0 0 0-7 8L3 17a3 3 0 0 0 4 4l6-6a6 6 0 0 0 8-7l-4 4-5-5Z"/>',
  server: '<rect x="3" y="3" width="18" height="7" rx="2"/><rect x="3" y="14" width="18" height="7" rx="2"/><path d="M7 6.5h.01M7 17.5h.01M12 6.5h5M12 17.5h5"/>',
  pulse: '<path d="M2 12h5l3-8 4 16 3-8h5"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l4 2"/>',
  warning: '<path d="m12 3 10 18H2Z M12 9v5m0 3v.01"/>',
  refresh: '<path d="M20 7v5h-5M4 17v-5h5"/><path d="M6 6a8 8 0 0 1 14 6M4 12a8 8 0 0 0 14 6"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  arrow: '<path d="M4 12h16m-6-6 6 6-6 6"/>',
  download: '<path d="M12 3v12m-4-4 4 4 4-4M4 16v5h16v-5"/>',
  search: '<circle cx="10" cy="10" r="7"/><path d="m15 15 6 6"/>',
  check: '<path d="m5 12 4 4L19 6"/>',
  cpu: '<rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v4m6-4v4M9 18v4m6-4v4M2 9h4m-4 6h4m12-6h4m-4 6h4"/>',
};
const icon = (name) => `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${icons[name] || icons.server}</svg>`;
const pages = {
  dashboard: ['Dashboard', 'Monitor every server from one place.'],
  telemetry: ['Telemetry', 'Live performance, resource usage, and server logs.'],
  events: ['Events', 'Search your fleet’s activity and operational history.'],
  maintenance: ['Maintenance', 'Keep your worlds healthy and up to date.'],
};
const state = {
  page: 'dashboard', servers: [], events: [], serverId: '', fleetFilter: 'all',
  query: '', category: '', range: '60s', telemetry: null, logs: '', logQuery: '',
  selectedEventId: '', paused: false, loaded: false, lastUpdated: null, refreshing: false,
  modalAction: '', modalServerId: '', modalBusy: false, request: null,
  authRequired: false, loginBusy: false,
};
const tokenKey = 'rsdw-admin-token';
let searchTimer;
let toastTimer;
let modalOpener;

function number(value, suffix = '') {
  return value == null || value === '' || !Number.isFinite(Number(value)) ? '—' : `${Number(value).toLocaleString(undefined, {maximumFractionDigits:1})}${suffix}`;
}
function bytes(value) {
  if (value == null || !Number.isFinite(Number(value))) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let amount = Math.max(0, Number(value)), unit = 0;
  while (amount >= 1024 && unit < units.length - 1) { amount /= 1024; unit++; }
  return `${number(amount)} ${units[unit]}`;
}
function duration(value) {
  if (value == null || !Number.isFinite(Number(value))) return '—';
  const seconds = Math.max(0, Number(value));
  if (seconds >= 86400) return `${Math.floor(seconds / 86400)}d ${Math.floor(seconds % 86400 / 3600)}h`;
  if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h ${Math.floor(seconds % 3600 / 60)}m`;
  return `${Math.floor(seconds / 60)}m ${Math.floor(seconds % 60)}s`;
}
function date(value) {
  if (!value) return '—';
  const parsed = new Date(value);
  return Number.isNaN(parsed.valueOf()) ? '—' : parsed.toLocaleString();
}
function status(value = 'unknown') {
  const known = ['online', 'starting', 'attention', 'stopped', 'unknown', 'warning', 'critical', 'error', 'success', 'healthy'];
  return `<span class="status ${known.includes(value) ? value : 'unknown'}">${escapeHTML(value.charAt(0).toUpperCase() + value.slice(1))}</span>`;
}
function stat(label, value, name, tone = '') {
  return `<section class="panel stat">${icon(name)}<div><div class="stat-value ${tone}">${escapeHTML(value)}</div><div class="stat-label">${escapeHTML(label)}</div></div></section>`;
}
function selectedServer() { return state.servers.find((server) => server.id === state.serverId) || state.servers[0]; }
function scopedServers() { return state.servers.filter((server) => !state.serverId || server.id === state.serverId); }
function notice(message) {
  $('#toast').textContent = message;
  $('#toast').hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { $('#toast').hidden = true; }, 6000);
}
async function api(path, options = {}) {
  let token = '';
  try { token = sessionStorage.getItem(tokenKey) || ''; } catch { /* Storage may be blocked by browser privacy settings. */ }
  const response = await fetch(path, {credentials:'same-origin', ...options, headers:{'Accept':'application/json', ...(token ? {'Authorization':`Bearer ${token}`} : {}), ...(options.body ? {'Content-Type':'application/json'} : {}), ...options.headers}});
  const text = await response.text();
  let data;
  if (response.status === 401 && path !== '/api/session') {
    requireLogin();
    throw new Error('Admin sign in is required.');
  }
  try { data = text ? JSON.parse(text) : {}; } catch { throw new Error('The server returned an unreadable response. Please try again.'); }
  if (path !== '/api/session' && (data.auth?.required || (path === '/api/auth' && data.required)) && !token) {
    requireLogin();
    throw new Error('Admin sign in is required.');
  }
  if (!response.ok) throw new Error(typeof data.error === 'string' ? data.error : data.message || `Request failed (${response.status}). Please try again.`);
  return data;
}
function lockedState() {
  $('.page-controls').hidden = true;
  $('#content').setAttribute('aria-busy','false');
  $('#content').innerHTML = `<section class="panel empty"><div class="empty-icon">${icon('server')}</div><h2>Admin access required</h2><p>Sign in with your admin token to view and manage this cluster.</p><button class="primary" data-action="login" data-testid="open-login">Sign in</button></section>`;
  $('#connection-status').textContent = 'Waiting for admin sign in';
  $('#connection-status').classList.remove('connected');
}
function requireLogin() {
  state.authRequired = true;
  try { sessionStorage.removeItem(tokenKey); } catch { /* Sign in reports blocked storage on submission. */ }
  lockedState();
  if (!$('#login-dialog').open) {
    $('#login-error').hidden = true;
    $('#admin-token').value = '';
    $('#login-dialog').showModal();
    $('#admin-token').focus();
  }
}
function closeLogin() {
  if (state.loginBusy) return;
  $('#login-dialog').close();
  if (!$('#modal').open) $('[data-testid="open-login"]')?.focus();
}
async function submitLogin(event) {
  event.preventDefault();
  if (state.loginBusy || !$('#login-form').reportValidity()) return;
  state.loginBusy = true;
  $('#login-form').setAttribute('aria-busy','true');
  $('#login-error').hidden = true;
  $('#login-dialog').querySelectorAll('button').forEach((button) => { button.disabled = true; });
  try {
    const token = $('#admin-token').value;
    await api('/api/session',{method:'POST',body:JSON.stringify({token})});
    try { sessionStorage.setItem(tokenKey,token); } catch { throw new Error('Browser session storage is blocked. Enable it for this site, then sign in again.'); }
    state.authRequired = false;
    state.loginBusy = false;
    $('#admin-token').value = '';
    $('#login-dialog').close();
    $('.page-controls').hidden = false;
    await refresh();
    if (!state.authRequired) notice('Signed in to your cluster.');
  } catch (error) {
    $('#login-error').textContent = error.message;
    $('#login-error').hidden = false;
    $('#admin-token').setAttribute('aria-invalid','true');
    $('#login-error').focus();
  } finally {
    state.loginBusy = false;
    $('#login-form').setAttribute('aria-busy','false');
    $('#login-dialog').querySelectorAll('button').forEach((button) => { button.disabled = false; });
  }
}
function eventArray(data) { return Array.isArray(data) ? data : data.events || []; }
function samples() {
  const data = state.telemetry;
  return Array.isArray(data) ? data : data?.samples || data?.points || [];
}
function emptyState() {
  return `<section class="panel empty" data-testid="empty-state"><div class="empty-icon">${icon('server')}</div><div class="eyebrow">READY FOR YOUR FIRST WORLD</div><h2>Your next adventure starts here.</h2><p>Create your first Dragonwilds server. We’ll deploy it to your cluster and bring its health, logs, and lifecycle into one view.</p><button class="primary" data-action="add-server" data-testid="empty-add-server">${icon('plus')}Create your first server</button><div class="empty-steps"><span>${icon('check')}Configure your world</span><span>${icon('check')}Deploy to your cluster</span><span>${icon('check')}Monitor in one place</span></div></section>`;
}
function eventTable(events, detailed = false) {
  if (!events.length) return '<p class="no-results" data-testid="no-events">No events match this view.</p>';
  return `<div class="table-wrap"><table><thead><tr><th>Time</th><th>Server</th><th>${detailed ? 'Category' : 'Status'}</th><th>Event</th></tr></thead><tbody>${events.map((event) => `<tr class="${state.selectedEventId === event.id ? 'event-selected' : ''}"><td class="mono">${escapeHTML(date(event.timestamp))}</td><td>${escapeHTML(event.serverName || event.serverId || 'Cluster')}</td><td>${detailed ? escapeHTML(event.category || 'system') : status(event.severity || 'info')}</td><td>${detailed ? `<button class="link-button event-message" data-action="select-event" data-id="${escapeHTML(event.id)}" data-testid="event-row" aria-pressed="${state.selectedEventId === event.id}">${escapeHTML(event.message)}</button>` : escapeHTML(event.message)}</td></tr>`).join('')}</tbody></table></div>`;
}
function dashboard() {
  const servers = scopedServers();
  const online = servers.filter((server) => server.status === 'online').length;
  const attention = servers.filter((server) => server.status === 'attention' || server.updateAvailable).length;
  const filtered = servers.filter((server) => state.fleetFilter === 'all' || (state.fleetFilter === 'online' ? server.status === 'online' : server.status === 'attention' || server.updateAvailable));
  const stats = `<div class="stats">${stat('Registered servers', servers.length, 'server')}${stat('Online', online, 'pulse', 'green')}${stat('Needs attention', attention, 'warning', 'amber')}${stat('Updates available', servers.filter((server) => server.updateAvailable).length, 'refresh')}</div>`;
  if (!state.servers.length) return stats + emptyState();
  return `${stats}<section class="panel"><div class="panel-heading"><div><h2>Servers</h2><p>Your managed Dragonwilds worlds</p></div><button class="primary" data-action="add-server" data-testid="add-server">${icon('plus')}Add server</button></div><div class="toolbar chips" aria-label="Server status filter">${['all','online','attention'].map((filter) => `<button data-action="fleet-filter" data-value="${filter}" data-testid="filter-${filter}" aria-pressed="${state.fleetFilter === filter}">${filter === 'attention' ? 'Needs attention' : filter[0].toUpperCase()+filter.slice(1)}</button>`).join('')}</div>${filtered.length ? `<div class="table-wrap"><table><thead><tr><th>Name</th><th>Status</th><th>Players</th><th>Tick rate</th><th>CPU</th><th>Uptime</th><th>Actions</th></tr></thead><tbody>${filtered.map((server) => `<tr><td><strong>${escapeHTML(server.name)}</strong><small>${escapeHTML(server.region || server.namespace || 'Managed server')}</small></td><td>${status(server.status)}</td><td>${number(server.players)} / ${number(server.maxPlayers)}</td><td>${server.metricsAvailable ? number(server.tickRate, ' TPS') : '—'}</td><td>${server.metricsAvailable ? number(server.cpuPercent, '%') : '—'}</td><td class="mono">${duration(server.uptimeSeconds)}</td><td class="actions"><button class="link-button" data-action="view-server" data-id="${escapeHTML(server.id)}" data-testid="view-server">View server ${icon('arrow')}</button></td></tr>`).join('')}</tbody></table></div>` : '<p class="no-results">No servers match this filter.</p>'}</section><section class="panel"><div class="panel-heading"><h2>Fleet activity</h2><button class="link-button" data-action="view-events" data-testid="view-all-events">View all events ${icon('arrow')}</button></div>${eventTable(state.events.slice(0, 6))}</section>`;
}
function chart(key, label) {
  const points = samples();
  const valid = points.map((point, index) => ({value:point[key], index})).filter((point) => point.value != null && Number.isFinite(Number(point.value)));
  if (!valid.length) return `<p class="no-results">${escapeHTML(label)} samples are not available yet.</p>`;
  const ceiling = Math.max(1, ...valid.map((point) => Number(point.value))) * 1.15;
  const position = (point) => `${(point.index / Math.max(1, points.length - 1) * 660 + 35).toFixed(2)},${(170 - Number(point.value) / ceiling * 150).toFixed(2)}`;
  const path = points.map((point, index) => point[key] == null || !Number.isFinite(Number(point[key])) ? '' : `${index === 0 || points[index - 1][key] == null ? 'M' : 'L'}${position({value:point[key], index})}`).join(' ');
  return `<div class="chart-legend">${escapeHTML(label)}</div><svg class="chart" viewBox="0 0 710 190" role="img" aria-label="${escapeHTML(label)} over the selected period. Latest value ${escapeHTML(number(valid.at(-1).value))}.">${[20,70,120,170].map((y) => `<path d="M35 ${y}H695" stroke="#344155" stroke-width="1"/>`).join('')}<text x="0" y="24" fill="#a9b7cc" font-size="10">${number(ceiling)}</text><text x="12" y="173" fill="#a9b7cc" font-size="10">0</text><path d="${path}" fill="none" stroke="#22c55e" stroke-width="2"/>${valid.map((point) => `<circle cx="${position(point).split(',')[0]}" cy="${position(point).split(',')[1]}" r="2.5" fill="#22c55e"/>`).join('')}</svg><div class="chart-labels"><span>${escapeHTML(date(points[0]?.timestamp || points[0]?.time))}</span><span>${escapeHTML(date(points.at(-1)?.timestamp || points.at(-1)?.time))}</span></div>`;
}
function resource(label, value, percent) {
  return `<div class="resource"><div class="resource-header"><span>${label}</span><strong>${escapeHTML(value)}</strong></div>${percent == null ? '' : `<div class="metric-bar"><span style="width:${Math.min(100,Math.max(0,Number(percent) || 0))}%"></span></div>`}</div>`;
}
function telemetry() {
  const selected = selectedServer();
  if (!selected) return emptyState();
  const server = {...selected, ...(state.telemetry?.server || state.telemetry?.current || state.telemetry?.latest || {})};
  const metricsAvailable = Boolean(server.metricsAvailable);
  const unavailable = '—';
  return `<div class="toolbar"><strong>${escapeHTML(server.name)}</strong><label class="sr-only" for="telemetry-range">Telemetry time range</label><select id="telemetry-range" data-testid="telemetry-range">${[['60s','Last 60 seconds'],['5m','Last 5 minutes']].map(([value,label]) => `<option value="${value}" ${state.range===value?'selected':''}>${label}</option>`).join('')}</select><button data-action="export-telemetry" data-testid="export-telemetry">${icon('download')}Export CSV</button></div><div class="stats">${stat('Active players', `${number(server.players)} / ${number(server.maxPlayers)}`, 'server')}${stat('Tick rate', metricsAvailable ? number(server.tickRate, ' TPS') : unavailable, 'pulse')}${stat('World uptime', duration(server.uptimeSeconds), 'clock')}${stat('CPU usage', metricsAvailable ? number(server.cpuPercent, '%') : unavailable, 'cpu')}</div><div class="split"><section class="panel"><div class="panel-heading"><h2>Tick rate</h2>${status(server.status)}</div>${chart('tickRate','Tick rate (TPS)')}</section><section class="panel"><div class="panel-heading"><h2>Resource usage</h2></div>${resource('CPU', metricsAvailable ? number(server.cpuPercent,'%') : unavailable, metricsAvailable ? server.cpuPercent : null)}${resource('Memory', metricsAvailable ? `${bytes(server.memoryUsedBytes)} / ${bytes(server.memoryLimitBytes)}` : unavailable, metricsAvailable && server.memoryLimitBytes && server.memoryUsedBytes != null ? server.memoryUsedBytes / server.memoryLimitBytes * 100 : null)}${resource('Disk', metricsAvailable ? number(server.diskPercent,'%') : unavailable, metricsAvailable ? server.diskPercent : null)}${resource('Network', metricsAvailable && server.networkBytesPerSecond != null ? `${bytes(server.networkBytesPerSecond)}/s` : unavailable,null)}</section></div>${telemetryPanels()}<section class="panel section-gap"><div class="panel-heading"><div><h2>Server logs</h2><p>Latest 100 lines · ${escapeHTML(server.name)}</p></div><div class="toolbar"><button data-action="refresh-logs" data-testid="refresh-logs">${icon('refresh')}Refresh logs</button><button data-action="export-logs" data-testid="export-logs">${icon('download')}Download logs</button></div></div><div class="toolbar"><label class="search-field">${icon('search')}<span class="sr-only">Search logs</span><input id="log-search" data-testid="log-search" type="search" placeholder="Search these log lines…" value="${escapeHTML(state.logQuery)}"></label></div><pre class="log-console" id="log-output" tabindex="0" aria-label="Server logs" data-testid="log-output">${escapeHTML(filteredLogs() || 'No log lines match this view.')}</pre><p class="inline-note">Unavailable metrics appear as —. Values depend on the server’s telemetry source.</p></section>`;
}
function filteredLogs() { return state.logs.split('\n').filter((line) => line.toLowerCase().includes(state.logQuery.toLowerCase())).join('\n'); }
function telemetryPanels() {
  const checks = state.telemetry?.healthChecks || [];
  return `<div class="split section-gap"><section class="panel"><div class="panel-heading"><h2>Player count</h2></div>${chart('players','Active players')}</section><section class="panel"><div class="panel-heading"><h2>Health checks</h2></div>${checks.length ? `<dl class="detail-list">${checks.map((check) => `<div><dt>${escapeHTML(check.name)}</dt><dd>${status(String(check.status || 'unknown').toLowerCase())}<small class="check-time">${escapeHTML(date(check.at))}</small></dd></div>`).join('')}</dl>` : '<p class="no-results">No health checks reported yet.</p>'}</section></div>`;
}
function eventsPage() {
  const selected = state.events.find((event) => event.id === state.selectedEventId);
  const warnings = state.events.filter((event) => event.severity === 'warning').length;
  const critical = state.events.filter((event) => event.severity === 'critical' || event.severity === 'error').length;
  return `<div class="toolbar"><label class="search-field">${icon('search')}<span class="sr-only">Search events</span><input type="search" id="event-search" data-testid="event-search" placeholder="Search events…" value="${escapeHTML(state.query)}"></label><button data-action="export-events" data-testid="export-events">${icon('download')}Export CSV</button></div><div class="stats">${stat('Matching events',state.events.length,'events')}${stat('Warnings',warnings,'warning','amber')}${stat('Critical',critical,'pulse',critical?'red':'')}${stat('Servers in view',scopedServers().length,'server')}</div><div class="split"><section class="panel"><div class="panel-heading"><h2>Event stream</h2></div><div class="toolbar chips" aria-label="Event category">${[['','All'],['system','System'],['player','Players'],['health','Health'],['update','Updates']].map(([value,label])=>`<button data-action="event-category" data-value="${value}" data-testid="category-${value || 'all'}" aria-pressed="${state.category===value}">${label}</button>`).join('')}</div>${eventTable(state.events,true)}</section><section class="panel"><div class="panel-heading"><h2>Event details</h2></div>${selected ? `<dl class="detail-list"><div><dt>Event</dt><dd>${escapeHTML(selected.message)}</dd></div><div><dt>Server</dt><dd>${escapeHTML(selected.serverName || selected.serverId || 'Cluster')}</dd></div><div><dt>Severity</dt><dd>${status(selected.severity || 'info')}</dd></div><div><dt>Time</dt><dd>${escapeHTML(date(selected.timestamp))}</dd></div></dl><pre class="event-json">${escapeHTML(typeof selected.details === 'string' ? selected.details : JSON.stringify(selected.details || {},null,2))}</pre><button data-action="copy-event" data-testid="copy-event">Copy event JSON</button>` : '<p class="no-results">Select an event to inspect its details.</p>'}</section></div>`;
}
function maintenance() {
  const server = selectedServer();
  if (!server) return emptyState();
  return `<div class="split"><section class="panel"><div class="panel-heading"><div><h2>Server lifecycle</h2><p>${escapeHTML(server.name)}</p></div>${status(server.status)}</div><dl class="detail-list"><div><dt>Current image</dt><dd class="mono">${escapeHTML(server.currentImage || 'Not reported')}</dd></div><div><dt>Desired image</dt><dd class="mono">${escapeHTML(server.desiredImage || 'Not configured')}</dd></div><div><dt>World uptime</dt><dd>${duration(server.uptimeSeconds)}</dd></div><div><dt>Last restart</dt><dd>${escapeHTML(date(server.lastRestart))}</dd></div><div><dt>Namespace</dt><dd class="mono">${escapeHTML(server.namespace || '—')}</dd></div><div><dt>Connection endpoint</dt><dd class="mono">${escapeHTML(server.endpoint || 'Waiting for deployment')}</dd></div></dl><div class="action-grid"><div class="action-card"><button class="danger" data-action="restart" data-testid="restart-server">${icon('refresh')}Restart server</button><p>Disconnects active players and restarts this world.</p></div><div class="action-card"><button data-action="update" data-testid="update-image">${icon('download')}Update image</button><p>Choose a container image tag and roll out the update.</p></div><div class="action-card"><button data-action="check-update" data-testid="check-update">${icon('search')}Check update</button><p>Compare the current image with the desired image.</p></div></div><p class="inline-note">Restart and update actions require confirmation.</p></section><div class="stack"><section class="panel"><div class="panel-heading"><h2>Readiness</h2></div><dl class="detail-list"><div><dt>Server health</dt><dd>${status(server.status)}</dd></div><div><dt>Players connected</dt><dd>${number(server.players)} / ${number(server.maxPlayers)}</dd></div><div><dt>Image status</dt><dd class="${server.updateAvailable ? 'amber' : ''}">${server.updateAvailable ? 'Update available' : 'No update reported'}</dd></div><div><dt>Last seen</dt><dd>${escapeHTML(date(server.lastSeen))}</dd></div></dl><p class="inline-note">Choose a quiet moment for maintenance. Active players will be disconnected.</p></section><section class="panel"><h2>Update checks</h2><p class="inline-note">Checks compare the running image with the newest semver tag visible in the configured registry when the registry allows anonymous tag listing.</p></section></div></div><section class="panel section-gap"><div class="panel-heading"><h2>Recent activity</h2><button class="link-button" data-action="view-events" data-testid="maintenance-events">View events ${icon('arrow')}</button></div>${eventTable(state.events.filter((event) => event.serverId === server.id).slice(0,6))}</section>`;
}
function render() {
  if (state.authRequired) { lockedState(); return; }
  const focused = document.activeElement;
  const testId = focused?.getAttribute('data-testid');
  const selection = focused instanceof HTMLInputElement ? [focused.selectionStart, focused.selectionEnd] : null;
  const [title,description] = pages[state.page];
  document.title = `${title} · RSDW C2`;
  $('#page-title').textContent = title;
  $('#page-description').textContent = description;
  $('#navigation').innerHTML = Object.entries(pages).map(([page,[label]])=>`<a href="#${page}" data-testid="nav-${page}" ${page===state.page?'aria-current="page"':''}>${icon(page)}${label}</a>`).join('');
  const individual = state.page === 'telemetry' || state.page === 'maintenance';
  $('#server-filter').innerHTML = `${individual && state.servers.length ? '' : '<option value="">All servers</option>'}${state.servers.map((server)=>`<option value="${escapeHTML(server.id)}">${escapeHTML(server.name)}</option>`).join('')}`;
  $('#server-filter').value = individual ? selectedServer()?.id || '' : state.serverId;
  $('#server-filter').disabled = !state.servers.length;
  $('#content').innerHTML = ({dashboard,telemetry,events:eventsPage,maintenance})[state.page]();
  $('#content').setAttribute('aria-busy','false');
  if (testId) {
    const replacement = document.querySelector(`[data-testid="${CSS.escape(testId)}"]`);
    replacement?.focus({preventScroll:true});
    if (selection && replacement instanceof HTMLInputElement && replacement.type === 'search') replacement.setSelectionRange(...selection);
  }
}
function connection(failed = !$('#error-banner').hidden) {
  $('#connection-status').textContent = failed ? 'Connection issue · data may be stale' : state.paused ? 'Updates paused' : 'Connected · refreshes every 10s';
  $('#connection-status').classList.toggle('connected',!failed && !state.paused);
  $('#updated-at').textContent = state.lastUpdated ? `Updated ${state.lastUpdated.toLocaleTimeString()}` : 'Waiting for first update';
  $('#pause').textContent = state.paused ? 'Resume updates' : 'Pause updates';
  $('#pause').setAttribute('aria-pressed',String(state.paused));
}
async function loadLogs(signal) {
  const server = selectedServer();
  if (!server) return;
  const result = await api(`/api/servers/${encodeURIComponent(server.id)}/logs?tail=100`, {signal});
  const lines = typeof result === 'string' ? result : result.lines ?? result.logs ?? '';
  state.logs = Array.isArray(lines) ? lines.map((line) => typeof line === 'string' ? line : `${date(line.timestamp)} [${line.level || 'INFO'}] ${line.message || ''}`).join('\n') : String(lines);
}
async function refresh() {
  if (state.authRequired) { lockedState(); return; }
  state.request?.abort();
  const controller = new AbortController();
  state.request = controller;
  state.refreshing = true;
  $('#refresh').disabled = true;
  try {
    const bootstrap = await api('/api/bootstrap',{signal:controller.signal});
    if (controller.signal.aborted) return;
    if (!Array.isArray(bootstrap.servers)) throw new Error('The server inventory response is missing. Please refresh to try again.');
    state.servers = bootstrap.servers;
    state.loaded = true;
    if (state.serverId && !state.servers.some((server) => server.id === state.serverId)) state.serverId = '';
    $('#cluster-name').textContent = typeof bootstrap.cluster === 'string' ? bootstrap.cluster : bootstrap.cluster?.name || bootstrap.clusterName || 'Local cluster';
    $('#environment').textContent = bootstrap.mode === 'demo' ? 'Demo mode' : 'Kubernetes';
    const eventParams = new URLSearchParams({query:state.page === 'events' ? state.query : '', category:state.page === 'events' ? state.category : '', serverId:state.serverId});
    const eventsPromise = api(`/api/events?${eventParams}`,{signal:controller.signal}).then((result) => { state.events = eventArray(result); });
    const server = selectedServer();
    const requests = [eventsPromise];
    if (state.page === 'telemetry' && server) {
      requests.push(api(`/api/servers/${encodeURIComponent(server.id)}/telemetry?range=${encodeURIComponent(state.range)}`,{signal:controller.signal}).then((result) => { state.telemetry = result; }));
      requests.push(loadLogs(controller.signal));
    }
    const results = await Promise.allSettled(requests);
    if (controller.signal.aborted) return;
    const failure = results.find((result) => result.status === 'rejected');
    if (failure) throw failure.reason;
    state.lastUpdated = new Date();
    $('#error-banner').hidden = true;
    render();
    connection();
  } catch (error) {
    if (controller.signal.aborted) return;
    if (state.authRequired) { lockedState(); return; }
    $('#error-banner').textContent = error.message;
    $('#error-banner').hidden = false;
    if (state.loaded) render();
    else $('#content').innerHTML = '<section class="panel empty"><div class="empty-icon">'+icon('warning')+'</div><h2>Unable to connect</h2><p>Your server inventory could not be loaded. Check the connection and try again.</p><button data-action="retry" data-testid="retry-connection">Try again</button></section>';
    $('#content').setAttribute('aria-busy','false');
    connection(true);
  } finally {
    if (state.request === controller) { state.refreshing = false; $('#refresh').disabled = false; }
  }
}
function openModal(action) {
  modalOpener = document.activeElement;
  state.modalAction = action;
  state.modalServerId = selectedServer()?.id || '';
  $('#modal-error').hidden = true;
  $('#modal-submit').disabled = false;
  $('#modal-submit').classList.toggle('danger',action === 'restart');
  $('#modal-submit').classList.toggle('primary',action !== 'restart');
  if (action === 'add-server') {
    $('#modal-title').textContent = 'Create a server';
    $('#modal-submit').textContent = 'Deploy server';
    $('#modal-body').innerHTML = '<p>A new Dragonwilds world, deployed to your Kubernetes cluster.</p><div class="form-grid"><label class="field full">Server name<input name="name" data-testid="server-name" required maxlength="48" placeholder="My Dragonwilds world" autocomplete="off" autofocus></label><label class="field">Namespace<input name="namespace" data-testid="server-namespace" required maxlength="63" pattern="[a-z0-9]([a-z0-9-]*[a-z0-9])?" value="dragonwilds" title="Lowercase letters, numbers, and hyphens; start and end with a letter or number"><small>Lowercase letters, numbers, and hyphens.</small></label><label class="field">Region<input name="region" data-testid="server-region" required maxlength="63" value="local"></label><label class="field full">Owner ID<input name="ownerId" data-testid="server-owner" required maxlength="128" autocomplete="off" placeholder="Your game account ID"><small>The account that owns this world.</small></label><label class="field">Image tag<input name="imageTag" data-testid="server-image" required pattern="[A-Za-z0-9][A-Za-z0-9_.-]{0,63}" value="latest" title="A valid container image tag"></label><label class="field">Max players<input name="maxPlayers" data-testid="server-max-players" type="number" min="1" max="64" value="4" required></label></div>';
  } else {
    const server = selectedServer();
    $('#modal-title').textContent = action === 'restart' ? 'Restart this server?' : 'Update server image';
    $('#modal-submit').textContent = action === 'restart' ? 'Confirm restart' : 'Confirm update';
    $('#modal-body').innerHTML = `<p><strong>${escapeHTML(server.name)}</strong> will ${action === 'restart' ? 'restart' : 'restart with the selected image'}. Active players will be disconnected. Wait for a quiet moment before continuing.</p>${action === 'update' ? '<label class="field">Container image tag<input name="imageTag" data-testid="update-image-tag" required pattern="[A-Za-z0-9][A-Za-z0-9_.-]{0,63}" placeholder="e.g. latest" title="A valid container image tag"><small>Enter the tag to deploy from the configured image repository.</small></label>' : ''}`;
  }
  $('#modal').showModal();
  if (action !== 'add-server') $('[data-testid="cancel-modal"]').focus();
}
function closeModal() {
  if (state.modalBusy) return;
  $('#modal').close();
  modalOpener?.focus();
}
async function submitModal(event) {
  event.preventDefault();
  if (state.modalBusy || !$('#modal-form').reportValidity()) return;
  const values = Object.fromEntries(new FormData($('#modal-form')));
  const action = state.modalAction;
  const body = action === 'add-server' ? {...values, maxPlayers:Number(values.maxPlayers)} : action === 'update' ? {imageTag:values.imageTag} : {};
  const path = action === 'add-server' ? '/api/servers' : `/api/servers/${encodeURIComponent(state.modalServerId)}/actions/${action}`;
  state.modalBusy = true;
  $('#modal-form').setAttribute('aria-busy','true');
  $('#modal-submit').disabled = true;
  $('#modal-error').hidden = true;
  $('#modal').querySelectorAll('[data-action="close-modal"]').forEach((button) => { button.disabled = true; });
  try {
    await api(path,{method:'POST',body:JSON.stringify(body)});
    state.modalBusy = false;
    closeModal();
    notice(action === 'add-server' ? 'Server deployment requested.' : action === 'restart' ? 'Server restart requested.' : 'Image update requested.');
    await refresh();
  } catch (error) {
    $('#modal-error').textContent = error.message;
    $('#modal-error').hidden = false;
    $('#modal-error').focus();
  } finally {
    state.modalBusy = false;
    $('#modal-form').setAttribute('aria-busy','false');
    $('#modal-submit').disabled = false;
    $('#modal').querySelectorAll('[data-action="close-modal"]').forEach((button) => { button.disabled = false; });
  }
}
function download(filename,content,type) {
  const url = URL.createObjectURL(new Blob([content],{type}));
  const anchor = document.createElement('a');
  anchor.href = url; anchor.download = filename;
  document.body.append(anchor); anchor.click(); anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url),1000);
}
function exportCSV(filename,rows,columns) {
  const cell = (value) => {
    let text = typeof value === 'object' && value !== null ? JSON.stringify(value) : String(value ?? '');
    if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`;
    return `"${text.replace(/"/g,'""')}"`;
  };
  download(filename,[columns, ...rows.map((row) => columns.map((column) => row[column]))].map((row) => row.map(cell).join(',')).join('\r\n'),'text/csv;charset=utf-8');
  notice(`Exported ${rows.length} ${rows.length === 1 ? 'record' : 'records'}.`);
}
async function handleAction(event) {
  const button = event.target.closest('button[data-action]');
  if (!button || button.disabled) return;
  const action = button.dataset.action;
  try {
    switch (action) {
      case 'login': requireLogin(); break;
      case 'close-login': closeLogin(); break;
      case 'add-server': case 'restart': case 'update': openModal(action); break;
      case 'close-modal': closeModal(); break;
      case 'retry': await refresh(); break;
      case 'fleet-filter': state.fleetFilter = button.dataset.value; render(); break;
      case 'view-server': state.serverId = button.dataset.id; location.hash = 'telemetry'; break;
      case 'view-events': location.hash = 'events'; break;
      case 'event-category': state.category = button.dataset.value; state.selectedEventId = ''; await refresh(); break;
      case 'select-event': state.selectedEventId = button.dataset.id; render(); break;
      case 'copy-event': {
        const selected = state.events.find((item) => item.id === state.selectedEventId);
        await navigator.clipboard.writeText(JSON.stringify(selected,null,2)); notice('Event JSON copied.'); break;
      }
      case 'export-events': exportCSV('rsdw-events.csv',state.events,['timestamp','serverName','category','severity','message','details']); break;
      case 'export-telemetry': exportCSV('rsdw-telemetry.csv',samples(),['timestamp','players','tickRate','inboundBytesPerSecond','outboundBytesPerSecond']); break;
      case 'export-logs': download('rsdw-server.log',filteredLogs(),'text/plain;charset=utf-8'); notice('Log download ready.'); break;
      case 'refresh-logs': button.disabled = true; await loadLogs(); render(); notice('Server logs refreshed.'); break;
      case 'check-update': {
        button.disabled = true;
        const result = await api(`/api/servers/${encodeURIComponent(selectedServer().id)}/actions/check-update`,{method:'POST',body:'{}'});
        await refresh();
        const available = result.updateAvailable ?? result.server?.updateAvailable ?? selectedServer()?.updateAvailable;
        notice(available ? 'An image update is available.' : 'Update check complete. No image change reported.'); break;
      }
    }
  } catch (error) { notice(error.message || 'The action could not be completed.'); }
  finally { if (button.isConnected) button.disabled = false; }
}
function navigate() {
  const page = location.hash.slice(1).split('?')[0];
  state.page = pages[page] ? page : 'dashboard';
  $('#navigation').innerHTML = Object.entries(pages).map(([name,[label]])=>`<a href="#${name}" data-testid="nav-${name}" ${name===state.page?'aria-current="page"':''}>${icon(name)}${label}</a>`).join('');
  $('#page-title').textContent = pages[state.page][0];
  $('#page-description').textContent = pages[state.page][1];
  state.telemetry = null;
  state.logs = '';
  if (state.loaded) render();
  refresh();
}
$('#refresh').innerHTML = icon('refresh');
$('#refresh').addEventListener('click',refresh);
$('#pause').addEventListener('click',() => { state.paused = !state.paused; connection(); if (!state.paused) refresh(); });
$('#server-filter').addEventListener('change',(event) => { state.serverId = event.target.value; state.telemetry = null; state.logs = ''; state.selectedEventId = ''; refresh(); });
$('#modal-form').addEventListener('submit',submitModal);
$('#login-form').addEventListener('submit',submitLogin);
$('#login-dialog').addEventListener('cancel',(event) => { event.preventDefault(); closeLogin(); });
$('#modal').addEventListener('cancel',(event) => { event.preventDefault(); closeModal(); });
document.addEventListener('click',handleAction);
document.addEventListener('input',(event) => {
  if (event.target.id === 'admin-token') event.target.removeAttribute('aria-invalid');
  if (event.target.id === 'event-search') {
    state.query = event.target.value;
    state.selectedEventId = '';
    clearTimeout(searchTimer);
    searchTimer = setTimeout(refresh,300);
  }
  if (event.target.id === 'log-search') {
    state.logQuery = event.target.value;
    $('#log-output').textContent = filteredLogs() || 'No log lines match this view.';
  }
});
document.addEventListener('change',(event) => { if (event.target.id === 'telemetry-range') { state.range = event.target.value; refresh(); } });
window.addEventListener('hashchange',navigate);
document.addEventListener('visibilitychange',() => { if (!document.hidden && !state.paused && !$('#modal').open) refresh(); });
setInterval(() => {
  if (state.paused || state.authRequired || state.refreshing || document.hidden || $('#modal').open || $('#login-dialog').open || ['INPUT','SELECT'].includes(document.activeElement.tagName)) return;
  refresh();
},10000);
navigate();
