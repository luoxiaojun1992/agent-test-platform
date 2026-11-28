export type TLSMode = 'DISABLED_LOCAL_ONLY' | 'VERIFY_IDENTITY'

export interface ValidationDBInput {
  name: string
  host: string
  port: number
  admin_username: string
  admin_password_secret_name: string
  tls_mode: TLSMode
  ca_certificate_pem?: string
}

export interface ValidationDB extends ValidationDBInput {
  id: string
  created_at: string
}

export interface ValidationDBList {
  items: ValidationDB[]
  next_cursor: string | null
}

export interface SecretMetadata {
  name: string
  created_at: string
  updated_at: string
}

export interface SecretList {
  items: SecretMetadata[]
  next_cursor: string | null
}

export const generationStatuses = [
  'PENDING',
  'VALIDATING_INPUT',
  'GENERATING_SCENARIO',
  'VALIDATING_SCENARIO',
  'REPAIRING_SCENARIO',
  'GENERATING_DATASET',
  'VALIDATING_DATASET',
  'REPAIRING_DATASET',
  'GENERATING_TEST_CASES',
  'VALIDATING_TEST_CASES',
  'REPAIRING_TEST_CASES',
  'AWAITING_REVIEW',
  'PUBLISHING',
  'COMPLETED',
  'FAILED',
  'CANCELLED',
] as const

export type GenerationStatus = (typeof generationStatuses)[number]

export type JobKind = 'SCENARIO' | 'DATASET' | 'TEST_CASES'

export interface JobFailure {
  code: string
  message: string
  retryable: boolean
  details?: Record<string, unknown>
}

export interface GenerationJob {
  id: string
  job_kind: JobKind
  pipeline_id?: string | null
  input_scenario_revision_id?: string
  input_dataset_revision_id?: string
  status: GenerationStatus
  require_manual_review: boolean
  requested_test_case_count: number
  accepted_test_case_count?: number
  repair_round?: number
  error?: JobFailure | null
  created_at: string
  updated_at: string
}

export interface GenerationPackage {
  job_id: string
  scenario_revision_id: string
  dataset_revision_id: string
  test_case_revision_ids: string[]
  manifest: Record<string, unknown>
  manifest_sha256: string
}

export type ReviewDecision = 'APPROVE' | 'REJECT'

export interface GenerationReviewRequest {
  decision: ReviewDecision
  note?: string
}

export interface ScenarioDraft {
  name: string
  description: string
  domain: string
  business_rules: Array<{ id: string; text: string; source: string; critical: boolean }>
}

export interface DatasetDraft {
  schema_sql: string
  seed_sql: string
  constraints: { checks: unknown[] }
  metadata: { tables: Array<{ name: string; row_count: number; columns: unknown[] }> }
}

export interface TestCaseDraft {
  client_case_id: string
  turns?: { prompt?: string }[]
  difficulty: string
}

// Exactly one of scenario, dataset, or test_cases is present, matching job_kind.
export interface GenerationDraft {
  job_id: string
  job_kind: JobKind
  input_scenario_revision_id?: string
  input_dataset_revision_id?: string
  scenario?: ScenarioDraft
  dataset?: DatasetDraft
  test_cases?: TestCaseDraft[]
}

export interface ErrorResponse {
  code: string
  message: string
  retryable: boolean
  details?: Record<string, unknown>
}

export interface RecentJob {
  id: string
  createdAt: string
}