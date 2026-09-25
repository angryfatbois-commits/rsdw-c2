const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const {chromium} = require('playwright');
const root = path.resolve(__dirname, '..');
const webRoot = path.resolve(process.env.RSDW_UI_ROOT || root, 'web');
fs.mkdirSync(path.join(root, '.tmp'), {recursive:true});
const output = fs.mkdtempSync(path.join(root, '.tmp/backups-ui-'));
const server = http.createServer((request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname;
  const name = pathname === '/' ? 'index.html' : pathname.slice(1);
  const file = path.join(webRoot, name);
  if (!file.startsWith(webRoot + '/') || !fs.existsSync(file)) { response.writeHead(404).end(); return; }
  response.setHeader('Content-Type', name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html');
  response.end(fs.readFileSync(file));
});
const servers = [{id:'live',worldName:'Aurora Expanse',name:'Owner',status:'online'}, {id:'stopped',worldName:'Ember Hollow',name:'Owner',status:'stopped'}];
const profile = (id, name, sources = [{serverState:'running',collector:'server-files',path:'world.sav.backup'}, {serverState:'stopped',collector:'server-files',path:'world.sav'}]) => ({apiVersion:'rsdw-c2.petzko.dev/v1',kind:'BackupDefinition',id,name,revision:4,serverType:'dragonwilds',strategy:'logical-files',items:[{name:'world-save',kind:'file',requirement:'required',sources}]});
let profiles = [profile('dragonwilds-world-save','World save'), profile('stopped-only','Stopped only',[{serverState:'stopped',collector:'server-files',path:'only.sav'}]), ...Array.from({length:22},(_,i)=>profile('profile-'+i,'Profile '+i))];
const captured = {name:'world-save',kind:'file',sourcePath:'world.sav',size:42,sha256:'a'.repeat(64),objectKey:'opaque-object-1',consistency:'stopped-world'};
const multiProfile = profile('multi-required','Multiple required items');
multiProfile.items.push({name:'settings',kind:'file',requirement:'required',sources:[{serverState:'stopped',collector:'server-files',path:'settings.ini'}]});
const optionalProfile = {...multiProfile,id:'multi-optional',name:'Optional settings',items:multiProfile.items.map(item=>({...item,requirement:item.name==='settings'?'optional':'required'}))};
profiles.push(multiProfile,optionalProfile);
const runs = [{id:'done',manifestId:'done',serverId:'stopped',serverName:'Ember Hollow',definitionId:'dragonwilds-world-save',profileName:'World save',status:'succeeded',source:'stopped-sav',sourceLabel:'Stopped .sav',createdAt:'2026-09-20T12:00:00Z',items:[captured],itemCount:1,bundleSize:100}, {id:'failed',serverId:'live',serverName:'Aurora Expanse',definitionId:'dragonwilds-world-save',profileName:'World save',status:'failed',error:'Expected .sav.backup is missing',sourceLabel:'Running .sav.backup',createdAt:'2026-09-20T13:00:00Z'}];
let schedules = [{id:'daily',serverId:'stopped',definitionId:'stopped-only',backendId:'local',enabled:true,mode:'daily',dailyTimes:['02:00','14:30'],timezone:'Asia/Tokyo',timing:'Daily 02:00, 14:30',nextRun:'2026-09-21T12:00:00Z',lastResult:'succeeded'}, {id:'hourly',serverId:'live',definitionId:'dragonwilds-world-save',backendId:'local',enabled:true,mode:'interval',intervalValue:12,intervalUnit:'hours',timezone:'UTC',timing:'Every 12 hours'}];
let backends = [];
let requests = [], restoreAttempts = [], viewer = false, rejectRestore = true;
let healthFixture;
let browser, page;
async function main() {
  await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
  const base = 'http://127.0.0.1:' + server.address().port;
  browser = await chromium.launch({headless:true,executablePath:process.env.RSDW_TEST_CHROMIUM});
  page = await browser.newPage({viewport:{width:1600,height:1300}});
  const errors=[];
  page.on('pageerror', error=>errors.push(error.stack));
  await page.route('**/api/**',async route=> {
    const request = route.request(), url = new URL(request.url()).pathname, method=request.method();
    const reply=(data,status=200)=>route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
    if (url === '/api/auth') return reply({mode:'token',authenticated:true,required:false,subject:viewer?'viewer':'admin',role:viewer?'viewer':'admin',capabilities:viewer?{telemetry:true}:{backups:true,maintenance:true,telemetry:true,create:true}});
    if (url === '/api/bootstrap') return reply({servers,mode:'demo',cluster:'UI contract fixture'});
    if (url === '/api/users') return reply({users:[]});
    if (url === '/api/backups' && method === 'GET') return reply({runs,profiles,schedules,available:true,storage:[{id:'local',name:'Local',kind:'local',status:'connected',removable:false},...backends],limits:{usedBytes:100,repositoryBytes:1000000},...healthFixture});
    if (url.endsWith('/download')) return route.fulfill({status:200,contentType:'application/zip',body:Buffer.from('fixture bundle')});
    if (url.endsWith('/export')) return route.fulfill({status:200,contentType:'text/yaml',body:'apiVersion: rsdw-c2.petzko.dev/v1\nkind: BackupDefinition\nid: stopped-only\n'});
    if (url.endsWith('/restore')) {
      const raw=request.postData();
      restoreAttempts.push(raw);
      if (rejectRestore && !raw.includes('"destructiveConfirm":true')) return reply({code:'restore_confirmation_required',error:'Target contains save data. Confirm replacement.'},409);
      return reply({status:'restored'},202);
    }
    const body=request.postDataJSON();
    requests.push({url,method,body});
    if (url === '/api/backups/profiles/import') {
      if (body.yaml.includes('duplicate')) return reply({error:'Profile already exists'},409);
      if (body.yaml.includes('minecraft')) return reply({error:'Unsupported server type'},400);
      profiles.push(profile('imported','Imported',[{serverState:'running',collector:'server-files',path:'imported.sav.backup'}]));
      return reply(profiles.at(-1),201);
    }
    if (url === '/api/backups/profiles' && method === 'POST') {
      if (profiles.some(p=>p.id===body.id)) return reply({error:'Profile already exists'},409);
      profiles.push(body); return reply(body);
    }
    if (url.startsWith('/api/backups/profiles/')) {
      const id=url.split('/').at(-1), current=profiles.find(p=>p.id===id);
      if (method==='DELETE') {
        if (schedules.some(s=>s.definitionId===id)) return reply({error:'Profile is referenced by a schedule'},409);
        profiles=profiles.filter(p=>p.id!==id);return reply({});
      }
      assert.equal(body.revision,current.revision);
      profiles=profiles.map(p=>p.id===id?{...body,revision:body.revision+1}:p);return reply(profiles.find(p=>p.id===id));
    }
    if (url === '/api/backups/schedules' && method === 'POST') {const added={...body,id:'added',timezone:body.executionTimezone};schedules.push(added);return reply(added);}
    if (url.startsWith('/api/backups/schedules/')) {
      const id=url.split('/')[4];
      if (url.endsWith('/run')) return reply({id:'extra',scheduleId:id},201);
      if (method==='DELETE') {schedules=schedules.filter(s=>s.id!==id);return reply({});}
      schedules=schedules.map(s=>s.id===id?{...body,id,timezone:body.executionTimezone}:s);return reply(schedules.find(s=>s.id===id));
    }
    if (url === '/api/backups/backends' && method === 'POST') {
      if (backends.some(b=>b.id===body.id)) return reply({error:'a storage backend with this id already exists'},400);
      const added={id:body.id,name:body.name,kind:'s3',status:'connected',removable:true,endpoint:body.endpoint,bucket:body.bucket};
      backends.push(added);return reply(added,201);
    }
    if (url.startsWith('/api/backups/backends/') && method === 'DELETE') {
      const id=url.split('/').at(-1);
      if (schedules.some(s=>s.enabled&&s.backendId===id)) return reply({error:'storage backend is used by an enabled schedule; disable or reassign it first'},409);
      backends=backends.filter(b=>b.id!==id);return reply({});
    }
    if (url === '/api/backups/runs') return reply({id:'extra',...body},201);
    if (url.endsWith('/create-server')) {assert.match(body.ownerId,/^[a-f0-9]{32}$/);return reply({id:'clone',worldName:body.serverName,name:body.ownerName,status:'stopped'},201);}
    return reply({});
  });
  const go=async hash=> {await page.goto(base+'/#'+hash);await page.locator('#content[aria-busy="false"]').waitFor();};
  const cancel=()=>page.getByTestId('cancel-modal').click();
  const save=async()=>{await page.getByTestId('confirm-modal').click();await page.locator('#modal').waitFor({state:'hidden'});};
  const choose=async id=>{await page.locator('[data-action="select-backup-profile"][data-id="'+id+'"]').click();};
  const healthySchedule = {...schedules[1],lastResult:undefined};
  const terminal = {...runs[1],backendId:'local'};
  const recovered = {...terminal,id:'recovered',status:'succeeded',error:'',createdAt:'2026-09-20T14:00:00Z'};
  const interrupted = {...terminal,id:'interrupted',status:'interrupted',error:'Process restarted',createdAt:'2026-09-20T15:00:00Z'};
  const cases = [
    {name:'no schedules has a neutral indicator',runs:[],schedules:[],healthy:'0 of 0',attention:false,indicator:'idle'},
    {name:'disabled schedules remain neutral even when disconnected after failure',runs:[terminal],schedules:[{...healthySchedule,enabled:false}],available:false,healthy:'0 of 1',attention:true,indicator:'idle'},
    {name:'never-run schedule is not healthy',runs:[],healthy:'0 of 1',attention:false,indicator:'attention'},
    {name:'first run in progress is not yet healthy',runs:[{...terminal,status:'running'}],healthy:'0 of 1',attention:false,indicator:'attention'},
    {name:'historical failure followed by success needs no attention',runs:[terminal,recovered],healthy:'1 of 1',attention:false,indicator:''},
    {name:'disabled schedules do not prevent green for healthy enabled schedules',runs:[recovered],schedules:[healthySchedule,{...healthySchedule,id:'disabled',definitionId:'stopped-only',enabled:false}],healthy:'1 of 2',attention:false,indicator:''},
    {name:'one healthy schedule cannot hide another never-run schedule',runs:[recovered],schedules:[healthySchedule,{...healthySchedule,id:'new',definitionId:'stopped-only'}],healthy:'1 of 2',attention:false,indicator:'attention'},
    {name:'newer in-progress run preserves latest terminal success',runs:[terminal,{...recovered,id:'pending',status:'running',createdAt:'2026-09-20T15:00:00Z'},recovered],healthy:'1 of 1',attention:false,indicator:''},
    {name:'success on another profile does not clear failure',runs:[terminal,{...recovered,definitionId:'stopped-only'}],healthy:'0 of 1',attention:true,indicator:'attention'},
    {name:'success on another backend does not clear failure',runs:[terminal,{...recovered,backendId:'another-backend'}],healthy:'0 of 1',attention:true,indicator:'attention'},
    {name:'unavailable storage prevents healthy status',runs:[recovered],available:false,healthy:'0 of 1',attention:false,indicator:'attention'},
    {name:'interrupted run supersedes older failure',runs:[terminal,interrupted],healthy:'0 of 1',attention:true,attentionIds:['interrupted'],indicator:'attention'},
    {name:'attention isolates server profile and backend and ignores recovered history',runs:[terminal,recovered,{...interrupted,id:'other-server',serverId:'stopped'},{...interrupted,id:'other-profile',definitionId:'stopped-only'},{...terminal,id:'other-backend',backendId:'remote'}],healthy:'1 of 1',attention:true,attentionIds:['other-backend','other-profile','other-server'],indicator:'attention'},
  ];
  for (const scenario of cases) {
    healthFixture={runs:scenario.runs,schedules:scenario.schedules || [healthySchedule],available:scenario.available !== false};
    await page.goto(base+'/?health='+encodeURIComponent(scenario.name)+'#backups');
    await page.getByTestId('backup-health').waitFor();
    assert.equal(await page.locator('.healthy-count').innerText(),scenario.healthy,scenario.name);
    const indicator = page.locator('.health-indicator');
    assert.equal((await indicator.getAttribute('class')).trim(),('health-indicator '+scenario.indicator).trim(),scenario.name);
    const expectedColor = scenario.indicator === 'idle' ? '--color-muted-foreground' : scenario.indicator === 'attention' ? '--warning' : '--success';
    assert.equal(await indicator.evaluate((node,token)=>{
      const swatch=document.createElement('span');
      swatch.style.backgroundColor='var('+token+')';
      node.append(swatch);
      const matches=getComputedStyle(node).backgroundColor===getComputedStyle(swatch).backgroundColor;
      swatch.remove();
      return matches;
    },expectedColor),true,scenario.name+' indicator color');
    assert.equal(await page.locator('.backup-attention-row').count(),Number(scenario.attention),scenario.name);
    assert.equal(await page.getByTestId('backup-attention').count(),Number(scenario.attention),scenario.name);
    if (scenario.attention) {
      await page.getByTestId('backup-attention').click();
      const ids = await page.locator('.backup-history tbody tr').evaluateAll(rows=>rows.map(row=>row.dataset.backupId).sort());
      assert.deepEqual(ids,scenario.attentionIds || ['failed'],scenario.name);
      if (ids.includes('interrupted')) {
        const row = page.locator('[data-backup-id="interrupted"]');
        assert.equal(await row.locator('.result').innerText(),'Interrupted');
        assert.equal(await row.getByRole('button',{name:'Retry',exact:true}).isEnabled(),true);
        const retry = page.waitForRequest(request=>request.url().endsWith('/api/backups/runs') && request.method()==='POST');
        await row.getByRole('button',{name:'Retry',exact:true}).click();
        assert.deepEqual((await retry).postDataJSON(),{serverId:'live',definitionId:'dragonwilds-world-save',backendId:'local',acknowledge:true});
        await page.getByText('Backup retry started.',{exact:true}).waitFor();
      }
      await page.getByRole('button',{name:'Show all results'}).click();
    }
    console.log('PASS '+scenario.name);
  }
  healthFixture=undefined;
  if (process.env.RSDW_UI_HEALTH_ONLY === '1') {assert.deepEqual(errors,[]);return;}
  await go('backups');await page.getByTestId('backup-health').waitFor();
  const health=await page.getByTestId('backup-health').innerText();
  for(const label of ['Next backup','Last successful','Coverage','Needs attention']) assert.ok(health.includes(label));
  const panels=await page.locator('.backup-overview-grid > *').evaluateAll(nodes=>nodes.map(n=>({x:n.getBoundingClientRect().x,width:n.getBoundingClientRect().width})));
  assert.equal(panels[0].x,panels[1].x);assert.equal(panels[0].width,panels[1].width);
  await page.locator('#backup-server-filter').selectOption('stopped');assert.equal(await page.locator('.backup-history tbody tr').count(),1);
  await page.locator('#backup-server-filter').selectOption('');await page.getByTestId('backup-attention').click();assert.equal(await page.locator('.backup-history tbody tr').count(),1);assert.match(await page.locator('.backup-history').innerText(),/Expected .sav.backup/);
  await page.getByRole('button',{name:'Show all results'}).click();
  await page.screenshot({path:path.join(output,'overview.png'),fullPage:true});
  await page.getByTestId('backup-details-done').click();
  assert.ok((await page.locator('#modal-body').innerText()).includes(captured.sha256));assert.ok((await page.locator('#modal-body').innerText()).includes(captured.objectKey));
  let download=page.waitForEvent('download');await save();assert.match((await download).suggestedFilename(),/zip$/);
  await page.getByTestId('backup-details-failed').click();assert.equal(await page.getByTestId('confirm-modal').innerText(),'Close');await save();
  await page.getByTestId('run-backup-header').click();await page.getByTestId('backup-run-server').selectOption('stopped');assert.match(await page.locator('.schedule-preview').innerText(),/Read flat .sav/);
  await page.getByTestId('backup-run-server').selectOption('live');assert.match(await page.locator('.schedule-preview').innerText(),/Server-created .sav.backup/);
  await page.getByTestId('backup-run-profile').selectOption('stopped-only');assert.match(await page.locator('.schedule-preview').innerText(),/No running rule/);
  await page.getByTestId('backup-run-profile').selectOption('multi-required');
  assert.match(await page.locator('.schedule-preview').innerText(),/No running rule for required items: settings/);
  assert.doesNotMatch(await page.locator('.schedule-preview .source-chip').innerText(),/Server-created/);
  await page.getByTestId('backup-run-server').selectOption('stopped');
  assert.match(await page.locator('.schedule-preview').innerText(),/world-save: world.sav; settings: settings.ini/);
  await page.getByTestId('backup-run-server').selectOption('live');
  await page.getByTestId('backup-run-profile').selectOption('multi-optional');
  assert.equal(await page.locator('.schedule-preview .source-chip').innerText(),'Server-created .sav.backup');
  assert.match(await page.locator('.schedule-preview').innerText(),/settings: No running rule \(optional, skipped\)/);
  await page.getByTestId('backup-run-profile').selectOption('dragonwilds-world-save');await page.screenshot({path:path.join(output,'run-form.png'),fullPage:true});await save();assert.equal(requests.at(-1).body.serverId,'live');
  await page.locator('[data-action="retry-backup"]').click();assert.equal(requests.at(-1).url,'/api/backups/runs');
  await go('backups/settings/profiles');await page.getByTestId('backup-profile-list').waitFor();
  await page.waitForFunction(()=>document.querySelector('.profile-list')?.clientHeight>0);
  const list=await page.evaluate(()=>{const n=document.querySelector('.profile-list');return {height:n.clientHeight,total:n.scrollHeight,row:n.querySelector('.profile-row').getBoundingClientRect().height};});
  assert.ok(list.total>list.height,JSON.stringify(list));assert.ok(list.row<=72,JSON.stringify(list));
  await page.getByTestId('backup-profile-search').fill('Stopped only');assert.equal(await page.locator('.profile-row').count(),1);await page.getByTestId('clear-backup-profile-filter').click();
  await page.screenshot({path:path.join(output,'profiles.png'),fullPage:true});
  await choose('stopped-only');await page.getByRole('button',{name:'Edit profile',exact:true}).click();assert.equal(await page.getByTestId('backup-running-path-0').inputValue(),'');
  assert.doesNotMatch(await page.getByTestId('backup-yaml-preview').innerText(),/serverState: "running"/);
  await page.getByTestId('backup-profile-name').fill('Stopped only edited');await save();
  assert.equal(profiles.find(p=>p.id==='stopped-only').revision,5);assert.deepEqual(profiles.find(p=>p.id==='stopped-only').items[0].sources,[{serverState:'stopped',collector:'server-files',path:'only.sav'}]);
  await choose('stopped-only');await page.getByRole('button',{name:'Delete profile',exact:true}).click();await page.getByTestId('confirm-modal').click();await page.locator('#modal-error').waitFor({state:'visible'});assert.match(await page.locator('#modal-error').innerText(),/referenced/);await cancel();
  await choose('profile-0');await page.getByRole('button',{name:'Delete profile',exact:true}).click();await save();assert.equal(profiles.some(p=>p.id==='profile-0'),false);
  await page.getByTestId('new-backup-profile').click();await page.getByTestId('backup-profile-name').fill('New profile');await page.getByTestId('backup-running-path-0').fill('');await page.getByTestId('backup-stopped-path-0').fill('');await page.getByTestId('confirm-modal').click();await page.locator('#modal-error').waitFor({state:'visible'});assert.match(await page.locator('#modal-error').innerText(),/Each item needs/);await page.getByTestId('backup-stopped-path-0').fill('world.sav');await page.screenshot({path:path.join(output,'profile-form.png'),fullPage:true});await save();assert.equal(profiles.at(-1).items[0].sources.length,1);
  await page.getByTestId('new-backup-profile').click();await page.getByTestId('backup-profile-name').fill('New profile');await page.getByTestId('confirm-modal').click();await page.locator('#modal-error').waitFor({state:'visible'});assert.match(await page.locator('#modal-error').innerText(),/already exists/);await cancel();
  await page.getByTestId('import-backup-profile').click();await page.getByTestId('backup-profile-yaml').setInputFiles({name:'profile.yaml',mimeType:'text/yaml',buffer:Buffer.from('running-only')});await save();
  await page.getByTestId('import-backup-profile').click();await page.getByTestId('backup-profile-yaml').setInputFiles({name:'unsupported.yaml',mimeType:'text/yaml',buffer:Buffer.from('serverType: minecraft')});await page.getByTestId('confirm-modal').click();await page.locator('#modal-error').waitFor({state:'visible'});assert.match(await page.locator('#modal-error').innerText(),/Unsupported server type/);await cancel();
  await choose('imported');await page.getByRole('button',{name:'Edit profile',exact:true}).click();assert.equal(await page.getByTestId('backup-stopped-path-0').inputValue(),'');await cancel();
  download=page.waitForEvent('download');await page.locator('[data-action="export-backup-profile"]').click();assert.match((await download).suggestedFilename(),/yaml$/);
  await page.getByRole('link',{name:'Storage',exact:true}).click();await page.getByTestId('storage-card-local').waitFor();
  assert.equal(await page.getByTestId('delete-storage-backend-local').count(),0);
  await page.getByTestId('add-storage-backend').click();
  await page.getByTestId('storage-backend-name').fill('Offsite mirror');
  await page.getByTestId('storage-backend-endpoint').fill('s3.example.com');
  await page.getByTestId('storage-backend-bucket').fill('rsdw-backups');
  await page.getByTestId('storage-backend-secret-name').fill('rsdw-s3-offsite');
  await save();
  await page.getByTestId('storage-card-offsite-mirror').waitFor();
  assert.equal(requests.at(-1).url,'/api/backups/backends');
  assert.deepEqual(requests.at(-1).body.secretRef,{name:'rsdw-s3-offsite',key:'accessKeyId'});
  await page.getByTestId('add-storage-backend').click();
  await page.getByTestId('storage-backend-name').fill('Offsite mirror');
  await page.getByTestId('storage-backend-endpoint').fill('s3.example.com');
  await page.getByTestId('storage-backend-bucket').fill('rsdw-backups');
  await page.getByTestId('storage-backend-secret-name').fill('rsdw-s3-offsite');
  await page.getByTestId('confirm-modal').click();
  await page.locator('#modal-error').waitFor({state:'visible'});
  assert.match(await page.locator('#modal-error').innerText(),/already exists/);
  await cancel();
  await page.locator('[data-action="delete-storage-backend"][data-id="offsite-mirror"]').click();
  await page.getByTestId('delete-storage-backend-confirm').check();
  await save();
  assert.equal(await page.getByTestId('storage-card-offsite-mirror').count(),0);
  assert.equal(requests.at(-1).url,'/api/backups/backends/offsite-mirror');
  await go('backups/schedules');await page.getByTestId('run-schedule-daily').waitFor();
  await page.getByTestId('add-backup-schedule').click();
  await page.getByTestId('backup-schedule-profile').selectOption('multi-required');
  assert.match(await page.locator('#backup-schedule-preview').innerText(),/settings: No running rule \(required, run will fail\)/);
  assert.match(await page.locator('#backup-schedule-preview').innerText(),/world-save: world.sav; settings: settings.ini/);
  await cancel();
  for (const mode of ['daily','cron']) {
    await page.locator('[data-action="edit-backup-schedule"][data-id="hourly"]').click();
    await page.getByTestId('backup-schedule-mode').selectOption('interval');
    await page.locator('[name="intervalValue"]').fill('0');
    assert.equal(await page.locator('#modal-form').evaluate(form=>form.checkValidity()),false);
    await page.getByTestId('backup-schedule-mode').selectOption(mode);
    assert.equal(await page.locator('[name="intervalValue"]').isDisabled(),true);
    assert.equal(await page.locator('[name="intervalUnit"]').isDisabled(),true);
    assert.equal(await page.locator('#modal-form').evaluate(form=>form.checkValidity()),true);
    await save();
    assert.equal(requests.at(-1).body.mode,mode);
    assert.equal('intervalValue' in requests.at(-1).body,false);
    assert.equal('intervalUnit' in requests.at(-1).body,false);
    assert.deepEqual(mode==='daily'?requests.at(-1).body.dailyTimes:requests.at(-1).body.cron,mode==='daily'?['02:00']:'0 2 * * *');
    console.log('PASS invalid hidden interval permits '+mode+' submission');
  }
  await page.locator('[data-action="edit-backup-schedule"][data-id="hourly"]').click();
  await page.getByTestId('backup-schedule-mode').selectOption('interval');
  assert.equal(await page.locator('[name="intervalValue"]').isEnabled(),true);
  assert.equal(await page.locator('[name="intervalUnit"]').isEnabled(),true);
  await page.locator('[name="intervalValue"]').fill('12');
  await page.locator('[name="intervalUnit"]').selectOption('hours');
  await save();
  await page.locator('[data-action="edit-backup-schedule"][data-id="daily"]').click();assert.deepEqual(await page.getByTestId('backup-schedule-time').evaluateAll(nodes=>nodes.map(n=>n.value)),['02:00','14:30']);assert.equal(await page.getByTestId('backup-schedule-timezone').inputValue(),'Asia/Tokyo');assert.ok(await page.getByTestId('backup-schedule-timezone').locator('option').count()>100);await save();assert.deepEqual(schedules.find(s=>s.id==='daily').dailyTimes,['02:00','14:30']);
  await page.locator('[data-action="edit-backup-schedule"][data-id="hourly"]').click();assert.equal(await page.locator('[name="intervalUnit"]').inputValue(),'hours');await save();assert.equal(schedules.find(s=>s.id==='hourly').intervalUnit,'hours');
  await page.getByTestId('add-backup-schedule').click();await page.getByRole('button',{name:'Add another time'}).click();await page.getByTestId('backup-schedule-time').nth(1).fill('17:00');await page.getByTestId('backup-schedule-confirm').check();await save();assert.deepEqual(schedules.at(-1).dailyTimes,['02:00','17:00']);
  await page.getByTestId('run-schedule-daily').click();assert.match(await page.locator('#modal-body').innerText(),/unchanged/);await save();assert.equal(requests.at(-1).url,'/api/backups/schedules/daily/run');
  await page.locator('#backup-schedule-menu').selectOption('added');await page.getByTestId('delete-backup-schedule-menu').click();await page.getByTestId('delete-backup-confirm').check();await save();assert.equal(schedules.some(s=>s.id==='added'),false);
  await page.screenshot({path:path.join(output,'schedules.png'),fullPage:true});
  runs.push({...runs[0],id:'running-copy',manifestId:'running-manifest',definitionId:'imported',profileName:'Imported running save',source:'running-bak',sourceLabel:'Running .sav.backup',items:[{...captured,sourcePath:'imported.sav.backup'}]});
  await go('maintenance?serverId=stopped');await page.getByTestId('restore-server').click();
  const metadata=page.getByTestId('restore-backup-metadata');
  assert.deepEqual(await metadata.locator('dt').allTextContents(),['Profile','Source']);
  assert.deepEqual(await metadata.locator('dd').allTextContents(),['World save','Stopped .sav']);
  await page.getByTestId('restore-backup-select').selectOption('running-manifest');
  assert.deepEqual(await metadata.locator('dd').allTextContents(),['Imported running save','Running .sav.backup']);
  await page.getByTestId('restore-custom').check();
  assert.equal(await metadata.isVisible(),false);
  await page.getByTestId('restore-from-backup').check();
  assert.deepEqual(await metadata.locator('dd').allTextContents(),['Imported running save','Running .sav.backup']);
  await page.getByTestId('restore-backup-select').selectOption('done');
  assert.deepEqual(await metadata.locator('dd').allTextContents(),['World save','Stopped .sav']);
  assert.match(await page.locator('.restore-warning').innerText(),/C2 verifies whether the target is empty\. Replacing existing save data requires a separate confirmation\./);
  assert.equal(await page.getByTestId('restore-empty-confirm').locator('..').innerText(),'I confirm the server is stopped and authorize C2 to inspect the target for existing save data.');
  assert.doesNotMatch(await page.locator('#modal-body').innerText(),/target world contains no save data|verify the target is empty before restoring/);
  await page.getByTestId('restore-empty-confirm').check();await page.getByTestId('confirm-modal').click();await page.getByTestId('restore-destructive-confirm').waitFor();assert.equal(restoreAttempts.length,1);
  assert.match(restoreAttempts[0],/"confirmEmpty":true/);
  assert.match(restoreAttempts[0],/"destructiveConfirm":false/);
  assert.match(restoreAttempts[0],/"manifestId":"done"/);
  console.log('PASS reactive restore profile/source metadata and inspection consent before separate replacement confirmation');
  await page.getByTestId('confirm-modal').click();assert.equal(restoreAttempts.length,1);await page.getByTestId('restore-destructive-confirm').check();await save();assert.match(restoreAttempts[1],/"destructiveConfirm":true/);assert.match(restoreAttempts[1],/"confirmEmpty":true/);assert.match(await page.locator('#toast').innerText(),/restored successfully/);
  rejectRestore=false;await page.getByTestId('restore-server').click();await page.getByTestId('restore-custom').check();await page.getByTestId('restore-custom-file').setInputFiles({name:'custom.sav',mimeType:'application/octet-stream',buffer:Buffer.from('world bytes')});await page.getByTestId('restore-empty-confirm').check();await save();assert.match(restoreAttempts.at(-1),/world bytes/);assert.match(restoreAttempts.at(-1),/"destructiveConfirm":false/);
  await page.getByTestId('restore-server').click();await page.getByTestId('create-server-from-backup').click();await page.getByTestId('create-from-backup-name').fill('New world');await page.getByTestId('create-from-backup-owner').fill('a'.repeat(32));await page.locator('[name="confirmCreate"]').check();await save();assert.equal(requests.at(-1).body.ownerId,'a'.repeat(32));assert.equal(requests.at(-1).body.serverName,'New world');
  await page.setViewportSize({width:375,height:812});await go('backups');await page.getByTestId('backup-health').waitFor();assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await page.screenshot({path:path.join(output,'mobile.png'),fullPage:true});
  viewer=true;await go('backups');await page.getByRole('heading',{name:'Servers',exact:true}).waitFor();assert.equal(await page.getByTestId('run-backup-header').count(),0);assert.equal(await page.locator('a[href="#backups"]').count(),0);
  assert.deepEqual(errors,[]);console.log('PASS backup UI contract, forms, revisions, actions, restore confirmation, manifest details, responsive layout, and viewer restrictions.');console.log(output);
}
main().catch(async error=>{console.error(error);if(page){console.error((await page.locator('body').innerText()).slice(-3000));await page.screenshot({path:path.join(output,'failure.png'),fullPage:true});}process.exitCode=1;}).finally(async()=>{await browser?.close();server.close();});
