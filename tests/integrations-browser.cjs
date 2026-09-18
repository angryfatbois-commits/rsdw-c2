const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const {chromium} = require('playwright');

const root = path.resolve(__dirname, '..');
const output = fs.mkdtempSync(path.join(root, '.tmp/integrations-browser-'));
const stateFile = path.join(output, 'state.json');
const token = 'integration-browser-admin';
const server = spawn(path.join(root, '.tmp/rsdw-c2'), [], {
  cwd:root, env:{...process.env, RSDW_AUTH_MODE:'token', RSDW_DEMO_DATA:'true', RSDW_ADMIN_TOKEN:token, RSDW_STATE_FILE:stateFile, RSDW_LISTEN_ADDR:'127.0.0.1:0'}, stdio:['ignore','pipe','pipe'],
});
const exited = once(server, 'exit');
let logs = '', browser;

async function run() {
  const base = await new Promise((resolve,reject) => {
    const timeout = setTimeout(() => reject(new Error('Demo server did not start')),15000);
    server.on('error',reject);
    server.on('exit',() => {clearTimeout(timeout); reject(new Error('Demo server exited'));});
    server.stderr.on('data',(chunk) => {
      logs += chunk;
      const match = logs.match(/listening on (127\.0\.0\.1:\d+)/);
      if (match) {clearTimeout(timeout); resolve(`http://${match[1]}`);}
    });
  });
  browser = await chromium.launch({headless:true,executablePath:process.env.RSDW_TEST_CHROMIUM});
  const page = await browser.newPage({viewport:{width:1440,height:1050}});
  const errors = [];
  page.on('pageerror',(e) => errors.push(e.message));
  const saved = () => JSON.parse(fs.readFileSync(stateFile,'utf8'));
  const checkDialogLayout = async (action) => {
    for (const viewport of [{width:1440,height:1000},{width:375,height:667},{width:667,height:375}]) {
      await page.setViewportSize(viewport);
      await page.getByTestId(action).click();
      const layout = await page.locator('#modal').evaluate((dialog) => {
        const heading = dialog.querySelector('h2');
        const bounds = dialog.getBoundingClientRect();
        const title = heading.getBoundingClientRect();
        return {
          focus:document.activeElement.id,
          headingVisible:title.top >= bounds.top && title.bottom <= bounds.bottom,
          fits:bounds.left >= 0 && bounds.right <= innerWidth && bounds.top >= 0 && bounds.bottom <= innerHeight && dialog.scrollWidth <= dialog.clientWidth,
          checkboxes:[...dialog.querySelectorAll('input[type=checkbox]')].map((input) => {
            const box = input.getBoundingClientRect();
            const label = input.closest('label').getBoundingClientRect();
            return {width:box.width,height:box.height,labelWidth:label.width,labelHeight:label.height};
          }),
        };
      });
      await page.screenshot({path:path.join(output,`${action}-${viewport.width}.png`)});
      assert.equal(layout.focus,'modal-title',JSON.stringify({action,viewport,layout}));
      assert.equal(layout.headingVisible,true,'Initial heading must be visible without scrolling');
      assert.equal(layout.fits,true,'Dialog must fit without horizontal scrolling');
      assert.ok(layout.checkboxes.length > 0);
      for (const box of layout.checkboxes) {
        assert.ok(box.width >= 12 && box.width <= 24 && box.height >= 12 && box.height <= 24,JSON.stringify(box));
        assert.ok(box.labelWidth > box.width && box.labelHeight >= 44,'Checkbox labels must provide a generous click target');
      }
      const checkbox = page.locator('input[name=integrationServer]').first();
      const checked = await checkbox.isChecked();
      await checkbox.locator('xpath=ancestor::label').click();
      assert.equal(await checkbox.isChecked(),!checked,'Clicking the label toggles its checkbox');
      await checkbox.focus();
      await page.keyboard.press('Space');
      assert.equal(await checkbox.isChecked(),checked,'Space toggles the native checkbox');
      await page.screenshot({path:path.join(output,`${action}-checkboxes-${viewport.width}.png`)});
      await page.getByTestId('cancel-modal').click();
      assert.equal(await page.getByTestId(action).evaluate((button) => button === document.activeElement),true);
    }
    await page.setViewportSize({width:1440,height:1050});
  };
  const submit = async (method,endpoint,status) => {
    const response = page.waitForResponse((r) => r.url() === base+endpoint && r.request().method() === method);
    await page.getByTestId('confirm-modal').click();
    const result = await response;
    assert.equal(result.status(),status,await result.text());
    await page.locator('#modal').waitFor({state:'hidden'});
    return result.json();
  };
  await page.goto(base+'/#integrations');
  assert.equal((await fetch(base+'/api/integrations')).status,401);
  await page.getByTestId('open-login').click();
  await page.getByTestId('admin-token').fill(token);
  await page.getByTestId('login-submit').click();
  await page.getByTestId('add-integration').waitFor();
  assert.match(await page.locator('#content').innerText(),/No Discord bots configured/);
  await checkDialogLayout('add-integration');
  assert.equal(await page.getByRole('heading',{name:'Message previews',exact:true}).count(),0);
  assert.equal(await page.getByTestId('discord-message-preview').count(),0);
  await page.getByTestId('add-integration').click();
  await page.getByTestId('integration-name').fill('<Guild & friends>');
  await page.getByTestId('integration-guild').fill('123');
  await page.getByTestId('integration-channel').fill('456');
  await page.getByTestId('integration-secret-name').fill('discord-bot');
  await page.getByTestId('integration-secret-key').fill('token');
  assert.equal(await page.locator('#modal input[type=password]').count(),0);
  await page.locator('input[name=integrationServer][value=scuffedtards]').check();
  for (const kind of ['restart_requested','restart_completed','player_joined','server_down']) await page.locator(`input[name=integrationRule][value=${kind}]`).check();
  for (const kind of ['backup_started','backup_completed','backup_failed']) assert.equal(await page.locator(`input[value=${kind}]`).isDisabled(),true);
  assert.match(await page.locator('#modal-body').innerText(),/approximate count increase/);
  await page.screenshot({path:path.join(output,'configuration.png'),fullPage:true});
  const item = await submit('POST','/api/integrations',201);
  assert.equal(item.secretRef.name,'discord-bot');
  assert.deepEqual(saved().integrations[item.id],item);
  await page.getByTestId('edit-integration').waitFor();
  await checkDialogLayout('edit-integration');
  assert.equal(await page.getByRole('heading',{name:'Message previews',exact:true}).count(),0);
  assert.match(await page.locator('#content').innerText(),/<Guild & friends>/);
  assert.equal(await page.locator('#content guild').count(),0);
  await page.getByTestId('test-integration').click();
  await page.waitForFunction(async () => {
    const data = await fetch('/api/integrations',{headers:{Authorization:'Bearer integration-browser-admin'}}).then((r) => r.json());
    return data.deliveries.some((d) => d.status === 'sent' && d.result.includes('Demo'));
  });
  await page.getByTestId('edit-integration').click();
  await page.getByTestId('integration-enabled').selectOption('false');
  await page.getByTestId('integration-secret-name').fill('discord-rotated');
  await page.getByTestId('integration-secret-key').fill('next');
  await page.locator('input[value=server_down]').uncheck();
  await submit('PUT',`/api/integrations/${item.id}`,200);
  assert.equal(saved().integrations[item.id].enabled,false);
  assert.equal(saved().integrations[item.id].rules.server_down,undefined);
  await page.reload();
  await page.getByTestId('edit-integration').waitFor();
  assert.match(await page.locator('#content').innerText(),/discord-rotated/);
  await page.getByTestId('edit-integration').click();
  await page.getByTestId('integration-enabled').selectOption('true');
  await submit('PUT',`/api/integrations/${item.id}`,200);
  await page.getByTestId('nav-maintenance').click();
  await page.getByTestId('restart-server').click();
  await submit('POST','/api/servers/scuffedtards/actions/restart',200);
  await page.getByTestId('nav-integrations').click();
  await page.waitForFunction(async () => {
    const data = await fetch('/api/integrations',{headers:{Authorization:'Bearer integration-browser-admin'}}).then((r) => r.json());
    return data.deliveries.filter((d) => ['restart_requested','restart_completed'].includes(d.event.kind) && d.status === 'sent').length === 2;
  });
  await page.locator('#refresh').click();
  const recent = page.getByTestId('discord-recent-delivery');
  const completed = recent.filter({hasText:'Restart completed'});
  const sentCompleted = completed.filter({hasText:'Sent'}).first();
  await sentCompleted.waitFor();
  assert.ok(await recent.count() >= 3);
  assert.match(await sentCompleted.innerText(),/SERVER[\s\S]*ScuffedTards/);
  assert.match(await sentCompleted.innerText(),/bare minimum/);
  await page.screenshot({path:path.join(output,'deliveries.png'),fullPage:true});
  await page.setViewportSize({width:390,height:844});
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth),true);
  for (const card of await recent.all()) {
    const box = await card.boundingBox();
    assert.ok(box.width > 200 && box.x >= 0 && box.x + box.width <= 390);
  }
  await page.screenshot({path:path.join(output,'deliveries-mobile.png'),fullPage:true});
  await page.setViewportSize({width:1440,height:1050});
  await page.getByTestId('edit-integration').click();
  await page.getByTestId('integration-name').fill('Unsaved private draft');
  await page.evaluate(() => sessionStorage.setItem('rsdw-admin-token','expired'));
  const response = page.waitForResponse((r) => r.url() === base+`/api/integrations/${item.id}` && r.request().method() === 'PUT');
  await page.getByTestId('confirm-modal').click();
  assert.equal((await response).status(),401);
  await page.locator('#login-dialog').waitFor({state:'visible'});
  assert.equal(await page.locator('#modal').isVisible(),false);
  assert.equal(await page.locator('#modal-body').innerText(),'');
  assert.doesNotMatch(await page.locator('#content').innerText(),/Guild & friends|discord-rotated|Unsaved private draft/);
  assert.equal(await page.getByTestId('discord-recent-delivery').count(),0);
  assert.deepEqual(errors,[]);
  assert.ok(!logs.includes(token));
  console.log('PASS add/edit dialog focus, visible headings, checkbox layout and keyboard/label interaction at desktop/narrow/short viewports; authenticated Discord configuration, unavailable backup rules, approximate labels, associations, rotation, disable/enable, demo tests and restart deliveries, persistence, escaping, and expired-session clearing.');
  console.log(`Browser evidence: ${output}`);
}

run().catch((e) => {console.error(e); process.exitCode=1;}).finally(async () => {
  if (browser) await browser.close();
  server.kill();
  await exited;
});
