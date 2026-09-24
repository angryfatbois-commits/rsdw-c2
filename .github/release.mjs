import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import semanticRelease from 'semantic-release';

export async function release({ run = execFileSync, semantic = semanticRelease, env = process.env } = {}) {
  if (env.GITHUB_EVENT_NAME !== 'push' || env.GITHUB_REF !== 'refs/heads/main' || env.GITHUB_REPOSITORY !== 'angryfatbois-commits/rsdw-c2') {
    throw new Error('Releases require a push to angryfatbois-commits/rsdw-c2 main');
  }
  const git = (...args) => run('git', args, { encoding: 'utf8' }).trim();
  if (git('rev-parse', 'HEAD') !== env.GITHUB_SHA) throw new Error('Checkout does not match the verified commit');

  const result = await semantic();
  const tags = git('tag', '--points-at', 'HEAD').split('\n').filter(tag => /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(tag));
  if (tags.length > 1) throw new Error('Multiple release tags point at this commit');
  const tag = tags[0];
  if (result?.nextRelease && result.nextRelease.gitTag !== tag) throw new Error('Release tag does not match the verified commit');
  if (!tag) return '';

  try {
    run('gh', ['api', `repos/${env.GITHUB_REPOSITORY}/releases/tags/${tag}`], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (error) {
    if (!/HTTP 404/.test(String(error.stderr))) throw error;
    run('gh', ['release', 'create', tag, '--verify-tag', '--generate-notes', '--title', tag], { stdio: 'inherit' });
  }
  return tag.slice(1);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const version = await release();
  appendFileSync(process.env.GITHUB_OUTPUT, `version=${version}\n`);
  console.log(version ? `Release artifacts use ${version}` : 'No release needed');
}
