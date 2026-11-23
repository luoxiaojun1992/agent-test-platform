import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useParams } from 'react-router'
import { cancelRun, getRun, listModels, listRunEvaluations, listToolCalls, reevaluateRun } from '../api2'
import { Alert, Badge, Card, DataTable, Descriptions, JsonView, Modal, Mono, PageHeader, PassTag, Score, Tabs } from '../admin'
import { formatDate } from '../utils'

const cancellable = new Set(['QUEUED', 'PREPARING', 'RUNNING', 'EVALUATING'])
const reevaluable = new Set(['COMPLETED', 'EVALUATION_FAILED'])

export function RunDetailPage({ token }: { token: string }) {
  const { runId = '' } = useParams()
  const client = useQueryClient()
  const [tab, setTab] = useState('result')
  const [reevalOpen, setReevalOpen] = useState(false)
  const [alias, setAlias] = useState('')
  const [newRun, setNewRun] = useState('')

  const run = useQuery({ queryKey: ['run', runId, token], enabled: token !== '', queryFn: () => getRun(token, runId), refetchInterval: (query) => (cancellable.has(query.state.data?.status ?? '') ? 3000 : false) })
  const calls = useQuery({ queryKey: ['run-calls', runId, token], enabled: token !== '', queryFn: () => listToolCalls(token, runId) })
  const attempts = useQuery({ queryKey: ['run-evals', runId, token], enabled: token !== '', queryFn: () => listRunEvaluations(token, runId), refetchInterval: cancellable.has(run.data?.status ?? '') ? 3000 : false })
  const models = useQuery({ queryKey: ['models', token], enabled: token !== '' && reevalOpen, queryFn: () => listModels(token) })
  const judges = (models.data?.items ?? []).filter((model) => model.role === 'judge')
  const chosenAlias = alias || judges.find((model) => model.active)?.alias || judges[0]?.alias || ''

  const cancel = useMutation({ mutationFn: () => cancelRun(token, runId), onSuccess: () => client.invalidateQueries({ queryKey: ['run', runId] }) })
  const reevaluate = useMutation({
    mutationFn: () => reevaluateRun(token, runId, chosenAlias),
    onSuccess: (result) => { setNewRun(result.run_ids[0]); setReevalOpen(false) },
  })

  const data = run.data
  return (
    <>
      <PageHeader title="运行详情" description={runId}
        actions={<>
          <Link className="btn" to="/runs">返回列表</Link>
          {data && cancellable.has(data.status) && <button type="button" className="btn btn-danger" disabled={cancel.isPending} onClick={() => cancel.mutate()}>取消运行</button>}
          {data && reevaluable.has(data.status) && <button type="button" className="btn btn-primary" onClick={() => { setAlias(''); setReevalOpen(true) }}>重新评测</button>}
        </>} />
      <Alert error={run.error ?? cancel.error} />
      {newRun && <div className="alert alert-info" role="status">已创建新的评测运行：<Link to={`/runs/${newRun}`}>{newRun}</Link>（原运行保持不变）</div>}
      {data && (
        <Card>
          <Descriptions items={[
            { label: '状态', value: <Badge value={data.status} /> },
            { label: '执行结果', value: data.execution_outcome ? <Badge value={data.execution_outcome} /> : '—' },
            { label: '结论', value: <PassTag pass={data.pass} /> },
            { label: '确定性得分', value: <Score value={data.deterministic_score} /> },
            { label: '最终得分', value: <Score value={data.final_score} /> },
            { label: 'Agent', value: <Mono>{data.agent_id}</Mono> },
            { label: '测试用例版本', value: <Mono>{data.test_case_revision_id}</Mono> },
            { label: '创建时间', value: formatDate(data.created_at) },
          ]} />
        </Card>
      )}
      <Tabs active={tab} onChange={setTab} tabs={[
        { key: 'result', label: '执行结果' }, { key: 'tools', label: `Tool 调用（${calls.data?.items.length ?? 0}）` },
        { key: 'checks', label: '确定性检查' }, { key: 'evals', label: `评测尝试（${attempts.data?.items.length ?? 0}）` },
      ]} />
      {tab === 'result' && data && (
        <Card title="Agent 最终输出"><JsonView value={data.final_output} />{data.error ? <><h3>错误</h3><JsonView value={data.error} /></> : null}</Card>
      )}
      {tab === 'tools' && (
        <Card flush>
          <DataTable label="Tool 调用" rows={calls.data?.items ?? []} rowKey={(call) => call.call_id} loading={calls.isPending} empty="该运行没有 Tool 调用记录"
            columns={[
              { key: 'seq', title: '#', width: 50, render: (call) => call.sequence },
              { key: 'tool', title: 'Tool', render: (call) => <Mono>{call.tool_name}</Mono> },
              { key: 'status', title: '状态', render: (call) => <Badge value={call.status} /> },
              { key: 'dur', title: '耗时(ms)', render: (call) => call.duration_ms },
              { key: 'args', title: '参数', render: (call) => <JsonView value={call.arguments} /> },
              { key: 'res', title: '结果', render: (call) => <JsonView value={call.result ?? call.error} /> },
              { key: 'src', title: '来源', render: (call) => call.trace_source },
            ]} />
        </Card>
      )}
      {tab === 'checks' && data && (
        <>
          <Card title="确定性检查"><JsonView value={data.deterministic_details} /></Card>
          <Card title="状态变更 diff"><JsonView value={data.state_diff} /></Card>
          <Card title="前置条件"><JsonView value={data.precondition_results} /></Card>
          <Card title="后置条件"><JsonView value={data.postcondition_results} /></Card>
        </>
      )}
      {tab === 'evals' && (
        <Card flush>
          <DataTable label="评测尝试" rows={attempts.data?.items ?? []} rowKey={(attempt) => attempt.id} loading={attempts.isPending} empty="暂无评测尝试"
            columns={[
              { key: 'no', title: '尝试', width: 70, render: (attempt) => `#${attempt.attempt_no}` },
              { key: 'status', title: '状态', render: (attempt) => <Badge value={attempt.status} /> },
              { key: 'alias', title: 'Judge 模型', render: (attempt) => <Mono>{attempt.judge_model_alias}</Mono> },
              { key: 'llm', title: 'LLM 得分', render: (attempt) => <Score value={attempt.llm_score} /> },
              { key: 'final', title: '最终得分', render: (attempt) => <Score value={attempt.final_score} /> },
              { key: 'pass', title: '结论', render: (attempt) => <PassTag pass={attempt.pass} /> },
              { key: 'created', title: '创建时间', render: (attempt) => formatDate(attempt.created_at) },
            ]} />
        </Card>
      )}
      {reevalOpen && (
        <Modal title="重新评测" onClose={() => setReevalOpen(false)} footer={<>
          <button type="button" className="btn" onClick={() => setReevalOpen(false)}>取消</button>
          <button type="button" className="btn btn-primary" disabled={!chosenAlias || reevaluate.isPending} onClick={() => reevaluate.mutate()}>确认重新评测</button>
        </>}>
          <p className="muted">将复制本次运行的执行证据，创建一个新的测试运行并用所选 Judge 重新评测，原运行不受影响。</p>
          <Alert error={reevaluate.error} />
          <label className="form-item"><span className="form-label">Judge 模型</span>
            <select value={chosenAlias} onChange={(event) => setAlias(event.target.value)} aria-label="Judge 模型">
              {judges.map((model) => <option key={model.alias} value={model.alias}>{model.alias}（{model.model}）{model.active ? ' · 当前启用' : ''}</option>)}
            </select>
          </label>
        </Modal>
      )}
    </>
  )
}
