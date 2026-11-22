import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { listSecrets, putSecret } from './api'
import { Alert, Card, DataTable, Field, Modal, Mono, Notice, PageHeader, useCursorList } from './admin'
import { formatDate } from './utils'

// Mirrors the server rules in internal/secrets: names match this pattern, and a value is 1-4096 bytes
// once encoded as UTF-8 and must not be blank.
const namePattern = /^[a-z0-9][a-z0-9-]{0,127}$/
const maxValueBytes = 4096

export function SecretsPage({ token }: { token: string }) {
  const client = useQueryClient()
  const [open, setOpen] = useState(false)
  const secrets = useCursorList(['secrets', token], token !== '', async () => ({ ...(await listSecrets(token)) }))
  return (
    <>
      <PageHeader title="凭据管理" description="凭据在 Platform MySQL 中加密保存，仅由 worker 读取。值只写不读：保存后不会再显示。输入已存在的名称即可替换其值。"
        actions={<button type="button" className="btn btn-primary" onClick={() => setOpen(true)}>保存凭据</button>} />
      {token === '' && <Notice>请先在右上角输入 Platform API token。</Notice>}
      <Alert error={secrets.query.error} />
      <Card flush>
        <DataTable label="凭据列表" rows={secrets.rows} rowKey={(secret) => secret.name} loading={secrets.query.isPending && token !== ''} empty="暂无凭据"
          columns={[
            { key: 'name', title: '名称', render: (secret) => <Mono>{secret.name}</Mono> },
            { key: 'created', title: '创建时间', render: (secret) => formatDate(secret.created_at) },
            { key: 'updated', title: '更新时间', render: (secret) => formatDate(secret.updated_at) },
          ]} />
      </Card>
      {open && <SaveSecret token={token} onClose={() => setOpen(false)} onDone={() => { setOpen(false); void client.invalidateQueries({ queryKey: ['secrets'] }) }} />}
    </>
  )
}

function SaveSecret({ token, onClose, onDone }: { token: string; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const save = useMutation({ mutationFn: () => putSecret(token, name, value), onSuccess: onDone })
  const valid = namePattern.test(name) && value.trim() !== '' && new TextEncoder().encode(value).length <= maxValueBytes
  return (
    <Modal title="保存凭据" onClose={onClose} footer={<>
      <button type="button" className="btn" onClick={onClose}>取消</button>
      <button type="button" className="btn btn-primary" disabled={!valid || save.isPending} onClick={() => save.mutate()}>保存</button>
    </>}>
      <Alert error={save.error} />
      <div className="form-grid">
        <Field label="凭据名称" hint="小写字母、数字和连字符。Agent、Agent 数据库与校验用 MySQL 通过该名称引用凭据。">
          <input value={name} onChange={(event) => setName(event.target.value.trim())} maxLength={128} spellCheck={false} autoComplete="off" aria-label="凭据名称" />
        </Field>
        <Field label="凭据值" hint="最多 4096 字节，只会发送一次，之后不再显示。">
          <input type="password" value={value} onChange={(event) => setValue(event.target.value)} autoComplete="new-password" spellCheck={false} aria-label="凭据值" />
        </Field>
      </div>
    </Modal>
  )
}
