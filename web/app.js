'use strict';

const $ = (selector) => document.querySelector(selector);
const escapeHTML = (value) => String(value ?? '').replace(/[&<>"']/g, (char) => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[char]));
const PATTERN = Object.freeze({
  eosId: '[0-9a-fA-F]{32}',
  dnsLabel: '[a-z0-9](([a-z0-9]|-)*[a-z0-9])?',
  snowflake: '[0-9]{1,20}',
});
const DEPLOY_WATCH_TIMEOUT_MS = 10 * 60 * 1000;
const categoryLabels = { system:'System', player:'Players', health:'Health', update:'Updates' };
const icons = {
  dashboard: '<rect x="3" y="3" width="7" height="7"/><rect x="14" y="3" width="7" height="7"/><rect x="3" y="14" width="7" height="7"/><rect x="14" y="14" width="7" height="7"/>',
  telemetry: '<path d="M4 20V13m5 7V7m6 13V3m5 17V10"/>',
  events: '<rect x="4" y="4" width="16" height="17" rx="2"/><path d="M8 2v4m8-4v4M4 10h16m-12 4h8m-8 3h5"/>',
  maintenance: '<path d="m14 6 4 4m-2-7a6 6 0 0 0-7 8L3 17a3 3 0 0 0 4 4l6-6a6 6 0 0 0 8-7l-4 4-5-5Z"/>',
  reboots: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l4 2M5 4l-2 2m16-2 2 2"/>',
  users: '<circle cx="9" cy="8" r="3"/><path d="M3 19c0-3 3-5 6-5s6 2 6 5"/><circle cx="17" cy="9" r="2.5"/><path d="M16 19c0-2 1.5-3.5 3.5-4"/>',
  integrations: '<rect x="5" y="7" width="14" height="11" rx="2"/><path d="M9 7V5m6 2V5M9 18v2m6-2v2M8 13h.01M16 13h.01M11 13h2"/>',
  server: '<rect x="3" y="3" width="18" height="7" rx="2"/><rect x="3" y="14" width="18" height="7" rx="2"/><path d="M7 6.5h.01M7 17.5h.01M12 6.5h5M12 17.5h5"/>',
  pulse: '<path d="M2 12h5l3-8 4 16 3-8h5"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l4 2"/>',
  warning: '<path d="m12 3 10 18H2Z M12 9v5m0 3v.01"/>',
  refresh: '<path d="M20 7v5h-5M4 17v-5h5"/><path d="M6 6a8 8 0 0 1 14 6M4 12a8 8 0 0 0 14 6"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  back: '<path d="m15 6-6 6 6 6"/>',
  arrow: '<path d="M4 12h16m-6-6 6 6-6 6"/>',
  download: '<path d="M12 3v12m-4-4 4 4 4-4M4 16v5h16v-5"/>',
  search: '<circle cx="10" cy="10" r="7"/><path d="m15 15 6 6"/>',
  check: '<path d="m5 12 4 4L19 6"/>',
  cpu: '<rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v4m6-4v4M9 18v4m6-4v4M2 9h4m-4 6h4m12-6h4m-4 6h4"/>',
};
const icon = (name) => `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${icons[name] || icons.server}</svg>`;
const pages = {
  dashboard: ['Dashboard', 'Monitor every server from one place.'],
  servers: ['Server overview', 'Status, players, and server controls.'],
  telemetry: ['Telemetry', 'Live performance and resource usage.'],
  events: ['Events', 'Search every server event in one place.'],
  maintenance: ['Maintenance', 'Make safe changes to your servers.'],
  users: ['Saved IDs', 'Manage reusable Dragonwilds player IDs.'],
  integrations: ['Integrations', 'Connect Discord and other alert destinations.'],
  reboots: ['Reboots', 'Schedule per-server restarts with predictable timezone rules.'],
};
const discordStatusCopy = {
  not_connected: ['Not connected', 'unknown'],
  connected: ['Connected', 'enabled'],
  disconnected: ['Disconnected', 'error'],
};
const state = {
  deletions: {},
  routeError: '',
  routeScope: false,
  deployWatches: {},
  page: 'dashboard', integrationView: 'hub', servers: [], events: [], users: [], serverId: '', fleetFilter: 'all',
  integrations: [], deliveries: [], alertRules: [], pendingRestarts: {}, integrationsDemo: false, modalIntegrationId: '',
  reboots: [], rebootHistory: [], rebootsAvailable: true, rebootsDemo: false, displayTimezone: '', authSubject: '', modalRebootId: '', previewSequence: 0,
  query: '', category: '', range: '60s', eventRange: '24h', eventTotal: 0, eventWarnings: 0, eventCritical: 0, telemetry: null, rosterObservation: '', rosterDeadline: 0, logs: '', logQuery: '',
  selectedEventId: '', paused: false, loaded: false, lastUpdated: null, refreshing: false,
  modalAction: '', modalServerId: '', modalUserId: '', modalBusy: false, modalInitialSettings: {}, request: null,
  authRequired: false, loginBusy: false, authMode: '', identity: '', csrfToken: '', role: 'denied', capabilities: {}, epoch: 0, logoutCSRF: '', signInFailed: false,
};
const tokenKey = 'rsdw-admin-token';
let searchTimer;
let toastTimer;
let modalOpener;
let refreshSequence = 0;
let rosterExpiryTimer;
const can = (capability) => state.capabilities[({servers:'telemetry', users:'create', 'edit-settings':'maintenance', 'add-user':'create', 'edit-user':'create', 'delete-user':'create', 'add-integration':'integrations', 'edit-integration':'integrations', 'test-integration':'integrations', 'add-reboot':'reboots', 'edit-reboot':'reboots', 'delete-reboot':'reboots', 'preview-reboot':'reboots'})[capability] || capability] === true;
function parseLocationHash(hash) {
  const raw = String(hash ?? '').replace(/^#/, '').split('?')[0];
  const slash = raw.indexOf('/');
  const pageKey = slash === -1 ? raw : raw.slice(0, slash);
  const view = slash === -1 ? '' : raw.slice(slash + 1);
  if (pageKey === 'servers') {
    try {
      const serverId = decodeURIComponent(view);
      return {page:'servers', integrationView:'hub', serverId, scoped:true, malformed: !serverId || view.includes('/')};
    } catch { return {page:'servers', integrationView:'hub', serverId:'', scoped:true, malformed:true}; }
  }
  if (['telemetry','maintenance','events','reboots'].includes(pageKey)) {
    const query = String(hash).split('?').slice(1).join('?');
    if (query) {
      const params = new URLSearchParams(query);
      return {page:pageKey, integrationView:'hub', serverId:params.get('serverId') || '', scoped:params.has('serverId')};
    }
  }
  if (pageKey === 'integrations') {
    return {page:'integrations', integrationView: ['discord','webhook'].includes(view) ? view : 'hub'};
  }
  return {page: pages[pageKey] ? pageKey : 'dashboard', integrationView:'hub'};
}
function scopedHash(page, id = state.serverId) {
  return `#${page}${id ? `?serverId=${encodeURIComponent(id)}` : ''}`;
}
function applyRouteScope() {
  const route = parseLocationHash(location.hash);
  state.routeScope = route.scoped === true;
  if (route.serverId !== undefined) state.serverId = route.serverId;
  if (route.page === 'servers' && can('telemetry')) state.page = 'servers';
}
function validateOverviewRoute() {
  const route = parseLocationHash(location.hash);
  if (route.page === 'servers' && (route.malformed || !state.servers.some((server) => server.id === state.serverId))) {
    state.routeError = `Server overview unavailable: ${route.malformed || !state.serverId ? 'malformed server ID' : `unknown server ID "${state.serverId}"`}.`;
    state.page = 'dashboard';
    state.serverId = '';
    state.routeScope = false;
    clearTelemetry();
    history.replaceState(null, '', '#dashboard');
  }
}
function pageHeading() {
  if (state.page === 'integrations' && state.integrationView === 'webhook') return ['HTTPS webhooks', 'Send selected server alerts to an HTTPS endpoint.'];
  if (state.page === 'integrations' && state.integrationView === 'discord') {
    return ['Discord', 'Configure Discord alerts for your Dragonwilds servers.'];
  }
  return pages[state.page] || pages.dashboard;
}
function alertingIntegration(item) {
  return item?.enabled === true && Array.isArray(item.serverIds) && item.serverIds.length > 0 && item.rules && Object.values(item.rules).some((value) => value === true);
}
function deliveryTime(delivery) {
  const value = Date.parse(delivery?.updatedAt || delivery?.event?.timestamp || '');
  return Number.isFinite(value) ? value : 0;
}
function deliveryWasCancelled(delivery) {
  const result = String(delivery?.result || '');
  return result.startsWith('Cancelled by ') || result === 'Integration no longer enables this delivery';
}
function latestAttemptedDeliveryFor(integrationId, deliveries) {
  let latest = null;
  for (const delivery of deliveries || []) {
    if (delivery.integrationId !== integrationId || deliveryWasCancelled(delivery)) continue;
    if (!latest || deliveryTime(delivery) > deliveryTime(latest)) latest = delivery;
  }
  return latest;
}
function discordConnectionStatus(integrations, deliveries) {
  const list = integrations || [];
  if (!list.length) return 'not_connected';
  const alerting = list.filter(alertingIntegration);
  if (!alerting.length) return 'disconnected';
  return alerting.every((item) => latestAttemptedDeliveryFor(item.id, deliveries)?.status === 'failed') ? 'disconnected' : 'connected';
}
const staleRequest = () => new DOMException('Session changed', 'AbortError');
const monotonicNow = () => typeof performance !== 'undefined' && typeof performance.now === 'function' ? performance.now() : Date.now();
const rosterObservationKey = (telemetry) => telemetry?.server?.id && telemetry?.metrics?.players?.observedAt ? `${telemetry.server.id}\n${telemetry.metrics.players.observedAt}` : '';

function clearTelemetry() {
  state.telemetry = null;
  state.rosterObservation = '';
  state.rosterDeadline = 0;
}

function acceptTelemetry(result, startedAt, receivedAt) {
  const key = rosterObservationKey(result);
  const metric = result?.metrics?.players;
  const freshForMs = Number(result?.playerRoster?.freshForMs);
  state.telemetry = result;
  if (key && metric?.status === 'available' && result.playerRoster?.status === 'available' && Number.isFinite(freshForMs) && freshForMs >= 0 && freshForMs <= 45000) {
    const deadline = receivedAt + Math.max(0, freshForMs - Math.max(0, receivedAt - startedAt));
    state.rosterDeadline = state.rosterObservation === key && state.rosterDeadline > 0 ? Math.min(state.rosterDeadline, deadline) : deadline;
    state.rosterObservation = key;
  } else {
    state.rosterObservation = '';
    state.rosterDeadline = 0;
  }
}

function number(value, suffix = '') {
  return value == null || value === '' || !Number.isFinite(Number(value)) ? '—' : `${Number(value).toLocaleString(undefined, Math.abs(Number(value)) < 1 ? {maximumSignificantDigits:3} : {maximumFractionDigits:1})}${suffix}`;
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
function date(value, timeOnly = false) {
  if (!value) return '—';
  const parsed = new Date(value);
  if (Number.isNaN(parsed.valueOf())) return '—';
  const timeZone = validDisplayTimezone() || undefined;
  const options = timeZone ? {timeZone} : undefined;
  return timeOnly ? parsed.toLocaleTimeString(undefined, options) : parsed.toLocaleString(undefined, options);
}
function validTimezone(value) {
  if (!value) return false;
  try { new Intl.DateTimeFormat(undefined, {timeZone:value}).format(); return true; } catch { return false; }
}
function validDisplayTimezone() { return validTimezone(state.displayTimezone) ? state.displayTimezone : ''; }
function browserTimezone() {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
}
function timezoneOptions(selected = state.displayTimezone || browserTimezone()) {
  let zones = ['UTC', 'America/New_York', 'America/Los_Angeles', 'Europe/London', 'Europe/Berlin', 'Asia/Tokyo', 'Australia/Sydney'];
  try { if (typeof Intl.supportedValuesOf === 'function') zones = ['UTC', ...Intl.supportedValuesOf('timeZone')]; } catch {}
  if (selected && !zones.includes(selected)) zones.unshift(selected);
  return [...new Set(zones)].sort((a, b) => a === selected ? -1 : b === selected ? 1 : a.localeCompare(b));
}
function displayPreferenceKey() { return `rsdw-display-timezone:${state.authMode}:${state.authSubject || 'unknown'}`; }
function loadDisplayTimezone() {
  const fallback = browserTimezone();
  let saved = '';
  try { saved = localStorage.getItem(displayPreferenceKey()) || ''; } catch {}
  state.displayTimezone = validTimezone(saved) ? saved : validTimezone(fallback) ? fallback : '';
}
function saveDisplayTimezone(value) {
  if (!validTimezone(value)) return;
  state.displayTimezone = value;
  try { localStorage.setItem(displayPreferenceKey(), value); } catch {}
}
function zonedDate(value, zone) {
  if (!value) return '—';
  const parsed = new Date(value);
  if (Number.isNaN(parsed.valueOf())) return '—';
  const selected = validTimezone(zone) ? zone : validDisplayTimezone() || 'UTC';
  let formatted;
  try { formatted = parsed.toLocaleString(undefined, {timeZone:selected, timeZoneName:'shortOffset'}); }
  catch { formatted = parsed.toLocaleString(undefined, {timeZone:selected}); }
  return `${formatted} (${selected})`;
}
function status(value = 'unknown') {
  const known = ['online', 'starting', 'attention', 'stopped', 'deleting', 'stale', 'unknown', 'warning', 'critical', 'error', 'success', 'healthy', 'enabled', 'disabled'];
  const label = {deleting:'DELETING', stale:'STALE', enabled:'Enabled', disabled:'Disabled'}[value] || value.charAt(0).toUpperCase() + value.slice(1);
  return `<span class="status ${known.includes(value) ? value : 'unknown'}">${escapeHTML(label)}</span>`;
}
function stat(label, value, name, tone = '') {
  if ((tone === 'amber' || tone === 'red') && !(Number(value) > 0)) tone = '';
  return `<section class="panel stat">${icon(name)}<div><div class="stat-value ${tone} ${String(value).length > 10 ? 'stat-text' : ''}">${escapeHTML(value)}</div><div class="stat-label">${escapeHTML(label)}</div></div></section>`;
}
function selectedServer() { return state.servers.find((server) => server.id === state.serverId) || (state.routeScope || state.page === 'servers' ? undefined : state.servers[0]); }
function worldLabel(server) { return [server.worldName, server.name, server.id].map((value) => String(value || '').trim()).find(Boolean) || ''; }
function serverLabel(server) {
  const label = worldLabel(server);
  return state.servers.some((other) => other.id !== server.id && worldLabel(other) === label) ? `${label} (${server.id})` : label;
}
function parseAdminIds(value) {
  const ids = [];
  const seen = new Set();
  for (const part of String(value || '').split(',')) {
    const id = part.trim().toLowerCase();
    if (!id || seen.has(id)) continue;
    seen.add(id);
    ids.push(id);
  }
  return ids;
}
function normalizePlayerIdClient(value) {
  const id = String(value || '').trim().toLowerCase();
  if (!new RegExp(`^${PATTERN.eosId}$`).test(id)) throw new Error('Administrator IDs must contain exactly 32 hexadecimal characters, without spaces or separators.');
  return id;
}
function adminIdsFields(users, selectedPlayerIds = []) {
  const selected = new Set(parseAdminIds((selectedPlayerIds || []).join(',')));
  const leftover = [...selected].filter((id) => !(users || []).some((user) => String(user.playerId || '').toLowerCase() === id));
  const saved = (users || []).map((user) => {
    const playerId = String(user.playerId || '');
    const checked = selected.has(playerId.toLowerCase()) ? ' checked' : '';
    return `<label class="choice-row"><input type="checkbox" name="adminPlayerId" value="${escapeHTML(playerId)}" data-testid="admin-saved-id"${checked}><span class="choice-copy"><strong>${escapeHTML(user.name || 'Saved ID')}</strong><small class="mono">${escapeHTML(playerId)}</small></span></label>`;
  }).join('') || '<p>No saved IDs yet. Add them on the Saved IDs page, or enter a player ID below.</p>';
  return `<fieldset class="form-section field full" data-testid="admin-ids"><legend>Administrator EOS IDs</legend><p>Optional. Tick saved IDs and/or type one extra 32-hex ID. Empty is allowed.</p>${saved}<label class="field">Additional administrator ID<input name="adminPlayerIdManual" data-testid="admin-manual-id" maxlength="32" pattern="${PATTERN.eosId}" value="${escapeHTML(leftover[0] || '')}" autocomplete="off" title="Exactly 32 hexadecimal characters, without spaces or separators"><small>Optional extra ID that is not in Saved IDs.</small></label></fieldset>`;
}
function adminIdsFromForm(form) {
  const ids = [];
  const seen = new Set();
  for (const value of [...form.getAll('adminPlayerId'), ...form.getAll('adminPlayerIdManual')]) {
    const trimmed = String(value || '').trim();
    if (!trimmed) continue;
    const id = normalizePlayerIdClient(trimmed);
    if (seen.has(id)) continue;
    seen.add(id);
    ids.push(id);
  }
  return ids.join(',');
}
function serviceTypeField() {
  return `<label class="field full">Service exposure<select name="serviceType" data-testid="server-service-type"><option value="LoadBalancer" selected>LoadBalancer (requires a provider)</option><option value="NodePort">NodePort (local kind testing)</option><option value="ClusterIP">ClusterIP (cluster network only)</option></select></label>`;
}
function createSettingsFromForm(form) {
  const values = Object.fromEntries(form);
  const save = form.get('save');
  return {
    name: values.name,
    worldName: values.worldName,
    ownerId: values.ownerId,
    imageTag: values.imageTag,
    namespace: values.namespace,
    maxPlayers: Number(values.maxPlayers),
    memoryLimitMiB: Number(values.memoryLimitMiB),
    cpuLimitMillis: Number(values.cpuLimitMillis),
    gamePort: Number(values.gamePort),
    storageGiB: Number(values.storageGiB),
    serviceType: values.serviceType,
    adminIds: adminIdsFromForm(form),
    debugLevel: Number(values.debugLevel),
    validateGameFiles: values.validateGameFiles === 'true',
    autoStopOnUpdate: values.autoStopOnUpdate === 'true',
    additionalArgs: values.additionalArgs,
    serverPassword: values.serverPassword,
    adminPassword: values.adminPassword,
    ...(save && save.name ? {save} : {}),
  };
}
function createdServerFromResponse(result) {
  const id = String(result?.id ?? '').trim();
  if (!id) throw new Error('The server was created, but the response did not include an ID.');
  return {id, status: String(result.status || ''), label: worldLabel(result) || id};
}
function rememberCreatedServer(result) {
  const created = createdServerFromResponse(result);
  const record = {...result, id: created.id};
  const index = state.servers.findIndex((server) => server.id === created.id);
  if (index === -1) state.servers.push(record);
  else state.servers[index] = {...state.servers[index], ...record};
  return created;
}
function beginDeployWatch(created, startedAt = Date.now()) {
  const id = createdServerFromResponse(created).id;
  state.deployWatches[id] = {
    serverId: id,
    startedAt,
    // Demo 201 is already online; latch so a later unknown bootstrap cannot un-ready this watch.
    ready: created.status === 'online',
  };
}
function deployProgress(server, now = Date.now()) {
  const watch = server?.id ? state.deployWatches[server.id] : null;
  if (!watch) return null;
  const elapsedMs = Math.max(0, now - watch.startedAt);
  const statusValue = server.status || 'unknown';
  const label = serverLabel(server);
  const progress = (phase, title, detail) => ({serverId: watch.serverId, phase, status: statusValue, title, detail, elapsedMs});
  if (watch.ready || statusValue === 'online') {
    watch.ready = true;
    return progress('ready', `${label} is ready`, 'The game pod is ready and the engine reported healthy.');
  }
  if (['attention', 'error', 'stopped', 'stale', 'deleting'].includes(statusValue)) {
    return progress('failed', 'Deploy needs attention', `Bootstrap reports ${statusValue}. Use lifecycle details below.`);
  }
  if (elapsedMs >= DEPLOY_WATCH_TIMEOUT_MS) {
    return progress('timed_out', 'Deploy is taking longer than expected', 'Still not ready after 10 minutes. This is not a ready server.');
  }
  return progress('starting', `Deploying ${label}`, 'Helm accepted. Waiting for the game pod and engine. A successful install is not a ready server. Join address is not reported yet.');
}
function deployProgressPanel(progress) {
  const steps = progress.phase === 'ready'
    ? [['done', 'Accepted'], ['done', 'Starting'], ['done', 'Healthy']]
    : progress.phase === 'failed'
      ? [['done', 'Accepted'], ['failed', 'Starting'], ['pending', 'Healthy']]
      : [['done', 'Accepted'], ['current', 'Starting'], ['pending', 'Healthy']];
  const tone = {ready:'online', failed:'error', timed_out:'warning', starting:'starting'}[progress.phase] || 'starting';
  return `<section class="panel deploy-progress" data-testid="deploy-progress" data-phase="${escapeHTML(progress.phase)}" role="status"><div class="panel-heading"><div><h2>${escapeHTML(progress.title)}</h2></div>${status(tone)}</div><ol class="deploy-steps">${steps.map(([stateName, label]) => `<li data-step="${stateName}">${escapeHTML(label)}</li>`).join('')}</ol><p>${escapeHTML(progress.detail)}</p></section>`;
}
function eventServerLabel(event) { const server = state.servers.find((item) => item.id === event.serverId); return server ? serverLabel(server) : event.serverName || event.serverId || 'Cluster'; }
function scopedServers() { return state.servers.filter((server) => !state.serverId || server.id === state.serverId); }
function notice(message) {
  $('#toast').textContent = message;
  $('#toast').hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { $('#toast').hidden = true; }, 5000);
}
async function api(path, options = {}) {
  const epoch = state.epoch;
  const identity = state.csrfToken;
  let token = '';
  if (state.authMode !== 'oidc') {
    try { token = sessionStorage.getItem(tokenKey) || ''; } catch {}
  }
  const multipart = typeof FormData !== 'undefined' && options.body instanceof FormData;
  const response = await fetch(path, {credentials:'same-origin', ...options, headers:{'Accept':'application/json', ...(token ? {'Authorization':`Bearer ${token}`} : {}), ...(state.csrfToken ? {'X-CSRF-Token':state.csrfToken} : {}), ...(options.body && !multipart ? {'Content-Type':'application/json'} : {}), ...options.headers}});
  const text = await response.text();
  if (epoch !== state.epoch || options.signal?.aborted) throw staleRequest();
  if (state.authMode === 'oidc' && path !== '/api/auth' && response.ok && identity && response.headers.get('X-RSDW-Session') && response.headers.get('X-RSDW-Session') !== identity) {
    requireLogin();
    throw staleRequest();
  }
  if (response.status === 401 && path !== '/api/session') {
    if (path === '/api/auth/logout') return {};
    requireLogin();
    throw new Error('Sign in is required.');
  }
  let data;
  try { data = text ? JSON.parse(text) : {}; } catch { throw new Error('The server returned an unreadable response. Please try again.'); }
  if (!response.ok) {
    const error = new Error(typeof data.error === 'string' ? data.error : `Request failed (${response.status}). Please try again.`);
    error.status = response.status;
    error.fields = data && typeof data.fields === 'object' ? data.fields : {};
    throw error;
  }
  return data;
}
function clearProtectedState() {
  state.routeError = '';
  state.deletions = {};
  state.epoch++;
  state.request?.abort();
  clearTimeout(searchTimer);
  clearTimeout(toastTimer);
  clearTimeout(rosterExpiryTimer);
  Object.assign(state, {servers:[], events:[], users:[], telemetry:null, rosterObservation:'', rosterDeadline:0, logs:'', query:'', category:'', eventRange:'24h', eventTotal:0, eventWarnings:0, eventCritical:0, logQuery:'', serverId:'', routeScope:false, selectedEventId:'', loaded:false, lastUpdated:null, modalAction:'', modalServerId:'', modalUserId:'', modalBusy:false, modalInitialSettings:{}, modalRebootId:'', identity:'', authSubject:'', csrfToken:'', capabilities:{}, role:'denied', displayTimezone:'', previewSequence:0, deployWatches:{}});
  Object.assign(state, {integrations:[], deliveries:[], alertRules:[], pendingRestarts:{}, integrationsDemo:false, modalIntegrationId:'', reboots:[], rebootHistory:[], rebootsAvailable:true, rebootsDemo:false});
  $('#modal').close();
  $('#modal-body').innerHTML = '';
  $('#modal-error').textContent = '';
  $('#error-banner').textContent = '';
  $('#error-banner').hidden = true;
  $('#toast').textContent = '';
  $('#toast').hidden = true;
  $('#server-filter').innerHTML = '<option value="">All servers</option>';
  $('#navigation').innerHTML = '';
  $('#cluster-name').textContent = 'Not signed in';
  $('#updated-at').textContent = 'Waiting for sign in';
  $('#session-controls').hidden = true;
  $('#session-role').textContent = '';
  $('#admin-token').value = '';
  $('#environment').textContent = '';
  $('#content').innerHTML = '';
}
function lockedState() {
  $('.page-controls').hidden = true;
  $('#content').setAttribute('aria-busy','false');
  const message = state.authMode === 'oidc' ? 'Sign in with your identity provider to access this cluster.' : 'Sign in with your admin token to access this cluster.';
  $('#content').innerHTML = `<section class="panel empty"><div class="empty-icon">${icon('server')}</div><h2>Sign in required</h2><p>${message}</p>${state.signInFailed ? '<p class="notice error" role="alert">Sign in failed or this identity has no assigned role. Contact your administrator or try another identity.</p>' : ''}<button class="primary" data-action="login" data-testid="open-login">Sign in</button></section>`;
  $('#connection-status').textContent = 'Waiting for sign in';
  $('#connection-status').classList.remove('connected');
}
function requireLogin(openDialog = true) {
  clearProtectedState();
  state.authRequired = true;
  try { sessionStorage.removeItem(tokenKey); } catch {}
  lockedState();
  if (state.authMode === 'oidc') { $('#login-dialog').close(); return; }
  if (openDialog && !$('#login-dialog').open) {
    $('#login-error').hidden = true;
    $('#admin-token').value = '';
    $('#login-dialog').showModal();
    $('#admin-token').focus();
  }
}
function applyAuth(auth) {
  const identity = auth.authenticated ? `${auth.mode}:${auth.subject}:${auth.role}:${auth.csrfToken}` : '';
  const changed = state.identity !== identity;
  if (changed) clearProtectedState();
  state.authMode = auth.mode;
  if (auth.mode === 'oidc') {
    try { sessionStorage.removeItem(tokenKey); } catch {}
    $('#login-dialog').close();
  }
  if (!auth.authenticated) { requireLogin(false); return false; }
  Object.assign(state, {identity, authSubject:auth.subject || (auth.mode === 'token' ? 'token-admin' : ''), csrfToken:auth.csrfToken || '', role:auth.role, capabilities:auth.capabilities || {}, authRequired:false});
  if (changed || !validDisplayTimezone()) loadDisplayTimezone();
  $('.page-controls').hidden = false;
  $('#session-controls').hidden = !auth.required;
  $('#session-role').textContent = auth.role === 'admin' ? 'Admin' : 'Viewer';
  if (!can(state.page)) state.page = 'dashboard';
  return true;
}
async function discoverAuth(signal) {
  let auth = await api('/api/auth', {signal});
  if (auth.mode === 'oidc' && state.authMode !== 'oidc') {
    state.authMode = 'oidc';
    try { sessionStorage.removeItem(tokenKey); } catch {}
    auth = await api('/api/auth', {signal});
  }
  return auth;
}
async function logout() {
  const csrf = state.csrfToken || state.logoutCSRF;
  const oidc = state.authMode === 'oidc';
  refreshSequence++;
  requireLogin(false);
  if (oidc) {
    state.logoutCSRF = csrf;
    try { await api('/api/auth/logout', {method:'POST', headers:{'X-CSRF-Token':csrf}}); state.logoutCSRF = ''; }
    catch (error) {
      if (error.name !== 'AbortError') {
        $('#session-controls').hidden = false;
        $('#session-role').textContent = 'Sign out not confirmed';
        notice('Could not confirm server sign out. Retry sign out before leaving this browser.');
      }
    }
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
function eventListParams({limit, offset = 0, preview = false, serverId = state.serverId} = {}) {
  const params = new URLSearchParams();
  if (serverId) params.set('serverId', serverId);
  if (!preview) {
    params.set('query', state.query);
    params.set('category', state.category);
    if (state.eventRange === '24h') params.set('since', new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString());
  }
  params.set('limit', String(limit));
  params.set('offset', String(offset));
  return params;
}
function acceptEvents(result, append = false) {
  const incoming = eventArray(result);
  if (append) {
    const seen = new Set(state.events.map((event) => event.id));
    state.events = state.events.concat(incoming.filter((event) => event.id && !seen.has(event.id)));
  } else {
    state.events = incoming;
  }
  const total = Number(result?.total);
  state.eventTotal = Number.isFinite(total) ? total : incoming.length;
  const warnings = Number(result?.warnings);
  state.eventWarnings = Number.isFinite(warnings) ? warnings : 0;
  const critical = Number(result?.critical);
  state.eventCritical = Number.isFinite(critical) ? critical : 0;
}
function samples() {
  const data = state.telemetry;
  return Array.isArray(data) ? data : data?.samples || data?.points || [];
}
const metricLabels = {
  players: 'API-reported players', uptimeSeconds: 'API uptime', engineReady: 'Engine ready',
  cpuCores: 'CPU cores', cpuPercent: 'CPU limit used', cpuLimitCores: 'CPU limit',
  memoryUsedBytes: 'Memory working set', memoryLimitBytes: 'Memory limit',
  diskUsedBytes: 'Data filesystem used', diskCapacityBytes: 'Data filesystem capacity', diskPercent: 'Data filesystem usage',
  inboundBytesPerSecond: 'Pod inbound', outboundBytesPerSecond: 'Pod outbound', networkBytesPerSecond: 'Pod traffic', tickRate: 'Tick rate',
  tickP50Ms: 'Tick duration p50', tickP95Ms: 'Tick duration p95', tickP99Ms: 'Tick duration p99',
  tickWindowSeconds: 'Tick measurement window', tickSampleCount: 'Completed tick samples',
};
function metricValue(server, key) {
  const reading = server.metrics?.[key];
  if (reading) return reading.status === 'available' && typeof reading.value === 'number' && Number.isFinite(reading.value) ? reading.value : null;
  return state.mode === 'demo' ? server[key] ?? null : null;
}
function metricText(server, key, suffix = '') {
  const value = metricValue(server, key);
  if (value != null) return number(value, suffix);
  const reading = server.metrics?.[key];
  return ({unsupported:'Unsupported', stale:'Stale', error:'Collection failed', warming_up:'Collecting', warming:'Collecting', starting:'Starting', faulted:'Collection failed', stopped:'Stopped', unavailable:'Unavailable'})[reading?.status] || 'Awaiting sample';
}
function metricSources(server) {
  const metrics = server.metrics || {};
  return `<section class="panel section-gap"><div class="panel-heading"><h2>Metric sources</h2></div><p class="inline-note">Collection is scheduled every 15 seconds, even with no browser open. Slow sources can lengthen the interval. History covers up to one hour and resets when the dashboard service restarts.</p><div class="table-wrap"><table><thead><tr><th>Metric</th><th>Status</th><th>Source</th><th>Observed</th><th>Details</th></tr></thead><tbody>${Object.entries(metrics).map(([key, reading]) => `<tr><td>${escapeHTML(metricLabels[key] || key)}</td><td>${escapeHTML(reading.status)}</td><td>${escapeHTML(reading.source || 'Not configured')}</td><td>${escapeHTML(date(reading.observedAt))}</td><td>${escapeHTML(reading.reason || reading.unit || '')}</td></tr>`).join('')}</tbody></table></div></section>`;
}
function telemetryRows() {
  const metrics = state.telemetry?.metrics || state.telemetry?.server?.metrics || {};
  return samples().flatMap((sample) => Object.keys(metricLabels).map((key) => ({
    timestamp: sample.timestamp, metric: key, value: sample[key] ?? '',
    observedAt: sample.observedAt?.[key] || '',
    status: sample.status?.[key] || (sample[key] == null ? 'unavailable' : 'available'),
    reason: sample.reason?.[key] || '',
    unit: metrics[key]?.unit || '', source: metrics[key]?.source || '',
  })));
}
function emptyState() {
  if (!can('create')) return '<section class="panel empty"><h2>No servers registered</h2><p>Servers will appear here after an administrator adds them.</p></section>';
  return `<section class="panel empty" data-testid="empty-state"><div class="empty-icon">${icon('server')}</div><h2>No servers registered</h2><p>Create a Dragonwilds server to view its health, logs, and activity.</p><button class="primary" data-action="add-server" data-testid="empty-add-server">${icon('plus')}Create your first server</button></section>`;
}
function eventTable(events, detailed = false) {
  if (!events.length) return '<p class="no-results" data-testid="no-events">No events match this view.</p>';
  return `<div class="table-wrap"><table><thead><tr><th>Time</th><th>Server</th><th>${detailed ? 'Category' : 'Status'}</th><th>Event</th></tr></thead><tbody>${events.map((event) => `<tr class="${state.selectedEventId === event.id ? 'event-selected' : ''}"><td class="mono" title="${escapeHTML(date(event.timestamp))}">${escapeHTML(date(event.timestamp, true))}</td><td>${escapeHTML(eventServerLabel(event))}</td><td>${detailed ? escapeHTML(categoryLabels[event.category] || (event.category || 'system').replace(/^./, (c) => c.toUpperCase())) : status(event.severity || 'info')}</td><td>${detailed ? `<button class="link-button event-message" data-action="select-event" data-id="${escapeHTML(event.id)}" data-testid="event-row" aria-pressed="${state.selectedEventId === event.id}">${escapeHTML(event.message)}</button>` : escapeHTML(event.message)}</td></tr>`).join('')}</tbody></table></div>`;
}
function dashboard() {
  const servers = scopedServers();
  const online = servers.filter((server) => server.status === 'online').length;
  const attention = servers.filter((server) => server.status === 'attention' || server.status === 'stale' || server.updateAvailable).length;
  const filtered = servers.filter((server) => state.fleetFilter === 'all' || (state.fleetFilter === 'online' ? server.status === 'online' : server.status === 'attention' || server.status === 'stale' || server.updateAvailable));
  const stats = `<div class="stats">${stat('Registered servers', servers.length, 'server')}${stat('Online', online, 'pulse', 'green')}${stat('Needs attention', attention, 'warning', 'amber')}${can('updateCheck') ? stat('Updates available', servers.filter((server) => server.updateAvailable).length, 'refresh') : stat('Reporting metrics', servers.filter((server) => server.metricsAvailable).length, 'pulse')}</div>`;
  if (!state.servers.length) return stats + emptyState();
  return `${stats}<section class="panel"><div class="panel-heading"><div><h2>Servers</h2></div>${can('create') ? `<button class="primary" data-action="add-server" data-testid="add-server">${icon('plus')}Add server</button>` : ''}</div><div class="toolbar chips" aria-label="Server status filter">${['all','online','attention'].map((filter) => `<button data-action="fleet-filter" data-value="${filter}" data-testid="filter-${filter}" aria-pressed="${state.fleetFilter === filter}">${filter === 'attention' ? 'Needs attention' : filter[0].toUpperCase()+filter.slice(1)}</button>`).join('')}</div>${filtered.length ? `<div class="table-wrap"><table><thead><tr><th>Name</th><th>Status</th><th>Players</th><th>Tick rate</th><th>CPU</th><th>Uptime</th><th>Actions</th></tr></thead><tbody>${filtered.map((server) => {
    const progress = deployProgress(server);
    const deploying = progress && progress.phase !== 'ready' && progress.phase !== 'failed';
    return `<tr${deploying ? ' class="deploying"' : ''}><td><strong>${escapeHTML(serverLabel(server))}</strong><small>${escapeHTML(server.region || server.namespace || 'Managed server')}</small></td><td>${status(server.status)}</td><td>${metricText(server, 'players')} / ${number(server.maxPlayers)}</td><td>${metricText(server, 'tickRate', ' TPS')}</td><td>${metricText(server, 'cpuPercent', '%')}</td><td class="mono">${duration(metricValue(server, 'uptimeSeconds'))}</td><td class="actions"><button class="link-button" data-action="view-server" data-id="${escapeHTML(server.id)}" data-testid="view-server">View server ${icon('arrow')}</button></td></tr>`;
  }).join('')}</tbody></table></div>` : '<p class="no-results">No servers match this filter.</p>'}</section>${can('events') ? `<section class="panel"><div class="panel-heading"><h2>Fleet activity</h2><button class="link-button" data-action="view-events" data-testid="view-all-events">View all events ${icon('arrow')}</button></div>${eventTable(state.events.slice(0, 6))}</section>` : ''}`;
}
function chart(key, label, secondaryKey = '', secondaryLabel = '') {
  const points = samples();
  const timeOf = (point, field) => Date.parse(point.observedAt?.[field] || point.timestamp);
  const end = Math.max(...points.map((point) => Date.parse(point.timestamp)).filter(Number.isFinite));
  const start = end - ({'60s':60, '5m':300, '1h':3600}[state.range] || 60) * 1000;
  const present = (point, field) => typeof point[field] === 'number' && Number.isFinite(point[field]) && timeOf(point, field) >= start && timeOf(point, field) <= end;
  const primary = points.filter((point) => present(point, key));
  const secondary = points.filter((point) => secondaryKey && present(point, secondaryKey));
  if (!primary.length && !secondary.length) {
    const reading = state.telemetry?.metrics?.[key] || state.telemetry?.server?.metrics?.[key];
    return `<div class="chart-well"><p class="no-results">${escapeHTML(reading?.reason || `${label} samples are not available yet.`)}</p></div>`;
  }
  const ceiling = Math.max(key === 'cpuCores' ? 0.001 : 1, ...primary.map((point) => point[key]), ...secondary.map((point) => point[secondaryKey])) * 1.15;
  const position = (point, field) => `${(Math.max(0, Math.min(1, (timeOf(point, field) - start) / (end - start))) * 660 + 35).toFixed(2)},${(170 - point[field] / ceiling * 150).toFixed(2)}`;
  const path = (field) => {
    let previous = null;
    return points.map((point) => {
      if (!present(point, field)) { previous = null; return ''; }
      const at = timeOf(point, field);
      if (previous === at) return '';
      const command = previous == null || at - previous > 45000 || at < previous ? 'M' : 'L';
      previous = at;
      return `${command}${position(point, field)}`;
    }).join(' ');
  };
  const legend = `<div class="chart-legends"><div class="chart-legend">${escapeHTML(label)}</div>${secondaryKey ? `<div class="chart-legend secondary">${escapeHTML(secondaryLabel)}</div>` : ''}</div>`;
  const summary = `${label} over the selected period. Latest value ${number(primary.at(-1)?.[key])}.${secondaryKey ? ` ${secondaryLabel}, latest value ${number(secondary.at(-1)?.[secondaryKey])}.` : ''}`;
  const graph = `<svg class="chart" viewBox="0 0 710 190" role="img" aria-label="${escapeHTML(summary)}">${[20,70,120,170].map((y) => `<path d="M35 ${y}H695" class="chart-grid" stroke-width="1"/>`).join('')}<text x="0" y="14">${number(ceiling)}</text><text x="12" y="173">0</text><path d="${path(key)}" fill="none" class="chart-line" stroke-width="2"/>${secondaryKey ? `<path d="${path(secondaryKey)}" fill="none" class="chart-line secondary" stroke-width="2"/>` : ''}${primary.map((point) => `<circle cx="${position(point, key).split(',')[0]}" cy="${position(point, key).split(',')[1]}" r="2" class="chart-point"/>`).join('')}${secondary.map((point) => `<circle cx="${position(point, secondaryKey).split(',')[0]}" cy="${position(point, secondaryKey).split(',')[1]}" r="2" class="chart-point secondary"/>`).join('')}</svg>`;
  const table = `<details class="chart-data" data-chart="${escapeHTML(key)}"><summary data-testid="chart-data-${escapeHTML(key)}">View ${escapeHTML(label)} data</summary><div class="table-wrap"><table><thead><tr><th>Observed</th><th>Series</th><th>Value</th></tr></thead><tbody>${points.flatMap((point) => [[key,label],[secondaryKey,secondaryLabel]].filter(([field]) => field).map(([field,name]) => `<tr><td>${escapeHTML(date(point.observedAt?.[field] || point.timestamp))}</td><td>${escapeHTML(name)}</td><td>${present(point, field) ? number(point[field]) : '—'}</td></tr>`)).join('')}</tbody></table></div></details>`;
  return `${legend}${graph}<div class="chart-labels"><span>${escapeHTML(date(start))}</span><span>${escapeHTML(date(end))}</span></div>${table}`;
}
function resource(label, value, percent) {
  return `<div class="resource"><div class="resource-header"><span>${label}</span><strong>${escapeHTML(value)}</strong></div>${percent == null ? '' : `<div class="metric-bar"><span style="width:${Math.min(100,Math.max(0,Number(percent) || 0))}%"></span></div>`}</div>`;
}
function telemetry() {
  const selected = selectedServer();
  if (!selected) return emptyState();
  const server = {...selected, ...(state.telemetry?.server || state.telemetry?.current || state.telemetry?.latest || {})};
  server.metrics = state.telemetry?.metrics || server.metrics;
  const value = (key) => metricValue(server, key);
  const text = (key, suffix = '') => metricText(server, key, suffix);
  const logsAvailable = can('logs') && server.status !== 'stopped';
  return `<div class="toolbar"><strong>${escapeHTML(serverLabel(server))}</strong><label class="sr-only" for="telemetry-range">Telemetry time range</label><select id="telemetry-range" data-testid="telemetry-range">${[['60s','Last 60 seconds'],['5m','Last 5 minutes'],['1h','Last hour']].map(([value,label]) => `<option value="${value}" ${state.range===value?'selected':''}>${label}</option>`).join('')}</select><button class="primary" data-action="export-telemetry" data-testid="export-telemetry">${icon('download')}Export CSV</button></div><div class="stats">${stat('API-reported players', `${metricText(server, 'players')} / ${number(server.maxPlayers)}`, 'server')}${stat('Tick rate', text('tickRate', ' TPS'), 'pulse')}${stat('API uptime', duration(metricValue(server, 'uptimeSeconds')), 'clock')}${stat('CPU usage', text('cpuPercent', '%'), 'cpu')}</div><div class="split telemetry-layout"><div class="stack"><section class="panel"><div class="panel-heading"><h2>Tick rate</h2>${status(server.status)}</div>${chart('tickRate','Tick rate (TPS)')}</section><div class="mini-charts"><section class="panel"><div class="panel-heading"><h2>Player count</h2></div>${chart('players','Active players')}</section><section class="panel"><div class="panel-heading"><h2>Network traffic</h2></div>${chart('inboundBytesPerSecond','Inbound (bytes/s)','outboundBytesPerSecond','Outbound (bytes/s)')}</section></div>${connectedPlayers()}</div><div class="stack"><section class="panel"><div class="panel-heading"><h2>Resource usage</h2></div>${resource('CPU cores', text('cpuCores', ' cores'), null)}${resource('CPU limit used', text('cpuPercent', '%'), value('cpuPercent'))}${resource('Memory working set', value('memoryUsedBytes') == null ? text('memoryUsedBytes') : `${bytes(value('memoryUsedBytes'))} / ${bytes(value('memoryLimitBytes'))}`, value('memoryLimitBytes') > 0 && value('memoryUsedBytes') != null ? value('memoryUsedBytes') / value('memoryLimitBytes') * 100 : null)}${resource('Data filesystem', value('diskUsedBytes') == null ? text('diskUsedBytes') : `${bytes(value('diskUsedBytes'))} / ${bytes(value('diskCapacityBytes'))}`, value('diskPercent'))}${resource('Pod network', value('networkBytesPerSecond') == null ? text('networkBytesPerSecond') : `${bytes(value('networkBytesPerSecond'))}/s`, null)}<p class="inline-note">Pod traffic includes all containers. Data filesystem capacity may be shared on kind; it is not the world-save size.</p></section>${telemetryPanels()}</div></div><div class="mini-charts section-gap"><section class="panel"><div class="panel-heading"><h2>CPU history</h2></div>${chart('cpuCores','CPU cores')}</section><section class="panel"><div class="panel-heading"><h2>Memory history</h2></div>${chart('memoryUsedBytes','Memory working set (bytes)')}</section></div>${tickDurations(server)}${metricSources(server)}<section class="panel section-gap metric-definitions"><div class="panel-heading"><h2>Metric definitions</h2></div>${state.telemetry?.metricDefinitions?.length ? `<div class="table-wrap"><table><thead><tr><th>Metric</th><th>Description</th></tr></thead><tbody>${state.telemetry.metricDefinitions.map((definition) => `<tr><td>${escapeHTML(definition.metric)}</td><td>${escapeHTML(definition.description)}</td></tr>`).join('')}</tbody></table></div>` : '<p class="no-results">No metric definitions reported yet.</p>'}</section>${logsAvailable ? `<section class="panel section-gap"><div class="panel-heading"><div><h2>Server logs</h2><p>Latest 100 lines · ${escapeHTML(serverLabel(server))}</p></div><div class="toolbar"><button data-action="refresh-logs" data-testid="refresh-logs">${icon('refresh')}Refresh logs</button><button data-action="export-logs" data-testid="export-logs">${icon('download')}Download logs</button></div></div><div class="toolbar"><label class="search-field">${icon('search')}<span class="sr-only">Search logs</span><input id="log-search" data-testid="log-search" type="search" placeholder="Search these log lines…" value="${escapeHTML(state.logQuery)}"></label></div><pre class="log-console" id="log-output" tabindex="0" aria-label="Server logs" data-testid="log-output">${escapeHTML(filteredLogs() || 'No log lines match this view.')}</pre><p class="inline-note">Unavailable metrics appear as —. Values depend on the server’s telemetry source.</p></section>` : ''}`;
}
function serverOverview() {
  const selected = selectedServer();
  if (!selected) return '<section class="panel empty"><p>Loading server inventory…</p></section>';
  const data = state.telemetry?.server?.id === selected.id ? state.telemetry : null;
  const server = {...selected, ...(data?.server || {}), metrics:data?.metrics || selected.metrics};
  const next = state.rebootsAvailable ? state.reboots.filter((item) => item.serverId === selected.id && item.enabled && Number.isFinite(Date.parse(item.nextRun))).sort((a,b) => Date.parse(a.nextRun) - Date.parse(b.nextRun))[0] : null;
  const actions = [
    can('restart') ? '<div class="action-card"><button data-action="restart" data-testid="restart-server">Restart server</button></div>' : '',
    can('edit-settings') ? '<div class="action-card"><button data-action="edit-settings" data-testid="edit-settings">Edit settings</button></div>' : '',
    can('update') ? '<div class="action-card"><button data-action="update" data-testid="update-image">Update image</button></div>' : '',
    can('updateCheck') ? '<div class="action-card"><button data-action="check-update" data-testid="check-update">Check update</button></div>' : '',
    can('delete') ? '<div class="action-card"><button class="danger" data-action="delete" data-testid="delete-server">Delete server</button></div>' : '',
  ].filter(Boolean).join('');
  const engine = server.metrics?.engineReady;
  const evidence = engine ? `<p>Engine readiness: ${escapeHTML(metricValue(server, 'engineReady') === 1 ? 'Ready' : metricValue(server, 'engineReady') === 0 ? 'Not ready' : metricText(server, 'engineReady'))}${engine.reason ? ` · ${escapeHTML(engine.reason)}` : ''}</p>` : '';
  const links = ['telemetry','maintenance','events','reboots']
    .filter((page) => can(page))
    .map((page) => `<a href="${escapeHTML(scopedHash(page, selected.id))}">${pages[page][0]}</a>`)
    .join('');
  const details = can('maintenance') || can('reboots')
    ? `<section class="panel"><dl class="detail-list">${can('reboots') ? `<div><dt>Next reboot</dt><dd data-testid="overview-next-reboot">${next ? escapeHTML(zonedDate(next.nextRun, state.displayTimezone || 'UTC')) : 'Unavailable'}</dd></div>` : ''}${can('maintenance') ? `<div><dt>Join endpoint</dt><dd class="mono">${escapeHTML(selected.endpoint || 'Unavailable')}${selected.endpoint ? ' <button data-action="copy-endpoint" data-testid="copy-endpoint">Copy endpoint</button>' : ''}</dd></div>` : ''}</dl></section>`
    : '';
  const actionPanel = actions && !state.deletions[selected.id] && selected.status !== 'deleting'
    ? `<section class="panel"><div class="panel-heading"><h2>Server actions</h2></div><div class="action-grid">${actions}</div><p class="inline-note">Restart and update actions require confirmation and disconnect active players.</p></section>`
    : '';
  return `<section class="panel" data-testid="server-overview"><div class="panel-heading"><h2>${escapeHTML(serverLabel(server))}</h2>${status(server.status)}</div>${evidence}<div class="toolbar">${links}</div><div class="stats">${stat('API-reported players', `${metricText(server, 'players')} / ${number(server.maxPlayers)}`, 'server')}${stat('Tick rate', metricText(server, 'tickRate', ' TPS'), 'pulse')}${stat('Memory working set', metricValue(server, 'memoryUsedBytes') == null ? metricText(server, 'memoryUsedBytes') : bytes(metricValue(server, 'memoryUsedBytes')), 'cpu')}${stat('Memory limit', metricValue(server, 'memoryLimitBytes') == null ? metricText(server, 'memoryLimitBytes') : bytes(metricValue(server, 'memoryLimitBytes')), 'cpu')}</div></section><div class="stack section-gap">${connectedPlayers()}${details}${actionPanel}</div>`;
}
function connectedPlayers() {
  const data = state.telemetry;
  const reading = data?.metrics?.players;
  const roster = data?.playerRoster;
  const players = roster?.players;
  const key = rosterObservationKey(data);
  const expired = state.rosterObservation === key && Number.isFinite(state.rosterDeadline) ? monotonicNow() >= state.rosterDeadline : (() => {
    const age = Date.now() - Date.parse(reading?.observedAt);
    return !Number.isFinite(age) || age > 45000 || age < -5000;
  })();
  const playerField = (value, fallback) => escapeHTML(typeof value === 'string' && value.trim() ? value : fallback);
  let content = '<p class="no-results">Connected players are unavailable.</p>';
  if (data?.server?.id === selectedServer()?.id) {
    if (reading?.status === 'stale' || (reading?.status === 'available' && expired)) {
      content = '<p class="no-results">Connected players are stale. Refresh telemetry to see the current roster.</p>';
    } else if (reading?.status === 'error') {
      content = '<p class="no-results">Connected players are unavailable. The latest collection failed.</p>';
    } else if (reading?.status === 'available' && roster?.status === 'error') {
      content = '<p class="no-results">Connected players are unavailable. The game API returned an invalid roster.</p>';
    } else if (reading?.status === 'available' && roster?.status === 'unavailable') {
      content = '<p class="no-results">Connected player names are unavailable. The game API did not provide roster details.</p>';
    } else if (reading?.status === 'available' && Number.isFinite(reading.value) && Array.isArray(players)) {
      if (players.length) {
        content = `<ul class="player-roster">${players.map((player) => `<li><dl><div><dt>Name</dt><dd>${playerField(player?.name, 'Name unavailable')}</dd></div><div><dt>Character name</dt><dd>${playerField(player?.characterName, 'Character name unavailable')}</dd></div></dl></li>`).join('')}</ul>`;
      } else {
        content = `<p class="no-results">${reading.value === 0 ? 'No players are connected.' : `The server reports ${escapeHTML(number(reading.value))} connected players, but the roster returned no entries.`}</p>`;
      }
    }
  }
  return `<section class="panel" aria-labelledby="connected-players-heading" data-testid="connected-players"><div class="panel-heading"><h2 id="connected-players-heading">Connected players</h2></div>${content}</section>`;
}
function tickDurations(server) {
  return `<section class="panel section-gap"><div class="panel-heading"><h2>Tick execution duration</h2></div><p class="inline-note">Elapsed time inside UDomGameEngine::Tick, including its world tick. Excludes work outside that call. Each point is a separate measurement window, not a percentile of the selected chart range.</p><div class="stats">${stat('p50 duration', metricText(server, 'tickP50Ms', ' ms'), 'clock')}${stat('p95 duration', metricText(server, 'tickP95Ms', ' ms'), 'clock')}${stat('p99 duration', metricText(server, 'tickP99Ms', ' ms'), 'clock')}${stat('Completed ticks', metricText(server, 'tickSampleCount'), 'pulse')}</div><p class="inline-note">Latest measurement window: ${metricText(server, 'tickWindowSeconds', ' seconds')}.</p>${chart('tickP95Ms', 'p95 tick duration (ms)', 'tickP99Ms', 'p99 tick duration (ms)')}</section>`;
}
function filteredLogs() { return state.logs.split('\n').filter((line) => line.toLowerCase().includes(state.logQuery.toLowerCase())).join('\n'); }
function telemetryPanels() {
  const checks = state.telemetry?.healthChecks || [];
  return `<section class="panel"><div class="panel-heading"><h2>Health checks</h2></div>${checks.length ? `<dl class="detail-list">${checks.map((check) => `<div><dt>${escapeHTML(check.name)}</dt><dd>${status(String(check.status || 'unknown').toLowerCase())}<small class="check-time">${escapeHTML(date(check.at))}</small></dd></div>`).join('')}</dl>` : '<p class="no-results">No health checks reported yet.</p>'}</section>`;
}
function eventsPage() {
  if (!can('events')) return dashboard();
  const selected = state.events.find((event) => event.id === state.selectedEventId);
  const older = state.events.length < state.eventTotal ? `<button data-action="load-older-events" data-testid="load-older-events" title="Older matches reset when the page refreshes."${state.refreshing ? ' disabled' : ''}>Load older</button>` : '';
  return `<div class="toolbar"><label class="search-field">${icon('search')}<span class="sr-only">Search events</span><input type="search" id="event-search" data-testid="event-search" placeholder="Search events…" value="${escapeHTML(state.query)}"></label><label class="sr-only" for="event-range">Event time range</label><select id="event-range" data-testid="event-range">${[['24h','Last 24 hours'],['all','All retained history']].map(([value,label]) => `<option value="${value}" ${state.eventRange===value?'selected':''}>${label}</option>`).join('')}</select><button data-action="export-events" data-testid="export-events">${icon('download')}Export CSV</button></div><div class="stats">${stat('Matching events',state.eventTotal,'events')}${stat('Warnings',state.eventWarnings,'warning','amber')}${stat('Critical',state.eventCritical,'pulse',state.eventCritical?'red':'')}${stat('Last event',state.events[0] ? date(state.events[0].timestamp,true) : '—','clock')}</div><div class="split"><section class="panel"><div class="panel-heading"><h2>Event stream</h2></div><div class="toolbar chips" aria-label="Event category">${[['','All'],['system','System'],['player','Players'],['health','Health'],['update','Updates']].map(([value,label])=>`<button data-action="event-category" data-value="${value}" data-testid="category-${value || 'all'}" aria-pressed="${state.category===value}">${label}</button>`).join('')}</div>${eventTable(state.events,true)}${older}</section><section class="panel"><div class="panel-heading"><h2>Event details</h2></div>${selected ? `<dl class="detail-list"><div><dt>Event</dt><dd>${escapeHTML(selected.message)}</dd></div><div><dt>Server</dt><dd>${escapeHTML(eventServerLabel(selected))}</dd></div><div><dt>Severity</dt><dd>${status(selected.severity || 'info')}</dd></div><div><dt>Time</dt><dd>${escapeHTML(date(selected.timestamp))}</dd></div></dl><pre class="event-json" tabindex="0" aria-label="Event details JSON">${escapeHTML(typeof selected.details === 'string' ? selected.details : JSON.stringify(selected.details || {},null,2))}</pre><button data-action="copy-event" data-testid="copy-event">Copy event JSON</button>` : '<p class="no-results">Select an event to inspect its details.</p>'}</section></div>`;
}
function maintenance() {
  if (!can('maintenance')) return dashboard();
  const server = selectedServer();
  if (!server) return deletionReceipts() + emptyState();
  const progress = deployProgress(server);
  const banner = progress ? deployProgressPanel(progress) : '';
  if (state.deletions[server.id]) {
    const message = server.status === 'deleting' ? 'Deletion is in progress. You can leave this page; C2 will keep working and update the receipt.' : server.status === 'stale' ? 'Deletion exceeded the 10-minute cleanup window. Retry the recorded operation below.' : 'Deletion is pending. Retry the recorded operation below. Other lifecycle actions are unavailable.';
    return banner + deletionReceipts() + `<section class="panel"><h2>${escapeHTML(serverLabel(server))}</h2><p>${message}</p></section>`;
  }
  const activity = state.events.filter((event) => event.serverId === server.id);
  const changes = activity.filter((event) => ['system', 'update'].includes(event.category));
  const parked = server.status === 'stopped';
  const lifecycleActions = parked
    ? `<div class="action-card"><button class="primary" data-action="start" data-testid="start-server">${icon('refresh')}Start server</button><p>Bring this world back online. World data and this inventory row stay.</p></div>`
    : `<div class="action-card"><button class="danger" data-action="restart" data-testid="restart-server">${icon('refresh')}Restart server</button><p>Disconnects active players and restarts this world.</p></div><div class="action-card"><button data-action="update" data-testid="update-image">${icon('download')}Update image</button><p>Choose a container image tag and roll out the update.</p></div><div class="action-card"><button data-action="check-update" data-testid="check-update">${icon('search')}Check update</button><p>Compare the current image with the desired image.</p></div><div class="action-card"><button class="danger" data-action="stop" data-testid="stop-server">${icon('warning')}Stop server</button><p>Disconnects active players. World data and this inventory row stay.</p></div>`;
  return `${banner}<div class="split"><section class="panel"><div class="panel-heading"><div><h2>Server lifecycle</h2><p>${escapeHTML(serverLabel(server))}</p></div>${status(server.status)}</div><dl class="detail-list lifecycle-details"><div><dt>Current image</dt><dd class="mono">${escapeHTML(server.currentImage || 'Not reported')}</dd></div><div><dt>Desired image</dt><dd class="mono">${escapeHTML(server.desiredImage || 'Not configured')}</dd></div><div><dt>API uptime</dt><dd>${duration(metricValue(server, 'uptimeSeconds'))}</dd></div><div><dt>Last restart</dt><dd>${escapeHTML(date(server.lastRestart))}</dd></div><div><dt>Namespace</dt><dd class="mono">${escapeHTML(server.namespace || '—')}</dd></div><div><dt>Connection endpoint</dt><dd class="mono">${escapeHTML(server.endpoint || 'Endpoint unavailable. Ask your cluster operator for the server address and game port.')}</dd></div><div><dt>Memory limit</dt><dd>${server.memoryLimitMiB ? `${number(server.memoryLimitMiB)} MiB` : 'Not recorded'}</dd></div><div><dt>CPU limit</dt><dd>${server.cpuLimitMillis ? `${number(server.cpuLimitMillis)} millicores` : 'Not recorded'}</dd></div><div><dt>Player limit</dt><dd>${number(server.maxPlayers)}</dd></div><div><dt>Creator</dt><dd>${escapeHTML(server.name || 'Not recorded')}</dd></div><div><dt>Stable server ID</dt><dd class="mono">${escapeHTML(server.id)}</dd></div></dl><div class="action-grid">${lifecycleActions}</div><p class="inline-note">Stop, start, restart, and update require confirmation.</p></section><div class="stack"><section class="panel"><div class="panel-heading"><h2>Readiness</h2></div><dl class="detail-list"><div><dt>Server health</dt><dd>${status(server.status)}</dd></div><div><dt>Players connected</dt><dd>${metricText(server, 'players')} / ${number(server.maxPlayers)}</dd></div><div><dt>Image status</dt><dd class="${server.updateAvailable ? 'amber' : ''}">${server.updateAvailable ? 'Update available' : 'No update reported'}</dd></div><div><dt>Last seen</dt><dd>${escapeHTML(date(server.lastSeen))}</dd></div></dl><p class="inline-note">Choose a quiet moment for maintenance. Active players will be disconnected.</p></section><section class="panel"><div class="panel-heading"><h2>Recent changes</h2></div>${changes.length ? `<div class="table-wrap"><table><thead><tr><th>Change</th><th>Time</th></tr></thead><tbody>${changes.slice(0,4).map((event) => `<tr><td>${escapeHTML(event.message)}</td><td title="${escapeHTML(date(event.timestamp))}">${escapeHTML(date(event.timestamp, true))}</td></tr>`).join('')}</tbody></table></div>` : '<p class="no-results">No changes recorded for this server.</p>'}</section></div></div><section class="panel section-gap"><div class="panel-heading"><h2>Audit trail</h2><button class="link-button" data-action="view-events" data-testid="maintenance-events">View events ${icon('arrow')}</button></div>${eventTable(activity.slice(0,10))}<p class="inline-note">Recorded server events. Actor identity is not reported by this source.</p></section>`;
}
function deletionServer(record) { return state.servers.find((server) => server.id === record.serverId); }
function deletionInProgress(record) { return !record.completed && deletionServer(record)?.status === 'deleting'; }
function deletionStateLabel(record) {
  if (record.completed) return 'Completed';
  if (deletionInProgress(record)) return 'In progress';
  if (deletionServer(record)?.status === 'stale') return 'Timed out after 10 minutes; retry required';
  return 'Pending, retry required';
}
function deletionReceipts() {
  if (!can('delete')) return '';
  const records = Object.values(state.deletions);
  if (!records.length) return '';
  return `<section class="panel section-gap" data-testid="deletion-receipts"><div class="panel-heading"><h2>Deletion receipts</h2></div>${records.map((record) => `<article>
    <h3>${escapeHTML(record.worldLabel)}</h3><p>Server ID <code>${escapeHTML(record.serverId)}</code>. ${deletionStateLabel(record)}. World data choice <strong>${escapeHTML(record.mode)}</strong>.</p>
    ${record.lastError ? `<p role="status">${escapeHTML(record.lastError)}</p>` : ''}
    ${record.plan.world?.length ? `<ul>${record.plan.world.map((ref) => `<li>${record.mode === 'keep' ? 'Retained' : record.completed ? 'Deleted' : 'Selected'} volume <code>${escapeHTML(ref.namespace)}/${escapeHTML(ref.name)}</code>, UID <code>${escapeHTML(ref.uid)}</code>.</li>`).join('')}</ul>` : '<p>No real storage was changed in demo mode.</p>'}
    ${record.plan.seeds?.length ? `<ul>${record.plan.seeds.map((ref) => `<li>${record.mode === 'keep' ? 'Retained' : record.completed ? 'Deleted' : 'Selected'} source-save PVC <code>${escapeHTML(ref.namespace)}/${escapeHTML(ref.name)}</code>, UID <code>${escapeHTML(ref.uid)}</code>.</li>`).join('')}</ul>${record.mode === 'keep' ? '<p>The uploaded source save may be the only copy if import did not finish. Verify its UID and preserve it for recovery through <code>saveSeed</code>. A source-save PVC is an import source, not a world volume for <code>persistence.existingClaim</code>.</p>' : ''}` : ''}
    ${record.plan.retainedSecrets?.length ? `<p>These external or legacy Secrets were kept because C2 could not prove ownership. No ownership adoption is required to finish deletion. Secret values are not included.</p><ul>${record.plan.retainedSecrets.map((ref) => `<li>Retained Secret <code>${escapeHTML(ref.namespace)}/${escapeHTML(ref.name)}</code>, UID <code>${escapeHTML(ref.uid)}</code>.</li>`).join('')}</ul>` : ''}
    ${record.mode === 'keep' ? '<p>To recover a retained world, an operator must verify its UID and exclusive use, then set <code>persistence.existingClaim</code> to this claim in the same namespace. Keep this receipt. C2 does not automatically reuse deleted identities.</p>' : '<p>PVC deletion does not erase provider snapshots or backing volumes with a Retain reclaim policy. Ask the storage operator about those copies.</p>'}
    ${record.completed || deletionInProgress(record) ? '' : `<button class="danger" data-action="delete" data-id="${escapeHTML(record.serverId)}" data-testid="retry-deletion">Retry deletion</button>`}
  </article>`).join('')}</section>`;
}
function usersPage() {
  if (!can('users')) return dashboard();
  return `<section class="panel"><div class="panel-heading"><h2>Saved player IDs</h2><button class="primary" data-action="add-user" data-testid="add-user">${icon('plus')}Add saved ID</button></div><p class="inline-note">Use a saved ID when creating a server. Editing or deleting it leaves existing servers unchanged.</p>${state.users.length ? `<div class="table-wrap"><table><thead><tr><th>Name</th><th>Player ID</th><th>Actions</th></tr></thead><tbody>${state.users.map((user) => `<tr><td>${escapeHTML(user.name)}</td><td class="mono">${escapeHTML(user.playerId)}</td><td class="actions"><button data-action="edit-user" data-id="${escapeHTML(user.id)}" data-testid="edit-user" aria-label="Edit ${escapeHTML(user.name)}">Edit</button><button class="danger" data-action="delete-user" data-id="${escapeHTML(user.id)}" data-testid="delete-user" aria-label="Delete ${escapeHTML(user.name)}">Delete</button></td></tr>`).join('')}</tbody></table></div>` : `<div class="empty"><div class="empty-icon">${icon('users')}</div><h3>No player IDs saved yet</h3><p>Manual ID entry is always available when creating a server.</p></div>`}</section>`;
}
function deliveryStatus(value = 'unknown') {
  const normalized = String(value || 'unknown').toLowerCase();
  const tones = {sent:'success', failed:'error', retry:'warning', uncertain:'warning', pending:'starting', sending:'starting'};
  const label = normalized.charAt(0).toUpperCase() + normalized.slice(1);
  return `<span class="status ${tones[normalized] || 'unknown'}">${escapeHTML(label)}</span>`;
}
function deliveryServerLabel(delivery) {
  const event = delivery?.event || {};
  const server = state.servers.find((item) => item.id === event.serverId);
  if (server) return serverLabel(server);
  if (event.serverName) return event.serverName;
  return String(event.kind || '') === 'integration_test' ? 'Bot-level test' : 'Unknown server';
}
function discordDeliveryCard(delivery) {
  const event = delivery.event || {};
  const embed = delivery.embed || {};
  const kind = String(event.kind || '');
  const server = deliveryServerLabel(delivery);
  const bot = state.integrations.find((item) => item.id === delivery.integrationId)?.name || delivery.integrationId || 'Unknown bot';
  const title = embed.title || event.message || 'Discord alert';
  const description = embed.description || event.message || 'No message copy available.';
  const timestamp = embed.timestamp || event.timestamp || delivery.updatedAt;
  const attempts = Number.isFinite(Number(delivery.attempts)) ? `${delivery.attempts} attempt${Number(delivery.attempts) === 1 ? '' : 's'}` : 'Attempts unavailable';
  const retry = delivery.status === 'retry' && delivery.nextAttempt ? `<span>Next attempt ${escapeHTML(date(delivery.nextAttempt))}</span>` : '';
  return `<article class="delivery-card" data-testid="discord-recent-delivery" data-kind="${escapeHTML(kind)}">
    <div class="delivery-card-header"><div class="delivery-card-server"><h3>${escapeHTML(server)}</h3>${timestamp ? `<time datetime="${escapeHTML(timestamp)}">${escapeHTML(date(timestamp))}</time>` : ''}</div>${deliveryStatus(delivery.status)}</div>
    <div class="delivery-card-body"><p class="delivery-card-title">${escapeHTML(title)}</p><p>${escapeHTML(description)}</p></div>
    <div class="delivery-card-meta">${delivery.id ? `<span class="mono">${escapeHTML(delivery.id)}</span>` : ''}<span>Bot ${escapeHTML(bot)}</span><span>${escapeHTML(attempts)}</span><span>${escapeHTML(delivery.result || 'No result recorded.')}</span>${retry}</div>
  </article>`;
}
function discordMark() {
  return '<svg class="discord-mark" viewBox="0 0 127.14 96.36" aria-hidden="true"><path fill="#5865F2" d="M107.7,8.07A105.15,105.15,0,0,0,81.47,0a72.06,72.06,0,0,0-3.36,6.83A97.68,97.68,0,0,0,49,6.83,72.06,72.06,0,0,0,45.64,0,105.69,105.69,0,0,0,19.39,8.09C2.79,32.65-1.71,56.6.54,80.21h0A105.73,105.73,0,0,0,32.71,96.36,77.7,77.7,0,0,0,39.6,85.25a68.42,68.42,0,0,1-10.85-5.18c.91-.66,1.8-1.34,2.66-2a75.57,75.57,0,0,0,64.32,0c.87.71,1.76,1.39,2.66,2a68.68,68.68,0,0,1-10.87,5.19,77,77,0,0,0,6.89,11.1A105.25,105.25,0,0,0,126.6,80.22h0C129.24,52.84,122.09,29.11,107.7,8.07ZM42.45,65.69C36.18,65.69,31,60,31,53s5-12.74,11.43-12.74S54,46,53.89,53,48.84,65.69,42.45,65.69Zm42.24,0C78.41,65.69,73.25,60,73.25,53s5-12.74,11.44-12.74S96.23,46,96.12,53,91.08,65.69,84.69,65.69Z"/></svg>';
}
function discordStatus(value) {
  const [label, tone] = discordStatusCopy[value] || discordStatusCopy.not_connected;
  return `<span class="status ${tone}">${label}</span>`;
}
function integrationsHub() {
  const connection = discordConnectionStatus(state.integrations.filter((item) => (item.provider || 'discord') === 'discord'), state.deliveries);
  const [label] = discordStatusCopy[connection];
  const webhookStatus = discordConnectionStatus(state.integrations.filter((item) => item.provider === 'webhook'), state.deliveries);
  return `<div class="provider-grid"><a class="provider-card" href="#integrations/discord" data-testid="discord-integration-card" aria-label="Discord, ${label}">${discordMark()}<span class="provider-card-copy"><strong>Discord</strong><span class="provider-card-summary">Alerts for Dragonwilds servers.</span>${discordStatus(connection)}</span></a><a class="provider-card" href="#integrations/webhook" data-testid="webhook-integration-card">${icon('integrations')}<span class="provider-card-copy"><strong>HTTPS webhooks</strong><span class="provider-card-summary">Structured JSON alerts for your endpoint.</span>${discordStatus(webhookStatus)}</span></a></div>`;
}
function discordBotCard(item) {
  const servers = item.serverIds.map((id) => escapeHTML(serverLabel(state.servers.find((server) => server.id === id) || {id}))).join(', ') || 'No servers';
  const rules = state.alertRules.filter((rule) => item.rules[rule.kind]).map((rule) => escapeHTML(rule.label)).join(', ') || 'No rules enabled';
  return `<article class="bot-card"><div class="bot-card-heading"><h3>${escapeHTML(item.name)}</h3>${status(item.enabled ? 'enabled' : 'disabled')}</div>
    <dl class="detail-list">${item.provider === 'webhook' ? `<div><dt>Endpoint</dt><dd>${escapeHTML(item.webhookUrl)}</dd></div>` : `<div><dt>Guild</dt><dd class="mono">${escapeHTML(item.guildId)}</dd></div><div><dt>Channel</dt><dd class="mono">${escapeHTML(item.channelId)}</dd></div>`}<div><dt>Secret</dt><dd>${escapeHTML(item.secretRef.name)} / ${escapeHTML(item.secretRef.key)}</dd></div><div><dt>Quiet hours</dt><dd>${item.quietHours ? `${escapeHTML(item.quietHours.start)} to ${escapeHTML(item.quietHours.end)} (${escapeHTML(item.quietHours.timezone)})` : 'Off'}</dd></div><div><dt>Servers</dt><dd>${servers}</dd></div><div><dt>Rules</dt><dd>${rules}</dd></div></dl>
    <div class="bot-card-actions"><button data-action="edit-integration" data-id="${escapeHTML(item.id)}" data-testid="edit-integration">Configure</button><button data-action="test-integration" data-id="${escapeHTML(item.id)}" data-testid="test-integration">Send test</button></div></article>`;
}
function discordAlertsPage() {
  const webhook = state.integrationView === 'webhook';
  const provider = webhook ? 'webhook' : 'discord';
  const integrations = state.integrations.filter((item) => (item.provider || 'discord') === provider);
  const deliveries = state.deliveries.filter((item) => (item.provider || 'discord') === provider);
  const pending = Object.entries(state.pendingRestarts).map(([id,op]) => `<p class="inline-note">Restart ${escapeHTML(op.id)} for ${escapeHTML(serverLabel(state.servers.find((server) => server.id === id) || {id}))} awaits a fresh observation. Requested ${escapeHTML(date(op.requestedAt))}.${op.commandUncertain ? ' Command outcome is unknown.' : ''}</p>`).join('');
  const demo = state.integrationsDemo ? 'Demo mode simulates deliveries and restarts. No external messages are sent. ' : '';
  const bots = integrations.length
    ? `<div class="bot-card-list">${integrations.map(discordBotCard).join('')}</div>`
    : `<div class="empty"><div class="empty-icon">${icon('integrations')}</div><h3>No ${webhook ? 'HTTPS webhooks' : 'Discord bots'} configured yet</h3><p>Add a destination to route selected server alerts.</p></div>`;
  return `<a class="back-link" href="#integrations" data-testid="integrations-back">${icon('back')}Back</a>
    <section class="panel"><div class="panel-heading"><div><h2>${webhook ? 'HTTPS webhooks' : 'Discord bots'}</h2><p>Send selected server alerts to ${webhook ? 'an HTTPS endpoint' : 'a Discord channel'}.</p></div><button class="primary" data-action="add-integration" data-testid="add-integration">${icon('plus')}Add ${webhook ? 'HTTPS webhook' : 'Discord bot'}</button></div>
    ${pending}<p class="inline-note">${demo}Player joined alerts name characters only when fresh roster evidence supports them; otherwise they report approximate count increases. Backup alerts are unavailable until a backup producer exists.</p>${bots}</section>
    <section class="panel section-gap"><div class="panel-heading"><div><h2>Recent messages</h2><p>Newest first. Each message names the Dragonwilds server it belongs to.</p></div></div>
    <p class="inline-note">Uncertain means a message may have been sent. Check the destination before sending a new test. Uncertain deliveries never retry automatically. Disabling a rule cancels queued alerts; an in-flight send may finish.</p>
    ${deliveries.length ? `<div class="recent-delivery-list">${[...deliveries].sort((a, b) => deliveryTime(b) - deliveryTime(a)).map(discordDeliveryCard).join('')}</div>` : '<p class="no-results">No deliveries recorded yet.</p>'}</section>`;
}
function integrationsPage() {
  if (!can('integrations')) return dashboard();
  return ['discord','webhook'].includes(state.integrationView) ? discordAlertsPage() : integrationsHub();
}
function integrationForm(item) {
  const provider = item ? (item.provider || 'discord') : (state.integrationView === 'webhook' ? 'webhook' : 'discord');
  const quiet = item?.quietHours;
  const servers = state.servers.map((server) => `<label class="choice-row"><input type="checkbox" name="integrationServer" value="${escapeHTML(server.id)}" ${item?.serverIds?.includes(server.id) ? 'checked' : ''}><span class="choice-copy"><strong>${escapeHTML(serverLabel(server))}</strong></span></label>`).join('') || '<p>No servers available.</p>';
  const rules = state.alertRules.map((rule) => `<label class="choice-row"><input type="checkbox" name="integrationRule" value="${escapeHTML(rule.kind)}" ${rule.available ? '' : 'disabled'} ${item?.rules?.[rule.kind] && rule.available ? 'checked' : ''}><span class="choice-copy"><strong>${escapeHTML(rule.label)}</strong><small>${escapeHTML(rule.source)}</small></span></label>`).join('');
  return `<div class="error-summary" id="integration-error-summary" role="alert" tabindex="-1" hidden><h3>Check the integration</h3><ul></ul></div><fieldset class="form-section"><legend>Integration</legend><div class="form-grid"><label class="field">Display name<input name="name" data-testid="integration-name" required maxlength="80" value="${escapeHTML(item?.name || '')}"></label><label class="field">Alerts<select name="enabled" data-testid="integration-enabled"><option value="true" ${item?.enabled !== false ? 'selected' : ''}>Enabled</option><option value="false" ${item?.enabled === false ? 'selected' : ''}>Disabled</option></select></label><label class="field">Provider<select name="provider" id="integration-provider"><option value="discord" ${provider === 'discord' ? 'selected' : ''}>Discord</option><option value="webhook" ${provider === 'webhook' ? 'selected' : ''}>HTTPS webhook</option></select></label></div></fieldset>
    <fieldset class="form-section" data-provider="discord" ${provider === 'discord' ? '' : 'hidden disabled'}><legend>Discord destination</legend><div class="form-grid"><label class="field">Guild ID<input name="guildId" data-testid="integration-guild" required pattern="${PATTERN.snowflake}" value="${escapeHTML(item?.guildId || '')}"></label><label class="field">Channel ID<input name="channelId" data-testid="integration-channel" required pattern="${PATTERN.snowflake}" value="${escapeHTML(item?.channelId || '')}"></label></div></fieldset>
    <fieldset class="form-section" data-provider="webhook" ${provider === 'webhook' ? '' : 'hidden disabled'}><legend>Webhook destination</legend><label class="field">HTTPS endpoint<input name="webhookUrl" data-testid="integration-webhook-url" type="url" required maxlength="2048" value="${escapeHTML(item?.webhookUrl || '')}" placeholder="https://alerts.example.com/events"><small>Public HTTPS on port 443. No credentials, query, or fragment in the URL. The Secret is sent as a bearer token. Redirects are not followed.</small></label></fieldset>
    <fieldset class="form-section"><legend>Kubernetes Secret</legend><p>Reference a pre-created Kubernetes Secret in the C2 namespace. Never enter a bot token here. Change the Secret name or key to rotate the reference. Sending a test also works while disabled.</p><div class="form-grid"><label class="field">Secret name<input name="secretName" data-testid="integration-secret-name" required maxlength="253" value="${escapeHTML(item?.secretRef?.name || '')}" autocomplete="off"></label><label class="field">Secret key<input name="secretKey" data-testid="integration-secret-key" required maxlength="253" value="${escapeHTML(item?.secretRef?.key || 'token')}" autocomplete="off"></label></div></fieldset>
    <fieldset class="form-section"><legend>Quiet hours</legend><label class="choice-row"><input type="checkbox" name="quietEnabled" id="integration-quiet-enabled" ${quiet ? 'checked' : ''}><span>Mute this integration during quiet hours</span></label><p>Applies to all selected rules. Suppressed alerts are not replayed. Tests bypass quiet hours.</p><div class="form-grid" id="integration-quiet-fields" ${quiet ? '' : 'hidden'}><label class="field">Start time<input type="time" name="quietStart" required value="${escapeHTML(quiet?.start || '22:00')}" ${quiet ? '' : 'disabled'}></label><label class="field">End time<input type="time" name="quietEnd" required value="${escapeHTML(quiet?.end || '08:00')}" ${quiet ? '' : 'disabled'}></label><label class="field">IANA timezone<input name="quietTimezone" required value="${escapeHTML(quiet?.timezone || state.displayTimezone || 'UTC')}" placeholder="America/New_York" ${quiet ? '' : 'disabled'}></label></div></fieldset>
    <fieldset class="form-section"><legend>Dragonwilds servers</legend>${servers}</fieldset>
    <fieldset class="form-section"><legend>Alert rules</legend>${rules}</fieldset>`;
}
function syncIntegrationFields() {
  const provider = $('#integration-provider').value;
  document.querySelectorAll('[data-provider]').forEach((section) => { section.hidden = section.disabled = section.dataset.provider !== provider; });
  const quiet = $('#integration-quiet-enabled').checked;
  $('#integration-quiet-fields').hidden = !quiet;
  $('#integration-quiet-fields').querySelectorAll('input').forEach((input) => { input.disabled = !quiet; });
}
function showIntegrationErrors(fields) {
  const form = $('#modal-form');
  form.querySelectorAll('[data-integration-error]').forEach((element) => { element.textContent = ''; });
  form.querySelectorAll('[aria-invalid]').forEach((element) => element.removeAttribute('aria-invalid'));
  const summary = $('#integration-error-summary');
  summary.querySelector('ul').innerHTML = Object.entries(fields).map(([name, message]) => {
    const input = form.elements.namedItem(name);
    if (!input?.id) return `<li>${escapeHTML(message)}</li>`;
    input.setAttribute('aria-invalid', 'true');
    const inline = document.getElementById(`${input.id}-error`);
    if (inline) inline.textContent = message;
    return `<li><a href="#${escapeHTML(input.id)}">${escapeHTML(message)}</a></li>`;
  }).join('');
  summary.hidden = false;
  summary.focus();
}
function rebootResultLabel(result) {
  return ({awaiting_reconciliation:'Awaiting reconciliation', completed:'Completed', failed:'Failed', skipped:'Skipped', missed:'Missed during downtime', uncertain:'Uncertain'})[result] || 'No execution yet';
}
function rebootResult(result) {
  const tone = ({completed:'success',failed:'error',skipped:'warning',missed:'warning',uncertain:'unknown',awaiting_reconciliation:'warning'})[result] || 'unknown';
  return `<span class="result result-${tone}">${escapeHTML(rebootResultLabel(result))}</span>`;
}
function rebootTimingLabel(schedule) {
  if (schedule.mode === 'cron') return `Cron <span class="mono">${escapeHTML(schedule.cron)}</span>`;
  if (schedule.mode === 'interval') return `Every ${escapeHTML(schedule.intervalValue)} ${escapeHTML(schedule.intervalUnit)}`;
  return `Daily ${escapeHTML((schedule.dailyTimes || []).join(', '))}`;
}
function timezoneSelect(name, selected, testId) {
  return `<select name="${name}" id="${name}" data-testid="${testId || name}" required>${timezoneOptions(selected).map((zone) => `<option value="${escapeHTML(zone)}" ${zone === selected ? 'selected' : ''}>${escapeHTML(zone)}</option>`).join('')}</select>`;
}
function dailyTimeRow(value, index) {
  return `<div class="daily-time-row"><label class="field"><span>Time ${index + 1}</span><input type="time" name="dailyTime" data-testid="reboot-daily-time" value="${escapeHTML(value)}" required aria-describedby="dailyTimes-error"></label><button type="button" class="subtle" data-action="remove-daily-time" aria-label="Remove time ${index + 1}">Remove</button></div>`;
}
function updateDailyTimeControls() {
  const rows = Array.from(document.querySelectorAll('#dailyTimes .daily-time-row'));
  rows.forEach((row, index) => {
    row.querySelector('span').textContent = `Time ${index + 1}`;
    row.querySelector('button').disabled = rows.length === 1;
    row.querySelector('button').setAttribute('aria-label', `Remove time ${index + 1}`);
  });
}
function addDailyTime() {
  const container = $('#dailyTimes');
  if (!container) return;
  container.insertAdjacentHTML('beforeend', dailyTimeRow('', container.querySelectorAll('.daily-time-row').length));
  updateDailyTimeControls();
  container.lastElementChild.querySelector('input')?.focus();
}
function removeDailyTime(button) {
  const container = $('#dailyTimes');
  if (!container || container.querySelectorAll('.daily-time-row').length === 1) return;
  button.closest('.daily-time-row')?.remove();
  updateDailyTimeControls();
}
function rebootsPage() {
  const selected = state.displayTimezone || browserTimezone();
  const availability = state.rebootsAvailable ? '' : '<p class="notice info" role="status">Scheduled execution is disabled for this deployment. Existing schedules remain visible so you can inspect, disable, or delete them.</p>';
  const demo = state.rebootsDemo ? '<p class="notice info">Demo mode simulates scheduled restarts. No Kubernetes operation is sent.</p>' : '';
  const schedules = (state.reboots || []).filter((item) => !state.routeScope || item.serverId === state.serverId);
  const history = (state.rebootHistory || []).filter((item) => !state.routeScope || item.serverId === state.serverId);
  return `${availability}${demo}<section class="panel"><div class="panel-heading"><div><h2>Scheduled reboots</h2><p>Each schedule targets one server. A claimed restart can no longer be canceled.</p></div><div class="page-controls"><label class="field timezone-control">Display timezone${timezoneSelect('display-timezone', selected, 'display-timezone')}</label>${can('reboots') && state.rebootsAvailable ? `<button class="primary" data-action="add-reboot" data-testid="add-reboot">${icon('plus')}Add schedule</button>` : ''}</div></div><p class="inline-note">Connected players may be disconnected. An unknown player count is never treated as zero. Preview and execution use the schedule's execution timezone; this preference only formats the console.</p>${schedules.length ? `<div class="table-wrap"><table><thead><tr><th>Server</th><th>Timing</th><th>Execution zone</th><th>State</th><th>Next run</th><th>Last result</th><th>Actions</th></tr></thead><tbody>${schedules.map((schedule) => `<tr><td><strong>${escapeHTML(schedule.serverName || schedule.serverId)}</strong><small class="mono">${escapeHTML(schedule.serverId)}</small></td><td>${rebootTimingLabel(schedule)}</td><td class="mono">${escapeHTML(schedule.executionTimezone)}</td><td>${schedule.enabled ? '<span class="result result-success">Enabled</span>' : '<span class="result result-unknown">Disabled</span>'}</td><td class="mono">${schedule.nextRun ? escapeHTML(zonedDate(schedule.nextRun, selected)) : '—'}</td><td>${rebootResult(schedule.lastResult)}${schedule.lastReason ? `<small>${escapeHTML(schedule.lastReason)}</small>` : ''}</td><td class="actions"><button data-action="edit-reboot" data-id="${escapeHTML(schedule.id)}" data-testid="edit-reboot">Edit</button><button class="danger" data-action="delete-reboot" data-id="${escapeHTML(schedule.id)}" data-testid="delete-reboot">Delete</button></td></tr>`).join('')}</tbody></table></div>` : '<div class="empty"><div class="empty-icon">'+icon('clock')+'</div><h3>No reboot schedules</h3><p>Create a per-server schedule. Saving never triggers an immediate restart.</p></div>'}</section><section class="panel section-gap"><div class="panel-heading"><div><h2>Execution history</h2><p>History is retained for audit and distinguishes requested work from observed completion.</p></div></div>${history.length ? `<div class="table-wrap"><table><thead><tr><th>When</th><th>Server</th><th>Occurrence</th><th>Result</th><th>Operation</th><th>Details</th></tr></thead><tbody>${history.map((execution) => `<tr><td class="mono">${escapeHTML(zonedDate(execution.recordedAt, selected))}</td><td>${escapeHTML(execution.serverName || execution.serverId)}</td><td class="mono">${escapeHTML(zonedDate(execution.occurrenceAt, selected))}<small>${escapeHTML(execution.occurrenceId || execution.id || '')}</small></td><td>${rebootResult(execution.result)}</td><td class="mono">${escapeHTML(execution.operationId || '—')}</td><td>${escapeHTML(execution.reason || '')}</td></tr>`).join('')}</tbody></table></div>` : '<p class="no-results">No scheduled reboot executions recorded yet.</p>'}</section>`;
}
function rebootForm(item) {
  const zone = item?.executionTimezone || state.displayTimezone || browserTimezone();
  const enabled = item?.enabled !== false;
  const mode = item?.mode || 'daily';
  const dailyTimes = item?.dailyTimes?.length ? item.dailyTimes : ['05:00'];
  const dailyRows = dailyTimes.map((value, index) => dailyTimeRow(value, index)).join('');
  return `<p>Choose exactly one timing mode. The first run is strictly after this save. Editing the target, timing, or execution timezone starts a new schedule anchor.</p><div class="error-summary" id="reboot-error-summary" role="alert" tabindex="-1" hidden><h3>Check the schedule</h3><ul></ul></div><div class="form-grid reboot-form"><label class="field">Server<select name="serverId" id="serverId" data-testid="reboot-server" required>${state.servers.map((server) => `<option value="${escapeHTML(server.id)}" ${server.id === (item?.serverId || state.modalServerId) ? 'selected' : ''}>${escapeHTML(serverLabel(server))}</option>`).join('')}</select><small id="serverId-error" data-reboot-error></small></label><label class="field">Execution timezone${timezoneSelect('executionTimezone', zone, 'reboot-timezone')}<small id="executionTimezone-error" data-reboot-error>Stored with this schedule; it is not changed by the display preference.</small></label><label class="field">Timing mode<select name="mode" id="reboot-mode" data-testid="reboot-mode" aria-describedby="mode-error" required><option value="cron" ${mode === 'cron' ? 'selected' : ''}>Cron expression</option><option value="interval" ${mode === 'interval' ? 'selected' : ''}>Elapsed interval</option><option value="daily" ${mode === 'daily' ? 'selected' : ''}>Daily wall-clock times</option></select><small id="mode-error" data-reboot-error></small></label><span></span><label class="field full" data-reboot-field="cron" ${mode === 'cron' ? '' : 'hidden'}>Cron expression<input name="cron" id="cron" data-testid="reboot-cron" value="${escapeHTML(item?.cron || '0 5 * * *')}" placeholder="minute hour day-of-month month day-of-week" aria-describedby="cron-error"><small id="cron-error" data-reboot-error>Five fields. Sunday is 0 or SUN; day-of-month and day-of-week use standard cron OR semantics. No seconds, descriptors, or timezone prefixes.</small></label><label class="field" data-reboot-field="interval" ${mode === 'interval' ? '' : 'hidden'}>Every<input type="number" name="intervalValue" id="intervalValue" data-testid="reboot-interval-value" min="1" max="8760" value="${escapeHTML(item?.intervalValue || 12)}" aria-describedby="intervalValue-error"><small id="intervalValue-error" data-reboot-error>Hours: 1–8760. Days: 1–365. A day is exactly 24 elapsed hours.</small></label><label class="field" data-reboot-field="interval" ${mode === 'interval' ? '' : 'hidden'}>Unit<select name="intervalUnit" id="intervalUnit" data-testid="reboot-interval-unit" aria-describedby="intervalUnit-error"><option value="hours" ${item?.intervalUnit !== 'days' ? 'selected' : ''}>hours</option><option value="days" ${item?.intervalUnit === 'days' ? 'selected' : ''}>days</option></select><small id="intervalUnit-error" data-reboot-error></small></label><div class="field full" data-reboot-field="daily" ${mode === 'daily' ? '' : 'hidden'}><span>Daily times</span><div id="dailyTimes" class="daily-times" aria-describedby="dailyTimes-error">${dailyRows}</div><button type="button" class="subtle" data-action="add-daily-time" data-testid="add-daily-time">Add another time</button><small id="dailyTimes-error" data-reboot-error>Use the native time controls. Times must be unique HH:mm values; they are sorted before execution.</small></div><label class="field full">Restart warning (minutes)<input type="number" name="warningMinutes" id="warningMinutes" data-testid="reboot-warning-minutes" min="0" max="60" step="1" value="${escapeHTML(item?.warningMinutes || 0)}" aria-describedby="warningMinutes-error"><small id="warningMinutes-error" data-reboot-error>0 disables warnings. Enable the Restart warning rule on an integration to send advance notice. Manual restarts remain immediate.</small></label><label class="field full checkbox-field"><span><input type="checkbox" name="enabled" ${enabled ? 'checked' : ''}> Enable this schedule</span><small>A disabled schedule keeps its history and next run is cleared.</small></label><label class="field full checkbox-field"><span><input type="checkbox" name="acknowledgeDisconnect" id="acknowledgeDisconnect" aria-describedby="acknowledgeDisconnect-error"> I understand that an enabled scheduled reboot may disconnect connected players.</span><small id="acknowledgeDisconnect-error" data-reboot-error>Required every time an enabled schedule is saved.</small></label><div class="field full"><button type="button" class="subtle" data-action="preview-reboot" data-testid="preview-reboot">Preview next five runs</button><div id="reboot-preview" class="preview-results" aria-live="polite"></div></div></div>`;
}
const rebootErrorControls = {mode:'reboot-mode', serverId:'serverId', executionTimezone:'executionTimezone', cron:'cron', intervalValue:'intervalValue', intervalUnit:'intervalUnit', dailyTimes:'dailyTimes', enabled:'enabled', acknowledgeDisconnect:'acknowledgeDisconnect', warningMinutes:'warningMinutes'};
function clearRebootErrors() {
  const summary = $('#reboot-error-summary');
  if (summary) { summary.hidden = true; summary.querySelector('ul').innerHTML = ''; }
  document.querySelectorAll('[data-reboot-error]').forEach((element) => {
    if (element.dataset.defaultText === undefined) element.dataset.defaultText = element.textContent;
    element.textContent = element.dataset.defaultText;
    element.classList.remove('field-error');
  });
  Object.values(rebootErrorControls).forEach((id) => document.getElementById(id)?.removeAttribute('aria-invalid'));
}
function showRebootErrors(fields) {
  clearRebootErrors();
  const summary = $('#reboot-error-summary');
  if (!summary) return;
  const entries = Object.entries(fields);
  summary.querySelector('ul').innerHTML = entries.map(([field, message]) => {
    const controlId = rebootErrorControls[field] || field;
    document.getElementById(controlId)?.setAttribute('aria-invalid', 'true');
    const inline = document.getElementById(`${field}-error`) || document.getElementById(`${controlId}-error`);
    if (inline) { inline.textContent = message; inline.classList.add('field-error'); }
    return `<li><a href="#${escapeHTML(controlId)}">${escapeHTML(message)}</a></li>`;
  }).join('');
  summary.hidden = false;
  summary.focus();
}
function rebootRequestFromForm() {
  const fields = new FormData($('#modal-form'));
  const values = Object.fromEntries(fields);
  const body = {serverId:values.serverId, enabled:values.enabled === 'on', mode:values.mode, executionTimezone:values.executionTimezone, warningMinutes:Number(values.warningMinutes || 0), acknowledgeDisconnect:values.acknowledgeDisconnect === 'on'};
  if (values.mode === 'cron') body.cron = values.cron;
  if (values.mode === 'interval') { body.intervalValue = Number(values.intervalValue); body.intervalUnit = values.intervalUnit; }
  if (values.mode === 'daily') body.dailyTimes = fields.getAll('dailyTime').map((value) => value.trim()).filter(Boolean);
  return body;
}
function syncRebootModeFields() {
  const mode = $('#reboot-mode')?.value;
  document.querySelectorAll('[data-reboot-field]').forEach((field) => {
    const active = field.dataset.rebootField === mode;
    field.hidden = !active;
    field.querySelectorAll('input,select,button').forEach((input) => { input.disabled = !active; });
  });
}
async function previewReboot() {
  if (!can('reboots') || state.modalBusy) return;
  const body = rebootRequestFromForm();
  const sequence = ++state.previewSequence;
  const button = $('[data-action="preview-reboot"]');
  if (button) button.disabled = true;
  try {
    const result = await api('/api/reboots/preview', {method:'POST', body:JSON.stringify(body)});
    if (sequence !== state.previewSequence) return;
    const displayZone = validDisplayTimezone() || browserTimezone();
    $('#reboot-preview').innerHTML = `<p class="preview-heading">Next five runs in ${escapeHTML(displayZone)} (execution zone ${escapeHTML(result.executionTimezone)}):</p><ol>${(result.runs || []).map((run) => `<li class="mono">${escapeHTML(zonedDate(run, displayZone))}</li>`).join('')}</ol>`;
  } catch (error) {
    if (sequence !== state.previewSequence || error.name === 'AbortError') return;
    if (error.fields && Object.keys(error.fields).length) showRebootErrors(error.fields);
    $('#reboot-preview').innerHTML = `<p class="notice error" role="alert">${escapeHTML(error.message)}</p>`;
  } finally {
    if (button?.isConnected) button.disabled = false;
  }
}
function navHTML() {
  return Object.entries(pages).filter(([page]) => page !== 'servers' && can(page)).map(([page,[label]])=>`<a href="${escapeHTML(['telemetry','maintenance','events','reboots'].includes(page) ? scopedHash(page) : `#${page}`)}" data-testid="nav-${page}" title="${escapeHTML(label)}" ${page===state.page?'aria-current="page"':''}>${icon(page)}<span class="nav-label">${escapeHTML(label)}</span></a>`).join('');
}
function render() {
  if (state.authRequired) { lockedState(); return; }
  if (!can(state.page)) state.page = 'dashboard';
  const openCharts = Array.from(document.querySelectorAll('details[data-chart][open]'), (element) => element.dataset.chart);
  const focused = document.activeElement;
  const testId = focused?.getAttribute('data-testid');
  const selection = focused instanceof HTMLInputElement ? [focused.selectionStart, focused.selectionEnd] : null;
  const [title,description] = pageHeading();
  document.title = `${title} · RSDW C2`;
  $('#page-title').textContent = title;
  $('#page-description').textContent = description;
  $('#navigation').innerHTML = navHTML();
  const individual = state.page === 'telemetry' || state.page === 'maintenance';
  $('#server-filter').innerHTML = `${individual && state.servers.length ? '' : '<option value="">All servers</option>'}${state.servers.map((server)=>`<option value="${escapeHTML(server.id)}">${escapeHTML(serverLabel(server))}</option>`).join('')}`;
  $('#server-filter').value = individual ? selectedServer()?.id || '' : state.serverId;
  $('#server-filter').disabled = !state.servers.length;
  $('#server-filter').hidden = ['servers','users','integrations','reboots'].includes(state.page);
  $('#content').innerHTML = `${state.routeError ? `<p class="notice error" role="alert" data-testid="route-error">${escapeHTML(state.routeError)}</p>` : ''}${({servers:serverOverview,dashboard,telemetry,events:eventsPage,maintenance,users:usersPage,integrations:integrationsPage,reboots:rebootsPage})[state.page]()}`;
  for (const key of openCharts) {
    const disclosure = document.querySelector(`details[data-chart="${CSS.escape(key)}"]`);
    if (disclosure) disclosure.open = true;
  }
  if (state.page === 'maintenance' && selectedServer() && !state.deletions[selectedServer().id]) {
    const server = selectedServer();
    if (can('edit-settings') && server.status !== 'stopped') $('#content .action-grid').insertAdjacentHTML('beforeend', '<div class="action-card"><button data-action="edit-settings" data-testid="edit-settings">Edit settings</button><p>Change creator, world name, player limit, memory, and CPU. Requires a rollout.</p></div>');
    if (can('delete')) $('#content .action-grid').insertAdjacentHTML('beforeend', '<div class="action-card"><button class="danger" data-action="delete" data-testid="delete-server">Delete server</button><p>Disconnect players and remove this server. Keep world data by default.</p></div>');
    $('#content').insertAdjacentHTML('beforeend', deletionReceipts());
  }
  $('#content').setAttribute('aria-busy','false');
  clearTimeout(rosterExpiryTimer);
  const key = rosterObservationKey(state.telemetry);
  const rosterExpiresIn = state.rosterObservation === key && Number.isFinite(state.rosterDeadline)
    ? state.rosterDeadline - monotonicNow()
    : Date.parse(state.telemetry?.metrics?.players?.observedAt) + 45001 - Date.now();
  if (['telemetry','servers'].includes(state.page) && state.telemetry?.metrics?.players?.status === 'available' && rosterExpiresIn > 0) {
    rosterExpiryTimer = setTimeout(render, rosterExpiresIn);
  }
  if (testId) {
    const identity = focused.dataset.id ? `[data-id="${CSS.escape(focused.dataset.id)}"]` : '';
    const replacement = document.querySelector(`[data-testid="${CSS.escape(testId)}"]${identity}`);
    replacement?.focus({preventScroll:true});
    if (selection && replacement instanceof HTMLInputElement && replacement.type === 'search') replacement.setSelectionRange(...selection);
  }
}
function connection(failed = !$('#error-banner').hidden) {
  $('#connection-status').textContent = failed ? 'Connection issue · data may be stale' : state.paused ? 'Updates paused' : 'Connected · refreshes every 10s';
  $('#connection-status').classList.toggle('connected',!failed && !state.paused);
  $('#updated-at').textContent = state.lastUpdated ? `Updated ${date(state.lastUpdated,true)}` : 'Waiting for first update';
  $('#pause').textContent = state.paused ? 'Resume updates' : 'Pause updates';
  $('#pause').setAttribute('aria-pressed',String(state.paused));
}
async function loadLogs(signal) {
  if (!can('logs')) return;
  const epoch = state.epoch;
  const server = selectedServer();
  if (!server || server.status === 'stopped') return;
  const result = await api(`/api/servers/${encodeURIComponent(server.id)}/logs?tail=100`, {signal});
  if (epoch !== state.epoch || signal?.aborted || !can('logs')) throw staleRequest();
  const lines = typeof result === 'string' ? result : result.lines ?? result.logs ?? '';
  state.logs = Array.isArray(lines) ? lines.map((line) => typeof line === 'string' ? line : `${date(line.timestamp)} [${line.level || 'INFO'}] ${line.message || ''}`).join('\n') : String(lines);
}
async function refresh() {
  if (state.logoutCSRF) return;
  const sequence = ++refreshSequence;
  state.request?.abort();
  const controller = new AbortController();
  state.request = controller;
  let epoch = state.epoch;
  let page = state.page;
  const range = state.range;
  let eventRange = state.eventRange;
  let serverId = state.serverId;
  let selectedId = selectedServer()?.id;
  let telemetryReceived = false;
  const current = () => sequence === refreshSequence && epoch === state.epoch && page === state.page && range === state.range && eventRange === state.eventRange && serverId === state.serverId && selectedId === selectedServer()?.id && state.request === controller && !controller.signal.aborted;
  state.refreshing = true;
  $('#refresh').disabled = true;
  try {
    const auth = await discoverAuth(controller.signal);
    if (!current()) return;
    state.request = null;
    if (!applyAuth(auth)) return;
    applyRouteScope();
    epoch = state.epoch;
    page = state.page;
    eventRange = state.eventRange;
    serverId = state.serverId;
    selectedId = selectedServer()?.id;
    state.request = controller;
    const bootstrap = await api('/api/bootstrap',{signal:controller.signal});
    if (!current()) return;
    if (!Array.isArray(bootstrap.servers)) throw new Error('The server inventory response is missing. Please refresh to try again.');
    state.servers = bootstrap.servers;
    state.deletions = bootstrap.deletions || {};
    state.loaded = true;
    applyRouteScope();
    validateOverviewRoute();
    page = state.page;
    if (!state.routeScope && state.serverId && !state.servers.some((server) => server.id === state.serverId)) state.serverId = '';
    if (selectedId !== selectedServer()?.id) { clearTelemetry(); state.logs = ''; render(); }
    serverId = state.serverId;
    selectedId = selectedServer()?.id;
    $('#cluster-name').textContent = typeof bootstrap.cluster === 'string' ? bootstrap.cluster : bootstrap.cluster?.name || bootstrap.clusterName || 'Local cluster';
    state.mode = bootstrap.mode;
    $('#environment').textContent = bootstrap.mode === 'demo' ? 'Demo mode' : 'Kubernetes';
    const server = selectedServer();
    const eventParams = state.page === 'events' ? {limit:100, offset:0} : {limit:50, preview:true, serverId:state.page === 'maintenance' ? server?.id : state.serverId};
    const eventsPromise = can('events') ? api(`/api/events?${eventListParams(eventParams)}`,{signal:controller.signal}).then((result) => { if (epoch === state.epoch && !controller.signal.aborted) acceptEvents(result); }) : Promise.resolve();
    const requests = [eventsPromise];
    if (can('users')) requests.push(api('/api/users',{signal:controller.signal}).then((result) => { if (epoch === state.epoch && !controller.signal.aborted) state.users = result.users; }));
    if (can('integrations')) requests.push(api('/api/integrations',{signal:controller.signal}).then((result) => { if (epoch === state.epoch && !controller.signal.aborted) Object.assign(state,{integrations:result.integrations,deliveries:result.deliveries,alertRules:result.rules,pendingRestarts:result.pendingRestarts,integrationsDemo:result.demo}); }));
    if (can('reboots')) requests.push(api('/api/reboots',{signal:controller.signal}).then((result) => { if (current()) Object.assign(state,{reboots:result.schedules || [], rebootHistory:result.history || [], rebootsAvailable:result.available !== false, rebootsDemo:result.demo === true}); }).catch((error) => { if (current()) state.rebootsAvailable = false; throw error; }));
    if (['telemetry','servers'].includes(state.page) && server) {
      const telemetryStartedAt = monotonicNow();
      requests.push(api(`/api/servers/${encodeURIComponent(server.id)}/telemetry?range=${encodeURIComponent(range)}`,{signal:controller.signal}).then((result) => {
        if (!current()) return;
        if (result.server?.id !== selectedId) throw new Error('Telemetry returned a different server. Please refresh to try again.');
        acceptTelemetry(result, telemetryStartedAt, monotonicNow());
        telemetryReceived = true;
        render();
      }));
      if (state.page === 'telemetry' && can('logs') && server.status !== 'stopped') requests.push(loadLogs(controller.signal));
    }
    const results = await Promise.allSettled(requests);
    if (!current()) return;
    const failure = results.find((result) => result.status === 'rejected');
    if (failure) throw failure.reason;
    state.lastUpdated = new Date();
    $('#error-banner').hidden = true;
    render();
    connection();
  } catch (error) {
    if (!current() || error.name === 'AbortError') return;
    if (state.authRequired) { lockedState(); return; }
    $('#error-banner').textContent = error.message;
    $('#error-banner').hidden = false;
    if (!telemetryReceived) clearTelemetry();
    if (state.loaded) render();
    else $('#content').innerHTML = '<section class="panel empty"><div class="empty-icon">'+icon('warning')+'</div><h2>Unable to connect</h2><p>Your server inventory could not be loaded. Check the connection and try again.</p><button data-action="retry" data-testid="retry-connection">Try again</button></section>';
    $('#content').setAttribute('aria-busy','false');
    connection(true);
  } finally {
    if (sequence === refreshSequence) { state.refreshing = false; $('#refresh').disabled = false; }
  }
}
function editSettingsValues(server) {
  return {name:server.name || '', worldName:server.worldName || server.name || '', maxPlayers:server.maxPlayers, memoryLimitMiB:server.memoryLimitMiB || 2048, cpuLimitMillis:server.cpuLimitMillis || 1000};
}
function editSettingsPatch(initial, values) {
  const patch = {};
  for (const key of Object.keys(initial)) {
    const value = ['name','worldName'].includes(key) ? values[key].trim() : Number(values[key]);
    if (value !== initial[key]) patch[key] = value;
  }
  if (patch.worldName !== undefined) patch.confirmWorldName = values.confirmWorldName === 'true';
  return Object.keys(patch).length ? {...patch, confirm:true} : null;
}
function openModal(action, userId = '') {
  if (!can(action === 'add-server' ? 'create' : action)) return;
  $('#modal').classList.toggle('create-server-dialog', action === 'add-server');
  modalOpener = document.activeElement;
  state.modalAction = action;
  $('#modal-form').noValidate = ['add-integration','edit-integration'].includes(action);
  state.modalServerId = selectedServer()?.id || '';
  if (action === 'delete' && userId) state.modalServerId = userId;
  state.modalUserId = userId;
  state.modalRebootId = userId;
  $('#modal-error').hidden = true;
  $('#modal-submit').disabled = false;
  $('#modal-submit').classList.toggle('danger',['restart','delete-user','delete','stop'].includes(action));
  $('#modal-submit').classList.toggle('primary',!['restart','delete-user','delete','stop'].includes(action));
  if (action === 'add-reboot' || action === 'edit-reboot' || action === 'delete-reboot') {
    const item = state.reboots.find((schedule) => schedule.id === userId);
    if (action !== 'add-reboot' && !item) throw new Error('This reboot schedule no longer exists. Refresh the list.');
    $('#modal').classList.remove('create-server-dialog');
    $('#modal-title').textContent = action === 'delete-reboot' ? 'Delete reboot schedule?' : action === 'add-reboot' ? 'Add reboot schedule' : 'Edit reboot schedule';
    $('#modal-submit').textContent = action === 'delete-reboot' ? 'Delete schedule' : 'Save schedule';
    $('#modal-submit').classList.toggle('danger', action === 'delete-reboot');
    $('#modal-submit').classList.toggle('primary', action !== 'delete-reboot');
    $('#modal-body').innerHTML = action === 'delete-reboot'
      ? `<p>Delete the schedule for <strong>${escapeHTML(item.serverName || item.serverId)}</strong>? This removes future claims, but a restart already claimed by C2 cannot be canceled and remains in history.</p>`
      : rebootForm(item);
  } else if (action === 'add-integration' || action === 'edit-integration') {
    const item = state.integrations.find((i) => i.id === userId);
    if (action === 'edit-integration' && !item) throw new Error('This integration no longer exists. Refresh the list.');
    state.modalIntegrationId = userId;
    $('#modal-title').textContent = action === 'add-integration' ? 'Add integration' : 'Configure integration';
    $('#modal-submit').textContent = 'Save integration';
    $('#modal-body').innerHTML = integrationForm(item);
    $('#modal-form').querySelectorAll('input:not([type="checkbox"]),select').forEach((input) => {
      if (!input.id) input.id = `integration-${input.name}`;
      const errorId = `${input.id}-error`;
      input.setAttribute('aria-describedby', errorId);
      input.insertAdjacentHTML('afterend', `<small id="${errorId}" class="field-error" data-integration-error></small>`);
    });
  } else if (['add-user','edit-user','delete-user'].includes(action)) {
    const user = state.users.find((item) => item.id === userId);
    if (action !== 'add-user' && !user) throw new Error('This saved ID no longer exists. Refresh the list.');
    $('#modal-title').textContent = action === 'delete-user' ? 'Delete saved ID?' : action === 'edit-user' ? 'Edit saved ID' : 'Add saved ID';
    $('#modal-submit').textContent = action === 'delete-user' ? 'Delete saved ID' : 'Save ID';
    $('#modal-body').innerHTML = action === 'delete-user'
      ? `<p>Delete <strong>${escapeHTML(user.name)}</strong> from saved IDs? Existing servers keep their owner ID.</p>`
      : `<label class="field">Display name<input name="name" data-testid="user-name" required maxlength="48" value="${escapeHTML(user?.name || '')}" autocomplete="off" autofocus></label><label class="field">RSDW / EOS player ID<input name="playerId" data-testid="user-player-id" required maxlength="128" pattern="${PATTERN.eosId}" value="${escapeHTML(user?.playerId || '')}" autocomplete="off" title="Exactly 32 hexadecimal characters, without spaces or separators"><small>Copy the Player ID from the game's Settings menu. Each ID can be saved once.</small></label>`;
  } else if (action === 'edit-settings') {
    const server = selectedServer();
    state.modalInitialSettings = editSettingsValues(server);
    const image = server.currentImage ? `Running image ${escapeHTML(server.currentImage)}${server.desiredImage && server.desiredImage !== server.currentImage ? ` · Pending image ${escapeHTML(server.desiredImage)}` : ''}` : `Image ${escapeHTML(server.desiredImage || 'Not recorded')}`;
    $('#modal-title').textContent = 'Edit server settings';
    $('#modal-submit').textContent = 'Confirm and apply settings';
    $('#modal-body').innerHTML = `<p>Stable ID <code>${escapeHTML(server.id)}</code> · Release <code>${escapeHTML(server.namespace)}/${escapeHTML(server.release)}</code></p><p>Stable identity and deployment settings are not editable here: Owner ID ${escapeHTML(server.ownerId || 'Not recorded')} · ${image} · Storage ${escapeHTML(server.storageGiB || 40)} GiB · Port ${escapeHTML(server.gamePort || 7777)} · Service ${escapeHTML(server.serviceType || 'LoadBalancer')}</p><p>Applying changes replaces the game pod and can disconnect active players. This is not hot reload. The existing PVC is retained. C2 performs no save-file rename or migration. Whether this game build renames or selects an existing save from the world name is unverified.</p><div class="form-grid">${[['name','Creator name','text',1,48],['worldName','World name','text',1,2048],['maxPlayers','Max players','number',1,64],['memoryLimitMiB','Memory limit (MiB)','number',256,67584],['cpuLimitMillis','CPU limit (millicores)','number',100,64000]].map(([key,label,type,min,max]) => `<label class="field">${label}<input name="${key}" data-testid="edit-${key}" type="${type}" ${type === 'number' ? `min="${min}" max="${max}" step="1"` : `maxlength="${max}"`} required value="${escapeHTML(state.modalInitialSettings[key])}"></label>`).join('')}</div><label class="field full"><span><input type="checkbox" name="confirmWorldName" value="true" data-testid="confirm-world-name"> I understand that C2 does not rename or migrate save files, and that this game build's world-name save behavior is unverified.</span></label><p id="edit-status" role="status" aria-live="polite">Confirm to request a rollout. Rollout readiness is not verified by this operation.</p>`;
  } else if (action === 'delete') {
    const server = state.servers.find((item) => item.id === state.modalServerId);
    const receipt = state.deletions[state.modalServerId];
    if (!server && !receipt) throw new Error('This server is no longer available. Refresh Maintenance.');
    $('#modal-title').textContent = receipt ? 'Retry server deletion?' : 'Delete this server?';
    $('#modal-submit').textContent = receipt ? 'Retry deletion' : 'Delete server, keep world';
    $('#modal-body').innerHTML = `<p><strong>${escapeHTML(server ? serverLabel(server) : receipt.worldLabel)}</strong><br>Stable ID <code>${escapeHTML(state.modalServerId)}</code></p><p>Active players will be disconnected. This removes the server from C2 after resource cleanup succeeds.</p><p>Keep world data by default. The world volume remains in namespace <strong>${escapeHTML(server?.namespace || receipt.namespace)}</strong>. The completion receipt records its exact name and UID. An operator can recover it using <code>persistence.existingClaim</code> in that namespace after confirming no other server uses it.</p>${receipt ? `<p>Recorded choice <strong>${escapeHTML(receipt.mode)}</strong>. It cannot change on retry.</p><input type="hidden" name="mode" value="${escapeHTML(receipt.mode)}">` : '<label class="field">World data<select id="delete-mode" name="mode" data-testid="delete-mode"><option value="keep">Keep world data</option><option value="purge">Permanently delete world data</option></select></label>'}<div id="purge-confirmation" ${receipt?.mode === 'purge' ? '' : 'hidden'}><p class="notice error">Permanent deletion removes the selected world volume. This cannot be undone through C2. Storage-provider snapshots or retained backing volumes may remain.</p><label class="field">Type DELETE WORLD ${escapeHTML(state.modalServerId)}<input name="purgeConfirm" data-testid="purge-confirm" autocomplete="off" ${receipt?.mode === 'purge' ? 'required' : 'disabled'}></label></div>`;
  } else if (action === 'add-server') {
    $('#modal-title').textContent = 'Create a server';
    $('#modal-submit').textContent = 'Deploy server';
    $('#modal-body').innerHTML = `<p>A new Dragonwilds world, deployed to your Kubernetes cluster.</p><div class="form-grid"><label class="field full">Server name<input name="worldName" data-testid="server-world-name" required maxlength="128" placeholder="My Dragonwilds server" autofocus></label><label class="field full">Owner name<input name="name" data-testid="server-name" required maxlength="48" placeholder="Owner display name" autocomplete="off"></label><label class="field full">Owner EOS player ID<input name="ownerId" data-testid="server-owner" required maxlength="128" autocomplete="off" placeholder="Your game account ID"></label><div class="field full"><label for="create-image-tag">Image tag</label><select id="create-image-tag" name="imageTag" data-testid="server-image" required disabled></select><small id="image-tags-status" role="status"></small><button type="button" data-action="retry-image-tags" hidden>Retry image tags</button></div><label class="field full">Max players<input name="maxPlayers" data-testid="server-max-players" type="number" min="1" max="64" value="4" required></label></div><details id="server-advanced"><summary>Advanced</summary><div class="form-grid"><label class="field">Namespace<input name="namespace" data-testid="server-namespace" required maxlength="63" pattern="[a-z0-9](([a-z0-9]|-)*[a-z0-9])?" value="dragonwilds" title="Lowercase letters, numbers, and hyphens; start and end with a letter or number"></label></div></details>`;
  } else if (action === 'stop' || action === 'start') {
    const server = selectedServer();
    $('#modal-title').textContent = action === 'stop' ? 'Stop this server?' : 'Start this server?';
    $('#modal-submit').textContent = action === 'stop' ? 'Confirm stop' : 'Confirm start';
    $('#modal-body').innerHTML = action === 'stop'
      ? `<p><strong>${escapeHTML(serverLabel(server))}</strong> will stop. Active players will be disconnected. World data and this inventory row stay. Start it later from Maintenance.</p>`
      : `<p><strong>${escapeHTML(serverLabel(server))}</strong> will start. Same world, same server ID, same disk.</p>`;
  } else {
    const server = selectedServer();
    $('#modal-title').textContent = action === 'restart' ? 'Restart this server?' : 'Update server image';
    $('#modal-submit').textContent = action === 'restart' ? 'Confirm restart' : 'Confirm update';
    $('#modal-body').innerHTML = `<p><strong>${escapeHTML(serverLabel(server))}</strong> will ${action === 'restart' ? 'restart' : 'restart with the selected image'}. Active players will be disconnected. Wait for a quiet moment before continuing.</p>${action === 'update' ? '<div class="field"><label for="update-image-tag">Container image tag</label><select id="update-image-tag" name="imageTag" data-testid="update-image-tag" required disabled></select><small id="image-tags-status" role="status"></small><button type="button" data-action="retry-image-tags" hidden>Retry image tags</button></div>' : ''}`;
  }
  if (action === 'add-server') {
    $('#server-advanced .form-grid').insertAdjacentHTML('beforeend', '<label class="field full">Custom save (optional)<input type="file" name="save" accept=".sav" data-testid="server-save" aria-describedby="save-help"><small id="save-help">Select one .sav file, up to 32 MiB. Leave empty to create a new world. The save is imported only into an empty world.</small></label>');
    $('#server-advanced .form-grid').insertAdjacentHTML('beforeend', `<div class="field"><label for="memoryLimitMiB">Memory limit (MiB)</label><div class="resource-controls"><input id="memoryLimitMiB" name="memoryLimitMiB" data-testid="server-memory-limit" type="number" min="256" max="67584" value="6144" required readonly><button type="button" data-action="toggle-resource" data-field="memoryLimitMiB" aria-controls="memoryLimitMiB" aria-pressed="true" aria-label="Lock memory to player count">Locked to player count</button></div><small>2 GiB plus 1 GiB per player. Exceeding this limit can restart the server.</small></div><div class="field"><label for="cpuLimitMillis">CPU limit (millicores)</label><div class="resource-controls"><input id="cpuLimitMillis" name="cpuLimitMillis" data-testid="server-cpu-limit" type="number" min="100" max="64000" value="2000" required readonly><button type="button" data-action="toggle-resource" data-field="cpuLimitMillis" aria-controls="cpuLimitMillis" aria-pressed="true" aria-label="Lock CPU to player count">Locked to player count</button></div><small>500 millicores per player. 1000 millicores = 1 CPU core.</small></div><p class="field full inline-note">Kubernetes reserves 256 MiB and 100 millicores per game container. Limits are ceilings, not guaranteed capacity. Player-limit enforcement depends on the game build.</p>`);
    $('[data-testid="server-owner"]').pattern = PATTERN.eosId;
    $('[data-testid="server-owner"]').title = 'Exactly 32 hexadecimal characters, without spaces or separators';
    $('[data-testid="server-owner"]').parentElement.insertAdjacentHTML('beforebegin', `<label class="field full">Saved player ID<select id="saved-user" data-testid="saved-user"><option value="">Enter an ID manually</option>${state.users.map((user) => `<option value="${escapeHTML(user.id)}">${escapeHTML(user.name)} (${escapeHTML(user.playerId)})</option>`).join('')}</select><small>Select a saved ID to copy it into the editable owner ID field.</small></label>`);
    $('#server-advanced .form-grid').insertAdjacentHTML('beforeend', `<label class="field">Game UDP port<input name="gamePort" type="number" min="1024" max="65535" value="7777" required></label><label class="field">World storage (GiB)<input name="storageGiB" type="number" min="1" max="2048" value="40" required></label>${serviceTypeField()}<label class="field">Server password<input name="serverPassword" type="password" maxlength="2048" autocomplete="new-password"><small>Optional. Empty allows passwordless joins.</small></label><label class="field">Admin password<input name="adminPassword" type="password" maxlength="2048" autocomplete="new-password"><small>Optional. Stored in a Kubernetes Secret.</small></label>${adminIdsFields(state.users, [])}<label class="field">Logging<select name="debugLevel"><option value="0">Normal</option><option value="1">SteamCMD debug</option><option value="2">Game debug</option><option value="3">SteamCMD and game debug</option></select></label><label class="field">Validate game files<select name="validateGameFiles"><option value="false">No</option><option value="true">Yes (slower startup)</option></select></label><label class="field full">Stop on game update<select name="autoStopOnUpdate"><option value="false">Disabled</option><option value="true">Enabled (game-build dependent)</option></select></label><label class="field full">Additional startup arguments<input name="additionalArgs" maxlength="2048" placeholder="Optional Unreal startup arguments"><small>The player-count override is appended automatically. API authentication is configured automatically.</small></label>`);
  }
  $('#modal').showModal();
  if (action === 'add-reboot' || action === 'edit-reboot') $('#modal-form input[name="enabled"]')?.setAttribute('id', 'enabled');
  if (action === 'add-server' || action === 'update') loadImageTags();
  if (action === 'add-integration' || action === 'edit-integration') {
    $('#modal-title').tabIndex = -1;
    $('#modal-title').focus();
  } else if (action === 'edit-settings') $('[data-testid="edit-name"]').focus();
  else if (action === 'add-user' || action === 'edit-user') $('[data-testid="user-name"]').focus();
  else if (action === 'add-reboot' || action === 'edit-reboot') { syncRebootModeFields(); updateDailyTimeControls(); $('[data-testid="reboot-server"]')?.focus(); }
  else if (action !== 'add-server') $('[data-testid="cancel-modal"]').focus();
}
async function loadImageTags() {
  const select = $('#modal [name="imageTag"]');
  const status = $('#image-tags-status');
  const retry = $('[data-action="retry-image-tags"]');
  const current = state.modalAction === 'update' ? (selectedServer()?.currentImage || selectedServer()?.desiredImage || '').split(':').pop() : '';
  select.disabled = true;
  $('#modal-submit').disabled = true;
  status.textContent = 'Loading published image tags…';
  retry.hidden = true;
  const epoch = state.epoch;
  try {
    const tags = await api('/api/image-tags');
    if (!select.isConnected || !$('#modal').open || epoch !== state.epoch) return;
    if (!tags.length && !current) throw new Error('No published image releases are available.');
    select.innerHTML = tags.map((tag) => `<option value="${escapeHTML(tag)}">${escapeHTML(tag)}</option>`).join('');
    if (current && !tags.includes(current)) select.insertAdjacentHTML('beforeend', `<option value="${escapeHTML(current)}">${escapeHTML(current)} (current, unavailable)</option>`);
    if (current) select.value = current;
    select.disabled = false;
    $('#modal-submit').disabled = false;
    status.textContent = current && !tags.includes(current) ? 'The current tag is no longer published. Choose a release to change it.' : 'Published image releases, newest first.';
  } catch (error) {
    if (!select.isConnected || !$('#modal').open || epoch !== state.epoch) return;
    status.textContent = error.message;
    retry.hidden = false;
  }
}
function updateResources() {
  const players = Number($('#modal [name="maxPlayers"]').value);
  if (!Number.isInteger(players) || players < 1 || players > 64) return;
  for (const [name, value] of [['memoryLimitMiB', (2 + players) * 1024], ['cpuLimitMillis', 500 * players]]) {
    const input = $(`[name="${name}"]`);
    if (input.readOnly) input.value = value;
  }
}
function closeModal() {
  if (state.modalBusy) return;
  $('#modal').close();
  state.modalInitialSettings = {};
  modalOpener?.focus();
}
function createRequestBody(values) {
  const {save, ...settings} = values;
  if (!save?.name) return JSON.stringify(settings);
  if (!save.name.endsWith('.sav') || save.name.startsWith('.') || save.name.startsWith('-') || /[/\\\x00-\x1f\x7f]/.test(save.name)) throw new Error('Select a .sav file with a plain filename without a leading hyphen.');
  if (save.size === 0) throw new Error('Save file must not be empty.');
  if (save.size > 32 * 1024 * 1024) throw new Error('Save file must be at most 32 MiB.');
  const body = new FormData();
  body.append('request', JSON.stringify(settings));
  body.append('save', save);
  return body;
}
async function submitModal(event) {
  event.preventDefault();
  if (['add-integration','edit-integration'].includes(state.modalAction)) {
    const invalid = Array.from($('#modal-form').elements).filter((input) => input.willValidate && !input.validity.valid);
    if (invalid.length) { showIntegrationErrors(Object.fromEntries(invalid.map((input) => [input.name, input.validationMessage]))); return; }
  }
  if (!can(state.modalAction === 'add-server' ? 'create' : state.modalAction) || state.modalBusy || !$('#modal-form').reportValidity()) return;
  const epoch = state.epoch;
  const values = Object.fromEntries(new FormData($('#modal-form')));
  const action = state.modalAction;
  if (action === 'add-reboot' || action === 'edit-reboot') clearRebootErrors();
  let body, path, method = 'POST', message;
  switch (action) {
    case 'add-reboot': case 'edit-reboot': {
      body = rebootRequestFromForm();
      path = action === 'add-reboot' ? '/api/reboots' : `/api/reboots/${encodeURIComponent(state.modalRebootId)}`;
      method = action === 'add-reboot' ? 'POST' : 'PUT';
      message = 'Reboot schedule saved.';
      break;
    }
    case 'delete-reboot':
      path = `/api/reboots/${encodeURIComponent(state.modalRebootId)}`;
      method = 'DELETE';
      message = 'Reboot schedule deleted.';
      break;
    case 'edit-settings':
      body = editSettingsPatch(state.modalInitialSettings, values);
      if (!body) { closeModal(); notice('No settings changed. No rollout requested.'); return; }
      if (body.worldName !== undefined && !body.confirmWorldName) {
        $('#modal-error').textContent = 'Acknowledge the world-name save warning before applying this change.';
        $('#modal-error').hidden = false;
        $('[data-testid="confirm-world-name"]').focus();
        return;
      }
      path = `/api/servers/${encodeURIComponent(state.modalServerId)}/actions/edit-settings`;
      message = 'Settings apply requested. Rollout is in progress; readiness is not yet verified.';
      $('#edit-status').textContent = 'Applying settings. This request cannot be cancelled; wait for the result.';
      break;
    case 'delete':
      body = {confirm:state.modalServerId, mode:values.mode, ...(values.mode === 'purge' ? {purgeConfirm:values.purgeConfirm} : {})};
      path = `/api/servers/${encodeURIComponent(state.modalServerId)}`;
      method = 'DELETE';
      message = 'Server deleted. Its storage receipt is in Maintenance.';
      break;
    case 'add-integration': case 'edit-integration': {
      const fields = new FormData($('#modal-form'));
      body = {name:values.name,enabled:values.enabled === 'true',provider:values.provider || 'discord',...(values.provider === 'webhook' ? {webhookUrl:values.webhookUrl} : {guildId:values.guildId,channelId:values.channelId}),quietHours:values.quietEnabled === 'on' ? {start:values.quietStart,end:values.quietEnd,timezone:values.quietTimezone} : null,secretRef:{name:values.secretName,key:values.secretKey},serverIds:fields.getAll('integrationServer'),rules:Object.fromEntries(fields.getAll('integrationRule').map((kind) => [kind,true]))};
      path = action === 'add-integration' ? '/api/integrations' : `/api/integrations/${encodeURIComponent(state.modalIntegrationId)}`;
      method = action === 'add-integration' ? 'POST' : 'PUT';
      message = 'Integration saved.';
      break;
    }
    case 'add-user': case 'edit-user':
      body = {name:values.name, playerId:values.playerId};
      path = action === 'add-user' ? '/api/users' : `/api/users/${encodeURIComponent(state.modalUserId)}`;
      method = action === 'add-user' ? 'POST' : 'PUT';
      message = 'Player ID saved.';
      break;
    case 'delete-user':
      path = `/api/users/${encodeURIComponent(state.modalUserId)}`;
      method = 'DELETE';
      message = 'Saved ID deleted. Existing servers are unchanged.';
      break;
    case 'add-server':
      body = createSettingsFromForm(new FormData($('#modal-form')));
      path = '/api/servers';
      message = 'Server deployment requested. C2 is waiting for the game to become ready.';
      break;
    default:
      body = action === 'update' ? {imageTag:values.imageTag} : {};
      path = `/api/servers/${encodeURIComponent(state.modalServerId)}/actions/${action}`;
      message = action === 'restart' ? 'Server restart requested.' : action === 'stop' ? 'Server stop requested.' : action === 'start' ? 'Server start requested.' : 'Image update requested.';
  }
  state.modalBusy = true;
  $('#modal-form').setAttribute('aria-busy','true');
  $('#modal-submit').disabled = true;
  $('#modal-error').hidden = true;
  $('#modal').querySelectorAll('[data-action="close-modal"]').forEach((button) => { button.disabled = true; });
  try {
    const result = await api(path,{method,body:action === 'add-server' ? createRequestBody(body) : body ? JSON.stringify(body) : undefined});
    if (epoch !== state.epoch) return;
    if (action === 'add-server') {
      const created = rememberCreatedServer(result);
      beginDeployWatch(created);
      state.serverId = created.id;
      state.modalBusy = false;
      closeModal();
      notice(message);
      if (parseLocationHash(location.hash).page === 'maintenance') await refresh();
      else location.hash = 'maintenance';
      return;
    }
    state.modalBusy = false;
    closeModal();
    notice(action === 'delete' && !result.completed ? 'Deletion started. You can leave this page.' : message);
    await refresh();
    if (action === 'edit-settings') $('[data-testid="edit-settings"]')?.focus();
  } catch (error) {
    if (epoch !== state.epoch || error.name === 'AbortError') return;
    if (action === 'delete') await refresh();
    $('#modal-error').textContent = error.message;
    $('#modal-error').hidden = false;
    if (action === 'edit-settings') $('#edit-status').textContent = 'Apply failed. Review the error, inspect the release if needed, then retry or cancel.';
    let focusedSummary = false;
    if (['add-integration','edit-integration'].includes(action)) {
      showIntegrationErrors(error.fields && Object.keys(error.fields).length ? error.fields : {form:error.message});
      $('#modal-error').hidden = true;
      focusedSummary = true;
    }
    if ((action === 'add-reboot' || action === 'edit-reboot') && error.fields && Object.keys(error.fields).length) {
      showRebootErrors(error.fields);
      $('#modal-error').hidden = true;
      focusedSummary = true;
    }
    if (!focusedSummary) $('#modal-error').focus();
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
  const permission = {'add-server':'create', 'edit-settings':'maintenance', restart:'restart', stop:'stop', start:'start', update:'update', 'check-update':'updateCheck', 'view-events':'events', 'event-category':'events', 'select-event':'events', 'copy-event':'events', 'export-events':'events', 'load-older-events':'events', 'export-telemetry':'telemetry', 'export-logs':'logs', 'refresh-logs':'logs', 'add-reboot':'reboots', 'edit-reboot':'reboots', 'delete-reboot':'reboots', 'add-daily-time':'reboots', 'remove-daily-time':'reboots', 'preview-reboot':'reboots'}[action];
  if (permission && !can(permission)) return;
  try {
    switch (action) {
      case 'login': if (state.authMode === 'oidc') location.assign('/api/auth/login'); else requireLogin(); break;
      case 'logout': await logout(); break;
      case 'close-login': closeLogin(); break;
      case 'retry-image-tags': loadImageTags(); break;
      case 'toggle-resource': {
        const input = $(`[name="${button.dataset.field}"]`);
        input.readOnly = !input.readOnly;
        button.setAttribute('aria-pressed', String(input.readOnly));
        button.textContent = input.readOnly ? 'Locked to player count' : 'Manual override';
        updateResources();
        break;
      }
      case 'add-server': case 'restart': case 'stop': case 'start': case 'update': case 'edit-settings': openModal(action); break;
      case 'delete': openModal(action, button.dataset.id); break;
      case 'add-reboot': case 'edit-reboot': case 'delete-reboot': openModal(action, button.dataset.id || ''); break;
      case 'add-daily-time': addDailyTime(); break;
      case 'remove-daily-time': removeDailyTime(button); break;
      case 'preview-reboot': await previewReboot(); break;
      case 'add-user': case 'edit-user': case 'delete-user': openModal(action, button.dataset.id); break;
      case 'add-integration': case 'edit-integration': openModal(action, button.dataset.id); break;
      case 'test-integration':
        if (!can('integrations')) return;
        button.disabled = true;
        await api(`/api/integrations/${encodeURIComponent(button.dataset.id)}/test`,{method:'POST',body:'{}'});
        await refresh();
        notice('Test queued. Check its delivery status below.');
        break;
      case 'close-modal': closeModal(); break;
      case 'retry': await refresh(); break;
      case 'fleet-filter': state.fleetFilter = button.dataset.value; render(); break;
      case 'view-server': location.hash = `servers/${encodeURIComponent(button.dataset.id)}`; break;
      case 'view-events': location.hash = scopedHash('events'); break;
      case 'copy-endpoint':
        if (!can('maintenance') || !selectedServer()?.endpoint) return;
        await navigator.clipboard.writeText(selectedServer().endpoint);
        notice('Endpoint copied.'); break;
      case 'event-category': state.category = button.dataset.value; state.selectedEventId = ''; await refresh(); break;
      case 'select-event': state.selectedEventId = button.dataset.id; render(); break;
      case 'copy-event': {
        const selected = state.events.find((item) => item.id === state.selectedEventId);
        await navigator.clipboard.writeText(JSON.stringify(selected,null,2)); notice('Event JSON copied.'); break;
      }
      case 'load-older-events': {
        const result = await api(`/api/events?${eventListParams({limit:100, offset:state.events.length})}`);
        acceptEvents(result, true);
        render();
        break;
      }
      case 'export-events': {
        const result = await api(`/api/events?${eventListParams({limit:2500, offset:0})}`);
        exportCSV('rsdw-events.csv', eventArray(result), ['timestamp','serverName','category','severity','message','details']);
        break;
      }
      case 'export-telemetry': exportCSV('rsdw-telemetry.csv',telemetryRows(),['timestamp','metric','value','observedAt','status','reason','unit','source']); break;
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
  } catch (error) { if (error.name !== 'AbortError') notice(error.message || 'The action could not be completed.'); }
  finally { if (button.isConnected) button.disabled = false; }
}
function navigate() {
  window.scrollTo(0, 0);
  const parsed = parseLocationHash(location.hash);
  state.page = pages[parsed.page] && (!state.identity || can(parsed.page)) ? parsed.page : 'dashboard';
  state.integrationView = state.page === 'integrations' ? parsed.integrationView : 'hub';
  state.routeError = '';
  applyRouteScope();
  const [title, description] = pageHeading();
  $('#navigation').innerHTML = navHTML();
  $('#page-title').textContent = title;
  $('#page-description').textContent = description;
  clearTelemetry();
  state.logs = '';
  if (state.loaded) render();
  refresh();
}
function handleChange(event) {
  if (['integration-provider','integration-quiet-enabled'].includes(event.target.id)) syncIntegrationFields();
  if (event.target.id === 'delete-mode') {
    const purge = event.target.value === 'purge';
    $('#purge-confirmation').hidden = !purge;
    $('[data-testid="purge-confirm"]').disabled = !purge;
    $('[data-testid="purge-confirm"]').required = purge;
    $('#modal-submit').textContent = purge ? 'Permanently delete server and world' : 'Delete server, keep world';
  }
  if (event.target.id === 'saved-user' && can('create')) {
    const user = state.users.find((item) => item.id === event.target.value);
    if (user) $('[data-testid="server-owner"]').value = user.playerId;
  }
  if (event.target.id === 'telemetry-range') { state.range = event.target.value; clearTelemetry(); render(); refresh(); }
  if (event.target.id === 'event-range') { state.eventRange = event.target.value; state.selectedEventId = ''; render(); refresh(); }
  if (event.target.id === 'reboot-mode') { syncRebootModeFields(); updateDailyTimeControls(); }
  if (event.target.id === 'display-timezone') { saveDisplayTimezone(event.target.value); render(); }
  if (event.target.id === 'server-filter') {
    state.serverId = event.target.value; clearTelemetry(); state.logs = ''; state.selectedEventId = '';
    if (['telemetry','maintenance','events','reboots'].includes(state.page)) {
      state.routeScope = Boolean(state.serverId);
      history.replaceState(null, '', scopedHash(state.page));
    }
    render(); refresh();
  }
}
$('#refresh').innerHTML = icon('refresh');
$('#refresh').addEventListener('click',refresh);
$('#pause').addEventListener('click',() => { state.paused = !state.paused; connection(); if (!state.paused) refresh(); });
$('#modal-form').addEventListener('invalid', (event) => {
  const advanced = event.target.closest('details');
  if (advanced) advanced.open = true;
}, true);
$('#modal-form').addEventListener('submit',submitModal);
$('#login-form').addEventListener('submit',submitLogin);
$('#login-dialog').addEventListener('cancel',(event) => { event.preventDefault(); closeLogin(); });
$('#modal').addEventListener('cancel',(event) => { event.preventDefault(); closeModal(); });
document.addEventListener('click',handleAction);
document.addEventListener('input',(event) => {
  if (event.target.name === 'maxPlayers') updateResources();
  if (event.target.name === 'ownerId') $('#saved-user').value = '';
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
document.addEventListener('change',handleChange);
window.addEventListener('hashchange',navigate);
document.addEventListener('visibilitychange',() => {
  if (document.hidden) return;
  if (['telemetry','servers'].includes(state.page) && state.loaded) render();
  if (!state.paused && !$('#modal').open) refresh();
});
setInterval(() => {
  if (state.paused || state.authRequired || state.refreshing || document.hidden || $('#modal').open || $('#login-dialog').open || ['INPUT','SELECT'].includes(document.activeElement.tagName)) return;
  refresh();
},10000);
if (new URLSearchParams(location.search).get('signin') === 'failed') {
  state.signInFailed = true;
  history.replaceState(null, '', location.pathname + location.hash);
}
navigate();
