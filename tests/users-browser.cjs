const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const {chromium} = require('playwright');

const root = path.resolve(__dirname, '..');
fs.mkdirSync(path.join(root, '.tmp'), {recursive:true});
const output = fs.mkdtempSync(path.join(root, '.tmp/users-browser-'));
const stateFile = path.join(output, 'state.json');
const token = 'users-browser-test-token';
const playerID = '0123456789abcdef0123456789abcdef';
const changedID = '11111111111111111111111111111111';
const manualID = 'abcdef0123456789abcdef0123456789';
const server = spawn(path.join(root, '.tmp/rsdw-c2'), [], {
  cwd:root,
  env:{...process.env, RSDW_AUTH_MODE:'token', RSDW_DEMO_DATA:'true', RSDW_ADMIN_TOKEN:token, RSDW_STATE_FILE:stateFile, RSDW_LISTEN_ADDR:'127.0.0.1:0'},
  stdio:['ignore','pipe','pipe'],
});
const exited = once(server, 'exit');
let logs = '';
let browser;

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
  await page.route('**/api/image-tags', (route) => route.fulfill({json:['0.2.0','0.1.1','0.1.0']}));
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const saved = () => JSON.parse(fs.readFileSync(stateFile, 'utf8'));
  const api = async (method, endpoint, body) => {
    const response = await fetch(base + endpoint, {method, headers:{Authorization:`Bearer ${token}`, 'Content-Type':'application/json'}, body:body ? JSON.stringify(body) : undefined});
    return {status:response.status, data:response.status === 204 ? null : await response.json()};
  };
  const submit = async (method, endpoint, status) => {
    const pending = page.waitForResponse((response) => response.url() === base + endpoint && response.request().method() === method);
    await page.getByTestId('confirm-modal').click();
    const response = await pending;
    assert.equal(response.status(), status, await response.text());
    if (status < 300) await page.locator('#modal').waitFor({state:'hidden'});
    else if (status === 401) await page.locator('#login-dialog').waitFor({state:'visible'});
    else await page.locator('#modal-error').waitFor({state:'visible'});
    return status === 204 ? null : response.json();
  };
  const signIn = async () => {
    await page.getByTestId('admin-token').fill(token);
    await page.getByTestId('login-submit').click();
    await page.locator('#login-dialog').waitFor({state:'hidden'});
    await page.getByTestId('add-user').waitFor();
  };

  await page.goto(base + '/#users');
  await page.getByTestId('open-login').click();
  await page.getByTestId('admin-token').waitFor();
  assert.match(await page.locator('#content').innerText(), /Sign in required/);
  assert.equal((await fetch(base + '/api/users')).status, 401);
  await signIn();
  assert.match(await page.locator('#content').innerText(), /No player IDs saved yet/);
  await page.getByTestId('add-user').click();
  await page.getByTestId('user-name').fill('<Alice & friends>');
  for (const value of ['', 'not-an-id', '01234567-89ab-cdef-0123-456789ab', ' 0123456789abcdef0123456789abcde', playerID + 'f', playerID + ' ']) {
    await page.getByTestId('user-player-id').fill(value);
    assert.equal(await page.getByTestId('user-player-id').evaluate((input) => input.checkValidity()), false);
  }
  await page.getByTestId('user-player-id').fill(playerID.toUpperCase());
  const user = await submit('POST', '/api/users', 201);
  assert.ok(user.id);
  assert.equal(user.name, '<Alice & friends>');
  assert.equal(user.playerId, playerID);
  assert.deepEqual(saved().users[user.id], user);
  await page.getByTestId('edit-user').waitFor();
  assert.match(await page.locator('#content').innerText(), /<Alice & friends>/);
  assert.equal(await page.locator('#content alice').count(), 0);

  await page.getByTestId('add-user').click();
  await page.getByTestId('user-name').fill('Duplicate');
  await page.getByTestId('user-player-id').fill(playerID.toUpperCase());
  await submit('POST', '/api/users', 409);
  assert.equal(await page.locator('#modal-error').innerText(), 'this player ID is already saved');
  await page.getByTestId('cancel-modal').click();

  await page.getByTestId('nav-dashboard').click();
  await page.getByTestId('add-server').click();
  assert.deepEqual(await page.locator('#modal-body > .form-grid [name]').evaluateAll((inputs) => inputs.map((input) => input.name)), ['worldName','name','ownerId','imageTag','maxPlayers']);
  assert.equal(await page.locator('[name="region"]').count(), 0);
  assert.equal(await page.locator('#server-advanced').evaluate((details) => details.open), false);
  await page.getByTestId('server-image').selectOption('0.2.0');
  assert.deepEqual(await page.getByTestId('server-image').locator('option').evaluateAll((options) => options.map((option) => option.value)), ['0.2.0','0.1.1','0.1.0']);
  const players = page.getByTestId('server-max-players');
  const memory = page.getByLabel('Memory limit (MiB)', {exact:true});
  const cpu = page.getByLabel('CPU limit (millicores)', {exact:true});
  const lockMemory = page.getByRole('button', {name:'Lock memory to player count',exact:true});
  const lockCPU = page.getByRole('button', {name:'Lock CPU to player count',exact:true});
  await page.locator('#server-advanced summary').focus();
  await page.keyboard.press('Enter');
  assert.equal(await memory.isEditable(), false);
  for (const [count, mib, millis] of [[1,'3072','500'],[4,'6144','2000'],[64,'67584','32000']]) {
    await players.fill(String(count));
    assert.equal(await memory.inputValue(), mib);
    assert.equal(await cpu.inputValue(), millis);
  }
  for (const count of ['', '0', '65', '1.5']) {
    await players.fill(count);
    assert.equal(await memory.inputValue(), '67584');
    assert.equal(await cpu.inputValue(), '32000');
  }
  await players.fill('4');
  await lockMemory.click();
  await memory.fill('8192');
  await players.fill('6');
  assert.equal(await memory.inputValue(), '8192');
  assert.equal(await cpu.inputValue(), '3000');
  await lockCPU.click();
  await cpu.fill('2500');
  await players.fill('8');
  assert.equal(await memory.inputValue(), '8192');
  assert.equal(await cpu.inputValue(), '2500');
  await lockMemory.click();
  assert.equal(await memory.inputValue(), '10240');
  assert.equal(await lockMemory.getAttribute('aria-pressed'), 'true');
  await lockCPU.click();
  await players.fill('4');
  assert.equal(await cpu.inputValue(), '2000');
  await page.setViewportSize({width:390,height:844});
  assert.equal(await page.locator('#modal').evaluate((modal) => modal.scrollWidth <= modal.clientWidth), true);
  await page.screenshot({path:path.join(output, 'create-narrow.png'),fullPage:true});
  await page.setViewportSize({width:1440,height:1000});
  await page.locator('#server-advanced summary').click();
  await page.getByTestId('saved-user').selectOption(user.id);
  assert.equal(await page.getByTestId('server-owner').inputValue(), playerID);
  assert.equal(await page.getByTestId('server-owner').isEditable(), true);
  await page.getByTestId('server-name').fill('Saved owner world');
  await page.getByTestId('server-world-name').fill('Saved world');
  await page.locator('#server-advanced').evaluate((details) => { details.querySelector('[name="namespace"]').value = 'INVALID'; });
  await page.getByTestId('confirm-modal').click();
  assert.equal(await page.locator('#server-advanced').evaluate((details) => details.open), true);
  assert.equal(await page.getByTestId('server-namespace').evaluate((input) => input === document.activeElement), true);
  await page.getByTestId('server-namespace').fill('dragonwilds');
  const upload = page.getByTestId('server-save');
  assert.equal(await upload.getAttribute('accept'), '.sav');
  assert.match(await page.locator('#save-help').innerText(), /32 MiB/);
  for (const [name, buffer, message] of [
    ['world.zip', Buffer.from('not a save'), /Select a .sav file/],
    ['world.sav', Buffer.alloc(0), /must not be empty/],
    ['world.sav', Buffer.alloc(32 * 1024 * 1024 + 1), /at most 32 MiB/],
  ]) {
    await upload.setInputFiles({name, mimeType:'application/octet-stream', buffer});
    await page.getByTestId('confirm-modal').click();
    await page.locator('#modal-error').waitFor({state:'visible'});
    assert.match(await page.locator('#modal-error').innerText(), message);
    assert.equal(Object.values(saved().servers).some((item) => item.name === 'Saved owner world'), false);
    assert.equal(await page.getByTestId('confirm-modal').isEnabled(), true);
  }
  await upload.setInputFiles({name:'World.sav',mimeType:'application/octet-stream',buffer:Buffer.from('GVAS\0browser-save-fixture')});
  await submit('POST', '/api/servers', 503);
  assert.equal(await page.locator('#modal-error').innerText(), 'save upload requires Kubernetes storage');
  assert.equal(Object.values(saved().servers).some((item) => item.name === 'Saved owner world'), false);
  await page.screenshot({path:path.join(output, 'save-upload.png'),fullPage:true});
  await upload.setInputFiles([]);
  const created = await submit('POST', '/api/servers', 201);
  assert.equal(created.ownerId, playerID);
  assert.equal(created.worldName, 'Saved world');
  assert.equal(created.name, 'Saved owner world');
  assert.equal(created.memoryLimitMiB, 6144);
  assert.equal(created.cpuLimitMillis, 2000);
  assert.equal(saved().servers[created.id].memoryLimitMiB, 6144);
  assert.equal(saved().servers[created.id].cpuLimitMillis, 2000);
  assert.equal(created.currentImage.endsWith(':0.2.0'), true);
  assert.equal(saved().servers[created.id].ownerId, playerID);

  await page.getByTestId('add-server').click();
  await page.getByTestId('saved-user').selectOption(user.id);
  await page.getByTestId('server-owner').fill('bad-id');
  assert.equal(await page.getByTestId('saved-user').inputValue(), '');
  assert.equal(await page.getByTestId('server-owner').evaluate((input) => input.checkValidity()), false);
  await page.getByTestId('server-owner').fill(manualID + 'f');
  assert.equal(await page.getByTestId('server-owner').evaluate((input) => input.checkValidity()), false);
  await page.getByTestId('server-owner').fill(manualID.toUpperCase());
  await page.getByTestId('server-name').fill('Manual owner world');
  await page.getByTestId('server-world-name').fill('Manual world');
  const manual = await submit('POST', '/api/servers', 201);
  assert.equal(manual.ownerId, manualID);

  await page.getByTestId('nav-users').click();
  await page.getByTestId('edit-user').click();
  await page.getByTestId('user-name').fill('Renamed');
  await page.getByTestId('user-player-id').fill(changedID);
  const edited = await submit('PUT', `/api/users/${user.id}`, 200);
  assert.deepEqual(edited, {id:user.id, name:'Renamed', playerId:changedID});
  assert.deepEqual(saved().users[user.id], edited);
  assert.equal(saved().servers[created.id].ownerId, playerID);
  await page.reload();
  await page.getByTestId('edit-user').waitFor();
  assert.match(await page.locator('#content').innerText(), /Renamed/);
  await page.screenshot({path:path.join(output, 'saved-ids.png'), fullPage:true});

  await page.getByTestId('delete-user').click();
  await page.getByTestId('cancel-modal').click();
  assert.ok(saved().users[user.id]);
  await page.getByTestId('delete-user').click();
  await submit('DELETE', `/api/users/${user.id}`, 204);
  assert.deepEqual(saved().users, {});
  assert.equal(saved().servers[created.id].ownerId, playerID);
  assert.equal(saved().servers[manual.id].ownerId, manualID);
  assert.equal(saved().servers.scuffedtards.ownerId, 'demo-owner');

  await page.getByTestId('nav-dashboard').click();
  await page.getByTestId('add-server').click();
  await page.getByTestId('cancel-modal').click();
  await page.unroute('**/api/image-tags');
  await page.route('**/api/image-tags', (route) => route.fulfill({status:502,json:{error:'Registry unavailable'}}));
  await page.getByTestId('add-server').click();
  await page.getByRole('button', {name:'Retry image tags',exact:true}).waitFor();
  assert.equal(await page.getByTestId('confirm-modal').isEnabled(), false);
  assert.equal(await page.locator('#image-tags-status').innerText(), 'Registry unavailable');
  await page.unroute('**/api/image-tags');
  await page.route('**/api/image-tags', (route) => route.fulfill({json:['0.2.0','0.1.1','0.1.0']}));
  await page.getByRole('button', {name:'Retry image tags',exact:true}).click();
  await page.getByTestId('server-image').selectOption('0.1.1');
  await page.getByTestId('cancel-modal').click();
  await page.unroute('**/api/image-tags');
  let releaseTags;
  const delayedTags = new Promise((resolve) => { releaseTags = resolve; });
  let captureTags;
  const captured = new Promise((resolve) => { captureTags = resolve; });
  let firstTags = true;
  await page.route('**/api/image-tags', async (route) => {
    if (firstTags) {
      firstTags = false;
      captureTags();
      await delayedTags;
      await route.fulfill({json:['9.9.9']});
    } else await route.fulfill({json:['0.2.0','0.1.1','0.1.0']});
  });
  await page.getByTestId('add-server').click();
  await captured;
  await page.getByTestId('cancel-modal').click();
  await page.getByTestId('add-server').click();
  await page.getByTestId('server-image').selectOption('0.1.1');
  const staleResponse = page.waitForResponse('**/api/image-tags');
  releaseTags();
  await staleResponse;
  assert.equal(await page.getByTestId('server-image').inputValue(), '0.1.1');
  assert.equal(await page.getByTestId('saved-user').locator('option').count(), 1);
  await page.getByTestId('server-owner').fill(manualID);
  assert.equal(await page.getByTestId('server-owner').evaluate((input) => input.checkValidity()), true);
  await page.getByTestId('cancel-modal').click();

  await page.getByTestId('nav-maintenance').click();
  await page.locator('#server-filter').selectOption(created.id);
  await page.getByTestId('update-image').waitFor();
  await page.unroute('**/api/image-tags');
  await page.route('**/api/image-tags', (route) => route.fulfill({json:[]}));
  await page.getByTestId('update-image').click();
  await page.waitForFunction(() => document.querySelector('#image-tags-status').textContent !== 'Loading published image tags…');
  await page.getByTestId('update-image-tag').selectOption('0.2.0');
  assert.equal(await page.getByTestId('update-image-tag').inputValue(), '0.2.0');
  assert.match(await page.getByTestId('update-image-tag').innerText(), /current, unavailable/);
  await page.getByTestId('cancel-modal').click();
  await page.unroute('**/api/image-tags');
  await page.route('**/api/image-tags', (route) => route.fulfill({json:['0.2.0','0.1.1','0.1.0']}));
  await page.getByTestId('nav-users').click();
  await page.getByTestId('add-user').click();
  await page.getByTestId('user-name').fill('Retry');
  await page.getByTestId('user-player-id').fill(changedID);
  await page.route('**/api/users', (route) => route.fulfill({status:500, contentType:'application/json', body:'{"error":"could not persist saved IDs"}'}));
  await submit('POST', '/api/users', 500);
  assert.equal(await page.getByTestId('user-name').inputValue(), 'Retry');
  assert.equal(await page.getByTestId('confirm-modal').isEnabled(), true);
  await page.unroute('**/api/users');
  await page.evaluate(() => sessionStorage.setItem('rsdw-admin-token', 'expired-token'));
  await submit('POST', '/api/users', 401);
  await page.getByTestId('admin-token').waitFor();
  assert.equal(await page.locator('#modal').isVisible(), false);
  assert.match(await page.locator('#content').innerText(), /Sign in required/);
  await signIn();
  assert.deepEqual((await api('GET', '/api/users')).data, {users:[]});
  assert.deepEqual(errors, []);
  assert.ok(!logs.includes(playerID) && !logs.includes(changedID) && !logs.includes(manualID));
  console.log('PASS authenticated Saved IDs CRUD, save type/size errors, multipart upload rejection in demo mode, empty-world creation, selection, manual entry, persisted owner copies, cancel, retry, and expired-token handling.');
  console.log(`Browser evidence: ${output}`);
}

run().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  server.kill();
  await exited;
});
