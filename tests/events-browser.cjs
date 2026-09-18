const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const {chromium} = require('playwright');

const root = path.resolve(__dirname, '..');
fs.mkdirSync(path.join(root, '.tmp'), {recursive:true});
const output = fs.mkdtempSync(path.join(root, '.tmp/events-browser-'));
const stateFile = path.join(output, 'state.json');
const token = 'events-browser-test-token';
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
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));

  await page.goto(base + '/#events');
  await page.getByTestId('open-login').click();
  await page.getByTestId('admin-token').fill(token);
  const firstList = page.waitForResponse((response) => response.url().includes('/api/events?') && response.request().method() === 'GET');
  await page.getByTestId('login-submit').click();
  await page.getByTestId('nav-events').waitFor();
  const listed = await firstList;
  assert.equal(listed.status(), 200);
  assert.match(listed.url(), /since=/);
  await page.getByTestId('event-range').waitFor();
  assert.equal(await page.getByTestId('event-range').inputValue(), '24h');
  await page.getByTestId('event-row').first().waitFor();
  assert.match(await page.locator('#content').innerText(), /Update available/);
  assert.equal(await page.getByTestId('load-older-events').count(), 0);

  const updateList = page.waitForResponse((response) => response.url().includes('category=update') && response.request().method() === 'GET');
  await page.getByTestId('category-update').click();
  await updateList;
  await page.getByTestId('event-row').first().waitFor();
  const updateText = await page.locator('#content').innerText();
  assert.match(updateText, /Update available/);
  assert.doesNotMatch(updateText, /Player connected/);

  const allRange = page.waitForRequest((request) => request.url().includes('/api/events?') && !request.url().includes('since='));
  await page.getByTestId('event-range').selectOption('all');
  await allRange;
  const allCategories = page.waitForResponse((response) => /category=&/.test(new URL(response.url()).search) && response.request().method() === 'GET');
  await page.getByTestId('category-all').click();
  await allCategories;
  await page.getByTestId('event-row').first().waitFor();
  assert.match(await page.locator('#content').innerText(), /Player connected/);

  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByTestId('export-events').click(),
  ]);
  assert.equal(download.suggestedFilename(), 'rsdw-events.csv');
  const csv = fs.readFileSync(await download.path(), 'utf8');
  assert.match(csv, /Update available/);
  assert.match(csv, /"timestamp","serverName","category","severity","message","details"/);

  await page.screenshot({path:path.join(output, 'events.png'), fullPage:true});
  await page.setViewportSize({width:375, height:667});
  await page.screenshot({path:path.join(output, 'events-mobile.png'), fullPage:true});
  assert.equal(errors.length, 0, errors.join('\n'));
}

run().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => {
  await browser?.close();
  server.kill();
  await exited;
});
