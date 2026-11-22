import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, Check, KeyRound, LoaderCircle, RefreshCw, ShieldCheck, Square } from 'lucide-react'
import { Link, useParams } from 'react-router'
import { cancelGenerationJob, getGenerationJob } from './api'
import { jobProgress, type ProgressStep } from './progress'
import { PackagePanel } from './PackagePanel'
import { ReviewPanel } from './ReviewPanel'
import type { GenerationStatus } from './types'
import { EmptyState, ErrorBanner, StatusBadge } from './ui'
import { formatDate, shortID, titleCase } from './utils'

const assetListPath = { SCENARIO: '/scenarios', DATASET: '/datasets', TEST_CASES: '/test-cases' } as const
const assetListTitle = { SCENARIO: '业务场景', DATASET: '数据集', TEST_CASES: '测试用例' } as const

const terminalStatuses = new Set<GenerationStatus>(['COMPLETED', 'FAILED', 'CANCELLED'])

// The backend cancels any non-terminal Job except one already PUBLISHING (internal/generation/workflow.go).
function canCancel(status: GenerationStatus): boolean {
  return !terminalStatuses.has(status) && status !== 'PUBLISHING'
}

function StepMarker({ state, status }: { state: ProgressStep['state']; status: GenerationStatus }) {
  if (state === 'complete') return <Check size={14} />
  if (state === 'pending') return <span />
  return status === 'AWAITING_REVIEW' ? <ShieldCheck size={14} /> : <LoaderCircle className="spin" size={14} />
}

export function JobPage({ token }: { token: string }) {
  const { jobId = '' } = useParams()
  const queryClient = useQueryClient()
  const queryKey = ['generation-job', jobId, token] as const
  const query = useQuery({
    queryKey,
    queryFn: () => getGenerationJob(token, jobId),
    enabled: Boolean(token && jobId),
    refetchInterval: (state) => {
      const job = state.state.data
      if (job && terminalStatuses.has(job.status)) return false
      // A paused Job waits on a person, so it is polled less often than a Job a Worker is running.
      return job?.status === 'AWAITING_REVIEW' ? 10_000 : 2_000
    },
  })
  const cancelMutation = useMutation({
    mutationFn: () => cancelGenerationJob(token, jobId),
    onSuccess: (job) => queryClient.setQueryData(queryKey, job),
  })

  if (!token) return <EmptyState icon={<KeyRound size={24} />} title="Platform API token required" text="Enter the platform bearer token in the header." />
  if (query.isPending) return <EmptyState icon={<LoaderCircle className="spin" size={24} />} title="Loading generation job" text={shortID(jobId)} />
  if (query.error) {
    return (
      <div className="page-enter">
        <Link to="/scenarios" className="back-link"><ArrowLeft size={16} /> 返回资产</Link>
        <ErrorBanner error={query.error} action={<button className="secondary-button" onClick={() => query.refetch()}><RefreshCw size={16} /> Retry</button>} />
      </div>
    )
  }
  const job = query.data
  if (!job) return null

  const progress = jobProgress(job)

  return (
    <div className="page-enter job-page">
      <Link to={assetListPath[job.job_kind]} className="back-link"><ArrowLeft size={16} /> 返回{assetListTitle[job.job_kind]}</Link>
      <div className="job-heading">
        <div><div className="eyebrow">{titleCase(job.job_kind)} generation job</div><h1>{shortID(job.id)}</h1><code className="full-id">{job.id}</code></div>
        <div className="job-heading-actions">
          <StatusBadge status={job.status} />
          {canCancel(job.status) && (
            <button className="danger-button" onClick={() => cancelMutation.mutate()} disabled={cancelMutation.isPending}>
              {cancelMutation.isPending ? <LoaderCircle className="spin" size={16} /> : <Square size={15} />} Cancel
            </button>
          )}
        </div>
      </div>

      {(job.error || cancelMutation.error) && <ErrorBanner error={cancelMutation.error ?? job.error} />}

      {job.status === 'AWAITING_REVIEW' && (
        <ReviewPanel key={job.id} token={token} job={job} onReviewed={(reviewed) => queryClient.setQueryData(queryKey, reviewed)} />
      )}

      {job.status === 'COMPLETED' && <PackagePanel token={token} jobId={job.id} />}

      <div className="job-layout">
        <section className="workspace-panel progress-panel">
          <div className="panel-heading">
            <div><span className="panel-kicker">Pipeline</span><h2>Generation progress</h2></div>
            {!terminalStatuses.has(job.status) && <span className="live-indicator"><span /> Live</span>}
          </div>
          <ol className="stage-list">
            {progress.map((step) => (
              <li key={step.status} className={step.state === 'active' ? 'is-active' : step.state === 'complete' ? 'is-complete' : undefined}>
                <span className="stage-marker"><StepMarker state={step.state} status={step.status} /></span>
                <span>{titleCase(step.status)}{step.repairing && <span className="repair-tag">Repairing</span>}</span>
              </li>
            ))}
          </ol>
        </section>

        <aside className="job-facts">
          <div className="fact-row"><span>Status</span><strong>{titleCase(job.status)}</strong></div>
          <div className="fact-row"><span>Job kind</span><strong>{titleCase(job.job_kind)}</strong></div>
          <div className="fact-row"><span>Manual review</span><strong>{job.require_manual_review ? 'Required' : 'Off'}</strong></div>
          {job.job_kind === 'TEST_CASES' && (
            <>
              <div className="fact-row"><span>Requested cases</span><strong>{job.requested_test_case_count}</strong></div>
              <div className="fact-row"><span>Accepted cases</span><strong>{job.accepted_test_case_count ?? 0}</strong></div>
            </>
          )}
          <div className="fact-row"><span>Repair round</span><strong>{job.repair_round ?? 0} / 2</strong></div>
          {job.pipeline_id && <div className="fact-row"><span>Pipeline</span><strong title={job.pipeline_id}>{shortID(job.pipeline_id)}</strong></div>}
          {job.input_scenario_revision_id && <div className="fact-row"><span>Input scenario</span><strong title={job.input_scenario_revision_id}>{shortID(job.input_scenario_revision_id)}</strong></div>}
          {job.input_dataset_revision_id && <div className="fact-row"><span>Input dataset</span><strong title={job.input_dataset_revision_id}>{shortID(job.input_dataset_revision_id)}</strong></div>}
          <div className="fact-row"><span>Created</span><strong>{formatDate(job.created_at)}</strong></div>
          <div className="fact-row"><span>Updated</span><strong>{formatDate(job.updated_at)}</strong></div>
        </aside>
      </div>
    </div>
  )
}