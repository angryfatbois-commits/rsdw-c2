import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { parse, parseAllDocuments } from 'yaml';

export function checkChart(archive, version, mode = '--offline') {
  assert(['--offline', '--kind'].includes(mode), `Unknown chart check mode: ${mode}`);
  const helm = (...args) => execFileSync('helm', args, { encoding: 'utf8' });
  const metadata = parse(helm('show', 'chart', archive));
  assert.equal(metadata.version, version);
  assert.equal(metadata.appVersion, version);
  const settings = ['--namespace', 'rsdw-system', '--set', 'auth.adminTokenSecret.name=rsdw-c2-admin'];
  console.log(helm('lint', archive, ...settings));
  assert.throws(() => helm('template', 'rsdw-c2', archive), /auth.adminTokenSecret.name is required/);
  const manifest = helm('template', 'rsdw-c2', archive, ...settings);
  const deployment = parseAllDocuments(manifest).map(doc => doc.toJSON()).find(doc => doc?.kind === 'Deployment');
  const container = deployment.spec.template.spec.containers.find(item => item.name === 'rsdw-c2');
  assert.equal(container.image, `ghcr.io/angryfatbois-commits/rsdw-c2:${version}`);
  assert.deepEqual(container.readinessProbe.httpGet, { path: '/api/auth', port: 'http' });
  assert.deepEqual(container.env.find(item => item.name === 'RSDW_ADMIN_TOKEN').valueFrom.secretKeyRef,
    { name: 'rsdw-c2-admin', key: 'token' });
  helm('install', 'rsdw-c2', archive, '--dry-run=client', ...settings);
  console.log(`Chart ${version} passed metadata, default image, admin Secret, readiness, and client install checks`);
  if (mode === '--offline') return;

  execFileSync('docker', ['info'], { stdio: ['ignore', 'ignore', 'inherit'], timeout: 15000 });
  const dir = mkdtempSync(path.join(tmpdir(), 'rsdw-chart-'));
  const cluster = path.basename(dir).toLowerCase();
  const context = `kind-${cluster}`;
  const env = { ...process.env, KUBECONFIG: path.join(dir, 'kubeconfig'), KIND_EXPERIMENTAL_PROVIDER: 'docker' };
  const run = (command, ...args) => execFileSync(command, args, { env, encoding: 'utf8', timeout: 300000 });
  const kubectl = (...args) => run('kubectl', '--context', context, '--request-timeout=30s', ...args);
  try {
    run('kind', 'create', 'cluster', '--name', cluster, '--image', 'kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed', '--wait', '120s');
    run('kind', 'load', 'docker-image', `ghcr.io/angryfatbois-commits/rsdw-c2:${version}`, '--name', cluster);
    kubectl('create', 'namespace', 'rsdw-system');
    kubectl('-n', 'rsdw-system', 'create', 'secret', 'generic', 'rsdw-c2-admin', '--from-literal=token=ci-install-token');
    console.log(run('helm', 'install', 'rsdw-c2', archive, ...settings, '--kube-context', context, '--wait', '--timeout', '120s'));
    console.log(kubectl('-n', 'rsdw-system', 'rollout', 'status', `deployment/${deployment.metadata.name}`, '--timeout=120s'));
    const service = parseAllDocuments(manifest).map(doc => doc.toJSON()).find(doc => doc?.kind === 'Service');
    const auth = JSON.parse(kubectl('get', '--raw', `/api/v1/namespaces/rsdw-system/services/${service.metadata.name}:${service.spec.ports[0].port}/proxy/api/auth`));
    assert.equal(auth.required, true);
    const curl = (...args) => kubectl('-n', 'rsdw-system', 'exec', `deployment/${deployment.metadata.name}`, '--', 'curl', '--silent', '--show-error', '--max-time', '15', ...args, 'http://127.0.0.1:8080/api/bootstrap');
    assert.equal(curl('--output', '/dev/null', '--write-out', '%{http_code}'), '401');
    const bootstrap = JSON.parse(curl('--fail', '--header', 'Authorization: Bearer ci-install-token'));
    assert.deepEqual(bootstrap.servers, []);
    console.log(`Chart ${version} passed clean kind install, readiness, and admin-token authentication checks`);
  } catch (error) {
    for (const args of [['get', 'pods', '-A', '-o', 'wide'], ['get', 'events', '-A'], ['-n', 'rsdw-system', 'logs', `deployment/${deployment.metadata.name}`, '--all-containers']]) {
      try { console.error(kubectl(...args)); } catch (diagnosticError) { console.error(diagnosticError.message); }
    }
    throw error;
  } finally {
    try { run('kind', 'delete', 'cluster', '--name', cluster); }
    finally { rmSync(dir, { recursive: true, force: true }); }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  checkChart(process.argv[2], process.argv[3], process.argv[4]);
}
