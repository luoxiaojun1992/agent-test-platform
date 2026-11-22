import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useNavigate } from 'react-router'
import { createRuns, listAgents, listAgentVersions, type RevisionItem } from './api2'
import { Alert, Field, Modal } from './admin'
import { RevisionSelect, notDeprecated } from './revision'

interface Props {
  token: string
  initial?: RevisionItem[]
  onClose: () => void
}

// Runs the picked test cases on one Agent version. A test case already links its scenario and dataset, so nothing else is needed:
// the dataset's seed data is written into the database configured on the Agent, which is required.
export function RunTestsDialog({ token, initial, onClose }: Props) {
  const navigate = useNavigate()
  const client = useQueryClient()
  const [cases, setCases] = useState<RevisionItem[]>(initial ?? [])
  const [agentId, setAgentId] = useState('')
  const [versionId, setVersionId] = useState('')
  const [repetitions, setRepetitions] = useState(1)
  const agents = useQuery({ queryKey: ['agents-all', token], queryFn: () => listAgents(token, '') })
  const versions = useQuery({ queryKey: ['versions-of', token, agentId], enabled: agentId !== '', queryFn: () => listAgentVersions(token, agentId, '') })
  const agent = (agents.data?.items ?? []).find((item) => item.id === agentId)

  const start = useMutation({
    mutationFn: () => createRuns(token, { agent_version_id: versionId, test_case_revision_ids: cases.map((item) => item.id), repetitions }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['runs'] })
      onClose()
      navigate('/runs')
    },
  })

  return (
    <Modal title="运行测试" wide onClose={onClose} footer={<>
      <button type="button" className="btn" onClick={onClose}>取消</button>
      <button type="button" className="btn btn-primary" disabled={cases.length === 0 || !versionId || !agent?.database_profile_id || start.isPending} onClick={() => start.mutate()}>
        {start.isPending ? '提交中…' : `运行 ${cases.length} 个用例`}
      </button>
    </>}>
      <Alert error={start.error ?? agents.error} />
      <div className="form-grid">
        <RevisionSelect token={token} kind="test-cases" label="测试用例" multiple required initial={initial} filter={notDeprecated} onChange={setCases} />
        <Field label="被测 Agent">
          <select value={agentId} onChange={(event) => { setAgentId(event.target.value); setVersionId('') }} aria-label="选择 Agent">
            <option value="">请选择</option>
            {(agents.data?.items ?? []).map((agent) => <option key={agent.id} value={agent.id}>{agent.name}</option>)}
          </select>
        </Field>
        <Field label="Agent 版本">
          <select value={versionId} onChange={(event) => setVersionId(event.target.value)} disabled={!agentId} aria-label="选择 Agent 版本">
            <option value="">请选择</option>
            {(versions.data?.items ?? []).map((item) => <option key={item.id} value={item.id}>{item.version}（{item.target_model}）</option>)}
          </select>
        </Field>
        <Field label="重复次数" hint="每个用例重复运行的次数（1–10）">
          <input type="number" min={1} max={10} value={repetitions} onChange={(event) => setRepetitions(Math.min(10, Math.max(1, Number(event.target.value) || 1)))} aria-label="重复次数" />
        </Field>
        {agent && (agent.database_profile_id
          ? <p className="muted">数据集的种子数据会写入该 Agent 配置的数据库。</p>
          : <p className="muted" role="alert">该 Agent 尚未配置数据库，无法运行测试。请先在「Agent 管理」中为它配置数据库。</p>)}
      </div>
    </Modal>
  )
}
