import { useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Check, LoaderCircle, RefreshCw, ShieldCheck, X } from 'lucide-react'
import { getGenerationJobDraft, reviewGenerationJob } from './api'
import type {
  DatasetDraft,
  GenerationDraft,
  GenerationJob,
  ReviewDecision,
  ScenarioDraft,
  TestCaseDraft,
} from './types'
import { ErrorBanner } from './ui'
import { titleCase } from './utils'

const noteLimitBytes = 2_000

export function ReviewPanel({
  token,
  job,
  onReviewed,
}: {
  token: string
  job: GenerationJob
  onReviewed: (job: GenerationJob) => void
}) {
  const [note, setNote] = useState('')
  const draft = useQuery({
    queryKey: ['generation-job-draft', job.id, token],
    queryFn: () => getGenerationJobDraft(token, job.id),
    retry: false,
  })
  const decision = useMutation({
    mutationFn: (input: { decision: ReviewDecision; note: string }) =>
      reviewGenerationJob(token, job.id, { decision: input.decision, note: input.note.trim() || undefined }),
    onSuccess: onReviewed,
  })

  // The server limits the note in bytes, not characters, so multi-byte text reaches the limit sooner.
  const noteBytes = new TextEncoder().encode(note).length
  const noteTooLong = noteBytes > noteLimitBytes
  const busy = decision.isPending
  // Approving publishes the draft, so it requires the draft to be visible; rejecting does not.
  const canApprove = draft.isSuccess && !busy && !noteTooLong
  const canReject = !busy && !noteTooLong

  return (
    <section className="workspace-panel review-panel" aria-labelledby="review-heading">
      <div className="panel-heading">
        <div>
          <span className="panel-kicker">Manual review</span>
          <h2 id="review-heading">Approve or reject this draft</h2>
        </div>
        <ShieldCheck aria-hidden="true" size={22} />
      </div>

      {draft.isPending && (
        <div className="draft-loading"><LoaderCircle className="spin" aria-hidden="true" size={18} /> Loading draft</div>
      )}
      {draft.error && (
        <ErrorBanner
          error={draft.error}
          action={<button className="secondary-button" type="button" onClick={() => draft.refetch()}><RefreshCw size={16} /> Retry</button>}
        />
      )}
      {draft.data && <DraftContent draft={draft.data} />}

      <div className="review-form">
        <label className="field review-note">
          <span>Review note (optional)</span>
          <textarea
            value={note}
            onChange={(event) => setNote(event.target.value)}
            rows={3}
            placeholder="Why you approve or reject this draft. A rejection note is kept on the failed job."
          />
          <span className="field-meter">
            <span className={noteTooLong ? 'is-over' : ''}>{noteBytes.toLocaleString()} / {noteLimitBytes.toLocaleString()} bytes</span>
          </span>
        </label>
        <p className="field-hint">Approving publishes this stage. If the job belongs to a pipeline, the next stage starts as a new job.</p>
        {decision.error && <ErrorBanner error={decision.error} />}
        <div className="review-actions">
          <button className="danger-button" type="button" disabled={!canReject} onClick={() => decision.mutate({ decision: 'REJECT', note })}>
            {busy && decision.variables?.decision === 'REJECT' ? <LoaderCircle className="spin" aria-hidden="true" size={16} /> : <X aria-hidden="true" size={16} />}
            Reject
          </button>
          <button className="primary-button" type="button" disabled={!canApprove} onClick={() => decision.mutate({ decision: 'APPROVE', note })}>
            {busy && decision.variables?.decision === 'APPROVE' ? <LoaderCircle className="spin" aria-hidden="true" size={17} /> : <Check aria-hidden="true" size={17} />}
            Approve and publish
          </button>
        </div>
      </div>
    </section>
  )
}

function DraftContent({ draft }: { draft: GenerationDraft }) {
  if (draft.job_kind === 'SCENARIO' && draft.scenario) return <ScenarioDraftView scenario={draft.scenario} />
  if (draft.job_kind === 'DATASET' && draft.dataset) return <DatasetDraftView dataset={draft.dataset} />
  if (draft.job_kind === 'TEST_CASES') return <TestCasesDraftView testCases={draft.test_cases ?? []} />
  return <p className="draft-empty">This draft has no content to show.</p>
}

function ScenarioDraftView({ scenario }: { scenario: ScenarioDraft }) {
  return (
    <div className="draft-body">
      <div className="draft-title">
        <h3>{scenario.name}</h3>
        <span className="draft-tag">{scenario.domain}</span>
      </div>
      <p className="draft-description">{scenario.description}</p>
      <h4 className="draft-subhead">Business rules ({scenario.business_rules.length})</h4>
      <ol className="rule-list">
        {scenario.business_rules.map((rule) => (
          <li key={rule.id}>
            <span className="rule-id">{rule.id}</span>
            <span>{rule.text}</span>
            {rule.critical && <span className="critical-tag">Critical</span>}
          </li>
        ))}
      </ol>
      <RawJSON label="Full scenario JSON" value={scenario} />
    </div>
  )
}

function DatasetDraftView({ dataset }: { dataset: DatasetDraft }) {
  const { tables } = dataset.metadata
  return (
    <div className="draft-body">
      <h4 className="draft-subhead">Tables ({tables.length})</h4>
      <table className="table-summary">
        <thead>
          <tr><th>Table</th><th>Columns</th><th>Rows</th></tr>
        </thead>
        <tbody>
          {tables.map((table) => (
            <tr key={table.name}>
              <td>{table.name}</td>
              <td>{table.columns.length}</td>
              <td>{table.row_count}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <h4 className="draft-subhead">Schema SQL</h4>
      <pre className="code-block">{dataset.schema_sql}</pre>
      <h4 className="draft-subhead">Seed SQL</h4>
      <pre className="code-block">{dataset.seed_sql}</pre>
      <h4 className="draft-subhead">Constraints ({dataset.constraints.checks.length} checks)</h4>
      <RawJSON label="Constraints and metadata JSON" value={{ constraints: dataset.constraints, metadata: dataset.metadata }} />
    </div>
  )
}

function TestCasesDraftView({ testCases }: { testCases: TestCaseDraft[] }) {
  return (
    <div className="draft-body">
      <h4 className="draft-subhead">Accepted test cases ({testCases.length})</h4>
      <ol className="case-list">
        {testCases.map((testCase) => (
          <li key={testCase.client_case_id} className="case-card">
            <div className="case-title">
              <code>{testCase.client_case_id}</code>
              <span className="draft-tag">{titleCase(testCase.difficulty)}</span>
            </div>
            <p>{testCase.turns?.[0]?.prompt}</p>
            <RawJSON label="Full test case JSON" value={testCase} />
          </li>
        ))}
      </ol>
    </div>
  )
}

function RawJSON({ label, value }: { label: string; value: unknown }) {
  return (
    <details className="raw-json">
      <summary>{label}</summary>
      <pre>{JSON.stringify(value, null, 2)}</pre>
    </details>
  )
}
