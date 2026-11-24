import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { createAgent, createAgentDatabase, discoverTools, listAgents, type ToolDefinition } from '../api2'
import { Alert, Badge, Card, DataTable, Field, LoadMore, Modal, Mono, Notice, PageHeader, useCursorList } from '../admin'
import { formatDate } from '../utils'

export function AgentsPage({ token }: { token: string }) {
  const client = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const agents = useCursorList(['agents', token], token !== '', (cursor) => listAgents(token, cursor))

  return (
    <>
      <PageHeader title="Agent 管理" description="登记被测 Agent。平台只做评测：Agent 自己决定支持哪些 Tool、是否调用。"
        actions={<button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>新建 Agent</button>} />
      {token === '' && <Notice>请先输入 Platform API token。</Notice>}
      <Alert error={agents.query.error} />
      <Card flush>
        <DataTable label="Agent 列表" rows={agents.rows} rowKey={(agent) => agent.id} loading={agents.query.isPending && token !== ''}
          columns={[
            { key: 'name', title: '名称', render: (agent) => <strong>{agent.name}</strong> },
            { key: 'desc', title: '描述', render: (agent) => agent.description || <span className="muted">—</span> },
            { key: 'ep', title: '对话端点', render: (agent) => <Mono>{agent.chat_endpoint}</Mono> },
            { key: 'model', title: '被测模型', render: (agent) => agent.target_model },
            { key: 'mode', title: 'Tool 评估', render: (agent) => <Badge value={agent.tool_evaluation_mode === 'DETERMINISTIC_CHAIN' ? '确定性调用链' : '仅会话评测'} tone="info" /> },
            { key: 'tools', title: 'Tool 数', render: (agent) => agent.tool_catalog?.length ?? 0 },
            { key: 'db', title: '数据库', render: (agent) => (agent.database_profile_id ? <Badge value="已配置" tone="info" /> : <span className="muted">未配置</span>) },
            { key: 'created', title: '创建时间', render: (agent) => formatDate(agent.created_at) },
          ]} />
        <LoadMore query={agents.query} />
      </Card>
      {createOpen && <CreateAgent token={token} onClose={() => setCreateOpen(false)} onDone={() => { setCreateOpen(false); void client.invalidateQueries({ queryKey: ['agents'] }) }} />}
    </>
  )
}

function CreateAgent({ token, onClose, onDone }: { token: string; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [db, setDb] = useState({ host: '', port: '3306', database: '', username: '', secret: '', tls: 'DISABLED_LOCAL_ONLY' as 'DISABLED_LOCAL_ONLY' | 'VERIFY_IDENTITY' })
  const [endpoint, setEndpoint] = useState('')
  const [tokenRef, setTokenRef] = useState('')
  const [model, setModel] = useState('')
  const [mode, setMode] = useState<'DETERMINISTIC_CHAIN' | 'SESSION_LLM_ONLY'>('SESSION_LLM_ONLY')
  const [tools, setTools] = useState<ToolDefinition[]>([])
  const dbFilled = db.host !== '' || db.database !== '' || db.username !== '' || db.secret !== ''
  const dbReady = !dbFilled || (db.host && Number(db.port) > 0 && db.database && db.username && db.secret)
  const [oauth, setOAuth] = useState({ token_url: '', client_id: '', client_secret_ref: '' })
  const oauthFilled = oauth.token_url !== '' || oauth.client_id !== '' || oauth.client_secret_ref !== ''
  const oauthReady = !oauthFilled || (oauth.token_url !== '' && oauth.client_id !== '' && oauth.client_secret_ref !== '')
  const discover = useMutation({ mutationFn: () => discoverTools(token, { chat_endpoint: endpoint, target_bearer_token_ref: tokenRef }), onSuccess: (result) => { setTools(result.tools); setMode('DETERMINISTIC_CHAIN') } })
  const create = useMutation({
    mutationFn: async () => {
      const profile = dbFilled
        ? await createAgentDatabase(token, { name: `${name}-db`, host: db.host, port: Number(db.port), database: db.database, username: db.username, password_secret_ref: db.secret, tls_mode: db.tls })
        : null
      return createAgent(token, {
        name, description: description || undefined, database_profile_id: profile?.id ?? null,
        chat_endpoint: endpoint, target_bearer_token_ref: tokenRef, target_model: model, contract_version: '1.0',
        tool_evaluation_mode: mode, tool_catalog: mode === 'DETERMINISTIC_CHAIN' ? tools : null,
        ...(oauth.token_url ? { client_id: oauth.client_id, client_secret_ref: oauth.client_secret_ref, token_url: oauth.token_url } : {}),
      })
    },
    onSuccess: onDone,
  })
  const ready = name && endpoint && tokenRef && model && (mode === 'SESSION_LLM_ONLY' || tools.length > 0) && oauthReady
  return (
    <Modal title="新建 Agent" wide onClose={onClose} footer={<>
      <button type="button" className="btn" onClick={onClose}>取消</button>
      <button type="button" className="btn btn-primary" disabled={!ready || !dbReady || create.isPending} onClick={() => create.mutate()}>创建</button>
    </>}>
      <Alert error={create.error ?? discover.error} />
      <div className="form-grid">
        <Field label="名称"><input value={name} onChange={(event) => setName(event.target.value)} maxLength={255} aria-label="Agent 名称" /></Field>
        <Field label="描述"><textarea value={description} onChange={(event) => setDescription(event.target.value)} maxLength={2000} aria-label="Agent 描述" /></Field>
        <Field label="Agent 地址（基础 URL）" hint="平台会请求 {地址}/v1/chat 与 {地址}/v1/tools"><input value={endpoint} onChange={(event) => setEndpoint(event.target.value)} placeholder="https://agent.example.com" aria-label="Agent 地址" /></Field>
        <Field label="Bearer Token 凭据名" hint="先在「凭据管理」中保存"><input value={tokenRef} onChange={(event) => setTokenRef(event.target.value)} aria-label="Token 凭据名" /></Field>
        <Field label="被测模型 ID"><input value={model} onChange={(event) => setModel(event.target.value)} aria-label="被测模型" /></Field>
        <Field label="Tool 评估模式">
          <select value={mode} onChange={(event) => setMode(event.target.value as typeof mode)} aria-label="Tool 评估模式">
            <option value="SESSION_LLM_ONLY">仅会话评测（不评估 Tool 调用链）</option>
            <option value="DETERMINISTIC_CHAIN">确定性调用链（需要 Tool 目录）</option>
          </select>
        </Field>
        {mode === 'DETERMINISTIC_CHAIN' && (
          <div>
            <button type="button" className="btn" disabled={!endpoint || !tokenRef || discover.isPending} onClick={() => discover.mutate()}>{discover.isPending ? '读取中…' : '从 Agent 读取 Tool 列表'}</button>
            <p className="muted">共 {tools.length} 个 Tool{tools.length > 0 ? `：${tools.map((tool) => tool.name).join('、')}` : '（请先读取）'}</p>
          </div>
        )}
        <fieldset className="field-group">
          <legend>OAuth 认证（可选）</legend>
          <p className="muted">被测 Agent 需要机器间认证时，填写 client credentials 参数；平台会先换 access token 再调用。留空则使用上面的静态 Bearer Token。</p>
          <div className="form-grid">
            <Field label="Token 地址"><input value={oauth.token_url} onChange={(event) => setOAuth({ ...oauth, token_url: event.target.value })} placeholder="https://agent.example.com/oauth/token" aria-label="OAuth Token 地址" /></Field>
            <Field label="Client ID"><input value={oauth.client_id} onChange={(event) => setOAuth({ ...oauth, client_id: event.target.value })} aria-label="OAuth Client ID" /></Field>
            <Field label="Client Secret 凭据名" hint="先在「凭据管理」中保存"><input value={oauth.client_secret_ref} onChange={(event) => setOAuth({ ...oauth, client_secret_ref: event.target.value })} aria-label="OAuth Client Secret 凭据名" /></Field>
          </div>
        </fieldset>
        <fieldset className="field-group">
          <legend>Agent 数据库（可选，每个 Agent 独有）</legend>
          <p className="muted">仅用于运行测试前把数据集的种子数据写入被测 Agent 的数据库，不会发给 Agent；Agent 只会在提示词里被告知要使用的库名。账号需有建库与写入权限。</p>
          <div className="form-grid">
            <Field label="主机"><input value={db.host} onChange={(event) => setDb({ ...db, host: event.target.value })} aria-label="数据库主机" /></Field>
            <Field label="端口"><input value={db.port} onChange={(event) => setDb({ ...db, port: event.target.value })} aria-label="数据库端口" /></Field>
            <Field label="库名" hint="被测 Agent 的固定数据库；运行测试时注入并清理测试数据"><input value={db.database} onChange={(event) => setDb({ ...db, database: event.target.value })} aria-label="数据库库名" /></Field>
            <Field label="账号"><input value={db.username} onChange={(event) => setDb({ ...db, username: event.target.value })} aria-label="数据库账号" /></Field>
            <Field label="密码凭据名" hint="先在「凭据管理」中保存"><input value={db.secret} onChange={(event) => setDb({ ...db, secret: event.target.value })} aria-label="数据库密码凭据名" /></Field>
            <Field label="TLS">
              <select value={db.tls} onChange={(event) => setDb({ ...db, tls: event.target.value as typeof db.tls })} aria-label="数据库 TLS">
                <option value="DISABLED_LOCAL_ONLY">不加密（仅本地网络）</option>
                <option value="VERIFY_IDENTITY">校验证书</option>
              </select>
            </Field>
          </div>
        </fieldset>
      </div>
    </Modal>
  )
}
