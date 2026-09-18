const assert = require('node:assert/strict');
const path = require('node:path');
const {chromium} = require('playwright');

async function run() {
  const browser = await chromium.launch({headless:true, executablePath:process.env.RSDW_TEST_CHROMIUM});
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    const players = [
      {name:'<Alice & friends>',characterName:'<img src=x onerror=alert(1)>'},
      {name:'LongName'.repeat(60),characterName:'Character'.repeat(60)},
      {name:'',characterName:''},
    ];
    const server = {id:'world',name:'Test world',status:'online',maxPlayers:4};
    await page.route('http://roster.test/**', async (route) => {
      const url = new URL(route.request().url());
      const files = {'/':'index.html','/app.js':'app.js','/styles.css':'styles.css'};
      if (files[url.pathname]) {
        return route.fulfill({path:path.join(__dirname,'../web',files[url.pathname])});
      }
      let body;
      if (url.pathname === '/api/auth') body = {mode:'oidc',authenticated:true,subject:'viewer',role:'viewer',csrfToken:'session',capabilities:{dashboard:true,telemetry:true}};
      else if (url.pathname === '/api/bootstrap') body = {servers:[server],cluster:'Test'};
      else if (url.pathname === '/api/servers/world/telemetry') body = {server,metrics:{players:{value:3,status:'available',observedAt:new Date().toISOString()}},playerRoster:{status:'available',freshForMs:45000,players},samples:[]};
      else throw new Error(`Unexpected request ${url.pathname}`);
      await route.fulfill({json:body});
    });
    await page.goto('http://roster.test/#telemetry');
    const panel = page.getByTestId('connected-players');
    await panel.locator('li').first().waitFor();
    assert.equal(await panel.locator('li').count(),3);
    assert.equal(await panel.locator('dt').allTextContents().then((labels)=>labels.join('|')),'Name|Character name|Name|Character name|Name|Character name');
    assert.equal(await panel.locator('dd').first().textContent(),'<Alice & friends>');
    assert.equal(await panel.locator('img, script').count(),0);
    assert.match(await panel.innerText(),/Name unavailable/);
    assert.match(await panel.innerText(),/Character name unavailable/);
    for (const width of [1280,760,375]) {
      await page.setViewportSize({width,height:900});
      const layout = await panel.evaluate((element) => {
        const fields = [...element.querySelector('dl').children].map((field)=>field.getBoundingClientRect().toJSON());
        return {fields,overflow:document.documentElement.scrollWidth > innerWidth,panelScroll:element.scrollHeight > element.clientHeight + 1};
      });
      assert.equal(layout.overflow,false,`page overflow at ${width}px`);
      assert.equal(layout.panelScroll,false,`roster scrolls at ${width}px`);
      if (width <= 760) assert.ok(layout.fields[1].top >= layout.fields[0].bottom,`fields are not stacked at ${width}px`);
      else assert.equal(layout.fields[1].top,layout.fields[0].top);
    }
    await page.getByRole('button',{name:'Pause updates'}).click();
    await page.clock.install();
    await page.getByRole('button',{name:'Refresh data',exact:true}).click();
    await page.waitForFunction(()=>!state.refreshing);
    await page.clock.fastForward(46000);
    assert.match(await panel.innerText(),/Connected players are stale/);
    assert.equal(await panel.locator('li').count(),0);
    assert.deepEqual(errors,[]);
    console.log('Player roster desktop, narrow layout, escaping, and paused expiry checks passed.');
  } finally {
    await browser.close();
  }
}

run().catch((error)=>{console.error(error);process.exitCode=1;});
