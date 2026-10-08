'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { PassThrough } = require('node:stream');
const test = require('node:test');
const { configureClient, codexConfig, sourceCredentials, readSecret, originURL, commitFiles } = require('../assets/configure-client.cjs');

const origin = 'https://gateway.example:8443';
const key = 'gw_synthetic-secret-测试';
const script = path.resolve(__dirname, '../assets/configure-client.cjs');

function workspace(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'gateway-config-test-'));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const home = path.join(directory, "中文 用户's home");
  fs.mkdirSync(home);
  return { directory, home, temp: directory, env: { ...process.env, HOME: home, USERPROFILE: home, CODEX_HOME: '', ZDOTDIR: '' }, platform: 'linux' };
}

function put(filename, data) { fs.mkdirSync(path.dirname(filename), { recursive: true }); fs.writeFileSync(filename, data); }
function read(filename) { return fs.readFileSync(filename, 'utf8'); }
function backups(filename) { return fs.readdirSync(path.dirname(filename)).filter(name => name.startsWith(path.basename(filename) + '.bak-')).map(name => path.join(path.dirname(filename), name)); }
function fakeCodex(t, context, behavior = {}) {
  const calls = [];
  return { calls, run(command, args, options) {
    assert.equal(command, context.platform === 'win32' ? 'cmd.exe' : 'codex');
    const action = context.platform === 'win32' ? args.at(-1).split(' ').slice(1) : args;
    assert.ok(options.env.CODEX_HOME.startsWith(path.join(context.temp, 'gateway-codex-')));
    assert.notEqual(options.env.CODEX_HOME, context.env.CODEX_HOME);
    assert.ok(!args.join(' ').includes(key));
    const config = read(path.join(options.env.CODEX_HOME, 'config.toml'));
    calls.push({ action, config, input: options.input });
    if (behavior.missing) return { error: Object.assign(new Error('absent'), { code: 'ENOENT' }) };
    if (action[0] === 'features') return { status: config.includes('INVALID') || behavior.invalidCandidate ? 1 : 0, stdout: '', stderr: '' };
    assert.deepEqual(action, ['login', '--with-api-key']);
    assert.equal(options.input, key + '\n');
    assert.match(config, /cli_auth_credentials_store = "file"/);
    if (behavior.loginFailure) return { status: 1, stderr: 'login failed: ' + key };
    if (!behavior.missingAuth) put(path.join(options.env.CODEX_HOME, 'auth.json'), JSON.stringify({ OPENAI_API_KEY: key }));
    return { status: 0, stdout: 'Logged in' };
  } };
}

test('validates clients, origins and keys before creating files', t => {
  const context = workspace(t);
  for (const url of ['not-a-url', 'ftp://example.com', 'https://user:pass@example.com', 'https://example.com/v1', 'https://example.com?key=secret', 'https://example.com#x']) {
    assert.throws(() => configureClient('agy', url, key, context), /Gateway 地址/);
  }
  assert.throws(() => configureClient('unknown', origin, key, context), /客户端参数/);
  for (const invalid of ['', '  ', 'first\nsecond', 'space key', 'key\x00']) assert.throws(() => configureClient('agy', origin, invalid, context), /API Key/);
  assert.deepEqual(fs.readdirSync(context.home), []);
  assert.equal(originURL(origin + '/'), origin);
});

test('Codex first configuration persists auth and honors a Chinese CODEX_HOME', t => {
  const context = workspace(t);
  context.env.CODEX_HOME = path.join(context.directory, '专用 Codex 目录');
  const codex = fakeCodex(t, context);
  const result = configureClient('codex', origin, key, { ...context, run: codex.run });
  const config = read(path.join(context.env.CODEX_HOME, 'config.toml'));
  assert.equal(config, `model_provider = "openai"\nopenai_base_url = "${origin}/v1"\ncli_auth_credentials_store = "file"\n`);
  assert.equal(JSON.parse(read(path.join(context.env.CODEX_HOME, 'auth.json'))).OPENAI_API_KEY, key);
  assert.equal(fs.statSync(path.join(context.env.CODEX_HOME, 'auth.json')).mode & 0o777, 0o600);
  assert.equal(fs.existsSync(path.join(context.home, '.codex')), false);
  assert.equal(result.length, 0);
  assert.deepEqual(codex.calls.map(call => call.action), [['features', 'list'], ['login', '--with-api-key']]);
  assert.equal(fs.readdirSync(context.directory).some(name => name.startsWith('gateway-codex-')), false);
});

test('Codex preserves TOML tables, multiline strings and arrays, backs up and is repeatable', t => {
  const context = workspace(t);
  const home = path.join(context.home, '.codex');
  const filename = path.join(home, 'config.toml');
  const original = '# 用户自定义\nmodel = "gpt-5"\n"model_provider" = "old"\n\'openai_base_url\' = "old-url"\ncli_auth_credentials_store = "auto"\n' +
    'developer_instructions = """\n[not.a.table]\nmodel_provider = \\"preserve\\"\n"""\n' +
    'some_array = [\n "model_provider", # comment\n "other",\n]\n' +
    '[projects."/中文 project"]\ntrust_level = "trusted"\n[model_providers.old]\nname = "Keep me"\nbase_url = "https://previous.example/v1"\n';
  put(filename, original);
  put(path.join(home, 'auth.json'), '{"OPENAI_API_KEY":"previous-key"}');
  const codex = fakeCodex(t, context);
  const backupFiles = configureClient('codex', origin, key, { ...context, run: codex.run });
  assert.equal(backupFiles.length, 2);
  assert.equal(read(backups(filename)[0]), original);
  const config = read(filename);
  assert.match(config, /model = "gpt-5"/);
  assert.ok(config.includes(original.slice(original.indexOf('developer_instructions'))));
  assert.equal((config.match(/^model_provider = "openai"$/gm) || []).length, 1);
  const second = configureClient('codex', origin, key, { ...context, run: codex.run });
  assert.deepEqual(second, []);
  assert.equal(read(filename), config);
  for (const backup of backupFiles) assert.equal(fs.statSync(backup).mode & 0o777, 0o600);
});

test('TOML rewriting handles Unicode key escapes and quotes at multiline endings', () => {
  const original = '"\\u006dodel_provider" = "before"\n"\\U0000006Fpenai_base_url" = "before"\nnotes = """quoted ending""""\nnext = 42\n[table]\nmodel_provider = "untouched"\n';
  const changed = codexConfig(original, origin);
  assert.ok(!changed.includes('before'));
  assert.match(changed, /notes = """quoted ending""""\nnext = 42/);
  assert.ok(changed.endsWith('[table]\nmodel_provider = "untouched"\n'));
});

test('Codex parse, missing executable, login and credential failures leave original files intact', t => {
  for (const failure of ['parse', 'missing', 'invalidCandidate', 'loginFailure', 'missingAuth']) {
    const context = workspace(t);
    const filename = path.join(context.home, '.codex', 'config.toml');
    const auth = path.join(context.home, '.codex', 'auth.json');
    const original = failure === 'parse' ? 'INVALID = [' : 'model = "keep"\n';
    put(filename, original); put(auth, 'old credentials');
    const codex = fakeCodex(t, context, { [failure]: true });
    assert.throws(() => configureClient('codex', origin, key, { ...context, run: codex.run }), /解析|找不到|无法识别|登录失败|未成功/);
    assert.equal(read(filename), original);
    assert.equal(read(auth), 'old credentials');
    assert.equal(backups(filename).length, 0);
    assert.equal(fs.readdirSync(context.directory).some(name => name.startsWith('gateway-codex-')), false);
  }
});

test('Windows Codex calls its npm shim with a fixed command and key exclusively on stdin', t => {
  const context = { ...workspace(t), platform: 'win32' };
  delete context.env.ComSpec;
  const codex = fakeCodex(t, context);
  configureClient('codex', origin, key, { ...context, run: codex.run });
  assert.equal(codex.calls.at(-1).input, key + '\n');
});

test('a legacy default Codex profile cannot silently override the gateway provider', t => {
  const context = workspace(t);
  const filename = path.join(context.home, '.codex', 'config.toml');
  const original = 'profile = "work"\n[profiles.work]\nmodel_provider = "another-provider"\nmodel = "preserve-model"\n';
  put(filename, original);
  assert.throws(() => configureClient('codex', origin, key, context), /默认 profile/);
  assert.equal(read(filename), original);
  assert.equal(fs.existsSync(path.join(context.home, '.codex', 'auth.json')), false);
});

test('agy preserves settings, protects credentials, backs up files and updates shell setup once', t => {
  const context = workspace(t);
  const directory = path.join(context.home, '.gemini', 'antigravity-cli');
  const filename = path.join(directory, 'settings.json');
  const original = JSON.stringify({ modelProvider: 'other', theme: 'dark', nested: { keep: true } });
  put(filename, original);
  const bashrc = path.join(context.home, '.bashrc');
  put(bashrc, 'export KEEP_ME=unchanged\n');
  put(path.join(context.home, '.bash_profile'), 'export LOGIN_VALUE=preserved\n');
  const result = configureClient('agy', origin, key, context);
  assert.deepEqual(JSON.parse(read(filename)), { modelProvider: 'gemini', theme: 'dark', nested: { keep: true } });
  assert.equal(result.length, 3);
  assert.equal(read(backups(filename)[0]), original);
  assert.equal(read(backups(bashrc)[0]), 'export KEEP_ME=unchanged\n');
  const credentials = path.join(directory, 'gateway-credentials.sh');
  assert.equal(fs.statSync(credentials).mode & 0o777, 0o600);
  for (const name of ['.bashrc', '.bash_profile', '.zshrc', '.zprofile']) {
    const source = read(path.join(context.home, name));
    assert.ok(source.includes('Gateway agy credentials'));
    assert.ok(!source.includes(key));
  }
  assert.equal(fs.existsSync(path.join(context.home, '.profile')), false);
  const second = configureClient('agy', origin, key, context);
  assert.deepEqual(second, []);
  assert.equal((read(bashrc).match(/# >>> Gateway agy credentials >>>/g) || []).length, 1);
  const third = configureClient('agy', 'https://new.example', key + '-new', context);
  assert.equal(third.length, 1);
  assert.equal(read(backups(credentials)[0]).includes(key), true);
});

test('fresh Bash sessions load persisted agy settings without inherited gateway variables', t => {
  if (spawnSync('bash', ['--version']).error) return t.skip('Bash unavailable');
  const context = workspace(t);
  configureClient('agy', origin, key, context);
  const command = 'printf "%s\\n%s\\n" "$GOOGLE_GEMINI_BASE_URL" "$GEMINI_API_KEY"';
  const env = { ...context.env };
  delete env.GOOGLE_GEMINI_BASE_URL; delete env.GEMINI_API_KEY;
  // --rcfile exercises a new interactive shell; source .profile in a fresh
  // shell separately because system /etc/profile may replace HOME on CI.
  const interactive = spawnSync('bash', ['--noprofile', '--rcfile', path.join(context.home, '.bashrc'), '-ic', command], { env, encoding: 'utf8' });
  assert.equal(interactive.status, 0, interactive.stderr);
  assert.ok(interactive.stdout.endsWith(origin + '\n' + key + '\n'), interactive.stdout);
  const login = spawnSync('bash', ['--noprofile', '--norc', '-c', '. "$HOME/.profile"; ' + command], { env, encoding: 'utf8' });
  assert.equal(login.status, 0, login.stderr);
  assert.equal(login.stdout, origin + '\n' + key + '\n');
  assert.ok(!env.GEMINI_API_KEY);
});

test('agy shell persistence handles shell metacharacters safely and ZDOTDIR', t => {
  const context = workspace(t);
  context.env.ZDOTDIR = path.join(context.home, '定制 zsh');
  const specialKey = "gw_'\"$`\\synthetic";
  configureClient('agy', origin, specialKey, context);
  const shell = spawnSync('sh', ['-c', '. "$HOME/.gemini/antigravity-cli/gateway-credentials.sh"; printf "%s" "$GEMINI_API_KEY"'], { env: context.env, encoding: 'utf8' });
  assert.equal(shell.status, 0, shell.stderr);
  assert.equal(shell.stdout, specialKey);
  assert.equal(fs.existsSync(path.join(context.env.ZDOTDIR, '.zshrc')), true);
  assert.equal(fs.existsSync(path.join(context.env.ZDOTDIR, '.zprofile')), true);
  assert.equal(fs.existsSync(path.join(context.home, '.zshrc')), false);
});

test('agy parse failures and incomplete shell markers fail before writing credentials', t => {
  for (const original of ['{ invalid', '[]', 'null']) {
    const context = workspace(t);
    const filename = path.join(context.home, '.gemini', 'antigravity-cli', 'settings.json');
    put(filename, original);
    assert.throws(() => configureClient('agy', origin, key, context), /解析失败|JSON 对象/);
    assert.equal(read(filename), original);
    assert.equal(fs.existsSync(path.join(context.home, '.bashrc')), false);
  }
  const context = workspace(t);
  put(path.join(context.home, '.bashrc'), '# >>> Gateway agy credentials >>>\nbroken');
  assert.throws(() => configureClient('agy', origin, key, context), /标记不完整/);
  assert.equal(fs.existsSync(path.join(context.home, '.gemini')), false);
});

test('existing linked shell files are preserved and updated at the link target', t => {
  if (process.platform === 'win32') return t.skip('Requires Unix symlink permissions');
  const context = workspace(t);
  const target = path.join(context.home, 'dotfiles', 'bashrc');
  put(target, 'export KEEP=1\n');
  fs.symlinkSync(target, path.join(context.home, '.bashrc'));
  configureClient('agy', origin, key, context);
  assert.ok(fs.lstatSync(path.join(context.home, '.bashrc')).isSymbolicLink());
  assert.ok(read(target).startsWith('export KEEP=1\n'));
  assert.equal(backups(target).length, 1);
});

test('Windows agy merges settings and persists user variables with a private backup and stdin key', t => {
  const context = { ...workspace(t), platform: 'win32' };
  let registry = { GOOGLE_GEMINI_BASE_URL: 'https://previous.example', GEMINI_API_KEY: 'old-synthetic-key' };
  const calls = [];
  context.run = (command, args, options) => {
    calls.push(args);
    assert.equal(command, 'powershell.exe');
    assert.ok(!args.join(' ').includes(key));
    assert.ok(args.at(-1).includes("'User'"));
    if (options.input) {
      assert.ok(args.at(-1).includes('catch'));
      registry = JSON.parse(options.input);
      return { status: 0 };
    }
    return { status: 0, stdout: JSON.stringify(registry) };
  };
  const result = configureClient('agy', origin, key, context);
  assert.deepEqual(registry, { GOOGLE_GEMINI_BASE_URL: origin, GEMINI_API_KEY: key });
  assert.equal(calls.length, 2);
  assert.equal(result.length, 1);
  assert.equal(JSON.parse(read(result[0])).GEMINI_API_KEY, 'old-synthetic-key');
  assert.equal(fs.existsSync(path.join(context.home, '.bashrc')), false);
  assert.deepEqual(configureClient('agy', origin, key, context), []);
  assert.equal(calls.length, 3);
});

test('Windows environment write failures roll back agy settings', t => {
  const context = { ...workspace(t), platform: 'win32' };
  const filename = path.join(context.home, '.gemini', 'antigravity-cli', 'settings.json');
  put(filename, '{"keep":true}');
  context.run = (_, __, options) => options.input ? { status: 1 } : { status: 0, stdout: '{"GOOGLE_GEMINI_BASE_URL":null,"GEMINI_API_KEY":null}' };
  assert.throws(() => configureClient('agy', origin, key, context), /保存失败/);
  assert.equal(read(filename), '{"keep":true}');
  assert.equal(backups(filename).length, 1);
  const envBackup = fs.readdirSync(path.dirname(filename)).find(name => name.startsWith('gateway-environment.bak-'));
  assert.ok(envBackup);
});

test('file commit failures restore earlier writes and keep pre-change backups', t => {
  const context = workspace(t);
  const filename = path.join(context.home, 'existing');
  put(filename, 'original');
  assert.throws(() => commitFiles([
    { filename, data: 'changed' },
    { filename: path.join(context.home, 'created'), data: 'new' },
  ], () => { throw new Error('simulated failure'); }), /simulated failure/);
  assert.equal(read(filename), 'original');
  assert.equal(fs.existsSync(path.join(context.home, 'created')), false);
  assert.equal(read(backups(filename)[0]), 'original');
});

test('secret input is hidden for TTYs and handles backspace and cancellation', async () => {
  const input = new PassThrough(), output = new PassThrough();
  let printed = '';
  output.on('data', data => { printed += data; });
  input.isTTY = true;
  input.setRawMode = value => { input.isRaw = value; };
  const entered = readSecret(input, output);
  input.write('secretX\x7f-key\r');
  assert.equal(await entered, 'secret-key');
  assert.equal(input.isRaw, false);
  assert.ok(!printed.includes('secret-key'));
  const cancelled = readSecret(input, output);
  input.write('\x03');
  await assert.rejects(cancelled, /已取消/);
  assert.equal(input.isRaw, false);
});

test('CLI EOF cancellation and invalid arguments never write configuration or reveal a key', t => {
  const context = workspace(t);
  const cancelled = spawnSync(process.execPath, [script, 'agy', origin], { env: context.env, input: '', encoding: 'utf8' });
  assert.equal(cancelled.status, 1);
  assert.match(cancelled.stderr, /输入已取消/);
  assert.equal(fs.existsSync(path.join(context.home, '.gemini')), false);
  const extra = spawnSync(process.execPath, [script, 'agy', origin, key], { env: context.env, input: '', encoding: 'utf8' });
  assert.equal(extra.status, 1);
  assert.ok(!extra.stderr.includes(key));
});

test('CLI saves agy credentials without printing the synthetic key', t => {
  const context = workspace(t);
  const result = spawnSync(process.execPath, [script, 'agy', origin], { env: context.env, input: key + '\n', encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /agy --model gemini-pro-agent/);
  assert.match(result.stdout, /不改写旧别名/);
  assert.ok(!result.stdout.includes(key));
  assert.ok(!result.stderr.includes(key));
});

test('real installed Codex validates TOML and persists a synthetic key in isolated CODEX_HOME', { skip: process.env.GATEWAY_TEST_REAL_CODEX !== '1' }, t => {
  const context = workspace(t);
  const realKey = 'gw_synthetic-secret-ascii-only';
  context.env.CODEX_HOME = path.join(context.home, '专用 Codex');
  const filename = path.join(context.env.CODEX_HOME, 'config.toml');
  const authFile = path.join(context.env.CODEX_HOME, 'auth.json');
  const original = 'model = "gpt-5"\nmodel_reasoning_effort = "high"\n[projects."/中文 project"]\ntrust_level = "trusted"\n';
  put(filename, original);
  configureClient('codex', origin, realKey, context);
  assert.equal(JSON.parse(read(authFile)).OPENAI_API_KEY, realKey);
  assert.ok(read(filename).endsWith(original));
  const status = spawnSync('codex', ['login', 'status'], { env: context.env, cwd: context.home, encoding: 'utf8' });
  assert.equal(status.error, undefined);
  assert.equal(status.status, 0, status.stderr);
  assert.deepEqual(configureClient('codex', origin, realKey, context), []);
  for (const invalid of ['model = [', 'model = "first"\nmodel = "duplicate"\n']) {
    put(filename, invalid);
    const auth = read(authFile);
    assert.throws(() => configureClient('codex', origin, realKey, context), /配置解析或独立校验失败/);
    assert.equal(read(filename), invalid);
    assert.equal(read(authFile), auth);
  }
});

test('Claude user settings merge preserves unrelated values and backs up credentials privately', t => {
  const context = workspace(t), model = 'claude-sonnet-4-5-20250929';
  const filename = path.join(context.home, '.claude', 'settings.json');
  const original = JSON.stringify({permissions: {allow: ['Read']}, env: {CUSTOM_SETTING: 'keep'}, model: 'old-alias'});
  put(filename, original);
  const result = configureClient('claude', origin, key, {...context, model, models: [model]});
  const settings = JSON.parse(read(filename));
  assert.deepEqual(settings.permissions, {allow: ['Read']});
  assert.deepEqual(settings.env, {CUSTOM_SETTING: 'keep', ANTHROPIC_BASE_URL: origin, ANTHROPIC_AUTH_TOKEN: key});
  assert.equal(settings.model, model);
  assert.equal(result.length, 1);
  assert.equal(read(result[0]), original);
  assert.equal(fs.statSync(result[0]).mode & 0o777, 0o600);
  assert.equal(fs.statSync(filename).mode & 0o777, 0o600);
  assert.deepEqual(configureClient('claude', origin, key, {...context, model, models: [model]}), []);
});

test('Claude rejects conflicting auth, providers, malformed settings and unlisted model IDs before writes', t => {
  const model = 'claude-sonnet-4-5-20250929';
  for (const setting of [{apiKeyHelper: 'read-key'}, {env: {ANTHROPIC_API_KEY: 'other-secret'}}, {env: {CLAUDE_CODE_OAUTH_TOKEN: 'oauth-secret'}}, {env: {CLAUDE_CODE_USE_BEDROCK: '1'}}, {env: {ANTHROPIC_DEFAULT_HAIKU_MODEL: 'alias'}}]) {
    const context = workspace(t), filename = path.join(context.home, '.claude', 'settings.json'), original = JSON.stringify(setting);
    put(filename, original);
    assert.throws(() => configureClient('claude', origin, key, {...context, model, models: [model]}), error => /冲突/.test(error.message) && !/other-secret|oauth-secret/.test(error.message));
    assert.equal(read(filename), original);
    assert.equal(backups(filename).length, 0);
  }
  const context = workspace(t);
  assert.throws(() => configureClient('claude', origin, key, {...context, model: 'sonnet', models: ['sonnet']}), /精确模型/);
  assert.throws(() => configureClient('claude', origin, key, {...context, model, models: []}), /精确模型/);
  assert.throws(() => configureClient('claude', origin, key, {...context, model, models: [model], env: {...context.env, ANTHROPIC_AUTH_TOKEN: 'other'}}), /冲突/);
  assert.deepEqual(fs.readdirSync(context.home), []);
  const filename = path.join(context.home, '.claude', 'settings.json');
  for (const invalid of ['{invalid', '[]', '{"env":[]}']) {
    put(filename, invalid);
    assert.throws(() => configureClient('claude', origin, key, {...context, model, models: [model]}), /解析|JSON 对象/);
    assert.equal(read(filename), invalid);
  }
});

test('Claude catalog sends only Gateway auth, rejects redirects and excludes other providers', async () => {
  const {claudeModels} = require('../assets/configure-client.cjs');
  const models = await claudeModels(origin, key, async (url, options) => {
    assert.equal(url, origin + '/v1/models');
    assert.deepEqual(options.headers, {Authorization: 'Bearer ' + key});
    assert.equal(options.redirect, 'error');
    return {ok: true, json: async () => ({data: [{id: 'claude-sonnet-exact', owned_by: 'anthropic'}, {id: 'claude-opus-exact', owned_by: 'claude'}, {id: 'gpt-other'}, {id: 'claude-other-provider', owned_by: 'antigravity'}, {id: 'claude-unsafe\nvalue'}]})};
  });
  assert.deepEqual(models, ['claude-opus-exact', 'claude-sonnet-exact']);
  await assert.rejects(claudeModels(origin, key, async () => {throw new Error(key);}), error => !error.message.includes(key));
  await assert.rejects(claudeModels(origin, key, async () => ({ok: false, status: 403})), /HTTP 403/);
  await assert.rejects(claudeModels(origin, key, async () => ({ok: true, json: async () => ({data: []})})), /没有可用/);
});
