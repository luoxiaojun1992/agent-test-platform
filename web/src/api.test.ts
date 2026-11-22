import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, request } from './api'
import * as api2 from './api2'

function mockFetch(status: number, body: unknown, capture?: (url: string, init?: RequestInit) => void) {
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    capture?.(url, init)
    return new Response(body === null ? '' : JSON.stringify(body), { status })
  }))
}

afterEach(() => vi.unstubAllGlobals())

describe('request', () => {
  it('sends the bearer token and parses JSON', async () => {
    let seen: RequestInit | undefined
    mockFetch(200, { ok: true }, (_url, init) => { seen = init })
    await expect(request('/api/v1/x', 'tok')).resolves.toEqual({ ok: true })
    expect(new Headers(seen?.headers).get('Authorization')).toBe('Bearer tok')
  })

  it('turns an error body into an ApiError', async () => {
    mockFetch(409, { code: 'CONFLICT', message: 'already exists', retryable: false })
    await expect(request('/api/v1/x', 't')).rejects.toMatchObject({ name: 'ApiError', status: 409, code: 'CONFLICT', message: 'already exists', retryable: false })
  })

  it('treats 5xx without a body as retryable', async () => {
    mockFetch(503, null)
    const error = await request('/api/v1/x', 't').catch((value: unknown) => value)
    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).retryable).toBe(true)
  })
})

describe('api2 requests', () => {
  it('re-evaluates with an Idempotency-Key and the chosen judge alias', async () => {
    let url = ''
    let init: RequestInit | undefined
    mockFetch(202, { run_ids: ['R2'] }, (u, i) => { url = u; init = i })
    await expect(api2.reevaluateRun('tok', 'R1', 'judge-b')).resolves.toEqual({ run_ids: ['R2'] })
    expect(url).toBe('/api/v1/test-runs/R1/evaluations')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({ judge_model_alias: 'judge-b' })
    expect(new Headers(init?.headers).get('Idempotency-Key')).toMatch(/^reeval-/)
  })

  it('activates a model with PUT', async () => {
    let url = ''
    let init: RequestInit | undefined
    mockFetch(200, { role: 'judge', alias: 'judge-b' }, (u, i) => { url = u; init = i })
    await api2.activateModel('tok', 'judge', 'judge-b')
    expect(url).toBe('/api/v1/models/judge/active')
    expect(init?.method).toBe('PUT')
    expect(JSON.parse(String(init?.body))).toEqual({ alias: 'judge-b' })
  })

  it('builds list URLs with cursor and filters, skipping empty values', async () => {
    const urls: string[] = []
    mockFetch(200, { items: [], next_cursor: null }, (u) => urls.push(u))
    await api2.listRuns('tok', 'CUR', { status: 'COMPLETED', agent_id: '' })
    await api2.listRuns('tok', '', {})
    expect(urls[0]).toBe('/api/v1/test-runs?limit=50&cursor=CUR&status=COMPLETED')
    expect(urls[1]).toBe('/api/v1/test-runs?limit=50')
  })

  it('creates Runs directly from test case revisions', async () => {
    let url = ''
    let init: RequestInit | undefined
    mockFetch(202, { run_ids: ['R1'] }, (u, i) => { url = u; init = i })
    await api2.createRuns('tok', { agent_id: 'V1', test_case_revision_ids: ['C1', 'C2'], repetitions: 3 })
    expect(url).toBe('/api/v1/test-runs')
    expect(JSON.parse(String(init?.body))).toEqual({ agent_id: 'V1', test_case_revision_ids: ['C1', 'C2'], repetitions: 3 })
  })

  it('discovers Tools through the platform endpoint', async () => {
    let init: RequestInit | undefined
    mockFetch(200, { tools: [{ name: 't', description: '', input_schema: {} }] }, (_u, i) => { init = i })
    const result = await api2.discoverTools('tok', { chat_endpoint: 'http://a', target_bearer_token_ref: 'ref' })
    expect(result.tools).toHaveLength(1)
    expect(init?.method).toBe('POST')
  })
})
