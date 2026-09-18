const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {chromium} = require('playwright');
const {fixtureProcesses} = require('./fixture-processes.cjs');

const root = path.resolve(__dirname, '..');
fs.mkdirSync(path.join(root, '.tmp'), {recursive:true});
const output = fs.mkdtempSync(path.join(root, '.tmp/stop-browser-'));
const stateFile = path.join(output, 'state.json');
const token = 'stop-browser-token';
const world = {id:'world-1', name:'PETZKO', worldName:'PC2-US-EAST-01', namespace:'dragonwilds', release:'world-1', status:'online', maxPlayers:4, currentImage:'ghcr.io/petzkod5/rsdragonwilds-server:0.1.1'};
fs.writeFileSync(stateFile, JSON.stringify({servers:{[world.id]:world}}));
let browser;
const {start, close} = fixtureProcesses(root);

async function waitForToast(page) {
  await page.locator('#toast').waitFor({state:'hidden', timeout:5000}).catch(() => {});
}

async function noHorizontalOverflow(page) {
  return page.evaluate(() => {
    const panel = document.querySelector('#content .panel');
    const panelOverflow = panel ? panel.scrollWidth > panel.clientWidth + 1 : false;
    return panelOverflow || document.documentElement.scrollWidth > document.documentElement.clientWidth + 1;
  });
}

async function run() {
  const address = await start(path.join(root, '.tmp/rsdw-c2'), [], {
    RSDW_AUTH_MODE:'token', RSDW_DEMO_DATA:'true', RSDW_ADMIN_TOKEN:token,
    RSDW_STATE_FILE:stateFile, RSDW_LISTEN_ADDR:'127.0.0.1:0',
  }, /listening on (127\.0\.0\.1:\d+)/);
  const base = `http://${address}`;
  browser = await chromium.launch({headless:true, executablePath:process.env.RSDW_TEST_CHROMIUM});
  const page = await browser.newPage({viewport:{width:1440, height:1000}});
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const saved = () => JSON.parse(fs.readFileSync(stateFile, 'utf8'));

  await page.goto(base);
  await page.getByTestId('open-login').click();
  await page.getByTestId('admin-token').fill(token);
  await page.getByTestId('login-submit').click();
  await page.getByTestId('nav-maintenance').waitFor();
  await page.getByTestId('nav-maintenance').click();
  await page.getByTestId('stop-server').waitFor();
  assert.equal(await page.getByTestId('start-server').count(), 0);
  await page.screenshot({path:path.join(output, 'maintenance-running.png'), fullPage:true});

  await page.getByTestId('stop-server').click();
  assert.match(await page.locator('#modal-body').innerText(), /Active players will be disconnected/);
  assert.match(await page.locator('#modal-body').innerText(), /World data and this inventory row stay/);
  assert.equal(await page.locator('#modal-submit').evaluate((el) => el.classList.contains('danger')), true);
  assert.equal(await page.getByTestId('cancel-modal').evaluate((el) => el === document.activeElement), true);
  const stopPending = page.waitForResponse((response) => response.request().method() === 'POST' && response.url().endsWith(`/api/servers/${world.id}/actions/stop`));
  await page.getByTestId('confirm-modal').click();
  const stopResponse = await stopPending;
  assert.equal(stopResponse.status(), 200, await stopResponse.text());
  await page.locator('#modal').waitFor({state:'hidden'});
  await waitForToast(page);
  await page.getByTestId('start-server').waitFor();
  assert.equal(await page.getByTestId('stop-server').count(), 0);
  assert.equal(await page.getByTestId('restart-server').count(), 0);
  assert.equal(await page.getByTestId('update-image').count(), 0);
  assert.equal(await page.getByTestId('check-update').count(), 0);
  assert.equal(await page.getByTestId('edit-settings').count(), 0);
  assert.equal(await page.getByTestId('delete-server').count(), 1);
  assert.match(await page.locator('.panel-heading .status').first().innerText(), /Stopped/i);
  const afterStop = saved();
  assert.equal(afterStop.servers[world.id].status, 'stopped');
  assert.equal(afterStop.servers[world.id].id, world.id);
  await page.screenshot({path:path.join(output, 'maintenance-stopped.png'), fullPage:true});

  await page.getByTestId('start-server').click();
  assert.match(await page.locator('#modal-body').innerText(), /Same world, same server ID, same disk/);
  assert.equal(await page.getByTestId('cancel-modal').evaluate((el) => el === document.activeElement), true);
  const startPending = page.waitForResponse((response) => response.request().method() === 'POST' && response.url().endsWith(`/api/servers/${world.id}/actions/start`));
  await page.getByTestId('confirm-modal').click();
  const startResponse = await startPending;
  assert.equal(startResponse.status(), 200, await startResponse.text());
  await page.locator('#modal').waitFor({state:'hidden'});
  await waitForToast(page);
  await page.getByTestId('stop-server').waitFor();
  assert.equal(await page.getByTestId('start-server').count(), 0);
  const afterStart = saved();
  assert.equal(afterStart.servers[world.id].id, world.id);
  assert.match(afterStart.servers[world.id].status, /^(starting|online)$/);
  assert.match(await page.locator('.panel-heading .status').first().innerText(), /Starting|Online/i);

  assert.equal(await noHorizontalOverflow(page), false);
  await page.setViewportSize({width:390, height:844});
  await page.getByTestId('stop-server').waitFor();
  assert.equal(await noHorizontalOverflow(page), false);
  await page.screenshot({path:path.join(output, 'maintenance-mobile.png'), fullPage:true});

  const oidc = await start('go', ['test','-count=1','-run','^TestOIDCBrowserFixture$','-v','-timeout','90s'],
    {RSDW_OIDC_BROWSER_TEST:'1', RSDW_OIDC_BROWSER_HTTP:'0'}, /Console: (https:\/\/127\.0\.0\.1:\d+)/, true);
  const viewer = await browser.newPage({ignoreHTTPSErrors:true});
  viewer.on('pageerror', (error) => errors.push(error.message));
  await viewer.goto(oidc+'/api/auth/login');
  await viewer.getByRole('link', {name:'Viewer', exact:true}).click();
  await viewer.waitForURL(oidc+'/');
  await viewer.getByTestId('nav-dashboard').waitFor();
  await viewer.goto(oidc+'/#maintenance');
  await viewer.getByTestId('nav-dashboard').waitFor();
  assert.equal(await viewer.getByTestId('nav-maintenance').count(), 0);
  assert.equal(await viewer.getByTestId('stop-server').count(), 0);
  assert.equal(await viewer.getByTestId('start-server').count(), 0);
  const denied = await viewer.evaluate(async () => {
    const auth = await (await fetch('/api/auth')).json();
    const response = await fetch('/api/servers/scuffedtards/actions/stop', {method:'POST', headers:{'Content-Type':'application/json','X-CSRF-Token':auth.csrfToken}, body:'{}'});
    return {role:auth.role, status:response.status};
  });
  assert.deepEqual(denied, {role:'viewer', status:403});
  assert.deepEqual(errors, []);
  console.log('PASS stop/start maintenance, parked actions, demo start, viewer denial, desktop and mobile overflow.');
  console.log(`Browser evidence: ${output}`);
}

run().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  await close();
});
