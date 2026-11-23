import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router'
import { getAnalytics, listModels, listRuns, type AnalyticsOverview } from '../api2'
import { Alert, Badge, Card, DataTable, Mono, Notice, PageHeader, Score } from '../admin'
import { ColumnChart, DonutChart, HBarChart, LineChart, StackedBarChart } from '../charts'
import { formatDate, shortID } from '../utils'

const ranges = [7, 14, 30, 90]

const statusColor: Record<string, string> = {
  COMPLETED: '#2f9e6f', EVALUATING: '#4f8cff', RUNNING: '#4f8cff', PREPARING: '#8fb4ff', QUEUED: '#a9b0bd',
  INVALID_TEST_DATA: '#e0a030', EVALUATION_FAILED: '#d9534f', CANCELLED: '#7b8190',
}
const statusText: Record<string, string> = {
  COMPLETED: '已完成', EVALUATING: '评测中', RUNNING: '运行中', PREPARING: '准备中', QUEUED: '排队中',
  INVALID_TEST_DATA: '数据无效', EVALUATION_FAILED: '评测失败', CANCELLED: '已取消',
}
const assetText = { scenarios: '业务场景', datasets: '数据集', test_cases: '测试用例' } as const
const jobText: Record<string, string> = { SCENARIO: '业务场景', DATASET: '数据集', TEST_CASES: '测试用例' }

function percent(value: number | null | undefined) {
  return value === null || value === undefined ? '—' : `${Math.round(value * 10) / 10}%`
}

function Summary({ data }: { data: AnalyticsOverview }) {
  const runs = data.daily_runs
  const total = runs.reduce((sum, day) => sum + day.total, 0)
  const evaluated = runs.reduce((sum, day) => sum + day.evaluated, 0)
  const passed = runs.reduce((sum, day) => sum + day.passed, 0)
  const scored = runs.filter((day) => day.avg_score !== null)
  const weighted = scored.reduce((sum, day) => sum + (day.avg_score ?? 0) * day.evaluated, 0)
  const scoredRuns = scored.reduce((sum, day) => sum + day.evaluated, 0)
  const running = data.run_statuses.filter((item) => ['QUEUED', 'PREPARING', 'RUNNING', 'EVALUATING'].includes(item.status)).reduce((sum, item) => sum + item.count, 0)
  const assets = Object.fromEntries(data.assets.map((item) => [item.kind, item]))
  return (
    <div className="stat-grid">
      <div className="stat"><span>时间内运行数</span><strong>{total}</strong></div>
      <div className="stat"><span>通过率</span><strong>{evaluated ? percent((100 * passed) / evaluated) : '—'}</strong></div>
      <div className="stat"><span>平均得分</span><strong>{scoredRuns ? (weighted / scoredRuns).toFixed(1) : '—'}</strong></div>
      <div className="stat"><span>进行中</span><strong>{running}</strong></div>
      <div className="stat"><span>测试用例（可用/全部版本）</span><strong>{assets.test_cases?.usable_revisions ?? 0} / {assets.test_cases?.revisions ?? 0}</strong></div>
      <div className="stat"><span>Agent</span><strong>{data.agents.agents}</strong></div>
    </div>
  )
}

export function Dashboard({ token }: { token: string }) {
  const [days, setDays] = useState(14)
  const analytics = useQuery({ queryKey: ['analytics', token, days], enabled: token !== '', queryFn: () => getAnalytics(token, days), refetchInterval: 15000 })
  const recent = useQuery({ queryKey: ['dashboard-runs', token], enabled: token !== '', queryFn: () => listRuns(token, '', {}), refetchInterval: 5000 })
  const models = useQuery({ queryKey: ['models', token], enabled: token !== '', queryFn: () => listModels(token) })
  const data = analytics.data
  const labels = data?.daily_runs.map((day) => day.date) ?? []
  const active = (models.data?.items ?? []).filter((model) => model.active)

  return (
    <>
      <PageHeader title="仪表盘" description="测试结果趋势与测试资产概况（按 UTC 日期统计）"
        actions={<div className="range-toggle" role="group" aria-label="时间范围">
          {ranges.map((value) => <button key={value} type="button" aria-pressed={days === value} onClick={() => setDays(value)}>近 {value} 天</button>)}
        </div>} />
      {token === '' && <Notice>请先在右上角输入 Platform API token。</Notice>}
      <Alert error={analytics.error ?? recent.error ?? models.error} />
      {data && <Summary data={data} />}

      <div className="chart-grid">
        <Card title="每日运行数与通过数">
          <div className="chart-body">
            {data && <StackedBarChart label="每日运行数与通过数" labels={labels} series={[
              { name: '通过', color: '#2f9e6f', values: data.daily_runs.map((day) => day.passed) },
              { name: '未通过', color: '#d9534f', values: data.daily_runs.map((day) => day.evaluated - day.passed) },
              { name: '未评测', color: '#c4c9d4', values: data.daily_runs.map((day) => day.total - day.evaluated) },
            ]} />}
          </div>
        </Card>
        <Card title="通过率趋势">
          <div className="chart-body">
            {data && <LineChart label="每日通过率" labels={labels} max={100} unit="%" series={[{ name: '通过率', color: '#2f9e6f', values: data.daily_runs.map((day) => day.pass_rate) }]} />}
          </div>
        </Card>
        <Card title="平均得分趋势">
          <div className="chart-body">
            {data && <LineChart label="每日平均得分" labels={labels} max={100} series={[{ name: '平均得分', color: '#4f8cff', values: data.daily_runs.map((day) => day.avg_score) }]} />}
          </div>
        </Card>
        <Card title="得分分布">
          <div className="chart-body">
            {data && <ColumnChart label="最终得分分布" items={data.score_distribution.map((bucket) => ({ name: `${bucket.from}-${bucket.to}`, value: bucket.count }))} color="#4f8cff" />}
          </div>
        </Card>
        <Card title="运行状态（全部）">
          <div className="chart-body">
            {data && <DonutChart label="运行状态占比" items={data.run_statuses.map((item) => ({ name: statusText[item.status] ?? item.status, value: item.count, color: statusColor[item.status] ?? '#a9b0bd' }))} />}
          </div>
        </Card>
        <Card title="各 Agent 通过率">
          <div className="chart-body">
            {data && <HBarChart label="各 Agent 通过率" unit="%" items={data.by_agent.filter((item) => item.pass_rate !== null).map((item) => ({ name: item.agent_name, value: item.pass_rate ?? 0, color: '#2f9e6f' }))} />}
          </div>
        </Card>
        <Card title="测试资产数量">
          <div className="chart-body">
            {data && <HBarChart label="测试资产版本数" items={data.assets.map((item) => ({ name: `${assetText[item.kind]}（可用 ${item.usable_revisions}）`, value: item.revisions }))} />}
          </div>
        </Card>
        <Card title="每日新增测试资产">
          <div className="chart-body">
            {data && <StackedBarChart label="每日新增测试资产" labels={data.daily_assets.map((day) => day.date)} series={[
              { name: '业务场景', color: '#8b6fe0', values: data.daily_assets.map((day) => day.scenarios) },
              { name: '数据集', color: '#e0a030', values: data.daily_assets.map((day) => day.datasets) },
              { name: '测试用例', color: '#4f8cff', values: data.daily_assets.map((day) => day.test_cases) },
            ]} />}
          </div>
        </Card>
        <Card title="生成任务状态">
          <div className="chart-body">
            {data && <HBarChart label="生成任务数量" items={data.generation_jobs.map((item) => ({ name: `${jobText[item.kind] ?? item.kind} · ${item.status}`, value: item.count, color: item.status === 'COMPLETED' ? '#2f9e6f' : item.status === 'FAILED' ? '#d9534f' : '#a9b0bd' }))} />}
          </div>
        </Card>
      </div>

      <Card title="当前启用的模型">
        <DataTable label="启用的模型" rows={active} rowKey={(model) => model.role} loading={models.isPending && token !== ''} empty="尚未注册模型（启动 worker 时会自动注册）"
          columns={[
            { key: 'role', title: '角色', render: (model) => (model.role === 'judge' ? 'Judge 评判模型' : '生成模型') },
            { key: 'alias', title: '别名', render: (model) => <Mono>{model.alias}</Mono> },
            { key: 'model', title: '模型', render: (model) => `${model.provider} / ${model.model}` },
          ]} />
      </Card>
      <Card title="最近运行" extra={<Link to="/runs">查看全部</Link>} flush>
        <DataTable label="最近运行" rows={(recent.data?.items ?? []).slice(0, 8)} rowKey={(run) => run.id} loading={recent.isPending && token !== ''}
          columns={[
            { key: 'id', title: '运行 ID', render: (run) => <Link to={`/runs/${run.id}`}><Mono>{shortID(run.id)}</Mono></Link> },
            { key: 'status', title: '状态', render: (run) => <Badge value={run.status} /> },
            { key: 'score', title: '最终得分', render: (run) => <Score value={run.final_score} /> },
            { key: 'created', title: '创建时间', render: (run) => formatDate(run.created_at) },
          ]} />
      </Card>
    </>
  )
}
