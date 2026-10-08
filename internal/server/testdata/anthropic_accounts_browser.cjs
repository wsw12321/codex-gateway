'use strict';
// Synthetic accounts and credentials only. All requests are intercepted.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.join(__dirname, '../assets');
const screenshots = process.env.SCREENSHOT_DIR || path.join(__dirname, '../../../docs/screenshots');
const source = fs.readFileSync(path.join(assets, 'app.js'), 'utf8');
const app = source.slice(0, source.lastIndexOf('\nstart().catch('));
const origin = 'http://127.0.0.1:8765';
const initial = {user: {id: 'owner', username: 'owner', display_name: '团队管理员', role: 'owner', status: 'active'}, recently_verified: true,
  login_methods: {password: true}, devices: [], projects: [], api_keys: [], passkeys: []};
const accounts = [{id: 'claude-alpha', display_name: '研发团队 Claude', email_masked: 'cl***@example.test', allocation_weight: 3, concurrent_limit: 2,
  status: 'available', cliproxy_status: 'active', gateway_manual_status: 'enabled', gateway_quota_status: 'available', can_manage: true,
  plan: '', rolling_cost_usd: '21.4532', rolling_cost_share: '1', target_share: '1', request_count: 198, input_tokens: 912800,
  cached_input_tokens: 520000, cache_write_tokens: 125000, cache_write_5m_tokens: 100000, cache_write_1h_tokens: 25000,
  output_tokens: 42120, error_count: 1, equivalent_cost_usd: '64.942', access_mode: 'shared', authorized_user_ids: [], last_synced_at: '2026-10-08T04:00:00Z'}];
async function main() {
  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1100}, locale: 'zh-CN'});
    const errors = [], writes = []; let observed = false;
    page.on('pageerror', error => errors.push(error.message));
    await page.route('**/*', async route => {
      const request = route.request(), url = new URL(request.url()); assert.equal(url.origin, origin);
      const send = (body, type = 'application/json') => route.fulfill({status: 200, contentType: type, body: typeof body === 'string' ? body : JSON.stringify(body)});
      if (url.pathname === '/') return send(fs.readFileSync(path.join(assets, 'index.html'), 'utf8'), 'text/html');
      const filename = {'/static/app.js': ['application/javascript', null], '/static/style.css': ['text/css', 'style.css'], '/static/theme.js': ['application/javascript', 'theme.js'], '/static/favicon.svg': ['image/svg+xml', 'favicon.svg']}[url.pathname];
      if (filename) return send(filename[1] ? fs.readFileSync(path.join(assets, filename[1]), 'utf8') : app, filename[0]);
      if (url.pathname === '/favicon.ico') return route.fulfill({status: 204});
      if (url.pathname === '/admin/anthropic-accounts') return send({all: true, accounts: structuredClone(accounts), until: new Date().toISOString()});
      if (url.pathname === '/admin/anthropic-accounts/concurrency') return send({sampled_at: new Date().toISOString(), accounts: [{id: 'claude-alpha', active_requests: 1}]});
      if (url.pathname === '/admin/cpa/api/anthropic/accounts/claude-alpha/quota') {
        assert.equal(request.method(), 'POST');
        if (!observed) return send({status: 'ok', quota: [], windows: []});
        return send({status: 'ok', quota: [], observed_at: '2026-10-08T04:01:00Z', cooldown_until: '2026-10-08T05:00:00Z', windows: [
          {window: 'five_hour', used_percent: 82, reset_at: '2026-10-08T05:00:00Z', status: 'allowed_warning'},
          {window: 'seven_day', used_percent: 46, reset_at: '2026-10-12T00:00:00Z', status: 'allowed'},
        ]});
      }
      if (url.pathname === '/admin/billing/me') return send({cash_balance_usd: '0', subscriptions: [], ledger: []});
      if (url.pathname === '/admin/usage') return send({summary: {requests: 0, tokens: 0}, requests: []});
      if (url.pathname === '/admin/anthropic-accounts/claude-alpha/allocation-weight' && request.method() === 'PUT') {
        const body = request.postDataJSON(); writes.push(body); accounts[0].allocation_weight = body.weight; return send(accounts[0]);
      }
      throw new Error('Unexpected request ' + request.method() + ' ' + url.pathname);
    });
    await page.goto(origin + '/#anthropic-accounts');
    await page.evaluate(value => {bindUI(); initializeDateFilters(); renderState(value); setConnection('已连接', 'ok');}, initial);
    await page.waitForFunction(() => !upstreamAccountListLoading);
    const card = page.locator('[data-account-id="claude-alpha"]');
    await card.locator('summary').first().click();
    assert.match(await page.locator('#upstream-quota-warning').textContent(), /五小时、七天/);
    assert.match(await card.textContent(), /套餐未知/);
    const refresh = card.getByRole('button', {name: '刷新状态', exact: true});
    await refresh.click();
    await page.waitForFunction(() => document.querySelector('.upstream-quota-result').dataset.state === 'observed');
    assert.match(await card.locator('.upstream-quota-result').textContent(), /观测时间：未知/);
    assert.equal((await card.locator('.upstream-quota-window').allTextContents()).filter(value => value.includes('额度：未知')).length, 2);
    observed = true; await refresh.click();
    await page.waitForFunction(() => document.querySelector('.upstream-quota-result').textContent.includes('82%'));
    assert.match(await card.locator('.upstream-quota-result').textContent(), /五小时窗口已用 82%.*七天窗口已用 46%/);
    await card.locator('.upstream-allocation-input').fill('4');
    await card.locator('.upstream-allocation-save').click();
    await page.waitForFunction(() => !upstreamAccountOperation && !upstreamAccountListLoading);
    assert.equal(writes[0].weight, 4);
    assert.match(await card.locator('.upstream-quota-result').textContent(), /82%/);
    await page.evaluate(() => {document.activeElement?.blur(); window.scrollTo(0, 0);});
    await fs.promises.mkdir(screenshots, {recursive: true});
    await page.screenshot({path: path.join(screenshots, 'anthropic-accounts-desktop.png'), fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    await page.evaluate(() => {document.activeElement?.blur(); window.scrollTo(0, 0);});
    await page.screenshot({path: path.join(screenshots, 'anthropic-accounts-mobile.png'), fullPage: true});
    await page.evaluate(() => {stopUpstreamConcurrency(); location.hash = '#guide';});
    await page.locator('#guide-claude').waitFor({state: 'visible'});
    assert.match(await page.locator('#guide-claude-configure-code').textContent(), /claude/);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    await page.screenshot({path: path.join(screenshots, 'claude-guide-mobile.png'), fullPage: true});
    assert.deepEqual(errors, []);
    console.log('Claude account controls, quota snapshots, guide and responsive screenshots passed');
  } finally {await browser.close();}
}
main().catch(error => {console.error(error); process.exitCode = 1;});
