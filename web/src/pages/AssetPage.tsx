import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { searchRevisions, type AssetKind, type RevisionItem } from '../api2'
import { Alert, Badge, Card, DataTable, JsonView, Modal, Mono, Notice, PageHeader } from '../admin'
import { GenerateAssetDialog } from '../GenerateAssetDialog'
import { RunTestsDialog } from '../RunTestsDialog'
import { revisionTitle } from '../revision'
import { formatDate, shortID } from '../utils'

const meta: Record<AssetKind, { title: string; description: string; generate: string }> = {
  scenarios: { title: '业务场景', description: '描述被测 Agent 面对的业务、规则和难度。可由一段需求描述单独生成，无前置依赖。', generate: '生成业务场景' },
  datasets: { title: '数据集', description: '表结构与种子数据，用于在隔离环境里重建业务现场。必须基于一个业务场景版本生成，并在校验用 MySQL 上执行校验。运行测试时，这份初始 SQL 会原样在被测 Agent 的数据库里执行。', generate: '生成数据集' },
  'test-cases': { title: '测试用例', description: '提示词、期望输出、前后置条件和评分标准。必须基于一个业务场景版本和其对应的数据集版本生成。', generate: '生成测试用例' },
}

export function AssetPage({ token, kind }: { token: string; kind: AssetKind }) {
  const [input, setInput] = useState('')
  const [search, setSearch] = useState('')
  const [generating, setGenerating] = useState(false)
  const [running, setRunning] = useState<RevisionItem[] | null>(null)
  const [viewing, setViewing] = useState<RevisionItem | null>(null)
  useEffect(() => {
    const timer = setTimeout(() => setSearch(input.trim()), 250)
    return () => clearTimeout(timer)
  }, [input])
  const list = useQuery({ queryKey: ['assets', token, kind, search], enabled: token !== '', queryFn: () => searchRevisions(token, kind, search, '', 50), refetchInterval: 10_000 })
  const rows = list.data?.items ?? []
  const info = meta[kind]

  return (
    <>
      <PageHeader title={info.title} description={info.description}
        actions={<>
          {kind === 'test-cases' && <button type="button" className="btn" onClick={() => setRunning([])}>运行测试</button>}
          <button type="button" className="btn btn-primary" onClick={() => setGenerating(true)}>{info.generate}</button>
        </>} />
      {token === '' && <Notice>请先在右上角输入 Platform API token。</Notice>}
      <Alert error={list.error} />
      <div className="toolbar">
        <input className="filter-input" aria-label="搜索" placeholder="按 ID 或内容搜索" value={input} onChange={(event) => setInput(event.target.value)} />
      </div>
      <Card flush>
        <DataTable label={`${info.title}列表`} rows={rows} rowKey={(item) => item.id} loading={list.isPending && token !== ''} empty={search ? '没有匹配的结果' : `还没有${info.title}，点击右上角生成`}
          columns={[
            { key: 'title', title: '名称', render: (item) => <strong>{revisionTitle(kind, item)}</strong> },
            { key: 'rev', title: '版本', width: 70, render: (item) => `r${item.revision_no}` },
            { key: 'status', title: '状态', render: (item) => (item.lifecycle_status ? <Badge value={item.lifecycle_status} tone={item.lifecycle_status === 'DEPRECATED' ? 'muted' : 'success'} /> : '—') },
            ...(kind !== 'scenarios' ? [{ key: 'scenario', title: '业务场景', render: (item: RevisionItem) => <Mono>{shortID(item.scenario_revision_id ?? '')}</Mono> }] : []),
            ...(kind === 'test-cases' ? [{ key: 'dataset', title: '数据集', render: (item: RevisionItem) => <Mono>{shortID(item.dataset_revision_id ?? '')}</Mono> }] : []),
            { key: 'id', title: 'ID', render: (item) => <Mono>{item.id}</Mono> },
            { key: 'created', title: '创建时间', render: (item) => formatDate(item.created_at) },
            { key: 'op', title: '操作', align: 'right', render: (item) => (
              <span className="row-actions">
                <button type="button" className="btn btn-link" onClick={() => setViewing(item)}>详情</button>
                {kind === 'test-cases' && item.lifecycle_status !== 'DEPRECATED' && <button type="button" className="btn btn-link" onClick={() => setRunning([item])} aria-label={`运行 ${item.id}`}>运行测试</button>}
              </span>
            ) },
          ]} />
      </Card>
      {generating && <GenerateAssetDialog token={token} kind={kind} onClose={() => setGenerating(false)} />}
      {running && <RunTestsDialog token={token} initial={running} onClose={() => setRunning(null)} />}
      {viewing && <Modal title={`${info.title}详情`} wide onClose={() => setViewing(null)}><JsonView value={viewing} /></Modal>}
    </>
  )
}
