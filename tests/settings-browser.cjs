const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const {chromium} = require('playwright');

const root = path.resolve(__dirname, '..');
fs.mkdirSync(path.join(root, '.tmp'), {recursive:true});
const output = fs.mkdtempSync(path.join(root, '.tmp/settings-browser-'));
const stateFile = path.join(output, 'state.json');
const token = 'settings-browser-test-token';
const ownerID = '0123456789abcdef0123456789abcdef';
const server = spawn(path.join(root, '.tmp/rsdw-c2'), [], {
  cwd:root,
  env:{...process.env, RSDW_AUTH_MODE:'token', RSDW_DEMO_DATA:'true', RSDW_ADMIN_TOKEN:token, RSDW_STATE_FILE:stateFile, RSDW_LISTEN_ADDR:'127.0.0.1:0'},
  stdio:['ignore','pipe','pipe'],
});
const exited = once(server, 'exit');
let browser;
let logs = '';

async function run() {
  const base = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('Demo server did not start')), 15000);
    server.on('error', reject);
    server.on('exit', (code) => { clearTimeout(timeout); reject(new Error(`Demo server exited with ${code}`)); });
    server.stderr.on('data', (chunk) => {
      logs += chunk;
      const match = logs.match(/listening on (127\.0\.0\.1:\d+)/);
      if (match) { clearTimeout(timeout); resolve(`http://${match[1]}`); }
    });
  });
  browser = await chromium.launch({headless:true, executablePath:process.env.RSDW_TEST_CHROMIUM});
  const page = await browser.newPage({viewport:{width:1440, height:1000}});
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const saved = () => JSON.parse(fs.readFileSync(stateFile, 'utf8'));
  const createServer = async (worldName) => page.evaluate(async ({token, worldName, ownerID}) => {
    const response = await fetch('/api/servers', {method:'POST', headers:{Authorization:`Bearer ${token}`, 'Content-Type':'application/json'}, body:JSON.stringify({name:'PETZKO', worldName, ownerId:ownerID, maxPlayers:6, memoryLimitMiB:1536, cpuLimitMillis:750})});
    return {status:response.status, data:await response.json()};
  }, {token, worldName, ownerID});

  await page.goto(base + '/#maintenance');
  await page.getByTestId('open-login').click();
  await page.getByTestId('admin-token').fill(token);
  await page.getByTestId('login-submit').click();
  await page.getByTestId('nav-maintenance').waitFor();

  const first = await createServer('PC2-US-EAST-02');
  const second = await createServer('PC2-US-EAST-03');
  assert.equal(first.status, 201, JSON.stringify(first.data));
  assert.equal(second.status, 201, JSON.stringify(second.data));
  const target = first.data;
  const neighbor = second.data;
  await page.reload();
  await page.getByTestId('nav-maintenance').waitFor();
  const select = page.getByTestId('server-filter');
  await select.selectOption(target.id);
  await page.getByTestId('edit-settings').waitFor();

  let editRequests = 0;
  page.on('request', (request) => {
    if (request.method() === 'POST' && request.url().endsWith(`/api/servers/${encodeURIComponent(target.id)}/actions/edit-settings`)) editRequests++;
  });

  await page.getByTestId('edit-settings').click();
  await page.getByTestId('edit-name').waitFor();
  assert.equal(await page.getByTestId('edit-name').inputValue(), 'PETZKO');
  assert.equal(await page.getByTestId('edit-worldName').inputValue(), 'PC2-US-EAST-02');
  assert.equal(await page.getByTestId('edit-maxPlayers').inputValue(), '6');
  assert.equal(await page.getByTestId('edit-memoryLimitMiB').inputValue(), '1536');
  assert.equal(await page.getByTestId('edit-cpuLimitMillis').inputValue(), '750');
  assert.equal(await page.getByTestId('edit-name').evaluate((element) => element === document.activeElement), true);
  const warning = await page.locator('#modal-body').innerText();
  assert.match(warning, /not editable here/);
  assert.match(warning, /disconnect active players/);
  assert.match(warning, /no save-file rename or migration/);
  assert.match(warning, /unverified/);
  await page.getByTestId('confirm-modal').click();
  await page.locator('#modal').waitFor({state:'hidden'});
  assert.equal(editRequests, 0);

  await page.getByTestId('edit-settings').click();
  await page.getByTestId('edit-worldName').fill('Unconfirmed world');
  await page.getByTestId('confirm-modal').click();
  assert.equal(await page.locator('#modal').isVisible(), true);
  assert.match(await page.locator('#modal-error').innerText(), /Acknowledge the world-name save warning/);
  assert.equal(await page.getByTestId('confirm-world-name').evaluate((element) => element === document.activeElement), true);
  assert.equal(editRequests, 0);
  await page.getByTestId('cancel-modal').click();

  await page.getByTestId('edit-settings').click();
  await page.getByTestId('edit-name').fill('PETZKO East');
  await page.getByTestId('edit-worldName').fill('PC2-US-EAST-02-EDITED');
  await page.getByTestId('confirm-world-name').check();
  await page.getByTestId('edit-maxPlayers').fill('12');
  await page.getByTestId('edit-memoryLimitMiB').fill('3072');
  await page.getByTestId('edit-cpuLimitMillis').fill('1250');
  const applyResponse = page.waitForResponse((response) => response.url() === `${base}/api/servers/${encodeURIComponent(target.id)}/actions/edit-settings` && response.request().method() === 'POST');
  await page.getByTestId('confirm-modal').click();
  const response = await applyResponse;
  assert.equal(response.status(), 200, await response.text());
  await page.locator('#modal').waitFor({state:'hidden'});
  await page.getByTestId('edit-settings').waitFor();
  await page.waitForFunction(() => document.querySelector('#server-filter option:checked')?.textContent === 'PC2-US-EAST-02-EDITED');
  assert.equal(await page.getByTestId('edit-settings').evaluate((element) => element === document.activeElement), true);
  const after = saved();
  assert.equal(after.servers[target.id].name, 'PETZKO East');
  assert.equal(after.servers[target.id].worldName, 'PC2-US-EAST-02-EDITED');
  assert.equal(after.servers[target.id].maxPlayers, 12);
  assert.equal(after.servers[target.id].memoryLimitMiB, 3072);
  assert.equal(after.servers[target.id].cpuLimitMillis, 1250);
  assert.equal(after.servers[target.id].ownerId, ownerID);
  assert.equal(after.servers[target.id].release, target.release);
  assert.equal(after.servers[neighbor.id].name, 'PETZKO');
  assert.equal(after.servers[neighbor.id].worldName, 'PC2-US-EAST-03');
  assert.equal(await select.locator('option:checked').innerText(), 'PC2-US-EAST-02-EDITED');

  const unchanged = JSON.stringify(saved().servers[target.id]);
  await page.getByTestId('edit-settings').click();
  await page.getByTestId('edit-worldName').fill('Cancelled world');
  await page.getByTestId('cancel-modal').click();
  assert.equal(JSON.stringify(saved().servers[target.id]), unchanged);

  await page.getByTestId('edit-settings').click();
  await page.getByTestId('edit-maxPlayers').fill('0');
  await page.getByTestId('confirm-modal').click();
  assert.equal(await page.locator('#modal').isVisible(), true);
  assert.equal(editRequests, 1);
  await page.getByTestId('cancel-modal').click();

  await page.getByTestId('edit-settings').click();
  await page.getByTestId('edit-name').fill('Retry creator');
  await page.route(`**/api/servers/${encodeURIComponent(target.id)}/actions/edit-settings`, (route) => route.fulfill({status:502, contentType:'application/json', body:JSON.stringify({error:'Settings apply failed; retry after checking the release.'})}));
  await page.getByTestId('confirm-modal').click();
  await page.locator('#modal-error').waitFor({state:'visible'});
  assert.match(await page.locator('#modal-error').innerText(), /retry after checking the release/);
  assert.match(await page.locator('#edit-status').innerText(), /Apply failed/);
  assert.equal(await page.getByTestId('edit-name').inputValue(), 'Retry creator');
  assert.equal(await page.getByTestId('confirm-modal').isEnabled(), true);
  await page.unroute(`**/api/servers/${encodeURIComponent(target.id)}/actions/edit-settings`);
  const retryResponse = page.waitForResponse((item) => item.url() === `${base}/api/servers/${encodeURIComponent(target.id)}/actions/edit-settings` && item.request().method() === 'POST');
  await page.getByTestId('confirm-modal').click();
  assert.equal((await retryResponse).status(), 200);
  await page.locator('#modal').waitFor({state:'hidden'});
  assert.equal(saved().servers[target.id].name, 'Retry creator');

  assert.deepEqual(Object.keys(saved().servers).sort(), ['scuffedtards', target.id, neighbor.id].sort());
  assert.deepEqual(errors, []);
  console.log('PASS settings prepopulation, five-field edit, immutable owner/release preservation, in-place labels, no-op/cancel, validation, failed apply retry, warnings, stable-ID targeting, and dialog focus.');
  console.log(`Browser evidence: ${output}`);
}

run().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  server.kill();
  await exited;
});
