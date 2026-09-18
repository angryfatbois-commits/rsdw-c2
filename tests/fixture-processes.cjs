const {spawn} = require('node:child_process');
const {once} = require('node:events');

function fixtureProcesses(root) {
  const children = [];

  async function start(command, args, env, pattern, detached = false) {
    const child = spawn(command, args, {cwd:root, env:{...process.env, ...env}, detached, stdio:['ignore','pipe','pipe']});
    const exited = once(child, 'exit');
    children.push({child, exited, detached});
    return new Promise((resolve, reject) => {
      let logs = '';
      const timeout = setTimeout(() => reject(new Error(`Fixture did not start: ${logs}`)), 30000);
      child.on('error', (error) => { clearTimeout(timeout); reject(error); });
      child.on('exit', (code) => { clearTimeout(timeout); reject(new Error(`Fixture exited ${code}: ${logs}`)); });
      const read = (chunk) => {
        logs += chunk;
        const match = logs.match(pattern);
        if (match) { clearTimeout(timeout); resolve(match[1]); }
      };
      child.stdout.on('data', read);
      child.stderr.on('data', read);
    });
  }

  async function close() {
    for (const {child, exited, detached} of children.reverse()) {
      if (child.exitCode === null && child.signalCode === null) {
        if (detached) process.kill(-child.pid, 'SIGTERM');
        else child.kill();
      }
      await exited;
    }
  }

  return {start, close};
}

module.exports = {fixtureProcesses};
