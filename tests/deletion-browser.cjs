const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const {chromium} = require('playwright');

const root = path.resolve(__dirname, '..');
fs.mkdirSync(path.join(root, '.tmp'), {recursive:true});
const output = fs.mkdtempSync(path.join(root, '.tmp/deletion-browser-'));
const stateFile = path.join(output, 'state.json');
const token = 'deletion-browser-token';
const worlds = [
  {id:'petzko-01', name:'PETZKO', worldName:'PC2-US-EAST-01'},
  {id:'petzko-02', name:'PETZKO', worldName:'PC2-US-EAST-02'},
  {id:'legacy', name:'Legacy world'},
  {id:'id-only'},
  {id:'duplicate-a', name:'PETZKO', worldName:'Duplicate world'},
  {id:'duplicate-b', name:'PETZKO', worldName:'Duplicate world'},
  {id:'pending-keep', name:'PETZKO', worldName:'Interrupted keep'},
  {id:'pending-purge', name:'PETZKO', worldName:'Interrupted purge'},
].map((server) => ({namespace:'dragonwilds', release:server.id, status:'online', maxPlayers:4, ...server}));
// A saved receipt exercises rendering only; demo mode cannot prove Kubernetes retention.
const receipt = {serverId:'previous-world', worldLabel:'Previous world', namespace:'dragonwilds', release:'previous-world', mode:'keep', completed:true,
  plan:{world:[{kind:'PersistentVolumeClaim', namespace:'dragonwilds', name:'previous-world', uid:'world-uid'}],
    seeds:[{kind:'PersistentVolumeClaim', namespace:'dragonwilds', name:'uploaded-source', uid:'source-uid'}],
    retainedSecrets:[{kind:'Secret', namespace:'dragonwilds', name:'legacy-api', uid:'secret-uid'}]}};
const pendingReceipts = Object.fromEntries(['keep','purge'].map((mode) => [`pending-${mode}`, {
  serverId:`pending-${mode}`, worldLabel:`Interrupted ${mode}`, namespace:'dragonwilds', release:`pending-${mode}`,
  mode, completed:false, lastError:'Fixture interruption; retry required', plan:{seeds:receipt.plan.seeds},
}]));
fs.writeFileSync(stateFile, JSON.stringify({servers:Object.fromEntries(worlds.map((server) => [server.id, server])), deletions:{'previous-world':receipt, ...pendingReceipts}}));
const children = [];
let browser;

async function start(command, args, env, pattern, detached = false) {
  const child = spawn(command, args, {cwd:root, env:{...process.env, ...env}, detached, stdio:['ignore','pipe','pipe']});
  const exited = once(child, 'exit');
  children.push({child, exited, detached});
  return new Promise((resolve, reject) => {
    let logs = '';
    const timeout = setTimeout(() => reject(new Error(`Fixture did not start: ${logs}`)), 30000);
    child.on('error', (error) => { clearTimeout(timeout); reject(error); });
    child.on('exit', (code) => { clearTimeout(timeout); reject(new Error(`Fixture exited ${code}: ${logs}`)); });
    const read = (chunk) => {
      logs += chunk;
      const match = logs.match(pattern);
      if (match) { clearTimeout(timeout); resolve(match[1]); }
    };
    child.stdout.on('data', read);
    child.stderr.on('data', read);
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
  const inventory = () => Object.keys(saved().servers).sort();
  const initialInventory = inventory();
  const deletions = [];
  page.on('request', (request) => { if (request.method() === 'DELETE') deletions.push({url:request.url(), body:request.postDataJSON()}); });
  await page.goto(base);
  await page.getByTestId('open-login').click();
  await page.getByTestId('admin-token').fill(token);
  await page.getByTestId('login-submit').click();
  await page.getByTestId('add-server').waitFor();
  const select = page.locator('#server-filter');
  for (const [id, label] of [
    ['petzko-01','PC2-US-EAST-01'], ['petzko-02','PC2-US-EAST-02'],
    ['legacy','Legacy world'], ['id-only','id-only'],
    ['duplicate-a','Duplicate world (duplicate-a)'], ['duplicate-b','Duplicate world (duplicate-b)'],
  ]) {
    assert.equal(await select.locator(`option[value="${id}"]`).textContent(), label);
    const row = page.getByTestId('view-server').and(page.locator(`[data-id="${id}"]`)).locator('..').locator('..');
    assert.equal(await row.locator('td strong').textContent(), label);
  }
  await page.getByTestId('nav-maintenance').click();
  await select.selectOption('petzko-02');
  await page.locator('.panel-heading p').filter({hasText:'PC2-US-EAST-02'}).waitFor();
  const retained = page.getByTestId('deletion-receipts');
  assert.match(await retained.innerText(), /Secrets were kept because C2 could not prove ownership/);
  assert.match(await retained.innerText(), /dragonwilds\/legacy-api, UID secret-uid/);
  assert.match(await retained.innerText(), /persistence.existingClaim/);
  assert.match(await retained.innerText(), /Retained source-save PVC dragonwilds\/uploaded-source, UID source-uid/);
  assert.match(await retained.innerText(), /recovery through saveSeed/);
  assert.match(await retained.innerText(), /not a world volume for persistence.existingClaim/);
  await page.getByTestId('delete-server').click();
  assert.match(await page.locator('#modal-body').innerText(), /PC2-US-EAST-02\s+Stable ID petzko-02/);
  assert.equal(await page.getByTestId('delete-mode').inputValue(), 'keep');
  assert.equal(await page.getByTestId('cancel-modal').evaluate((el) => el === document.activeElement), true);
  await page.getByTestId('cancel-modal').click();
  assert.deepEqual(inventory(), initialInventory);
  assert.equal(deletions.length, 0);
  assert.equal(await page.getByTestId('delete-server').evaluate((el) => el === document.activeElement), true);

  async function submit(status) {
    const pending = page.waitForResponse((response) => response.request().method() === 'DELETE');
    await page.getByTestId('confirm-modal').click();
    const response = await pending;
    assert.equal(response.status(), status, await response.text());
    if (status === 200) await page.locator('#modal').waitFor({state:'hidden'});
    else await page.locator('#modal-error').waitFor();
    return response.json();
  }
  await page.getByTestId('delete-server').click();
  const kept = await submit(200);
  assert.equal(kept.serverId, 'petzko-02');
  assert.equal(kept.mode, 'keep');
  assert.equal(kept.completed, true);
  assert.deepEqual(inventory(), initialInventory.filter((id) => id !== 'petzko-02'));
  assert.deepEqual(deletions[0], {url:base+'/api/servers/petzko-02', body:{confirm:'petzko-02', mode:'keep'}});
  await page.reload();
  await retained.getByRole('heading', {name:'PC2-US-EAST-02', exact:true}).waitFor();
  assert.match(await retained.innerText(), /No real storage was changed in demo mode/);
  assert.deepEqual(saved().deletions['petzko-02'], kept);

  await select.selectOption('petzko-01');
  await page.locator('.panel-heading p').filter({hasText:'PC2-US-EAST-01'}).waitFor();
  await page.getByTestId('delete-server').click();
  await page.getByTestId('delete-mode').selectOption('purge');
  await page.getByTestId('confirm-modal').click();
  assert.equal(await page.getByTestId('purge-confirm').evaluate((el) => el.checkValidity()), false);
  assert.equal(deletions.length, 1);
  await page.getByTestId('purge-confirm').fill('DELETE WORLD petzko-02');
  await submit(400);
  assert.ok(saved().servers['petzko-01']);
  await page.getByTestId('purge-confirm').fill('DELETE WORLD petzko-01');
  const purged = await submit(200);
  assert.equal(purged.mode, 'purge');
  assert.equal(purged.completed, true);
  assert.deepEqual(inventory(), initialInventory.filter((id) => !['petzko-01','petzko-02'].includes(id)));
  await page.reload();
  await retained.getByRole('heading', {name:'PC2-US-EAST-01', exact:true}).waitFor();
  assert.match(await retained.innerText(), /backing volumes with a Retain reclaim policy/);

  for (const mode of ['keep','purge']) {
    const id = `pending-${mode}`;
    await select.selectOption(id);
    await page.getByText('Deletion is pending. Retry the recorded operation below. Other lifecycle actions are unavailable.').waitFor();
    assert.equal(await page.getByTestId('delete-server').count(), 0);
    assert.equal(await page.getByTestId('restart-server').count(), 0);
    assert.equal(await page.getByTestId('update-image').count(), 0);
    const pendingReceipt = retained.locator('article').filter({has:page.getByRole('heading', {name:`Interrupted ${mode}`, exact:true})});
    assert.match(await pendingReceipt.innerText(), /Pending, retry required/);
    assert.match(await pendingReceipt.innerText(), new RegExp(`${mode === 'keep' ? 'Retained' : 'Selected'} source-save PVC dragonwilds/uploaded-source, UID source-uid`));
    await pendingReceipt.getByTestId('retry-deletion').click();
    assert.match(await page.locator('#modal-body').innerText(), new RegExp(`Recorded choice ${mode}\\. It cannot change on retry`));
    assert.equal(await page.getByTestId('delete-mode').count(), 0);
    assert.equal(await page.locator('#modal-submit').evaluate((el) => el.classList.contains('danger')), true);
    if (mode === 'purge') {
      const before = deletions.length;
      await page.getByTestId('confirm-modal').click();
      assert.equal(await page.getByTestId('purge-confirm').evaluate((el) => el.checkValidity()), false);
      assert.equal(deletions.length, before);
      await page.getByTestId('purge-confirm').fill(`DELETE WORLD ${id}`);
    }
    const completed = await submit(200);
    assert.equal(completed.serverId, id);
    assert.equal(completed.mode, mode);
    assert.equal(completed.completed, true);
    assert.equal(saved().servers[id], undefined);
    assert.equal(deletions.at(-1).body.confirm, id);
    await page.reload();
    await pendingReceipt.getByText(`Server ID ${id}. Completed. World data choice ${mode}.`, {exact:true}).waitFor();
    assert.equal(await pendingReceipt.getByTestId('retry-deletion').count(), 0);
    assert.match(await pendingReceipt.innerText(), new RegExp(`${mode === 'keep' ? 'Retained' : 'Deleted'} source-save PVC dragonwilds/uploaded-source, UID source-uid`));
  }
  await page.screenshot({path:path.join(output, 'receipts.png'), fullPage:true});

  // Reuse the real OIDC fixture to prove viewer restrictions without spoofing capabilities.
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
  assert.equal(await viewer.getByTestId('delete-server').count(), 0);
  assert.equal(await viewer.getByTestId('retry-deletion').count(), 0);
  const denied = await viewer.evaluate(async () => {
    const auth = await (await fetch('/api/auth')).json();
    const response = await fetch('/api/servers/scuffedtards', {method:'DELETE', headers:{'Content-Type':'application/json','X-CSRF-Token':auth.csrfToken}, body:JSON.stringify({confirm:'scuffedtards',mode:'keep'})});
    return {role:auth.role, status:response.status};
  });
  assert.deepEqual(denied, {role:'viewer', status:403});
  assert.deepEqual(errors, []);
  console.log('PASS world labels and stable IDs, legacy fallback, viewer denial, cancel, keep default, purge confirmation, persisted receipts, pending keep/purge retries, and retained Secret identities. Demo does not prove Kubernetes retention.');
  console.log(`Browser evidence: ${output}`);
}

run().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  for (const {child, exited, detached} of children.reverse()) {
    if (child.exitCode === null && child.signalCode === null) {
      if (detached) process.kill(-child.pid, 'SIGTERM');
      else child.kill();
    }
    await exited;
  }
});
