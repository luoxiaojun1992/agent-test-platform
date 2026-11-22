import { useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router'
import { createGenerationJobRequest, type AssetKind, type RevisionItem } from './api2'
import { listValidationDbs } from './api'
import { Alert, Field, Modal } from './admin'
import { RevisionSelect, usableAsInput } from './revision'

const kindText: Record<AssetKind, string> = { scenarios: '业务场景', datasets: '数据集（表结构 + 种子数据）', 'test-cases': '测试用例' }
const jobKind: Record<AssetKind, 'SCENARIO' | 'DATASET' | 'TEST_CASES'> = { scenarios: 'SCENARIO', datasets: 'DATASET', 'test-cases': 'TEST_CASES' }
const difficulties = ['EASY', 'MEDIUM', 'HARD', 'EXTREME']
const coverageOptions = ['NORMAL', 'EDGE_CASE', 'AMBIGUOUS', 'MULTI_STEP', 'CONFLICT', 'ADVERSARIAL']

interface Props {
  token: string
  kind: AssetKind
  onClose: () => void
}

// One dialog per asset kind. A dataset needs a scenario revision and a validation MySQL; test cases need a scenario revision and one of its
// datasets. The dependencies are required, and the dataset list only offers datasets generated for the chosen scenario.
export function GenerateAssetDialog({ token, kind, onClose }: Props) {
  const navigate = useNavigate()
  const [description, setDescription] = useState('')
  const [scenario, setScenario] = useState<RevisionItem | null>(null)
  const [dataset, setDataset] = useState<RevisionItem | null>(null)
  const [validationDbIds, setValidationDbIds] = useState<string[]>([])
  const [domain, setDomain] = useState('')
  const [agentMode, setAgentMode] = useState<'ANSWER' | 'TASK'>('TASK')
  const [count, setCount] = useState(5)
  const [coverage, setCoverage] = useState<string[]>([])
  const [difficulty, setDifficulty] = useState('')
  const [manualReview, setManualReview] = useState(false)

  const dbs = useQuery({ queryKey: ['validation-dbs-all', token], enabled: token !== '' && kind === 'datasets', queryFn: () => listValidationDbs(token) })
  const needsScenario = kind !== 'scenarios'
  const needsDataset = kind === 'test-cases'
  const ready = description.trim() !== '' && (!needsScenario || scenario !== null) && (!needsDataset || dataset !== null) && (kind !== 'datasets' || validationDbIds.length > 0)

  const create = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = { description, job_kind: jobKind[kind], require_manual_review: manualReview, agent_mode: agentMode }
      if (domain.trim()) body.domain = domain.trim()
      if (needsScenario && scenario) body.input_scenario_revision_id = scenario.id
      if (needsDataset && dataset) body.input_dataset_revision_id = dataset.id
      if (kind === 'datasets') body.validation_db_ids = validationDbIds
      if (kind === 'test-cases') {
        body.test_case_count = count
        if (coverage.length > 0) body.coverage = coverage
        if (difficulty) body.difficulty = { min: difficulty, max: difficulty }
      }
      return createGenerationJobRequest(token, body)
    },
    onSuccess: (job) => navigate(`/jobs/${job.id}`),
  })

  return (
    <Modal title={`生成${kindText[kind]}`} wide onClose={onClose} footer={<>
      <button type="button" className="btn" onClick={onClose}>取消</button>
      <button type="button" className="btn btn-primary" disabled={!ready || create.isPending} onClick={() => create.mutate()}>{create.isPending ? '提交中…' : '开始生成'}</button>
    </>}>
      <Alert error={create.error} />
      <div className="form-grid">
        {needsScenario && (
          <RevisionSelect token={token} kind="scenarios" label="业务场景版本" required filter={usableAsInput}
            onChange={(items) => { setScenario(items[0] ?? null); setDataset(null) }} />
        )}
        {needsDataset && (
          <RevisionSelect key={scenario?.id ?? 'none'} token={token} kind="datasets" label="数据集版本" required disabled={scenario === null} disabledHint="请先选择业务场景版本"
            filter={(item) => usableAsInput(item) && item.scenario_revision_id === scenario?.id} onChange={(items) => setDataset(items[0] ?? null)} />
        )}
        {kind === 'datasets' && (
          <div className="form-item">
            <span className="form-label">校验用 MySQL<em className="required-mark"> *</em></span>
            <div className="check-list" role="group" aria-label="校验用 MySQL">
              {(dbs.data?.items ?? []).length === 0 && <span className="muted">{dbs.isPending ? '加载中…' : '尚未登记校验用 MySQL，请先到「系统 → 校验用 MySQL」登记。'}</span>}
              {(dbs.data?.items ?? []).map((db) => (
                <label key={db.id} className="check-row">
                  <input type="checkbox" checked={validationDbIds.includes(db.id)} aria-label={`校验库 ${db.name}`}
                    onChange={() => setValidationDbIds((current) => (current.includes(db.id) ? current.filter((id) => id !== db.id) : [...current, db.id]))} />
                  {db.name}
                </label>
              ))}
            </div>
            <span className="form-hint">只用于生成时执行并校验表结构和种子 SQL；运行测试时，同一份初始 SQL 会直接在 Agent 自己配置的数据库里执行，不做导出导入。</span>
          </div>
        )}
        <Field label={kind === 'scenarios' ? '需求描述' : '补充说明'} hint={kind === 'scenarios' ? '用一段话描述要测试的 Agent 与业务，例如“订单取消 Agent，覆盖已支付、已发货和模糊请求”。' : '给生成模型的补充要求。'}>
          <textarea value={description} onChange={(event) => setDescription(event.target.value)} maxLength={4000} rows={4} aria-label="描述" />
        </Field>
        {kind === 'scenarios' && <Field label="业务领域（可选）"><input value={domain} onChange={(event) => setDomain(event.target.value)} maxLength={128} aria-label="业务领域" /></Field>}
        {kind === 'test-cases' && (
          <>
            <Field label="Agent 模式">
              <select value={agentMode} onChange={(event) => setAgentMode(event.target.value as 'ANSWER' | 'TASK')} aria-label="Agent 模式">
                <option value="TASK">TASK（会修改数据）</option><option value="ANSWER">ANSWER（只回答问题）</option>
              </select>
            </Field>
            <Field label="用例数量（1–20）"><input type="number" min={1} max={20} value={count} onChange={(event) => setCount(Math.min(20, Math.max(1, Number(event.target.value) || 1)))} aria-label="用例数量" /></Field>
            <Field label="难度（可选）">
              <select value={difficulty} onChange={(event) => setDifficulty(event.target.value)} aria-label="难度">
                <option value="">不限</option>{difficulties.map((value) => <option key={value}>{value}</option>)}
              </select>
            </Field>
            <div className="form-item">
              <span className="form-label">覆盖类型（可选）</span>
              <div className="check-list" role="group" aria-label="覆盖类型">
                {coverageOptions.map((value) => (
                  <label key={value} className="check-row"><input type="checkbox" checked={coverage.includes(value)} aria-label={value}
                    onChange={() => setCoverage((current) => (current.includes(value) ? current.filter((item) => item !== value) : [...current, value]))} />{value}</label>
                ))}
              </div>
            </div>
          </>
        )}
        <label className="check-row"><input type="checkbox" checked={manualReview} onChange={(event) => setManualReview(event.target.checked)} aria-label="发布前人工审核" />发布前需要人工审核</label>
      </div>
    </Modal>
  )
}
