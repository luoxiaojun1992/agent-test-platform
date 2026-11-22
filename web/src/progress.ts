import type { GenerationJob, GenerationStatus, JobKind } from './types'

export interface ProgressStep {
  status: GenerationStatus
  state: 'complete' | 'active' | 'pending'
  repairing: boolean
}

// Stages each kind of Job goes through, in order (see internal/generation/workflow.go). A REPAIRING
// stage has no row of its own: the Job is shown on the VALIDATING row it repairs.
const stagesByKind: Record<JobKind, GenerationStatus[]> = {
  SCENARIO: ['PENDING', 'VALIDATING_INPUT', 'GENERATING_SCENARIO', 'VALIDATING_SCENARIO', 'AWAITING_REVIEW', 'PUBLISHING', 'COMPLETED'],
  DATASET: ['PENDING', 'VALIDATING_INPUT', 'GENERATING_DATASET', 'VALIDATING_DATASET', 'AWAITING_REVIEW', 'PUBLISHING', 'COMPLETED'],
  TEST_CASES: ['PENDING', 'VALIDATING_INPUT', 'GENERATING_TEST_CASES', 'VALIDATING_TEST_CASES', 'AWAITING_REVIEW', 'PUBLISHING', 'COMPLETED'],
}

const validatingByRepairing: Partial<Record<GenerationStatus, GenerationStatus>> = {
  REPAIRING_SCENARIO: 'VALIDATING_SCENARIO',
  REPAIRING_DATASET: 'VALIDATING_DATASET',
  REPAIRING_TEST_CASES: 'VALIDATING_TEST_CASES',
}

export function jobProgress(job: Pick<GenerationJob, 'job_kind' | 'status' | 'require_manual_review'>): ProgressStep[] {
  const stages = stagesByKind[job.job_kind].filter((stage) => stage !== 'AWAITING_REVIEW' || job.require_manual_review)
  const repairedStage = validatingByRepairing[job.status]
  const activeIndex = stages.indexOf(repairedStage ?? job.status)
  const finished = job.status === 'COMPLETED'
  return stages.map((status, index) => {
    let state: ProgressStep['state'] = 'pending'
    if (finished || (activeIndex >= 0 && index < activeIndex)) {
      state = 'complete'
    } else if (index === activeIndex) {
      state = 'active'
    }
    return { status, state, repairing: index === activeIndex && repairedStage !== undefined }
  })
}
