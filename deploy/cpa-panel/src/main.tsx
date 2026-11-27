import { useEffect, useRef, useState, type FormEvent } from 'react';
import { createRoot } from 'react-dom/client';
import { Button } from '../upstream/Button';
import { Card } from '../upstream/Card';
import { credentialMessage, accountIdentity, api, callbackFields, importFields, type Account, type EgressServer, type OAuth, type Provider, type QuotaSnapshot } from './api';
import './panel.css';

function SupplierForm({provider, id = '', egressID, disabled, onSaved, onError}: {
  provider: Provider; id?: string; egressID?: string; disabled: boolean; onSaved: () => Promise<void>; onError: (message: string) => void;
}) {
  const [models, setModels] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const fields = new FormData(form);
    const key = form.elements.namedItem('api_key') as HTMLInputElement;
    const url = id ? undefined : String(fields.get('url') || '');
    const selection = fields.getAll('models').map(String);
    key.value = '';
    setSaving(true);
    onError('');
    try {
      const result = await api.saveSupplier(provider, id, {url, api_key: String(fields.get('api_key') || ''), models: selection, egress_server_id: id ? undefined : egressID});
      if (result.status === 'models_required') {
        setModels(result.models);
        onError('供应商未返回匹配的模型，请勾选可用模型并重新输入 API Key。');
      } else {
        form.reset();
        setModels([]);
        await onSaved();
      }
    } catch (error) {
      onError(error instanceof Error ? error.message : '保存失败');
    } finally {
      setSaving(false);
    }
  }
  return <form onSubmit={submit} aria-busy={saving}>
    {!id && <label>HTTPS Base URL<input name="url" type="url" autoComplete="off" required maxLength={512} disabled={disabled || saving}/></label>}
    <label>{id ? '新的 API Key' : 'API Key'}<input name="api_key" type="password" autoComplete="new-password" required maxLength={4096} disabled={disabled || saving}/></label>
    {models.length > 0 && <fieldset className="supplier-models" disabled={disabled || saving}><legend>该供应商支持的已定价模型</legend>{models.map(model => <label key={model}><input name="models" type="checkbox" value={model}/>{model}</label>)}</fieldset>}
    <Button type="submit" disabled={disabled || saving}>{saving ? '核验中…' : id ? '保存新 Key' : '核验并添加'}</Button>
  </form>;
}

function App() {
  const [provider, setProvider] = useState<Provider>('codex');
  const providerRef = useRef(provider);
  providerRef.current = provider;
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [oauth, setOAuth] = useState<OAuth | null>(null);
  const [callback, setCallback] = useState('');
  const [quota, setQuota] = useState<QuotaSnapshot | null>(null);
  const [deleteID, setDeleteID] = useState('');
  const [egressServers, setEgressServers] = useState<EgressServer[]>([]);
  const [egressID, setEgressID] = useState('__preserve__');
  const [egressReady, setEgressReady] = useState(false);
  const [suppliersEnabled, setSuppliersEnabled] = useState(false);
  const [rotateID, setRotateID] = useState('');
  const selectedEgress = egressID === '__preserve__' ? undefined : egressID;
  const providerName = {codex: 'Codex', antigravity: 'Antigravity', anthropic: 'Claude'}[provider];

  async function refresh() {
    const result = await api.accounts(provider);
    if (providerRef.current !== provider) return;
    setAccounts(result.accounts);
    setSuppliersEnabled(result.suppliers_enabled === true);
    if (result.sync_warning) setMessage('账号状态同步暂不可用，请稍后刷新');
  }

  async function run(operation: () => Promise<unknown>, success = '操作完成') {
    setBusy(true); setMessage('');
    try { const result = await operation(); setMessage(typeof result === 'string' ? result : success); }
    catch (error) { setMessage(error instanceof Error ? error.message : '操作未完成'); }
    finally { setBusy(false); }
  }

  useEffect(() => {
    let active = true;
    api.egressServers().then(result => { if (active) { setEgressServers(result); setEgressReady(true); } })
      .catch(error => { if (active) setMessage(error.message); });
    return () => { active = false; };
  }, []);

  useEffect(() => {
    let active = true;
    setAccounts([]); setOAuth(null); setCallback(''); setQuota(null); setDeleteID(''); setRotateID(''); setSuppliersEnabled(false); setMessage('');
    api.accounts(provider).then(result => { if (active) { setAccounts(result.accounts); setSuppliersEnabled(result.suppliers_enabled === true); } })
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
        if (result.status === 'ok') { setMessage(credentialMessage(result.already_present)); void refresh(); }
        else setMessage(result.error === 'account_owned_elsewhere' ? '该账号已归其他用户所有。' : '授权未完成，请重新授权');
      }).catch(() => { if (active) { setOAuth(null); setMessage('授权流程已过期，请重新授权'); } });
    }, 2000);
    return () => { active = false; clearInterval(id); };
  }, [oauth]);

  return <main>
    <header className="page-header"><div><p className="eyebrow">百川阁 · 站点管理</p><h1>CPA 账号管理</h1><p>维护授权、凭据与供应商额度。</p></div><a href="/">返回控制台 ↗</a></header>
    <nav aria-label="供应商"><Button disabled={busy} variant={provider === 'codex' ? 'primary' : 'secondary'} onClick={() => setProvider('codex')}>Codex</Button><Button disabled={busy} variant={provider === 'antigravity' ? 'primary' : 'secondary'} onClick={() => setProvider('antigravity')}>Antigravity</Button><Button disabled={busy} variant={provider === 'anthropic' ? 'primary' : 'secondary'} onClick={() => setProvider('anthropic')}>Claude 账号</Button></nav>
    <p className="notice">凭据操作需要近期身份验证。若提示验证过期，请返回控制台验证后重试。账号共享、权重、权限和计费在百川阁控制台管理。</p>
    <div role="status" aria-live="polite" className="message">{message}</div>
    <Card title={`${providerName} 授权`}>
      <label htmlFor="egress">新增凭据使用的出口</label><select id="egress" disabled={busy || !!oauth || !egressReady} value={egressID} onChange={event => setEgressID(event.target.value)}>
        <option value="__preserve__">保留已有绑定；新账号继承全局出口</option><option value="">继承全局出口</option>
        {egressServers.map(node => <option key={node.id} value={node.id}>{node.name}</option>)}
      </select><p className="hint">服务器端授权与凭据校验使用所选出口，成功后自动绑定。浏览器授权页面使用本地网络。<a href="/#egress-servers">管理出口服务器 ↗</a></p>
      <div className="actions"><Button disabled={busy || !egressReady} onClick={() => run(async () => setOAuth(await api.begin(provider, selectedEgress)), '请打开授权页面并完成登录')}>新增 / 重新授权</Button><label className="file-label">导入 OAuth 凭据<input type="file" accept="application/json,.json" disabled={busy || !egressReady} onChange={event => {
        const file = event.currentTarget.files?.[0]; event.currentTarget.value = '';
        if (!file) return;
        void run(async () => {
          if (file.size > 128 * 1024) throw new Error('凭据文件过大');
          const credentials = importFields(await file.text(), provider);
          try {
            const result = await api.import(provider, credentials, selectedEgress);
            await refresh();
            return credentialMessage(result.already_present, true);
          } finally { for (const key of Object.keys(credentials)) credentials[key] = ''; }
        });
      }}/></label></div>
      <p className="hint">导入时验证真实身份，有刷新令牌时先刷新；同一账号重新授权保留本站权限。{provider === 'anthropic' ? '支持 CPA OAuth 凭据和 Claude Code claudeAiOauth 格式；只提取必要令牌；仅访问令牌的账号到期后不能自动刷新，套餐无法验证时显示未知。' : '迁移旧 Keyring 请使用服务端迁移工具。'}</p>
      {oauth && <form onSubmit={event => { event.preventDefault(); const raw = callback; setCallback(''); void run(async () => { const fields = callbackFields(raw); await api.callback(oauth.id, fields.code, fields.state); }, '已提交，正在完成授权'); }}>
        <a className="authorize" href={oauth.url} target="_blank" rel="noopener noreferrer">打开供应商授权页面 ↗</a>
        <p>登录后浏览器可能显示本地地址无法连接。复制地址栏中的完整回调 URL，在 5 分钟内提交。</p>
        <label htmlFor="callback">授权回调地址</label><input id="callback" type="url" autoComplete="off" value={callback} onChange={event => setCallback(event.target.value)} required/>
        <Button type="submit" disabled={busy || !callback}>提交回调</Button>
      </form>}
    </Card>
    {suppliersEnabled && <Card title="添加 API Key 供应商">
      <SupplierForm key={provider} provider={provider} egressID={selectedEgress} disabled={busy || !egressReady} onError={setMessage} onSaved={async () => { await refresh(); setMessage('供应商已添加'); }}/>
    </Card>}
    <Card title="账号" extra={<Button variant="secondary" size="sm" disabled={busy} onClick={() => run(refresh, '账号列表已更新')}>刷新列表</Button>}>
      {accounts.length === 0 && <p className="empty">暂无已登记账号</p>}
      <div className="accounts">{accounts.filter(account => account.credential_kind !== 'supplier' || account.can_manage).map(account => { const identity = accountIdentity(account); const supplier = account.credential_kind === 'supplier'; return <article key={account.id}>
        <div><h2>{identity.title}</h2><p>{identity.details.map(detail => `${detail} · `).join('')}<code>{account.id}</code></p>{supplier ? <p>API Key 供应商 · {account.gateway_manual_status === 'enabled' ? '已启用' : '已停用'} · {account.supplier_models?.join('、')}</p> : <p>供应商：{account.cliproxy_status} · 本站：{account.gateway_manual_status} · 额度：{account.gateway_quota_status}</p>}<p>出口：{account.egress_server_name || (account.egress_server_id ? '已绑定节点' : '继承全局出口')}</p></div>
        <div className="actions">
          <Button variant="secondary" size="sm" disabled={busy} onClick={() => run(async () => { await api.status(provider, account.id, account.gateway_manual_status !== 'enabled'); await refresh(); })}>{account.gateway_manual_status === 'enabled' ? '停用' : '启用'}</Button>
          {supplier ? <Button variant="secondary" size="sm" disabled={busy} aria-expanded={rotateID === account.id} onClick={() => setRotateID(rotateID === account.id ? '' : account.id)}>更换 API Key</Button> : <>
            <Button variant="secondary" size="sm" disabled={busy} onClick={() => run(async () => { await api.refresh(provider, account.id); await refresh(); })}>刷新凭据</Button>
            <Button variant="secondary" size="sm" disabled={busy} onClick={() => run(async () => setQuota(await api.quota(provider, account.id)), '额度已更新')}>{provider === 'anthropic' ? '刷新状态' : '查询额度'}</Button>
          </>}
          <Button variant="danger" size="sm" disabled={busy} onClick={() => setDeleteID(account.id)}>删除凭据</Button>
        </div>
        {supplier && rotateID === account.id && <SupplierForm key={account.id} id={account.id} provider={provider} disabled={busy} onError={setMessage} onSaved={async () => { setRotateID(''); await refresh(); setMessage('API Key 已更换'); }}/>}
        {deleteID === account.id && <div className="confirm"><p>将先停用 {identity.title} 并等待请求结束，再删除凭据。历史账单和账号权限记录将保留。</p><Button variant="danger" disabled={busy} onClick={() => run(async () => { await api.remove(provider, account.id); setDeleteID(''); await refresh(); }, '凭据已删除')}>确认删除</Button> <Button variant="secondary" disabled={busy} onClick={() => setDeleteID('')}>取消</Button></div>}
      </article>; })}</div>
    </Card>
    {quota && <Card title={provider === 'anthropic' ? 'Claude 观测额度' : '供应商额度'}>
      {provider === 'anthropic' ? <><p>观测时间：{quota.observed_at || '未知'} · 套餐：未知 · 冷却至：{quota.cooldown_until || '无观测'}</p><ul>{['five_hour', 'seven_day'].map(window => {
        const item = quota.windows?.find(value => value.window === window);
        return <li key={window}>{window === 'five_hour' ? '五小时窗口' : '七天窗口'}：{typeof item?.used_percent === 'number' ? `已用 ${item.used_percent}%` : '额度未知'} · 重置 {item?.reset_at || '未知'}</li>;
      })}</ul><p className="hint">刷新只读取 CPA 最新观测快照；没有新的上游响应时观测时间不会改变。</p></> : <>{!quota.quota.length && <p>未知：尚无额度观测数据。</p>}<ul>{quota.quota.map((item, index) => <li key={`${item.model}-${index}`}>{item.model}：剩余 {typeof item.remaining_fraction === 'number' ? `${(item.remaining_fraction * 100).toFixed(1)}%` : '未知'} · 重置 {item.reset_time || '未知'}</li>)}</ul></>}
    </Card>}
    <footer>百川阁 · 账号管理</footer>
  </main>;
}

createRoot(document.getElementById('root')!).render(<App />);
