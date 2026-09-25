const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const {chromium} = require('playwright');

const root = path.resolve(__dirname, '..');
fs.mkdirSync(path.join(root, '.tmp'), {recursive:true});
const runRoot = fs.mkdtempSync(path.join(root, '.tmp/backups-browser-'));
const stateFile = path.join(runRoot, 'state.json');
const screenshots = process.env.RSDW_BACKUP_SCREENSHOTS || path.join(runRoot, 'screenshots');
fs.mkdirSync(screenshots, {recursive:true});
const token = 'backups-browser-test-token';
const ownerID = '0123456789abcdef0123456789abcdef';
const server = spawn(path.join(root, '.tmp/rsdw-c2'), [], {
  cwd:root,
  env:{...process.env, RSDW_AUTH_MODE:'token', RSDW_DEMO_DATA:'true', RSDW_ADMIN_TOKEN:token, RSDW_STATE_FILE:stateFile, RSDW_LISTEN_ADDR:'127.0.0.1:0'},
  stdio:['ignore','pipe','pipe'],
});
const exited = once(server, 'exit');
let browser;
let page;
let logs = '';

async function start() {
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('Demo server did not start\n' + logs)), 15000);
    const ready = (chunk) => {
      logs += chunk;
      const match = logs.match(/listening on (127\.0\.0\.1:\d+)/);
      if (match) {
        clearTimeout(timeout);
        resolve('http://' + match[1]);
      }
    };
    server.on('error', reject);
    server.on('exit', (code) => {
      clearTimeout(timeout);
      reject(new Error('Demo server exited with ' + code + '\n' + logs));
    });
    server.stdout.on('data', ready);
    server.stderr.on('data', ready);
  });
}

async function run() {
  const base = await start();
  browser = await chromium.launch({headless:true, executablePath:process.env.RSDW_TEST_CHROMIUM});
  page = await browser.newPage({viewport:{width:1440, height:1000}});
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const api = async (method, endpoint, body) => page.evaluate(async ({token, method, endpoint, body}) => {
    const response = await fetch(endpoint, {
      method,
      headers:{Authorization:'Bearer ' + token, ...(body === undefined ? {} : {'Content-Type':'application/json'})},
      body:body === undefined ? undefined : JSON.stringify(body),
    });
    return {status:response.status, data:response.status === 204 ? null : await response.json()};
  }, {token, method, endpoint, body});
  const shot = async (name) => {
    await page.locator('#toast').waitFor({state:'hidden'});
    return page.screenshot({path:path.join(screenshots, name), fullPage:!await page.locator('#modal').isVisible()});
  };
  const submit = async (endpoint, method, expected) => {
    const pending = page.waitForResponse((response) => response.url() === base + endpoint && response.request().method() === method);
    await page.getByTestId('confirm-modal').click();
    const response = await pending;
    assert.equal(response.status(), expected, await response.text());
    if (expected < 300) await page.locator('#modal').waitFor({state:'hidden'});
    return response;
  };
  const validProfile = (id, name, itemCount) => ({
    apiVersion:'rsdw-c2.petzko.dev/v1',
    kind:'BackupDefinition',
    id,
    revision:1,
    name,
    serverType:'dragonwilds',
    strategy:'logical-files',
    items:Array.from({length:itemCount || 1}, (_, index) => ({
      name:index ? 'extra-' + index : 'world-save',
      kind:'file',
      requirement:'required',
      sources:[
        {serverState:'running', collector:'server-files', path:'RSDragonwilds/Saved/SaveGames'},
        {serverState:'stopped', collector:'server-files', path:'RSDragonwilds/Saved/SaveGames'},
      ],
    })),
  });

  await page.goto(base + '/#backups');
  await page.getByTestId('open-login').click();
  await page.getByTestId('admin-token').fill(token);
  await page.getByTestId('login-submit').click();
  await page.getByTestId('backup-health').waitFor();

  const created = await api('POST', '/api/servers', {name:'Issue 37 admin', worldName:'Issue 37 world', ownerId:ownerID, maxPlayers:4, imageTag:'0.1.1'});
  assert.equal(created.status, 201, JSON.stringify(created.data));
  const serverID = created.data.id;
  await page.reload();
  await page.getByTestId('backup-health').waitFor();

  await page.getByTestId('run-backup-header').click();
  await page.getByTestId('backup-run-server').selectOption(serverID);
  assert.equal(await page.getByTestId('backup-run-profile').inputValue(), 'dragonwilds-world-save');
  assert.equal(await page.locator('[name="runningPath"]').count(), 0);
  assert.equal(await page.locator('[name="stoppedPath"]').count(), 0);
  assert.match(await page.locator('#modal-body').innerText(), /Server-created \.sav\.backup/);
  await shot('06-run-backup-form.png');
  const firstRunResponse = await submit('/api/backups/runs', 'POST', 201);
  const firstRun = await firstRunResponse.json();
  await page.getByTestId('backup-details-' + firstRun.id).waitFor();

  const stopped = await api('POST', '/api/servers/' + encodeURIComponent(serverID) + '/actions/stop', {});
  assert.equal(stopped.status, 200, JSON.stringify(stopped.data));
  await page.reload();
  await page.getByTestId('backup-health').waitFor();
  await page.getByTestId('run-backup-header').click();
  await page.getByTestId('backup-run-server').selectOption(serverID);
  assert.match(await page.locator('#modal-body').innerText(), /Read flat \.sav/);
  await page.getByTestId('cancel-modal').click();
  await shot('01-backups-overview.png');

  await page.getByTestId('backups-settings').click();
  await page.getByTestId('backup-profile-list').waitFor();
  for (let index = 0; index < 23; index++) {
    const result = await api('POST', '/api/backups/profiles', validProfile('browser-profile-' + String(index).padStart(2, '0'), 'Browser profile ' + String(index).padStart(2, '0'), index % 4 === 0 ? 2 : 1));
    assert.equal(result.status, 200, JSON.stringify(result.data));
  }
  await page.reload();
  await page.getByTestId('backup-profile-list').waitFor();
  await shot('02-backup-profiles.png');
  await page.getByTestId('backup-profile-search').fill('Browser profile 22');
  assert.equal(await page.locator('[data-action="select-backup-profile"]').count(), 1);
  await page.getByTestId('clear-backup-profile-filter').click();

  await page.getByTestId('new-backup-profile').click();
  await page.getByTestId('backup-profile-name').fill('Browser multi-item profile');
  await page.getByTestId('add-backup-item').click();
  assert.equal(await page.locator('[data-profile-item-row]').count(), 2);
  assert.equal(await page.getByTestId('backup-profile-server-type').locator('option').count(), 1);
  await page.getByTestId('backup-item-name-1').fill('server-config');
  await page.getByTestId('backup-running-path-1').fill('RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini');
  await page.getByTestId('backup-stopped-path-1').fill('RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini');
  const previewYAML = await page.getByTestId('backup-yaml-preview').innerText();
  assert.match(previewYAML, /apiVersion: \"rsdw-c2\.petzko\.dev\/v1\"/);
  assert.match(previewYAML, /id: \"browser-multi-item-profile\"/);
  assert.match(previewYAML, /name: \"Browser multi-item profile\"/);
  assert.match(previewYAML, /serverType: \"dragonwilds\"/);
  assert.match(previewYAML, /sources:/);
  assert.match(previewYAML, /serverState: \"running\"/);
  assert.match(previewYAML, /serverState: \"stopped\"/);
  assert.match(previewYAML, /server-config/);
  const previewImport = await api('POST', '/api/backups/profiles/import', {yaml:previewYAML.replace('id: "browser-multi-item-profile"', 'id: "preview-import-profile"')});
  assert.equal(previewImport.status, 201, JSON.stringify(previewImport.data));
  await shot('15-backup-profile-items.png');
  await page.locator('#modal-body').evaluate((element) => { element.scrollTop = 0; });
  await shot('03-backup-profile-form.png');
  await page.locator('#modal-body').evaluate((element) => { element.scrollTop = element.scrollHeight; });
  await shot('16-backup-profile-yaml-preview.png');
  const multiProfileResponse = await submit('/api/backups/profiles', 'POST', 200);
  const multiProfile = await multiProfileResponse.json();
  await page.getByTestId('backup-profile-list').waitFor();
  await page.locator('[data-action="select-backup-profile"][data-id="' + multiProfile.id + '"]').click();
  await page.locator('[data-action="edit-backup-profile"][data-id="' + multiProfile.id + '"]').click();
  assert.equal(await page.locator('[data-profile-item-row]').count(), 2);
  await page.getByTestId('backup-profile-name').fill('Edited browser multi-item profile');
  const editedProfile = await submit('/api/backups/profiles/' + multiProfile.id, 'PUT', 200);
  assert.equal((await editedProfile.json()).revision, multiProfile.revision + 1);
  await page.locator('[data-action="select-backup-profile"][data-id="' + multiProfile.id + '"]').click();
  await page.locator('[data-action="delete-backup-profile"][data-id="' + multiProfile.id + '"]').click();
  await submit('/api/backups/profiles/' + multiProfile.id, 'DELETE', 204);

  await page.getByTestId('import-backup-profile').click();
  const importedYAML = [
    'apiVersion: rsdw-c2.petzko.dev/v1',
    'kind: BackupDefinition',
    'id: imported-browser-profile',
    'revision: 1',
    'name: Imported browser profile',
    'serverType: dragonwilds',
    'strategy: logical-files',
    'items:',
    '  - name: world-save',
    '    kind: file',
    '    requirement: required',
    '    sources:',
    '      - serverState: running',
    '        collector: server-files',
    '        path: RSDragonwilds/Saved/SaveGames',
    '      - serverState: stopped',
    '        collector: server-files',
    '        path: RSDragonwilds/Saved/SaveGames',
    '',
  ].join('\n');
  await page.getByTestId('backup-profile-yaml').setInputFiles({name:'imported.yaml', mimeType:'text/yaml', buffer:Buffer.from(importedYAML)});
  await submit('/api/backups/profiles/import', 'POST', 201);
  await page.getByTestId('backup-profile-list').waitFor();
  await page.locator('[data-action="select-backup-profile"][data-id="imported-browser-profile"]').click();
  const profileDownload = page.waitForEvent('download');
  await page.locator('[data-action="export-backup-profile"]').click();
  const exported = await profileDownload;
  assert.match(exported.suggestedFilename(), /imported-browser-profile\.yaml/);
  const exportedPath = await exported.path();
  assert.ok(exportedPath);
  const exportedYAML = fs.readFileSync(exportedPath, 'utf8');
  assert.match(exportedYAML, /apiVersion: rsdw-c2\.petzko\.dev\/v1/);
  assert.match(exportedYAML, /sources:/);
  assert.match(exportedYAML, /serverState:\s+running/);
  assert.match(exportedYAML, /serverState:\s+stopped/);
  const exportedImport = await api('POST', '/api/backups/profiles/import', {yaml:exportedYAML});
  assert.equal(exportedImport.status, 409, JSON.stringify(exportedImport.data));
  await page.getByRole('link', {name:'Storage'}).click();
  await page.getByTestId('storage-card-local').waitFor();
  assert.equal(await page.getByTestId('delete-storage-backend-local').count(), 0);
  await page.getByTestId('add-storage-backend').click();
  await page.getByTestId('storage-backend-name').fill('Offsite mirror');
  await page.getByTestId('storage-backend-endpoint').fill('s3.example.com');
  await page.getByTestId('storage-backend-bucket').fill('rsdw-backups');
  await page.getByTestId('storage-backend-secret-name').fill('rsdw-s3-offsite');
  await shot('07a-add-storage-backend-form.png');
  // Demo mode has no Kubernetes Secret access, so adding an S3 backend must
  // fail cleanly with a clear message rather than silently succeeding.
  const backendResponse = page.waitForResponse((response) => response.url() === base + '/api/backups/backends' && response.request().method() === 'POST');
  await page.getByTestId('confirm-modal').click();
  const backendResult = await backendResponse;
  assert.equal(backendResult.status(), 400, await backendResult.text());
  await page.locator('#modal-error').waitFor({state:'visible'});
  assert.match(await page.locator('#modal-error').innerText(), /demo mode/i);
  await shot('07b-add-storage-backend-demo-error.png');
  await page.getByTestId('cancel-modal').click();
  await shot('07-backup-settings.png');

  await page.goto(base + '/#backups/schedules');
  await page.getByTestId('add-backup-schedule').click();
  await page.getByTestId('backup-schedule-server').selectOption(serverID);
  await page.getByTestId('backup-schedule-profile').selectOption('dragonwilds-world-save');
  await page.getByTestId('backup-schedule-confirm').check();
  await shot('17-backup-schedule-preview.png');
  await page.locator('#modal-body').evaluate((element) => { element.scrollTop = 0; });
  await shot('05-backup-schedule-form.png');
  const scheduleResponse = await submit('/api/backups/schedules', 'POST', 200);
  const schedule = await scheduleResponse.json();
  await page.getByTestId('run-schedule-' + schedule.id).waitFor();
  await shot('04-backup-schedules.png');
  await page.getByTestId('run-schedule-' + schedule.id).click();
  await page.getByText(/Next scheduled run/).waitFor();
  await shot('08-scheduled-run-now.png');
  const nextRunText = await page.locator('.preview-row').filter({hasText:'Next scheduled run'}).innerText();
  const extraRunResponse = await submit('/api/backups/schedules/' + schedule.id + '/run', 'POST', 201);
  assert.equal((await extraRunResponse.json()).scheduleId, schedule.id);
  assert.match(nextRunText, /unchanged/);

  await page.goto(base + '/#backups');
  await page.getByTestId('backup-health').waitFor();
  await shot('01-backups-overview.png');
  await page.getByTestId('backup-details-' + firstRun.id).click();
  await page.getByText('Captured items').waitFor();
  assert.equal(await page.getByTestId('restore-server').count(), 0);
  await shot('09-backup-details.png');
  const bundleDownload = page.waitForEvent('download');
  await page.getByTestId('confirm-modal').click();
  const bundle = await bundleDownload;
  assert.match(bundle.suggestedFilename(), /\.zip$/);

  await page.goto(base + '/#backups/schedules');
  await page.locator('#backup-schedule-menu').selectOption(schedule.id);
  await page.getByTestId('delete-backup-schedule-menu').click();
  await page.getByTestId('delete-backup-confirm').waitFor();
  await shot('10-schedule-delete-confirmation.png');
  await page.getByTestId('cancel-modal').click();

  await page.goto(base + '/#maintenance?serverId=' + encodeURIComponent(serverID));
  await page.getByTestId('restore-server').waitFor();
  await page.getByTestId('restore-server').click();
  await page.getByTestId('restore-from-backup').waitFor();
  await shot('11-maintenance-restore-from-backup.png');
  await page.getByTestId('restore-custom').check();
  await page.getByTestId('restore-custom-file').setInputFiles({name:'custom.sav', mimeType:'application/octet-stream', buffer:Buffer.from('custom-save')});
  await page.getByTestId('restore-empty-confirm').check();
  await shot('12-maintenance-restore-custom-sav.png');
  const customRestore = page.waitForResponse((response) => response.url() === base + '/api/servers/' + encodeURIComponent(serverID) + '/restore' && response.request().method() === 'POST');
  await page.getByTestId('confirm-modal').click();
  const initialRestore = await customRestore;
  if (initialRestore.status() === 409) {
    assert.equal((await initialRestore.json()).code, 'restore_confirmation_required');
    await page.getByTestId('restore-destructive-confirm').check();
    const completedRestore = await submit('/api/servers/' + encodeURIComponent(serverID) + '/restore', 'POST', 202);
    assert.equal((await completedRestore.json()).status, 'restored');
  } else {
    assert.equal(initialRestore.status(), 202, await initialRestore.text());
    assert.equal((await initialRestore.json()).status, 'restored');
  }
  await page.locator('#modal').waitFor({state:'hidden'});

  await page.getByTestId('restore-server').click();
  await page.getByTestId('restore-empty-confirm').check();
  const trackedConflict = await submit('/api/servers/' + encodeURIComponent(serverID) + '/restore', 'POST', 409);
  assert.equal((await trackedConflict.json()).code, 'restore_confirmation_required');
  await page.getByTestId('restore-destructive-confirm').waitFor();
  assert.equal(await page.getByTestId('restore-destructive-confirm').isChecked(), false);
  await shot('14-restore-destructive-confirmation.png');
  await page.getByTestId('restore-destructive-confirm').check();
  const trackedRestore = await submit('/api/servers/' + encodeURIComponent(serverID) + '/restore', 'POST', 202);
  assert.equal((await trackedRestore.json()).status, 'restored');

  await page.getByTestId('restore-server').click();
  await page.getByTestId('create-server-from-backup').click();
  await page.getByTestId('create-from-backup-select').waitFor();
  const clonedBackupId = await page.getByTestId('create-from-backup-select').inputValue();
  await page.getByTestId('create-from-backup-name').fill('Issue 37 cloned world');
  await page.getByLabel('Owner name').fill('Issue 37 cloned owner');
  await page.getByTestId('create-from-backup-owner').fill(ownerID);
  await page.locator('#modal-form [name="confirmCreate"]').check();
  await shot('13-create-server-from-backup.png');
  const clonedResponse = await submit('/api/backups/' + clonedBackupId + '/create-server', 'POST', 201);
  const cloned = await clonedResponse.json();
  assert.equal(cloned.worldName, 'Issue 37 cloned world');
  assert.equal(cloned.name, 'Issue 37 cloned owner');
  assert.equal(await page.locator('#modal').isVisible(), false);
  assert.match(await page.locator('#toast').innerText(), /New server created from the backup/);

  assert.deepEqual(errors, []);
  for (const name of ['01-backups-overview.png','02-backup-profiles.png','03-backup-profile-form.png','04-backup-schedules.png','05-backup-schedule-form.png','06-run-backup-form.png','07-backup-settings.png','08-scheduled-run-now.png','09-backup-details.png','10-schedule-delete-confirmation.png','11-maintenance-restore-from-backup.png','12-maintenance-restore-custom-sav.png','13-create-server-from-backup.png']) {
    assert.ok(fs.statSync(path.join(screenshots, name)).size > 0, name);
  }
  console.log('PASS backup overview, profiles, multi-item authoring, YAML import/export, storage, schedules, Run now, bundle download, delete confirmation, tracked/custom restore, and create-server flows.');
  console.log('Browser evidence: ' + screenshots);
}

run().catch(async (error) => {
  console.error(error);
  if (page) {
    console.error('Browser URL:', page.url());
    console.error('Browser title:', await page.title().catch(() => ''));
    console.error('Visible text:', (await page.locator('body').innerText().catch(() => '')).slice(0, 3000));
    await page.screenshot({path:path.join(screenshots, 'failure.png'), fullPage:true}).catch(() => {});
  }
  process.exitCode = 1;
}).finally(async () => {
  if (browser) await browser.close();
  server.kill();
  await exited;
});
