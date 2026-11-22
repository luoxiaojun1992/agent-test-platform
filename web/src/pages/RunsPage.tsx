import { useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { listRuns } from '../api2'
import { RunTestsDialog } from '../RunTestsDialog'
import { Alert, Badge, Card, DataTable, LoadMore, Mono, Notice, PageHeader, PassTag, Score, useCursorList } from '../admin'
import { formatDate, shortID } from '../utils'


const statuses = ['QUEUED', 'PREPARING', 'RUNNING', 'EVALUATING', 'COMPLETED', 'INVALID_TEST_DATA', 'EVALUATION_FAILED', 'CANCELLED']

export function RunsPage({ token }: { token: string }) {
  const navigate = useNavigate()
  const [status, setStatus] = useState('')
  const [running, setRunning] = useState(false)
  const { query, rows } = useCursorList(['runs', token, status], token !== '', (cursor) => listRuns(token, cursor, { status }), 5000)
  return (
    <>
      <PageHeader title="测试运行" description="每条记录是一次测试用例在某个 Agent 版本上的执行与评测。列表每 5 秒自动刷新。"
        actions={<button type="button" className="btn btn-primary" onClick={() => setRunning(true)}>运行测试</button>} />
      {token === '' && <Notice>请先输入 Platform API token。</Notice>}
      <Alert error={query.error} />
      <div className="toolbar">
        <select className="filter-select" aria-label="状态筛选" value={status} onChange={(event) => setStatus(event.target.value)}>
          <option value="">全部状态</option>
          {statuses.map((item) => <option key={item} value={item}>{item}</option>)}
        </select>
      </div>
      <Card flush>
        <DataTable label="运行列表" rows={rows} rowKey={(run) => run.id} loading={query.isPending && token !== ''} onRowClick={(run) => navigate(`/runs/${run.id}`)}
          columns={[
            { key: 'id', title: '运行 ID', render: (run) => <Link to={`/runs/${run.id}`}><Mono>{shortID(run.id)}</Mono></Link> },
            { key: 'status', title: '状态', render: (run) => <Badge value={run.status} /> },
            { key: 'outcome', title: '执行结果', render: (run) => (run.execution_outcome ? <Badge value={run.execution_outcome} /> : <span className="muted">—</span>) },
            { key: 'det', title: '确定性得分', render: (run) => <Score value={run.deterministic_score} /> },
            { key: 'final', title: '最终得分', render: (run) => <Score value={run.final_score} /> },
            { key: 'pass', title: '结论', render: (run) => <PassTag pass={run.pass} /> },
            { key: 'agent', title: 'Agent', render: (run) => <Mono>{shortID(run.agent_id)}</Mono> },
            { key: 'created', title: '创建时间', render: (run) => formatDate(run.created_at) },
          ]} />
        <LoadMore query={query} />
      </Card>
      {running && <RunTestsDialog token={token} onClose={() => setRunning(false)} />}
    </>
  )
}
