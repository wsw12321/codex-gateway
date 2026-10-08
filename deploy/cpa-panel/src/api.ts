export type Provider = 'codex' | 'antigravity' | 'anthropic';
export type Account = {
  id: string;
  display_name?: string;
  email_masked: string;
  gateway_manual_status: string;
  cliproxy_status: string;
  gateway_quota_status: string;
  status: string;
};
export type OAuth = { id: string; url: string; expires_in: number };
export type QuotaSnapshot = {quota: Quota[]; observed_at?: string; cooldown_until?: string; windows?: {window: string; used_percent?: number; reset_at?: string; status: string}[]};
export type Quota = { model: string; remaining_fraction?: number; reset_time?: string; observed_at?: string; cooldown_until?: string };

async function request<T>(path: string, method = 'GET', data?: unknown): Promise<T> {
  const response = await fetch('/admin/cpa/api/' + path, {
    method,
    credentials: 'same-origin',
    redirect: 'error',
    headers: data === undefined ? {} : { 'Content-Type': 'application/json' },
    body: data === undefined ? undefined : JSON.stringify(data),
  });
  if (!response.ok) {
    const payload = await response.json().catch(() => ({}));
    throw new Error(payload?.error?.message || `操作未完成 (${response.status})`);
  }
  return response.json();
}

export const api = {
  accounts: (provider: Provider) => request<{ accounts: Account[]; sync_warning?: string }>(`${provider}/accounts?all=true`),
  status: (provider: Provider, id: string, enabled: boolean) => request(`${provider}/accounts/${id}/status`, 'PUT', { enabled }),
  begin: async (provider: Provider) => { const oauth = await request<OAuth>(`${provider}/oauth`, 'POST', {}); authorizationURL(oauth.url, provider); return oauth; },
  poll: (id: string) => request<{ status: 'ok' | 'wait' | 'error' }>(`oauth/${id}/status`),
  callback: (id: string, code: string, state: string) => request(`oauth/${id}/callback`, 'POST', { code, state }),
  refresh: (provider: Provider, id: string) => request(`${provider}/accounts/${id}/refresh`, 'POST', {}),
  quota: (provider: Provider, id: string) => request<QuotaSnapshot>(`${provider}/accounts/${id}/quota`, 'POST', {}),
  remove: (provider: Provider, id: string) => request(`${provider}/accounts/${id}`, 'DELETE', {}),
  import: (provider: Provider, credentials: Record<string, string>) => request(`${provider}/credentials`, 'POST', credentials),
};

export function callbackFields(raw: string): { code: string; state: string } {
  const url = new URL(raw);
  if (url.protocol !== 'http:' || !['localhost', '127.0.0.1', '[::1]'].includes(url.hostname) || url.username || url.password || url.hash) {
    throw new Error('请粘贴授权完成后的本地回调地址');
  }
  const code = url.searchParams.get('code');
  const state = url.searchParams.get('state');
  if (!code || !state || url.searchParams.getAll('code').length !== 1 || url.searchParams.getAll('state').length !== 1 || url.searchParams.has('error')) throw new Error('回调缺少授权码或授权未成功');
  return { code, state };
}

export function authorizationURL(raw: string, provider: Provider): string {
  const url = new URL(raw);
  const hosts = {codex: ['auth.openai.com'], antigravity: ['accounts.google.com'], anthropic: ['claude.ai']};
  if (url.protocol !== 'https:' || url.port || url.username || url.password || !hosts[provider].includes(url.hostname)) throw new Error('供应商授权地址无效');
  return url.href;
}

export function importFields(raw: string, provider: Provider = 'codex'): Record<string, string> {
  const value: unknown = JSON.parse(raw);
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('凭据必须是 JSON 对象');
  let input = value as Record<string, unknown>;
  if (provider === 'anthropic' && 'claudeAiOauth' in input) {
    const oauth = input.claudeAiOauth;
    if (!oauth || typeof oauth !== 'object' || Array.isArray(oauth)) throw new Error('claudeAiOauth 必须是 JSON 对象');
    const tokens = oauth as Record<string, unknown>;
    input = {refresh_token: tokens.refreshToken, access_token: tokens.accessToken};
  }
  const result: Record<string, string> = {};
  for (const key of (provider === 'anthropic' ? ['refresh_token', 'access_token'] : ['refresh_token', 'access_token', 'id_token'])) {
    const token = input[key];
    if (token === undefined) continue;
    if (typeof token !== 'string' || new TextEncoder().encode(token).length > 32768) throw new Error('令牌必须是最多 32768 字节的字符串');
    if (token.trim()) result[key] = token;
  }
  if (provider === 'anthropic') {
    if (!result.refresh_token && !result.access_token) throw new Error('Claude 凭据至少需要 access_token 或 refresh_token');
  } else if (!result.refresh_token) throw new Error('凭据缺少 refresh_token');
  return result;
}
