const assert = require('node:assert/strict');
const {spawn} = require('node:child_process');
const {once} = require('node:events');
const https = require('node:https');
const path = require('node:path');
const {chromium} = require('playwright');

const root = path.resolve(__dirname, '..');
const fixture = spawn('go', ['test', '-count=1', '-run', '^TestOIDCBrowserFixture$', '-v', '-timeout', '90s'], {
  cwd:root, detached:true,
  env:{...process.env,RSDW_OIDC_BROWSER_TEST:'1',RSDW_OIDC_BROWSER_HTTP:'0'},
  stdio:['ignore','pipe','pipe'],
});
const exited = once(fixture, 'exit');
let logs = '', browser;

function fixtureResponse(url, headers = {}) {
  return new Promise((resolve, reject) => {
    https.get(url, {headers,rejectUnauthorized:false}, (res) => {
      const chunks = [];
      res.on('data', (chunk) => chunks.push(chunk));
      res.on('end', () => resolve({status:res.statusCode,headers:res.headers,body:Buffer.concat(chunks)}));
      res.on('error', reject);
    }).on('error', reject);
  });
}

async function holdResponse(page, pathname) {
  let captured, release;
  const responseReady = new Promise((resolve) => { captured = resolve; });
  const released = new Promise((resolve) => { release = resolve; });
  await page.route((url) => url.pathname === pathname, async (route) => {
    const request = route.request();
    const headers = await request.allHeaders();
    const response = await fixtureResponse(request.url(), headers);
    captured();
    await released;
    await route.fulfill({...response,headers:Object.fromEntries(Object.entries(response.headers).map(([key,value]) => [key,Array.isArray(value) ? value.join('\n') : String(value)]))});
  });
  return {ready:responseReady,release};
}

async function run() {
  const base = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('OIDC fixture did not start')),30000);
    fixture.on('error',reject);
    fixture.on('exit',() => {clearTimeout(timeout);reject(new Error('OIDC fixture exited before startup'));});
    const output = (chunk) => {
      logs += chunk;
      const match = logs.match(/Console: (https:\/\/127\.0\.0\.1:\d+)/);
      if (match) {clearTimeout(timeout);resolve(match[1]);}
    };
    fixture.stdout.on('data',output);
    fixture.stderr.on('data',output);
  });
  browser = await chromium.launch({headless:true,executablePath:process.env.RSDW_TEST_CHROMIUM});
  const context = await browser.newContext({ignoreHTTPSErrors:true});
  const otherContext = await browser.newContext({ignoreHTTPSErrors:true});
  const older = await context.newPage();
  const newer = await context.newPage();
  const independent = await otherContext.newPage();
  const auth = (page) => page.evaluate(async () => (await fetch('/api/auth')).json());
  const choose = async (page, role) => {
    await page.getByRole('link',{name:role,exact:true}).click();
    await page.waitForURL(base+'/');
    assert.equal((await auth(page)).role,role.toLowerCase());
  };
  await independent.goto(base+'/api/auth/login');
  await choose(independent,'Admin');

  const initial = await holdResponse(newer,'/api/auth/login');
  const newerNavigation = newer.goto(base+'/api/auth/login',{timeout:60000});
  newerNavigation.catch(() => {});
  await initial.ready;
  console.log('Held initial login response before Chromium received its binding.');
  await older.goto(base+'/api/auth/login');
  console.log('Older login reached the identity provider.');
  const callback = await holdResponse(older,'/api/auth/callback');
  const authorization = new URL(await older.getByRole('link',{name:'Admin',exact:true}).getAttribute('href'),older.url());
  const redirect = await fixtureResponse(authorization);
  assert.equal(redirect.status,302);
  const olderSelection = older.goto(redirect.headers.location,{timeout:60000});
  olderSelection.catch(() => {});
  await callback.ready;
  console.log('Held admitted callback response before Chromium received its session.');
  initial.release();
  await newerNavigation;
  await choose(newer,'Viewer');
  const logout = newer.waitForResponse((response) => response.url() === base+'/api/auth/logout');
  await newer.getByTestId('logout').click();
  assert.equal((await logout).status(),204);
  assert.equal((await auth(newer)).authenticated,false);
  callback.release();
  await olderSelection;
  await older.waitForURL(base+'/');
  assert.equal((await auth(older)).authenticated,false,'A delayed callback session cookie must not restore authentication after logout');
  assert.equal(await older.evaluate(async () => (await fetch('/api/bootstrap')).status),401);
  assert.equal((await auth(independent)).role,'admin','Logout must not revoke another browser');
  await newer.goto(base+'/api/auth/login');
  await choose(newer,'Admin');
  const cookies = await context.cookies(base);
  for (const name of ['__Host-rsdw-session','__Host-rsdw-login']) {
    const cookie = cookies.find((item) => item.name === name);
    assert.ok(cookie?.secure && cookie.httpOnly && cookie.sameSite === 'Lax',name);
  }
  console.log('PASS Chromium HTTPS OIDC login, delayed callback after logout, protected-route rejection, independent browser isolation, and fresh login.');
}

let deadline;
Promise.race([run(),new Promise((_,reject) => {deadline = setTimeout(() => reject(new Error('OIDC browser check timed out')),90000);})]).catch((error) => {console.error(error);console.error(logs);process.exitCode=1;}).finally(async () => {
  clearTimeout(deadline);
  if (browser) await browser.close();
  if (fixture.exitCode === null) process.kill(-fixture.pid,'SIGINT');
  await exited;
});
