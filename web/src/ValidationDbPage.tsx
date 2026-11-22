import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { createValidationDb, listValidationDbs } from './api'
import type { TLSMode, ValidationDB, ValidationDBInput } from './types'
import { Alert, Card, DataTable, Field, Modal, Mono, Notice, PageHeader, useCursorList } from './admin'
import { formatDate } from './utils'

// Mirrors contracts/schemas/validation-mysql-request.schema.json. The server stays authoritative.
const namePattern = /^[a-z0-9][a-z0-9-]{0,62}$/
const usernamePattern = /^[A-Za-z0-9_]{1,32}$/
const secretNamePattern = /^[a-z0-9][a-z0-9-]{0,127}$/
const maxCABytes = 16 * 1024

const emptyInput: ValidationDBInput = {
  name: '', host: '', port: 3306, admin_username: '', admin_password_secret_name: '', tls_mode: 'DISABLED_LOCAL_ONLY', ca_certificate_pem: '',
}

export function ValidationDbPage({ token }: { token: string }) {
  const client = useQueryClient()
  const [open, setOpen] = useState(false)
  const dbs = useCursorList<ValidationDB>(['validation-dbs', token], token !== '', async () => ({ ...(await listValidationDbs(token)) }))
  return (
    <>
      <PageHeader title="校验用 MySQL" description="平台自己的 MySQL，只在生成数据集时用来执行并校验表结构和种子 SQL，不属于任何被测 Agent。被测 Agent 的数据库在「Agent 管理」中配置，运行测试时由平台执行同一份初始 SQL 写入。"
        actions={<button type="button" className="btn btn-primary" onClick={() => setOpen(true)}>登记校验库</button>} />
      {token === '' && <Notice>请先在右上角输入 Platform API token。</Notice>}
      <Alert error={dbs.query.error} />
      <Card flush>
        <DataTable label="校验库列表" rows={dbs.rows} rowKey={(item) => item.id} loading={dbs.query.isPending && token !== ''} empty="暂无校验用 MySQL"
          columns={[
            { key: 'name', title: '名称', render: (item) => <strong>{item.name}</strong> },
            { key: 'ep', title: '地址', render: (item) => <Mono>{item.host}:{item.port}</Mono> },
            { key: 'user', title: '管理员账号', render: (item) => item.admin_username },
            { key: 'secret', title: '密码凭据', render: (item) => <Mono>{item.admin_password_secret_name}</Mono> },
            { key: 'tls', title: 'TLS', render: (item) => item.tls_mode },
            { key: 'created', title: '登记时间', render: (item) => formatDate(item.created_at) },
          ]} />
      </Card>
      {open && <Register token={token} onClose={() => setOpen(false)} onDone={() => { setOpen(false); void client.invalidateQueries({ queryKey: ['validation-dbs'] }) }} />}
    </>
  )
}

function Register({ token, onClose, onDone }: { token: string; onClose: () => void; onDone: () => void }) {
  const [input, setInput] = useState<ValidationDBInput>(emptyInput)
  const update = <K extends keyof ValidationDBInput>(field: K, value: ValidationDBInput[K]) => setInput((current) => ({ ...current, [field]: value }))
  const create = useMutation({
    mutationFn: () => createValidationDb(token, { ...input, ca_certificate_pem: input.tls_mode === 'VERIFY_IDENTITY' && input.ca_certificate_pem ? input.ca_certificate_pem : undefined }),
    onSuccess: onDone,
  })
  const caBytes = new TextEncoder().encode(input.ca_certificate_pem ?? '').length
  const valid = namePattern.test(input.name) && input.host.trim() !== '' && Number.isInteger(input.port) && input.port >= 1 && input.port <= 65535 &&
    usernamePattern.test(input.admin_username) && secretNamePattern.test(input.admin_password_secret_name) && caBytes <= maxCABytes
  return (
    <Modal title="登记校验用 MySQL" wide onClose={onClose} footer={<>
      <button type="button" className="btn" onClick={onClose}>取消</button>
      <button type="button" className="btn btn-primary" disabled={!valid || create.isPending} onClick={() => create.mutate()}>登记</button>
    </>}>
      <Alert error={create.error} />
      <div className="form-grid">
        <Field label="名称" hint="小写字母、数字和连字符。"><input value={input.name} onChange={(event) => update('name', event.target.value.trim())} maxLength={63} aria-label="校验库名称" /></Field>
        <Field label="主机"><input value={input.host} onChange={(event) => update('host', event.target.value.trim())} maxLength={255} aria-label="主机" /></Field>
        <Field label="端口"><input type="number" min={1} max={65535} value={input.port} onChange={(event) => update('port', Number(event.target.value))} aria-label="端口" /></Field>
        <Field label="管理员账号"><input value={input.admin_username} onChange={(event) => update('admin_username', event.target.value.trim())} maxLength={32} aria-label="管理员账号" /></Field>
        <Field label="管理员密码凭据名" hint="需先在「凭据管理」中保存"><input value={input.admin_password_secret_name} onChange={(event) => update('admin_password_secret_name', event.target.value.trim())} maxLength={128} aria-label="密码凭据名" /></Field>
        <Field label="TLS 模式">
          <select value={input.tls_mode} onChange={(event) => update('tls_mode', event.target.value as TLSMode)} aria-label="TLS 模式">
            <option value="DISABLED_LOCAL_ONLY">DISABLED_LOCAL_ONLY（仅隔离的本地网络）</option>
            <option value="VERIFY_IDENTITY">VERIFY_IDENTITY（校验服务器证书）</option>
          </select>
        </Field>
        {input.tls_mode === 'VERIFY_IDENTITY' && (
          <Field label="CA 证书（可选）" hint="公开证书，保存在 Platform MySQL；留空则使用系统根证书。">
            <textarea value={input.ca_certificate_pem ?? ''} onChange={(event) => update('ca_certificate_pem', event.target.value)} rows={6} aria-label="CA 证书" />
          </Field>
        )}
      </div>
    </Modal>
  )
}
