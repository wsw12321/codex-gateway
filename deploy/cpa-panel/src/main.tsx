import { useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Button } from '../upstream/Button';
import { Card } from '../upstream/Card';
import { api, callbackFields, importFields, type Account, type OAuth, type Provider, type Quota } from './api';
import './panel.css';

function App() {
  const [provider, setProvider] = useState<Provider>('codex');
  const providerRef = useRef(provider);
  providerRef.current = provider;
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [oauth, setOAuth] = useState<OAuth | null>(null);
  const [callback, setCallback] = useState('');
  const [quota, setQuota] = useState<Quota[] | null>(null);
  const [deleteID, setDeleteID] = useState('');

  async function refresh() {
    const result = await api.accounts(provider);
    if (providerRef.current !== provider) return;
    setAccounts(result.accounts);
    if (result.sync_warning) setMessage('账号状态同步暂不可用，请稍后刷新');
  }

  async function run(operation: () => Promise<unknown>, success = '操作完成') {
    setBusy(true); setMessage('');
    try { await operation(); setMessage(success); }
    catch (error) { setMessage(error instanceof Error ? error.message : '操作未完成'); }
    finally { setBusy(false); }
  }

  useEffect(() => {
    let active = true;
    setAccounts([]); setOAuth(null); setCallback(''); setQuota(null); setDeleteID(''); setMessage('');
    api.accounts(provider).then(result => { if (active) setAccounts(result.accounts); })
      .catch(error => { if (active) setMessage(error.message); });
    return () => { active = false; };
  }, [provider]);

  useEffect(() => {
    if (!oauth) return;
    let active = true;
    const id = setInterval(() => {
      api.poll(oauth.id).then(result => {
        if (!active || result.status === 'wait') return;
        setOAuth(null); setCallback('');
        if (result.status === 'ok') { setMessage('授权完成'); void refresh(); }
        else setMessage('授权未完成，请重新授权');
      }).catch(() => { if (active) { setOAuth(null); setMessage('授权流程已过期，请重新授权'); } });
    }, 2000);
    return () => { active = false; clearInterval(id); };
  }, [oauth]);

  return <main>
    <header className="page-header"><div><p className="eyebrow">水源喵中转站 · 站点管理</p><h1>CPA 账号管理</h1><p>维护授权、凭据与供应商额度。</p></div><a href="/">返回控制台 ↗</a></header>
    <nav aria-label="供应商"><Button disabled={busy} variant={provider === 'codex' ? 'primary' : 'secondary'} onClick={() => setProvider('codex')}>Codex</Button><Button disabled={busy} variant={provider === 'antigravity' ? 'primary' : 'secondary'} onClick={() => setProvider('antigravity')}>Antigravity</Button></nav>
    <p className="notice">凭据操作需要近期身份验证。若提示验证过期，请返回控制台验证后重试。账号共享、权重、权限和计费在水源喵控制台管理。</p>
    <div role="status" aria-live="polite" className="message">{message}</div>
    <Card title={`${provider === 'codex' ? 'Codex' : 'Antigravity'} 授权`}>
      <div className="actions"><Button disabled={busy} onClick={() => run(async () => setOAuth(await api.begin(provider)), '请打开授权页面并完成登录')}>新增 / 重新授权</Button><label className="file-label">导入 OAuth 凭据<input type="file" accept="application/json,.json" disabled={busy} onChange={event => {
        const file = event.currentTarget.files?.[0]; event.currentTarget.value = '';
        if (!file) return;
        void run(async () => {
          if (file.size > 128 * 1024) throw new Error('凭据文件过大');
          const credentials = importFields(await file.text());
          try { await api.import(provider, credentials); await refresh(); }
          finally { for (const key of Object.keys(credentials)) credentials[key] = ''; }
        });
      }}/></label></div>
      <p className="hint">导入时会验证刷新和真实身份；同一账号重新授权保留中转站权限。迁移旧 Keyring 请使用服务端迁移工具。</p>
      {oauth && <form onSubmit={event => { event.preventDefault(); const raw = callback; setCallback(''); void run(async () => { const fields = callbackFields(raw); await api.callback(oauth.id, fields.code, fields.state); }, '已提交，正在完成授权'); }}>
        <a className="authorize" href={oauth.url} target="_blank" rel="noopener noreferrer">打开供应商授权页面 ↗</a>
        <p>登录后浏览器可能显示本地地址无法连接。复制地址栏中的完整回调 URL，在 5 分钟内提交。</p>
        <label htmlFor="callback">授权回调地址</label><input id="callback" type="url" autoComplete="off" value={callback} onChange={event => setCallback(event.target.value)} required/>
        <Button type="submit" disabled={busy || !callback}>提交回调</Button>
      </form>}
    </Card>
    <Card title="账号" extra={<Button variant="secondary" size="sm" disabled={busy} onClick={() => run(refresh, '账号列表已更新')}>刷新列表</Button>}>
      {accounts.length === 0 && <p className="empty">暂无已登记账号</p>}
      <div className="accounts">{accounts.map(account => <article key={account.id}>
        <div><h2>{account.display_name || account.email_masked || account.id}</h2><p>{account.email_masked} · <code>{account.id}</code></p><p>供应商：{account.cliproxy_status} · 中转站：{account.gateway_manual_status} · 额度：{account.gateway_quota_status}</p></div>
        <div className="actions">
          <Button variant="secondary" size="sm" disabled={busy} onClick={() => run(async () => { await api.status(provider, account.id, account.gateway_manual_status !== 'enabled'); await refresh(); })}>{account.gateway_manual_status === 'enabled' ? '停用' : '启用'}</Button>
          <Button variant="secondary" size="sm" disabled={busy} onClick={() => run(async () => { await api.refresh(provider, account.id); await refresh(); })}>刷新凭据</Button>
          <Button variant="secondary" size="sm" disabled={busy} onClick={() => run(async () => setQuota((await api.quota(provider, account.id)).quota), '额度已更新')}>查询额度</Button>
          <Button variant="danger" size="sm" disabled={busy} onClick={() => setDeleteID(account.id)}>删除凭据</Button>
        </div>
        {deleteID === account.id && <div className="confirm"><p>将先停用账号并等待请求结束，再删除凭据。历史账单和账号权限记录将保留。</p><Button variant="danger" disabled={busy} onClick={() => run(async () => { await api.remove(provider, account.id); setDeleteID(''); await refresh(); }, '凭据已删除')}>确认删除</Button> <Button variant="secondary" disabled={busy} onClick={() => setDeleteID('')}>取消</Button></div>}
      </article>)}</div>
    </Card>
    {quota && <Card title="供应商额度"><ul>{quota.map((item, index) => <li key={`${item.model}-${index}`}>{item.model}：剩余 {(item.remaining_fraction * 100).toFixed(1)}%{item.reset_time ? ` · 重置 ${item.reset_time}` : ''}</li>)}</ul></Card>}
    <footer>水源喵中转站 · 账号管理</footer>
  </main>;
}

createRoot(document.getElementById('root')!).render(<App />);
