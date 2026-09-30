#!/usr/bin/env node
'use strict';

// This file is downloaded and run locally. Credentials only enter through stdin.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const readline = require('node:readline');
const { randomBytes } = require('node:crypto');
const { spawnSync } = require('node:child_process');

function originURL(value) {
  let url;
  try { url = new URL(value); } catch { throw new Error('Gateway 地址无效。'); }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password ||
      url.search || url.hash || url.pathname !== '/') {
    throw new Error('Gateway 地址必须是 http(s) 站点 origin，不能包含路径、账号或查询参数。');
  }
  return url.origin;
}

function secretValue(value) {
  const key = value.trim();
  if (!key || /\s|[\x00-\x1f\x7f]/u.test(key)) throw new Error('API Key 不能为空，也不能包含空白或控制字符。');
  return key;
}

function readSecret(input = process.stdin, output = process.stdout) {
  return new Promise((resolve, reject) => {
    if (input.isTTY && typeof input.setRawMode === 'function') {
      let key = '';
      const wasRaw = input.isRaw;
      const cleanup = () => {
        input.removeListener('data', onData);
        input.removeListener('end', onEnd);
        input.removeListener('error', onError);
        input.setRawMode(Boolean(wasRaw));
        input.pause();
        output.write('\n');
      };
      const onEnd = () => { cleanup(); reject(new Error('输入已取消，未保存配置。')); };
      const onError = () => { cleanup(); reject(new Error('无法读取 API Key，未保存配置。')); };
      const onData = data => {
        for (const character of String(data)) {
          if (character === '\x03' || character === '\x04' || character === '\x1b') { onEnd(); return; }
          if (character === '\r' || character === '\n') { cleanup(); resolve(key); return; }
          if (character === '\x7f' || character === '\b') key = Array.from(key).slice(0, -1).join('');
          else if (character >= ' ') key += character;
        }
      };
      output.write('请输入 Gateway API Key（输入不显示，Ctrl+C 取消）：');
      input.setEncoding('utf8');
      input.setRawMode(true);
      input.on('data', onData);
      input.once('end', onEnd);
      input.once('error', onError);
      input.resume();
      return;
    }
    const reader = readline.createInterface({ input, terminal: false });
    let answered = false;
    output.write('请输入 Gateway API Key：');
    reader.once('line', line => {
      answered = true;
      reader.close();
      resolve(line);
    });
    reader.once('close', () => {
      if (!answered) reject(new Error('输入已取消，未保存配置。'));
    });
    reader.once('error', () => reject(new Error('无法读取 API Key，未保存配置。')));
  });
}

function readOptional(filename) {
  try { return fs.readFileSync(filename, 'utf8'); } catch (error) {
    if (error.code === 'ENOENT') return null;
    throw new Error(`无法读取配置文件：${filename}`, { cause: error });
  }
}

function utf8Text(value) { return value.replace(/^\uFEFF/u, ''); }
function shellQuote(value) { return "'" + value.replace(/'/g, "'\"'\"'") + "'"; }

// Split complete TOML statements without mistaking table-looking lines in
// multiline strings or arrays for table headers. Codex itself validates TOML.
function tomlStatements(text) {
  const statements = [];
  let start = 0, quote = '', depth = 0, comment = false;
  for (let index = 0; index < text.length; index++) {
    const character = text[index];
    if (comment) {
      if (character !== '\n') continue;
      comment = false;
    } else if (quote) {
      if (quote[0] === '"' && character === '\\') { index++; continue; }
      if (text.startsWith(quote, index)) {
        let length = quote.length;
        if (length === 3) while (text[index + length] === quote[0]) length++;
        index += length - 1;
        quote = '';
      }
      continue;
    } else if (character === '#' ) { comment = true; continue;
    } else if (character === '"' || character === "'") {
      quote = text.startsWith(character.repeat(3), index) ? character.repeat(3) : character;
      index += quote.length - 1;
      continue;
    } else if (character === '[' || character === '{') depth++;
    else if (character === ']' || character === '}') depth--;
    if (character === '\n' && depth === 0) { statements.push(text.slice(start, index + 1)); start = index + 1; }
  }
  if (start < text.length) statements.push(text.slice(start));
  return statements;
}

function rootAssignmentKey(statement) {
  const match = /^\s*([A-Za-z0-9_-]+|"(?:[^"\\]|\\.)*"|'[^']*')\s*=/u.exec(statement);
  if (!match) return null;
  const key = match[1];
  if (key.startsWith("'")) return key.slice(1, -1);
  if (!key.startsWith('"')) return key;
  try {
    // TOML adds eight-digit Unicode escapes to the JSON string escapes.
    return JSON.parse(key.replace(/\\U([0-9A-Fa-f]{8})/gu, (_, code) => String.fromCodePoint(parseInt(code, 16))));
  } catch { return null; }
}

function codexConfig(original, origin) {
  const settings = new Map([
    ['model_provider', 'openai'],
    ['openai_base_url', origin + '/v1'],
    ['cli_auth_credentials_store', 'file'],
  ]);
  let inRoot = true;
  const preserved = tomlStatements(utf8Text(original || '')).filter(statement => {
    if (/^\s*\[/u.test(statement)) inRoot = false;
    return !(inRoot && settings.has(rootAssignmentKey(statement)));
  }).join('');
  return [...settings].map(([key, value]) => `${key} = ${JSON.stringify(value)}\n`).join('') + preserved;
}

function atomicWrite(filename, data, mode) {
  fs.mkdirSync(path.dirname(filename), { recursive: true, mode: 0o700 });
  const temporary = filename + '.tmp-' + randomBytes(8).toString('hex');
  try {
    fs.writeFileSync(temporary, data, { flag: 'wx', mode });
    fs.chmodSync(temporary, mode);
    fs.renameSync(temporary, filename);
  } finally {
    try { fs.unlinkSync(temporary); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
}

// Take all snapshots and backups before changing any destination. Preserve
// symlinked dotfiles by writing their targets rather than replacing the links.
function commitFiles(changes, afterWrite) {
  const snapshots = [];
  const seen = new Set();
  for (const change of changes) {
    let filename = change.filename, data = null, mode = change.mode || 0o600;
    try {
      filename = fs.realpathSync(filename);
      const stat = fs.statSync(filename);
      if (!stat.isFile()) throw new Error(`配置路径不是普通文件：${filename}`);
      data = fs.readFileSync(filename);
      mode = stat.mode & 0o777;
    } catch (error) { if (error.code !== 'ENOENT') throw error; }
    if (seen.has(filename)) throw new Error(`配置文件路径重复：${filename}`);
    seen.add(filename);
    const nextMode = change.private ? 0o600 : mode;
    const nextData = Buffer.from(change.data);
    if (data && data.equals(nextData) && mode === nextMode) continue;
    snapshots.push({ filename, data, mode, nextMode, nextData });
  }
  const backups = [];
  for (const snapshot of snapshots) {
    if (snapshot.data === null) continue;
    const backup = snapshot.filename + '.bak-' + new Date().toISOString().replace(/[:.]/g, '-') + '-' + randomBytes(4).toString('hex');
    fs.writeFileSync(backup, snapshot.data, { flag: 'wx', mode: 0o600 });
    fs.chmodSync(backup, 0o600);
    backups.push(backup);
  }
  const changed = [];
  try {
    for (const snapshot of snapshots) {
      atomicWrite(snapshot.filename, snapshot.nextData, snapshot.nextMode);
      changed.push(snapshot);
    }
    if (afterWrite) afterWrite();
  } catch (error) {
    const rollbackErrors = [];
    for (const snapshot of changed.reverse()) {
      try {
        if (snapshot.data === null) fs.unlinkSync(snapshot.filename);
        else atomicWrite(snapshot.filename, snapshot.data, snapshot.mode);
      } catch { rollbackErrors.push(snapshot.filename); }
    }
    if (rollbackErrors.length) throw new Error(`配置失败，以下文件自动恢复失败，请从 .bak 备份恢复：${rollbackErrors.join('、')}`, { cause: error });
    throw error;
  }
  return backups;
}

function runCodex(args, home, context, input) {
  const options = { env: { ...context.env, CODEX_HOME: home }, cwd: home, input, encoding: 'utf8', windowsHide: true, timeout: 60000, maxBuffer: 4 * 1024 * 1024 };
  // npm installs a .cmd shim on Windows; its command is fixed and contains no key.
  const result = context.platform === 'win32'
    ? context.run(context.env.ComSpec || 'cmd.exe', ['/d', '/s', '/c', 'codex ' + args.join(' ')], options)
    : context.run('codex', args, options);
  if (result.error && result.error.code === 'ENOENT') throw new Error('找不到 Codex CLI，请先运行 npm install -g @openai/codex，并重新打开终端。');
  return result;
}

function configureCodex(key, origin, context) {
  const home = path.resolve(context.env.CODEX_HOME || path.join(context.home, '.codex'));
  const filename = path.join(home, 'config.toml');
  const original = readOptional(filename);
  // A legacy default profile may override the root provider in older clients;
  // newer clients reject it outright. Do not silently change its other values.
  if (original !== null) {
    for (const statement of tomlStatements(utf8Text(original))) {
      if (/^\s*\[/u.test(statement)) break;
      if (rootAssignmentKey(statement) === 'profile') throw new Error('检测到 Codex 默认 profile 配置，可能覆盖 Gateway 设置，未修改任何文件。请先迁移旧 profile 配置后再运行配置器。');
    }
  }
  const temporary = fs.mkdtempSync(path.join(context.temp, 'gateway-codex-'));
  fs.chmodSync(temporary, 0o700);
  try {
    if (original !== null) {
      fs.writeFileSync(path.join(temporary, 'config.toml'), original, { mode: 0o600 });
      const check = runCodex(['features', 'list'], temporary, context);
      if (check.error || check.status !== 0) throw new Error('现有 Codex 配置解析或独立校验失败，未修改任何配置；请检查 config.toml、引用文件路径和 CLI 版本。');
    }
    const candidate = codexConfig(original, origin);
    fs.writeFileSync(path.join(temporary, 'config.toml'), candidate, { mode: 0o600 });
    const check = runCodex(['features', 'list'], temporary, context);
    if (check.error || check.status !== 0) throw new Error('Codex 无法识别新的配置，未修改任何配置；请检查或更新 Codex CLI。');
    const login = runCodex(['login', '--with-api-key'], temporary, context, key + '\n');
    if (login.error || login.status !== 0) throw new Error('Codex 登录失败，未修改原配置或凭据；请确认 Codex CLI 可正常运行。');
    const auth = readOptional(path.join(temporary, 'auth.json'));
    let parsedAuth;
    try { parsedAuth = JSON.parse(auth); } catch { /* handled below */ }
    if (!parsedAuth || parsedAuth.OPENAI_API_KEY !== key) throw new Error('Codex 未成功将 API Key 保存到文件，未修改原配置或凭据。');
    return commitFiles([
      { filename, data: candidate, private: true },
      { filename: path.join(home, 'auth.json'), data: auth, private: true },
    ]);
  } finally { fs.rmSync(temporary, { recursive: true, force: true }); }
}

const sourceStart = '# >>> Gateway agy credentials >>>';
const sourceEnd = '# <<< Gateway agy credentials <<<';
function sourceCredentials(original, credentials) {
  const content = original || '';
  const begin = content.indexOf(sourceStart), end = content.indexOf(sourceEnd);
  if ((begin < 0) !== (end < 0) || (begin >= 0 && (end < begin || content.indexOf(sourceStart, begin + sourceStart.length) >= 0 || content.indexOf(sourceEnd, end + sourceEnd.length) >= 0))) {
    throw new Error('Shell 配置中的 Gateway 标记不完整或重复，未修改配置；请先检查对应文件。');
  }
  const quoted = shellQuote(credentials);
  const block = `${sourceStart}\nif [ -f ${quoted} ]; then\n  . ${quoted}\nfi\n${sourceEnd}`;
  if (begin >= 0) return content.slice(0, begin) + block + content.slice(end + sourceEnd.length);
  return content + (content && !content.endsWith('\n') ? '\n' : '') + '\n' + block + '\n';
}

const userEnvNames = ['GOOGLE_GEMINI_BASE_URL', 'GEMINI_API_KEY'];
const powershellPrelude = "$ErrorActionPreference = 'Stop'; [Console]::InputEncoding = New-Object System.Text.UTF8Encoding; [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding; ";
function windowsEnvironment(origin, key, context) {
  const options = { env: context.env, encoding: 'utf8', windowsHide: true, timeout: 30000 };
  const readScript = powershellPrelude + "@{ GOOGLE_GEMINI_BASE_URL = [Environment]::GetEnvironmentVariable('GOOGLE_GEMINI_BASE_URL', 'User'); GEMINI_API_KEY = [Environment]::GetEnvironmentVariable('GEMINI_API_KEY', 'User') } | ConvertTo-Json -Compress";
  const old = context.run('powershell.exe', ['-NoLogo', '-NoProfile', '-NonInteractive', '-Command', readScript], options);
  if (old.error || old.status !== 0) throw new Error('无法读取 Windows 用户环境变量，未保存配置。');
  let previous;
  try { previous = JSON.parse(utf8Text(old.stdout)); } catch { throw new Error('Windows 用户环境变量读取结果无效，未保存配置。'); }
  if (!previous || userEnvNames.some(name => previous[name] !== null && typeof previous[name] !== 'string')) throw new Error('Windows 用户环境变量读取结果无效，未保存配置。');
  const writeScript = powershellPrelude +
    "$gatewayValues = ConvertFrom-Json ([Console]::In.ReadToEnd()); $gatewayPrevious = @{}; " +
    "foreach ($gatewayName in @('GOOGLE_GEMINI_BASE_URL', 'GEMINI_API_KEY')) { $gatewayPrevious[$gatewayName] = [Environment]::GetEnvironmentVariable($gatewayName, 'User') }; " +
    "try { foreach ($gatewayName in @('GOOGLE_GEMINI_BASE_URL', 'GEMINI_API_KEY')) { [Environment]::SetEnvironmentVariable($gatewayName, $gatewayValues.$gatewayName, 'User') } } " +
    "catch { foreach ($gatewayName in @('GOOGLE_GEMINI_BASE_URL', 'GEMINI_API_KEY')) { [Environment]::SetEnvironmentVariable($gatewayName, $gatewayPrevious[$gatewayName], 'User') }; exit 1 }";
  return {
    previous,
    changed: previous.GOOGLE_GEMINI_BASE_URL !== origin || previous.GEMINI_API_KEY !== key,
    write() {
      const result = context.run('powershell.exe', ['-NoLogo', '-NoProfile', '-NonInteractive', '-Command', writeScript], {
        ...options, input: JSON.stringify({ GOOGLE_GEMINI_BASE_URL: origin, GEMINI_API_KEY: key }),
      });
      if (result.error || result.status !== 0) throw new Error('Windows 用户环境变量保存失败，已尝试恢复原值；文件配置已回滚。');
    },
  };
}

function configureAgy(key, origin, context) {
  const directory = path.join(context.home, '.gemini', 'antigravity-cli');
  const filename = path.join(directory, 'settings.json');
  const original = readOptional(filename);
  let settings = {};
  if (original !== null) {
    try { settings = JSON.parse(utf8Text(original)); } catch { throw new Error('agy settings.json 解析失败，未修改任何配置。'); }
    if (!settings || typeof settings !== 'object' || Array.isArray(settings)) throw new Error('agy settings.json 必须是 JSON 对象，未修改任何配置。');
  }
  settings.modelProvider = 'gemini';
  const changes = [{ filename, data: JSON.stringify(settings, null, 2) + '\n', private: true }];
  if (context.platform === 'win32') {
    const environment = windowsEnvironment(origin, key, context);
    let backup;
    if (environment.changed) {
      backup = path.join(directory, 'gateway-environment.bak-' + new Date().toISOString().replace(/[:.]/g, '-') + '-' + randomBytes(4).toString('hex') + '.json');
      // Save the original user variables before changing them. Keep this backup
      // even when the subsequent registry operation fails.
      atomicWrite(backup, JSON.stringify(environment.previous, null, 2) + '\n', 0o600);
    }
    const backups = commitFiles(changes, environment.changed ? () => environment.write() : undefined);
    if (backup) backups.push(backup);
    return backups;
  }
  const credentials = path.join(directory, 'gateway-credentials.sh');
  changes.push({ filename: credentials, private: true, data: `# Gateway agy credentials; keep this file private.\nexport GOOGLE_GEMINI_BASE_URL=${shellQuote(origin)}\nexport GEMINI_API_KEY=${shellQuote(key)}\n` });
  const loginFile = ['.bash_profile', '.bash_login', '.profile'].map(name => path.join(context.home, name)).find(name => fs.existsSync(name)) || path.join(context.home, '.profile');
  const zshHome = path.resolve(context.env.ZDOTDIR || context.home);
  const shells = [path.join(context.home, '.bashrc'), loginFile, path.join(zshHome, '.zshrc'), path.join(zshHome, '.zprofile')];
  for (const shell of new Set(shells)) changes.push({ filename: shell, data: sourceCredentials(readOptional(shell), credentials), mode: 0o644 });
  return commitFiles(changes);
}

function configureClient(client, rawOrigin, rawKey, options = {}) {
  if (!['codex', 'agy'].includes(client)) throw new Error('客户端参数必须为 codex 或 agy。');
  const origin = originURL(rawOrigin), key = secretValue(rawKey);
  const context = { home: os.homedir(), temp: os.tmpdir(), env: process.env, platform: process.platform, run: spawnSync, ...options };
  return client === 'codex' ? configureCodex(key, origin, context) : configureAgy(key, origin, context);
}

async function main() {
  try {
    const [client, origin, ...extra] = process.argv.slice(2);
    if (!['codex', 'agy'].includes(client) || !origin || extra.length) throw new Error('用法：node configure-client.cjs <codex|agy> <Gateway origin>');
    originURL(origin);
    const key = await readSecret();
    const backups = configureClient(client, origin, key);
    process.stdout.write(`\n${client} 配置和 API Key 已保存。${backups.length ? `已创建 ${backups.length} 个 .bak 备份文件。` : ''}\n`);
    process.stdout.write(`请完全退出并重新打开终端${process.platform === 'win32' ? '（包括 Windows Terminal / VS Code；若仍读取旧环境变量，请注销并重新登录 Windows）' : ''}，然后运行 ${client === 'codex' ? 'codex' : 'agy --model gemini-pro-agent'} 验证。\n`);
    if (client === 'agy') process.stdout.write('客户端必须原样发送已授权的 CPA 原生模型 ID；Gateway 不改写旧别名。推理档位通过请求参数设置。旧 CLI 会话缺少签名时请新建会话。\n');
  } catch (error) {
    process.stderr.write(`配置失败：${error.message}\n`);
    process.exitCode = 1;
  }
}

module.exports = { configureClient, codexConfig, sourceCredentials, readSecret, originURL, commitFiles };
if (require.main === module) main();
