const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const test = require('node:test');

const script = path.join(__dirname, 'fixture-chart/files/seed-save.sh');
const runImport = (source, name, world) => spawnSync('bash', [script, source, name, world], {encoding:'utf8'});

test('fixture import survives a fresh importer process and preserves changed saves and markers', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'rsdw-save-import-'));
  try {
    const seed = path.join(root, 'seed'), world = path.join(root, 'world');
    fs.mkdirSync(seed);
    const content = Buffer.from('GVAS\0private-save-fixture\xff', 'binary');
    fs.writeFileSync(path.join(seed, 'World.sav'), content, {mode:0o600});
    const imported = runImport(seed, 'World.sav', world);
    assert.equal(imported.status, 0, imported.stderr);
    assert.deepEqual(fs.readFileSync(path.join(world, 'World.sav')), content);
    assert.equal(fs.statSync(path.join(world, 'World.sav')).mode & 0o777, 0o600);
    assert.deepEqual(fs.readdirSync(world).sort(), ['.seed-complete','World.sav']);
    assert.doesNotMatch(imported.stdout + imported.stderr, /private-save-fixture/);
    fs.writeFileSync(path.join(world, 'World.sav'), 'world-after-playing');
    assert.equal(runImport(seed, 'World.sav', world).status, 0);
    assert.equal(fs.readFileSync(path.join(world, 'World.sav'), 'utf8'), 'world-after-playing');
    fs.unlinkSync(path.join(world, 'World.sav'));
    assert.equal(runImport(seed, 'World.sav', world).status, 0);
    assert.deepEqual(fs.readdirSync(world), ['.seed-complete']);
    fs.unlinkSync(path.join(world, '.seed-complete'));
    fs.writeFileSync(path.join(world, 'Existing.SAV'), 'existing-world');
    assert.equal(runImport(seed, 'World.sav', world).status, 0);
    assert.deepEqual(fs.readdirSync(world), ['Existing.SAV']);
    assert.equal(fs.readFileSync(path.join(world, 'Existing.SAV'), 'utf8'), 'existing-world');
  } finally { fs.rmSync(root, {recursive:true,force:true}); }
});

test('fixture importer rejects escaped symlinks, traversal, directories, and unsupported files', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'rsdw-save-invalid-'));
  try {
    const seed = path.join(root, 'seed'), world = path.join(root, 'world');
    fs.mkdirSync(seed);
    fs.writeFileSync(path.join(root, 'outside.sav'), 'outside');
    fs.symlinkSync('../outside.sav', path.join(seed, 'escaped.sav'));
    fs.mkdirSync(path.join(seed, 'directory.sav'));
    fs.writeFileSync(path.join(seed, 'archive.zip'), 'zip');
    for (const name of ['../outside.sav', path.join(root,'outside.sav'), 'escaped.sav', 'directory.sav', 'archive.zip']) {
      const result = runImport(seed, name, world);
      assert.equal(result.status, 1, `${name}: ${result.stderr}`);
      assert.equal(fs.existsSync(world), false);
    }
    assert.equal(fs.readFileSync(path.join(root,'outside.sav'),'utf8'), 'outside');
  } finally { fs.rmSync(root, {recursive:true,force:true}); }
});

test('rendered C2 RBAC limits cleanup deletion to pods and PVCs', () => {
  const result = spawnSync('helm', ['template', 'save-test', 'charts/rsdw-c2', '--set','auth.adminTokenSecret.name=admin', '--show-only','templates/rbac.yaml'], {encoding:'utf8'});
  assert.equal(result.status, 0, result.stderr);
  const deleteRules = result.stdout.split(/\n  - apiGroups:/).filter(rule => /^\s+verbs:.*"delete"/m.test(rule));
  assert.equal(deleteRules.length, 1);
  assert.match(deleteRules[0], /resources: \["pods", "persistentvolumeclaims"\]/);
  assert.match(deleteRules[0], /verbs: \["delete"\]/);
  assert.doesNotMatch(result.stdout, /"\*"|"deletecollection"/);
});

test('C2 chart enables uploads only with persistent state', () => {
  for (const enabled of ['true','false']) {
    const result = spawnSync('helm',['template','save-test','charts/rsdw-c2','--set','auth.adminTokenSecret.name=admin','--set',`state.persistence.enabled=${enabled}`,'--show-only','templates/deployment.yaml'],{encoding:'utf8'});
    assert.equal(result.status,0,result.stderr);
    assert.match(result.stdout,new RegExp(`name: RSDW_SAVE_UPLOADS_ENABLED\\s+value: "${enabled}"`));
  }
});

test('fixture deployment mounts the seed read-only and the world on persistent storage', () => {
  const args = ['template','seed-fixture',path.join(__dirname,'fixture-chart'),'--namespace','other-worlds','--set','image.repository=fixture,image.tag=test,api.bearerTokenSecret.name=api,server.env.RSDW_ADDITIONAL_ARGS='];
  const empty = spawnSync('helm',args,{encoding:'utf8'});
  assert.equal(empty.status,0,empty.stderr);
  assert.doesNotMatch(empty.stdout,/name: import-save|kind: PersistentVolumeClaim/);
  const seeded = spawnSync('helm',[...args,'--set','saveSeed.existingClaim=request-seed,saveSeed.path=World.sav'],{encoding:'utf8'});
  assert.equal(seeded.status,0,seeded.stderr);
  assert.match(seeded.stdout,/name: import-save/);
  assert.match(seeded.stdout,/name: seed, mountPath: \/seed, readOnly: true/);
  assert.match(seeded.stdout,/claimName: request-seed\s+readOnly: true/);
  assert.match(seeded.stdout,/claimName: seed-fixture-world/);
  assert.match(seeded.stdout,/kind: PersistentVolumeClaim/);
  assert.match(seeded.stdout,/runAsUser: 1000/);
});
