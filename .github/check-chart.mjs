import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';
import { parse, parseAllDocuments } from 'yaml';

export function checkChart(archive, version) {
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
  assert.equal(container.image, `ghcr.io/petzkod5/rsdw-c2:${version}`);
  assert.deepEqual(container.readinessProbe.httpGet, { path: '/api/auth', port: 'http' });
  assert.deepEqual(container.env.find(item => item.name === 'RSDW_ADMIN_TOKEN').valueFrom.secretKeyRef,
    { name: 'rsdw-c2-admin', key: 'token' });
  helm('install', 'rsdw-c2', archive, '--dry-run=client', ...settings);
  console.log(`Chart ${version} passed metadata, default image, admin Secret, readiness, and client install checks`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  checkChart(process.argv[2], process.argv[3]);
}
