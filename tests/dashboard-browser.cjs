const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {chromium} = require('playwright');

async function run() {
  const browser = await chromium.launch({headless:true, executablePath:process.env.RSDW_TEST_CHROMIUM});
  const output = path.join(__dirname,'../.tmp/dashboard');
  fs.mkdirSync(output,{recursive:true});
  try {
    const page = await browser.newPage({viewport:{width:1600,height:1155},locale:'en-US',timezoneId:'UTC'});
    await page.clock.install({time:new Date('2030-01-02T13:02:11Z')});
    const metric = (value) => ({value,status:value == null ? 'unavailable' : 'available'});
    const servers = [
      ['aurora','Aurora Expanse','online',5,12,60,6,14.9,36],
      ['brin','Brin Hollow','online',2,8,59,4.3,10.2,28],
      ['cinder','Cinder North','attention',2,4,null,null,null,71],
      ['ember','Ember Vale','stopped',0,16,null,null,null,null],
    ].map(([id,worldName,status,players,maxPlayers,tick,memory,limit,cpu],index) => ({id,worldName,status,maxPlayers,endpoint:`10.20.0.${21+index}:7777`,updateAvailable:id === 'cinder',metrics:{players:metric(players),tickRate:metric(tick),memoryUsedBytes:metric(memory == null ? null : memory*1073741824),memoryLimitBytes:metric(limit == null ? null : limit*1073741824),cpuPercent:metric(cpu)}}));
    const errors = [], requests = [];
    let viewer = false;
    await page.route('http://dashboard.test/**', async route => {
      const request = route.request(), url = new URL(request.url());
      const files = {
        '/':'index.html',
        '/app.js':'app.js',
        '/styles.css':'styles.css',
        '/assets/dragonwilds-login-desktop-v1.png':'assets/dragonwilds-login-desktop-v1.png',
        '/assets/dragonwilds-login-mobile-v1.png':'assets/dragonwilds-login-mobile-v1.png',
      };
      if (files[url.pathname]) return route.fulfill({path:path.join(__dirname,'../web',files[url.pathname])});
      let body;
      if (request.method() === 'POST') {
        assert.match(url.pathname,/^\/api\/servers\/(?:brin|cinder|ember)\/actions\/(?:restart|start|stop)$/);
        assert.equal(request.headers()['x-csrf-token'],'fixture');
        requests.push(url.pathname);
        body = servers.find(server => url.pathname.includes(`/${server.id}/`));
      } else if (url.pathname === '/api/auth') body = {mode:'oidc',authenticated:true,subject:viewer?'viewer':'admin',role:viewer?'viewer':'admin',csrfToken:'fixture',capabilities:viewer ? {dashboard:true,telemetry:true} : Object.fromEntries(['dashboard','telemetry','events','maintenance','create','integrations','reboots','restart','start','stop','updateCheck'].map(key => [key,true]))};
      else if (url.pathname === '/api/bootstrap') body = {servers,cluster:'dragonwilds-prod',mode:'kubernetes'};
      else if (url.pathname === '/api/reboots') body = {available:true,schedules:[{serverId:'aurora',enabled:true,nextRun:'2030-01-02T15:00:00Z'},{serverId:'brin',enabled:true,nextRun:'2030-01-03T05:00:00Z'}]};
      else if (url.pathname.endsWith('/telemetry')) { const server = servers.find(s => url.pathname.includes(`/${s.id}/`)); body = {server,metrics:server.metrics,samples:[]}; }
      else if (['/api/events','/api/users','/api/integrations'].includes(url.pathname)) body = {};
      else throw new Error(`Unexpected request ${request.method()} ${url.pathname}`);
      await route.fulfill({json:body});
    });
    page.on('pageerror',error => errors.push(error.message));
    await page.goto('http://dashboard.test/#dashboard');
    await page.locator('.console-row').last().waitFor();
    await page.waitForFunction(() => !state.refreshing);
    const row = id => page.locator(`.console-row[data-server-id="${id}"]`);
    assert.equal(await page.locator('.dashboard-stats .stat').count(),3);
    assert.deepEqual(await page.locator('.dashboard-stats .stat-value').allTextContents(),['4','3 / 4','9 / 40']);
    assert.equal(await row('cinder').locator('.console-badge').count(),2);
    assert.equal(await row('ember').locator('.console-bar').count(),0);
    assert.equal(await row('ember').getByRole('button').count(),1);
    assert.deepEqual(await page.locator('.console-row').evaluateAll(rows=>rows.map(row=>row.getBoundingClientRect().height)),[140,140,140,140]);
    for (const width of [1600,1280,900,760,390]) {
      await page.setViewportSize({width,height:1155});
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth > innerWidth),false,`overflow at ${width}`);
      if ([1600,390].includes(width)) await page.screenshot({path:path.join(output,`dashboard-${width}.png`),fullPage:true,animations:'disabled'});
    }
    await page.setViewportSize({width:1600,height:1155});
    await row('cinder').getByRole('button',{name:'Reboot'}).click();
    assert.match(await page.locator('#modal-body').innerText(),/Cinder North/);
    await page.getByRole('button',{name:'Confirm restart'}).click();
    await page.waitForFunction(()=>!state.modalBusy && !state.refreshing);
    assert.deepEqual(requests,['/api/servers/cinder/actions/restart']);
    assert.equal(new URL(page.url()).hash,'#dashboard');
    await row('brin').getByRole('button',{name:'Stop',exact:true}).click();
    assert.match(await page.locator('#modal-body').innerText(),/Brin Hollow/);
    assert.equal(requests.length,1);
    await page.getByRole('button',{name:'Confirm stop'}).click();
    await page.waitForFunction(()=>!state.modalBusy && !state.refreshing);
    assert.equal(requests[1],'/api/servers/brin/actions/stop');
    await row('ember').getByRole('button',{name:'Start',exact:true}).click();
    assert.match(await page.locator('#modal-body').innerText(),/Ember Vale/);
    await page.getByRole('button',{name:'Confirm start'}).click();
    await page.waitForFunction(()=>!state.modalBusy && !state.refreshing);
    assert.equal(requests[2],'/api/servers/ember/actions/start');
    assert.equal(requests.length,3);
    assert.equal(new URL(page.url()).hash,'#dashboard');
    await page.getByRole('button',{name:'Table view',exact:true}).click();
    assert.equal(await page.locator('tbody tr').count(),4);
    assert.match(await page.locator('tbody').innerText(),/10.20.0.23:7777/);
    await page.getByRole('button',{name:'Row view',exact:true}).click();
    const resourceBox = await row('brin').locator('.console-resources').boundingBox();
    assert.ok(resourceBox);
    await page.mouse.click(resourceBox.x + resourceBox.width / 2, resourceBox.y + resourceBox.height / 2);
    await page.waitForURL('**/#servers/brin');
    await page.getByTestId('server-overview').waitFor();
    await page.getByTestId('nav-dashboard').click();
    await page.locator('#server-filter').selectOption('');
    await row('cinder').getByTestId('view-server').focus();
    await page.keyboard.press('Enter');
    await page.waitForURL('**/#servers/cinder');
    viewer = true;
    await page.goto('http://dashboard.test/#dashboard');
    await page.locator('.console-row').last().waitFor();
    assert.equal(await page.locator('.console-actions button,.console-badge.update,.console-schedule').count(),0);
    assert.doesNotMatch(await page.locator('#content').innerText(),/10\.20\.0/);
    assert.deepEqual(errors,[]);
    console.log('Dashboard desktop/mobile layout, screenshots, row actions, navigation, table and viewer checks passed.');
  } finally { await browser.close(); }
}
run().catch(error => { console.error(error); process.exitCode=1; });
