import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { analyzeCommits } from '@semantic-release/commit-analyzer';
import { parse } from 'yaml';
import { release } from './release.mjs';
import config from '../release.config.cjs';

const sha = 'a'.repeat(40);
const env = {
  ...process.env,
  GITHUB_EVENT_NAME: 'push',
  GITHUB_REF: 'refs/heads/main',
  GITHUB_REPOSITORY: 'petzkod5/rsdw-c2',
  GITHUB_SHA: sha,
  GITHUB_RUN_ID: '123',
  GITHUB_RUN_ATTEMPT: '1',
  GITHUB_ACTOR: 'offline-test',
  GH_TOKEN: 'offline-test',
};

test('workflow has one verified main release path and builds PRs without publication', () => {
  const workflow = parse(readFileSync('.github/workflows/ci.yml', 'utf8'));
  assert.deepEqual(workflow.on, { pull_request: null, push: { branches: ['main'] } });
  assert.deepEqual(workflow.permissions, { contents: 'read' });
  assert.equal(workflow.concurrency['cancel-in-progress'], false);
  assert.equal(workflow.concurrency.queue, 'max');
  assert.equal(workflow.concurrency.group, 'ci-${{ github.ref }}');
  const { verify, image, release: publishing } = workflow.jobs;
  assert(verify.steps.some(step => step.run === 'bash scripts/verify.sh'));
  assert.equal(image.needs, 'verify');
  const build = image.steps.find(step => step.uses?.startsWith('docker/build-push-action@'));
  assert.equal(build.with.push, false);
  assert.equal(build.with.platforms, 'linux/amd64');
  assert.equal(build.with.load, true);
  assert.equal(build.with.tags, 'ghcr.io/petzkod5/rsdw-c2:0.0.0');
  for (const job of [image, publishing]) {
    const setup = job.steps.find(step => step.uses?.startsWith('helm/kind-action@'));
    assert.equal(setup.with.install_only, true);
  }
  const install = image.steps.at(-1);
  assert.match(install.run, /helm package charts\/rsdw-c2 --version 0\.0\.0 --app-version 0\.0\.0/);
  assert.match(install.run, /node \.github\/check-chart\.mjs "\$RUNNER_TEMP\/rsdw-c2-0\.0\.0\.tgz" 0\.0\.0 --kind/);
  assert.equal(publishing.steps.at(-1).env.CHART_CHECK_MODE, '--kind');
  assert.deepEqual(publishing.needs, ['verify', 'image']);
  assert.equal(publishing.if, "github.event_name == 'push' && github.ref == 'refs/heads/main' && github.repository == 'petzkod5/rsdw-c2'");
  assert.deepEqual(publishing.permissions, { contents: 'write', packages: 'write' });
  assert.equal(publishing.steps[0].with['fetch-depth'], 0);
  const resolveIndex = publishing.steps.findIndex(step => step.id === 'release');
  for (const step of publishing.steps.slice(resolveIndex + 1)) {
    assert.equal(step.if, "steps.release.outputs.version != ''");
  }
  for (const job of Object.values(workflow.jobs)) {
    for (const step of job.steps.filter(step => step.uses)) assert.match(step.uses, /@[a-f0-9]{40}$/);
  }
});

test('Conventional Commits select the version and chart features qualify', async () => {
  assert.deepEqual(config.branches, ['main']);
  assert.equal(config.tagFormat, 'v${version}');
  assert.deepEqual(config.plugins.map(plugin => plugin[0]), ['@semantic-release/commit-analyzer', '@semantic-release/release-notes-generator', '@semantic-release/github']);
  assert.equal(config.plugins[2][1].successCommentCondition, false);
  assert.equal(config.plugins[2][1].failCommentCondition, false);
  for (const [message, expected] of [
    ['feat(chart): add settings', 'minor'],
    ['fix: correct readiness', 'patch'],
    ['perf: reduce polling', 'patch'],
    ['feat: change configuration\n\nBREAKING CHANGE: rename a required setting', 'major'],
    ['feat(chart)!: rename a required setting', 'major'],
    ['fix!: require an admin token', 'major'],
    ['docs: document releases', null],
    ['chore: refresh dependencies', null],
    ['ci: update runners', null],
  ]) {
    const actual = await analyzeCommits(config.plugins[0][1], { commits: [{ hash: sha, message }], cwd: process.cwd(), logger: { log() {} } });
    assert.equal(actual, expected, message);
  }
});

test('configured release notes include a literal chart feature before tag creation', async () => {
  const [name, options] = config.plugins.find(([name]) => name === '@semantic-release/release-notes-generator');
  const { generateNotes } = await import(name);
  const notes = await generateNotes(options, {
    cwd: process.cwd(),
    options: { repositoryUrl: 'https://github.com/petzkod5/rsdw-c2' },
    commits: [{ hash: sha, message: 'feat(chart): add settings' }],
    lastRelease: { version: '1.1.0', gitTag: 'v1.1.0' },
    nextRelease: { version: '1.2.0', gitTag: 'v1.2.0' },
    logger: { log() {} },
  });
  assert.equal(typeof notes, 'string');
  assert(notes.trim().length > 0);
  assert.match(notes, /Features/);
  assert.match(notes, /\*\*chart:\*\* add settings/);
  assert.match(notes, /1\.2\.0/);
});

function releaseCalls(tags, { result = false, apiError, checkout = sha } = {}) {
  const calls = [];
  return {
    calls,
    options: {
      env,
      semantic: async () => { calls.push(['semantic']); return result; },
      run(command, args) {
        calls.push([command, ...args]);
        if (command === 'git') return args[0] === 'rev-parse' ? checkout : tags;
        if (command === 'gh' && args[0] === 'api' && apiError) throw Object.assign(new Error(apiError), { stderr: apiError });
        return '';
      },
    },
  };
}

test('no release performs no GitHub writes and emits no artifact version', async () => {
  const fixture = releaseCalls('');
  assert.equal(await release(fixture.options), '');
  assert.deepEqual(fixture.calls.map(call => call[0]), ['git', 'semantic', 'git']);
});

test('new release and rerun resolve the exact existing tag', async () => {
  for (const result of [false, { releases: [] }, { nextRelease: { gitTag: 'v1.2.3' } }]) {
    const fixture = releaseCalls('v1.2.3', { result });
    assert.equal(await release(fixture.options), '1.2.3');
    assert(!fixture.calls.some(call => call.includes('create')));
  }
});

test('a tag left before GitHub release creation is resumed without creating another tag', async () => {
  const fixture = releaseCalls('v1.2.3', { apiError: 'gh: Not Found (HTTP 404)' });
  assert.equal(await release(fixture.options), '1.2.3');
  assert.deepEqual(fixture.calls.at(-1), ['gh', 'release', 'create', 'v1.2.3', '--verify-tag', '--generate-notes', '--title', 'v1.2.3']);
});

test('release rejects wrong events, checkout, ambiguous tags, and API errors', async () => {
  for (const overrides of [{ GITHUB_EVENT_NAME: 'pull_request' }, { GITHUB_REF: 'refs/heads/other' }, { GITHUB_REPOSITORY: 'other/repo' }]) {
    const fixture = releaseCalls('');
    await assert.rejects(release({ ...fixture.options, env: { ...env, ...overrides } }), /Releases require/);
    assert.equal(fixture.calls.length, 0);
  }
  await assert.rejects(release(releaseCalls('', { checkout: 'other' }).options), /verified commit/);
  await assert.rejects(release(releaseCalls('v1.2.3\nv2.0.0').options), /Multiple release tags/);
  await assert.rejects(release(releaseCalls('v1.2.3', { result: { nextRelease: { gitTag: 'v1.2.4' } } }).options), /verified commit/);
  const fixture = releaseCalls('v1.2.3', { apiError: 'HTTP 503' });
  await assert.rejects(release(fixture.options), /503/);
  assert(!fixture.calls.some(call => call.includes('create')));
  for (const invalid of ['v1.2', 'v01.2.3', 'v1.2.3-beta.1']) assert.equal(await release(releaseCalls(invalid).options), '');
});

const fakeCommand = String.raw`#!/usr/bin/env node
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const command = path.basename(process.argv[1]);
const args = process.argv.slice(2);
const state = process.env.TEST_STATE;
const image = 'ghcr.io/petzkod5/rsdw-c2:1.2.3';
const output = value => process.stdout.write(value + '\n');
const fail = message => { process.stderr.write(message + '\n'); process.exit(1); };
fs.appendFileSync(path.join(state, 'calls'), JSON.stringify([command, ...args]) + '\n');
if (command === 'git') {
  output(process.env.TEST_TAG_MISMATCH && args[1].startsWith('refs/tags/') ? 'other' : process.env.GITHUB_SHA);
} else if (command === 'curl') {
  if (process.env.TEST_FAIL === 'smoke') fail('startup failed');
  output('{"required":true}');
} else if (command === 'docker') {
  if (args[0] === 'manifest') {
    if (process.env.TEST_FAIL === 'image-auth') fail('unauthorized');
    if (process.env.TEST_FAIL === 'image-network') fail('connection refused');
    if (!fs.existsSync(path.join(state, 'image'))) fail('no such manifest: ' + image);
    output('{}');
  } else if (args[0] === 'buildx') {
    fs.writeFileSync(path.join(state, 'image'), process.env.GITHUB_SHA);
    if (process.env.TEST_FAIL === 'after-image') fail('connection lost after image push');
  } else if (args[0] === 'pull') {
    if (!fs.existsSync(path.join(state, 'image'))) fail('image missing');
  } else if (args[0] === 'image') {
    const format = args[3];
    if (format.includes('Labels')) output(fs.readFileSync(path.join(state, 'image'), 'utf8'));
    else if (format.includes('Architecture')) output('linux/amd64');
    else output('ghcr.io/petzkod5/rsdw-c2@sha256:' + 'b'.repeat(64));
  } else if (args[0] === 'run') {
    if (args.includes('helm')) output('v4.2.2');
    else if (args.includes('kubectl')) output('{"gitVersion":"v1.36.2"}');
    else output('offline-container');
  } else if (args[0] === 'port') output('127.0.0.1:12345');
  else if (!['logs', 'rm'].includes(args[0])) fail('Unexpected docker command');
} else if (command === 'helm') {
  if (args[0] === 'registry') process.stdin.resume();
  else if (args[0] === 'pull') {
    if (process.env.TEST_FAIL === 'chart-auth') fail('unauthorized');
    if (process.env.TEST_FAIL === 'chart-network') fail('connection refused');
    if (!fs.existsSync(path.join(state, 'chart.tgz'))) fail('Error: ghcr.io/petzkod5/charts/rsdw-c2:1.2.3: not found');
    fs.copyFileSync(path.join(state, 'chart.tgz'), path.join(args[args.indexOf('--destination') + 1], 'rsdw-c2-1.2.3.tgz'));
  } else if (args[0] === 'push') {
    if (!fs.existsSync(path.join(state, 'image'))) fail('chart before image');
    fs.copyFileSync(args[1], path.join(state, 'chart.tgz'));
    if (process.env.TEST_FAIL === 'after-chart') fail('connection lost after chart push');
  } else {
    if (!['package', 'show', 'lint', 'template', 'install'].includes(args[0])) fail('Unexpected Helm command');
    const result = spawnSync(process.env.TEST_HELM, args, { stdio: 'inherit' });
    process.exit(result.status ?? 1);
  }
} else fail('Unexpected command ' + command);
`;

function publicationFixture(t) {
  const dir = mkdtempSync(path.join(tmpdir(), 'rsdw-release-test-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(path.join(dir, 'bin'));
  const executable = path.join(dir, 'fake.cjs');
  writeFileSync(executable, fakeCommand, { mode: 0o755 });
  for (const command of ['git', 'docker', 'helm', 'curl']) symlinkSync(executable, path.join(dir, 'bin', command));
  const helm = execFileSync('which', ['helm'], { encoding: 'utf8' }).trim();
  return {
    dir,
    run(overrides = {}, version = '1.2.3') {
      return spawnSync('bash', ['.github/publish.sh', version], {
        encoding: 'utf8',
        env: { ...env, CHART_CHECK_MODE: '--offline', PATH: `${dir}/bin:${process.env.PATH}`, TEST_HELM: helm, TEST_STATE: dir, ...overrides },
      });
    },
    calls() {
      const file = path.join(dir, 'calls');
      return existsSync(file) ? readFileSync(file, 'utf8').trim().split('\n').map(line => JSON.parse(line)) : [];
    },
  };
}

test('publish and retry preserve existing image and chart, using real Helm packaging', t => {
  const fixture = publicationFixture(t);
  const source = execFileSync('git', ['diff', '--', 'charts'], { encoding: 'utf8' });
  for (let attempt = 0; attempt < 2; attempt++) {
    const result = fixture.run();
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /Published and verified image/);
  }
  const calls = fixture.calls();
  assert.equal(calls.filter(call => call[0] === 'docker' && call[1] === 'buildx').length, 1);
  assert.equal(calls.filter(call => call[0] === 'helm' && call[1] === 'push').length, 1);
  assert.equal(execFileSync('git', ['diff', '--', 'charts'], { encoding: 'utf8' }), source);
});

for (const failure of ['after-image', 'after-chart']) {
  test(`resume after ${failure} without replacing either artifact`, t => {
    const fixture = publicationFixture(t);
    assert.notEqual(fixture.run({ TEST_FAIL: failure }).status, 0);
    const result = fixture.run();
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.equal(fixture.calls().filter(call => call[1] === 'buildx').length, 1);
    assert.equal(fixture.calls().filter(call => call[0] === 'helm' && call[1] === 'push').length, 1);
  });
}

for (const failure of ['image-auth', 'image-network', 'chart-auth', 'chart-network', 'smoke']) {
  test(`stop on ${failure} without attempting a replacement`, t => {
    const fixture = publicationFixture(t);
    writeFileSync(path.join(fixture.dir, 'image'), sha);
    assert.notEqual(fixture.run({ TEST_FAIL: failure }).status, 0);
    assert(!fixture.calls().some(call => call[1] === 'buildx' || call[1] === 'push'));
  });
}

test('reject another image revision and conflicting chart contents', t => {
  const fixture = publicationFixture(t);
  writeFileSync(path.join(fixture.dir, 'image'), 'another-commit');
  assert.notEqual(fixture.run().status, 0);
  assert(!fixture.calls().some(call => call[1] === 'buildx' || call[1] === 'push'));
  writeFileSync(path.join(fixture.dir, 'image'), sha);
  execFileSync('helm', ['package', 'charts/rsdw-c2', '--version', '1.2.3', '--app-version', '9.9.9', '--destination', fixture.dir]);
  copyFileSync(path.join(fixture.dir, 'rsdw-c2-1.2.3.tgz'), path.join(fixture.dir, 'chart.tgz'));
  assert.notEqual(fixture.run().status, 0);
  assert(!fixture.calls().some(call => call[1] === 'buildx' || call[1] === 'push'));
});

test('invalid version, wrong event, and wrong tag never contact a registry', t => {
  const fixture = publicationFixture(t);
  for (const version of ['v1.2.3', '1.2.3-beta', '01.2.3', '1.2']) assert.notEqual(fixture.run({}, version).status, 0);
  assert.notEqual(fixture.run({ GITHUB_EVENT_NAME: 'pull_request' }).status, 0);
  assert.notEqual(fixture.run({ TEST_TAG_MISMATCH: 'yes' }).status, 0);
  assert(!fixture.calls().some(call => ['docker', 'helm', 'curl'].includes(call[0])));
});

test('downloaded chart validation rejects an unknown install mode without reporting success', t => {
  const fixture = publicationFixture(t);
  const result = fixture.run({ CHART_CHECK_MODE: '--typo' });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Unknown chart check mode: --typo/);
  assert.doesNotMatch(result.stdout, /Published and verified image/);
  assert(existsSync(path.join(fixture.dir, 'chart.tgz')));
});

test('packaged chart validation rejects incorrect metadata, image defaults, and readiness', t => {
  const fixture = publicationFixture(t);
  const chart = path.join(fixture.dir, 'source');
  cpSync('charts/rsdw-c2', chart, { recursive: true });
  for (const failure of ['metadata', 'image', 'readiness']) {
    const values = readFileSync('charts/rsdw-c2/values.yaml', 'utf8');
    const deployment = readFileSync('charts/rsdw-c2/templates/deployment.yaml', 'utf8');
    writeFileSync(path.join(chart, 'values.yaml'), failure === 'image' ? values.replace('tag: ""', 'tag: "wrong"') : values);
    writeFileSync(path.join(chart, 'templates/deployment.yaml'), failure === 'readiness' ? deployment.replaceAll('path: /api/auth', 'path: /wrong') : deployment);
    execFileSync('helm', ['package', chart, '--version', '1.2.3', '--app-version', failure === 'metadata' ? '9.9.9' : '1.2.3', '--destination', fixture.dir]);
    const result = spawnSync('node', ['.github/check-chart.mjs', path.join(fixture.dir, 'rsdw-c2-1.2.3.tgz'), '1.2.3'], { encoding: 'utf8' });
    assert.notEqual(result.status, 0, failure);
    assert.match(result.stderr, /AssertionError/, failure);
  }
});
