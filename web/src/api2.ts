import { request } from './api'

export interface Agent {
  id: string; name: string; description?: string; database_profile_id: string | null
  chat_endpoint: string; target_model: string; contract_version: string
  tool_evaluation_mode: 'DETERMINISTIC_CHAIN' | 'SESSION_LLM_ONLY'; tool_catalog: ToolDefinition[] | null
  client_id?: string; token_url?: string; created_at: string
}
export interface ToolDefinition { name: string; description: string; input_schema: unknown }
export interface AgentInput {
  name: string; description?: string; database_profile_id?: string | null
  chat_endpoint: string; target_bearer_token_ref: string; target_model: string; contract_version: '1.0'
  tool_evaluation_mode: 'DETERMINISTIC_CHAIN' | 'SESSION_LLM_ONLY'; tool_catalog: ToolDefinition[] | null
  client_id?: string; client_secret_ref?: string; token_url?: string
}
export interface TestRun {
  id: string; status: string; execution_outcome?: string; test_case_revision_id: string; dataset_revision_id: string; agent_id: string
  final_output: string | null; precondition_results: unknown; postcondition_results: unknown; state_diff: unknown; deterministic_details: unknown
  error: unknown; deterministic_score: number | null; final_score: number | null; pass: boolean | null; created_at: string
}
export interface ToolCall {
  sequence: number; call_id: string; tool_name: string; arguments: unknown; result: unknown; error: unknown; status: string; duration_ms: number; trace_source: string
}
export interface EvaluationAttempt {
  id: string; test_run_id: string; attempt_no: number; status: string; judge_model_alias: string; llm_score: number | null; final_score: number | null; pass: boolean | null; created_at: string
}
export interface BenchmarkEntry { agent_id: string; run_count: number; pass_rate: number; avg_score: number; critical_failures: number }
export interface ModelEntry { alias: string; role: 'generation' | 'judge'; provider: string; model: string; active: boolean; created_at: string }
export interface CatalogItem { id: string; created_at: string }
export type AssetKind = 'scenarios' | 'datasets' | 'test-cases'
export interface RevisionItem {
  id: string; revision_no: number; created_at: string; lifecycle_status?: string; quality_level?: string
  scenario_id?: string; dataset_id?: string; test_case_id?: string; scenario_revision_id?: string; dataset_revision_id?: string
  content?: Record<string, unknown>; metadata?: Record<string, unknown>; schema_sql?: string; seed_sql?: string
}
export const revisionPath: Record<AssetKind, string> = { scenarios: 'scenario-revisions', datasets: 'dataset-revisions', 'test-cases': 'test-case-revisions' }

type PageOf<T> = { items: T[]; next_cursor: string | null }

const post = <T,>(token: string, path: string, body: unknown, prefix: string) =>
  request<T>(path, token, { method: 'POST', headers: { 'Idempotency-Key': `${prefix}-${crypto.randomUUID()}` }, body: body === undefined ? undefined : JSON.stringify(body) })

const query = (params: Record<string, string | undefined>) => {
  const search = new URLSearchParams()
  Object.entries(params).forEach(([name, value]) => { if (value) search.set(name, value) })
  const text = search.toString()
  return text ? `?${text}` : ''
}

export const listAgents = (token: string, cursor: string) => request<PageOf<Agent>>(`/api/v1/agents${query({ limit: '50', cursor })}`, token)
export const createAgent = (token: string, body: AgentInput) => post<Agent>(token, '/api/v1/agents', body, 'agent')
export const discoverTools = (token: string, body: { chat_endpoint: string; target_bearer_token_ref: string }) =>
  request<{ tools: ToolDefinition[] }>('/api/v1/agent-tool-discovery', token, { method: 'POST', body: JSON.stringify(body) })

export const searchRevisions = (token: string, kind: AssetKind, q: string, cursor: string, limit = 50) =>
  request<PageOf<RevisionItem>>(`/api/v1/${revisionPath[kind]}${query({ limit: String(limit), cursor, q })}`, token)
export const createGenerationJobRequest = (token: string, body: Record<string, unknown>) =>
  post<{ id: string; job_kind: string; status: string }>(token, '/api/v1/generation-jobs', body, 'gen')
export const listCatalog = (token: string, plural: string, cursor: string) => request<PageOf<CatalogItem>>(`/api/v1/${plural}${query({ limit: '50', cursor })}`, token)
export const listCatalogRevisions = (token: string, plural: string, id: string, cursor: string) => request<PageOf<Record<string, unknown>>>(`/api/v1/${plural}/${id}/revisions${query({ limit: '50', cursor })}`, token)

export interface AgentDatabaseInput { name: string; host: string; port: number; database: string; username: string; password_secret_ref: string; tls_mode: 'DISABLED_LOCAL_ONLY' | 'VERIFY_IDENTITY' }
// The Agent's own database is stored as a Database Profile; the platform only uses it to write test data into the Agent under test.
export const createAgentDatabase = (token: string, body: AgentDatabaseInput) =>
  post<{ id: string }>(token, '/api/v1/database-profiles', { ...body, engine: 'MYSQL', snapshot_mode: 'SCHEMA_ONLY', snapshot_timeout_seconds: 30 }, 'agentdb')

export const createRuns = (token: string, body: { agent_id: string; test_case_revision_ids: string[]; repetitions: number }) =>
  post<{ run_ids: string[] }>(token, '/api/v1/test-runs', body, 'runs')
export const listRuns = (token: string, cursor: string, filter: { status?: string; agent_id?: string; test_case_revision_id?: string }) =>
  request<PageOf<TestRun>>(`/api/v1/test-runs${query({ limit: '50', cursor, ...filter })}`, token)
export const getRun = (token: string, id: string) => request<TestRun>(`/api/v1/test-runs/${id}`, token)
export const listToolCalls = (token: string, id: string) => request<{ items: ToolCall[] }>(`/api/v1/test-runs/${id}/tool-calls`, token)
export const listRunEvaluations = (token: string, id: string) => request<{ items: EvaluationAttempt[] }>(`/api/v1/test-runs/${id}/evaluations`, token)
export const cancelRun = (token: string, id: string) => post<TestRun>(token, `/api/v1/test-runs/${id}/cancel`, undefined, 'cancel')
export const reevaluateRun = (token: string, id: string, alias: string) => post<{ run_ids: string[] }>(token, `/api/v1/test-runs/${id}/evaluations`, { judge_model_alias: alias }, 'reeval')
export const getBenchmark = (token: string, caseIds: string[]) => {
  const search = new URLSearchParams()
  caseIds.forEach((id) => search.append('test_case_revision_id', id))
  return request<{ items: BenchmarkEntry[] }>(`/api/v1/benchmark?${search.toString()}`, token)
}

export interface DailyRuns { date: string; total: number; evaluated: number; passed: number; pass_rate: number | null; avg_score: number | null }
export interface AnalyticsOverview {
  days: number
  assets: { kind: 'scenarios' | 'datasets' | 'test_cases'; resources: number; revisions: number; usable_revisions: number }[]
  agents: { agents: number }
  run_statuses: { status: string; count: number }[]
  generation_jobs: { kind: string; status: string; count: number }[]
  daily_runs: DailyRuns[]
  daily_assets: { date: string; scenarios: number; datasets: number; test_cases: number }[]
  score_distribution: { from: number; to: number; count: number }[]
  by_agent: { agent_name: string; agent_id: string; runs: number; evaluated: number; passed: number; pass_rate: number | null; avg_score: number | null }[]
}
export const getAnalytics = (token: string, days: number) => request<AnalyticsOverview>(`/api/v1/analytics/overview?days=${days}`, token)

export const listModels = (token: string) => request<{ items: ModelEntry[] }>('/api/v1/models', token)
export const activateModel = (token: string, role: string, alias: string) =>
  request<{ role: string; alias: string }>(`/api/v1/models/${role}/active`, token, { method: 'PUT', body: JSON.stringify({ alias }) })
