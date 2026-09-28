'use strict';

// Optional real-CMD smoke test. It never invokes the real configurator or writes
// user environment variables: a loopback server supplies a synthetic script.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const cp = require('node:child_process');
const vm = require('node:vm');

if (process.platform !== 'win32') {
  console.log('SKIP: client_setup_windows.cjs requires Windows Node.js and CMD.');
  process.exit(0);
}

const source = fs.readFileSync(path.join(__dirname, '../assets/app.js'), 'utf8');
const start = source.indexOf('function clientSetupCommand(client) {');
const end = source.indexOf('\nfunction renderGuide()', start);
assert.ok(start >= 0 && end > start, 'find the current dashboard launcher');
const launcherSource = source.slice(start, end) + '\nclientSetupCommand';
const syntheticKey = 'cgk_v1_synthetic_for_windows_smoke_only';

async function main() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'gateway-cmd-smoke-'));
  const home = path.join(root, '中文 用户', '持久化 测试');
  const temporary = path.join(home, '临时 文件');
  fs.mkdirSync(temporary, {recursive: true});
  let active;
  const requestErrors = [];
  const server = http.createServer((request, response) => {
    try {
      assert.equal(request.url, '/setup/configure-client.cjs');
      if (active.mode === 'download-failure') {
        response.writeHead(503);
        response.end('unavailable');
        return;
      }
      response.setHeader('Content-Type', 'application/javascript; charset=utf-8');
      response.end(`'use strict';
        const fs = require('node:fs');
        const assert = require('node:assert/strict');
        const readline = require('node:readline');
        assert.equal(process.platform, 'win32');
        assert.ok(__filename.includes('中文 用户'));
        assert.ok(__filename.includes('临时 文件'));
        assert.equal(process.argv[2], process.env.GATEWAY_SMOKE_CLIENT);
        assert.equal(process.argv[3], process.env.GATEWAY_SMOKE_ORIGIN);
        const reader = readline.createInterface({input: process.stdin, output: process.stdout});
        reader.question('Synthetic Gateway API Key: ', (key) => {
          assert.equal(key, '${syntheticKey}');
          fs.writeFileSync(process.env.GATEWAY_SMOKE_OUTPUT, JSON.stringify({
            client: process.argv[2], origin: process.argv[3], input: true,
            unicodePath: __filename.includes('中文 用户')
          }));
          reader.close();
          process.exitCode = process.env.GATEWAY_SMOKE_MODE === 'login-failure' ? 7 : 0;
        });`);
    } catch (error) {
      requestErrors.push(error.message);
      response.writeHead(500);
      response.end('synthetic request failed');
    }
  });

  try {
    await new Promise((resolve, reject) => {
      server.once('error', reject);
      server.listen(0, '127.0.0.1', resolve);
    });
    const origin = `http://127.0.0.1:${server.address().port}`;
    const launcher = vm.runInNewContext(launcherSource, {btoa, location: {origin}});
    for (const [client, mode] of [
      ['codex', 'success'], ['agy', 'success'],
      ['codex', 'login-failure'], ['agy', 'download-failure'],
    ]) {
      active = {mode};
      const output = path.join(home, `${client}-${mode}.json`);
      const command = launcher(client);
      assert.ok(!command.includes(syntheticKey));
      const environment = {...process.env};
      // Windows environment names are case insensitive. Keep only this fixture's
      // Node and System32 on PATH, without installing either CLI.
      for (const name of Object.keys(environment)) {
        if (['path', 'tmp', 'temp', 'home', 'userprofile', 'codex_home', 'gemini_api_key', 'openai_api_key'].includes(name.toLowerCase())) {
          delete environment[name];
        }
      }
      Object.assign(environment, {
        PATH: `${path.dirname(process.execPath)};${path.join(process.env.SystemRoot, 'System32')}`,
        TMP: temporary, TEMP: temporary, HOME: home, USERPROFILE: home,
        CODEX_HOME: path.join(home, 'codex'),
        GATEWAY_SMOKE_CLIENT: client, GATEWAY_SMOKE_ORIGIN: origin,
        GATEWAY_SMOKE_MODE: mode, GATEWAY_SMOKE_OUTPUT: output,
      });
      let prompted = false;
      let stdout = '';
      let stderr = '';
      const status = await new Promise((resolve, reject) => {
        const child = cp.spawn(path.join(process.env.SystemRoot, 'System32', 'cmd.exe'), ['/d', '/s', '/c', command], {
          cwd: process.env.SystemRoot, env: environment,
          windowsVerbatimArguments: true, stdio: ['pipe', 'pipe', 'pipe'],
        });
        const timeout = setTimeout(() => {
          child.kill();
          reject(new Error('Windows CMD launcher timed out'));
        }, 20000);
        child.stdout.on('data', chunk => {
          stdout += chunk;
          if (!prompted && stdout.includes('Synthetic Gateway API Key: ')) {
            prompted = true;
            try {
              assert.equal(fs.readdirSync(temporary).length, 1);
              child.stdin.end(syntheticKey + '\r\n');
            } catch (error) {
              clearTimeout(timeout);
              child.kill();
              reject(error);
            }
          }
        });
        child.stderr.on('data', chunk => { stderr += chunk; });
        child.stdin.on('error', error => { clearTimeout(timeout); reject(error); });
        child.once('error', error => { clearTimeout(timeout); reject(error); });
        child.once('close', code => { clearTimeout(timeout); resolve(code); });
      });
      assert.equal(status, mode === 'download-failure' ? 1 : mode === 'login-failure' ? 7 : 0, stderr);
      assert.deepEqual(requestErrors, []);
      assert.deepEqual(fs.readdirSync(temporary), [], 'launcher must clean temporary files');
      assert.equal(prompted, mode !== 'download-failure');
      if (mode !== 'download-failure') {
        assert.deepEqual(JSON.parse(fs.readFileSync(output, 'utf8')), {
          client, origin, input: true, unicodePath: true,
        });
      }
      console.log(JSON.stringify({
        platform: process.platform, node: process.version, client, mode, status,
        prompted, unicodePath: true, temporaryCleanup: true,
      }));
    }
  } finally {
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
    fs.rmSync(root, {recursive: true, force: true, maxRetries: 3, retryDelay: 100});
    assert.equal(fs.existsSync(root), false, 'remove the entire synthetic fixture');
  }
}

main().catch(error => { console.error(error); process.exitCode = 1; });
