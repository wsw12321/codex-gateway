export type Provider = 'codex' | 'antigravity';
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
export type Quota = { model: string; remaining_fraction: number; reset_time?: string };

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
  begin: (provider: Provider) => request<OAuth>(`${provider}/oauth`, 'POST', {}),
  poll: (id: string) => request<{ status: 'ok' | 'wait' | 'error' }>(`oauth/${id}/status`),
  callback: (id: string, code: string, state: string) => request(`oauth/${id}/callback`, 'POST', { code, state }),
  refresh: (provider: Provider, id: string) => request(`${provider}/accounts/${id}/refresh`, 'POST', {}),
  quota: (provider: Provider, id: string) => request<{ quota: Quota[] }>(`${provider}/accounts/${id}/quota`, 'POST', {}),
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
  if (!code || !state || url.searchParams.get('error')) throw new Error('回调缺少授权码或授权未成功');
  return { code, state };
}

export function importFields(raw: string): Record<string, string> {
  const value: unknown = JSON.parse(raw);
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('凭据必须是 JSON 对象');
  const input = value as Record<string, unknown>;
  if (typeof input.refresh_token !== 'string' || !input.refresh_token.trim()) throw new Error('凭据缺少 refresh_token');
  const result: Record<string, string> = { refresh_token: input.refresh_token };
  for (const key of ['access_token', 'id_token']) {
    if (typeof input[key] === 'string') result[key] = input[key];
  }
  return result;
}
