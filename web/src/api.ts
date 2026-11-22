import type {
  ErrorResponse,
  GenerationDraft,
  GenerationJob,
  GenerationPackage,
  GenerationReviewRequest,
  SecretList,
  SecretMetadata,
  ValidationDB,
  ValidationDBInput,
  ValidationDBList,
} from './types'

const apiBase = (import.meta.env.VITE_API_BASE_URL as string | undefined)?.replace(/\/$/, '') ?? ''

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly retryable: boolean

  constructor(status: number, response: ErrorResponse) {
    super(response.message)
    this.name = 'ApiError'
    this.status = status
    this.code = response.code
    this.retryable = response.retryable
  }
}

export async function request<T>(path: string, token: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('Accept', 'application/json')
  if (init?.body) headers.set('Content-Type', 'application/json')
  if (token) headers.set('Authorization', `Bearer ${token}`)

  const response = await fetch(`${apiBase}${path}`, { ...init, headers })
  const body = await response.text()
  let data: unknown = null
  if (body) {
    try {
      data = JSON.parse(body)
    } catch {
      throw new Error(`API returned invalid JSON (${response.status})`)
    }
  }
  if (!response.ok) {
    const failure = data as Partial<ErrorResponse> | null
    throw new ApiError(response.status, {
      code: failure?.code ?? 'HTTP_ERROR',
      message: failure?.message ?? `Request failed with status ${response.status}`,
      retryable: failure?.retryable ?? response.status >= 500,
      details: failure?.details,
    })
  }
  return data as T
}

export function listSecrets(token: string): Promise<SecretList> {
  return request('/api/v1/secrets?limit=200', token)
}

export function putSecret(token: string, name: string, value: string): Promise<SecretMetadata> {
  return request(`/api/v1/secrets/${encodeURIComponent(name)}`, token, {
    method: 'PUT',
    body: JSON.stringify({ value }),
  })
}

export function listValidationDbs(token: string): Promise<ValidationDBList> {
  return request('/api/v1/validation-mysql?limit=200', token)
}

export function createValidationDb(token: string, input: ValidationDBInput): Promise<ValidationDB> {
  return request('/api/v1/validation-mysql', token, {
    method: 'POST',
    headers: { 'Idempotency-Key': `target-${crypto.randomUUID()}` },
    body: JSON.stringify(input),
  })
}

export async function checkHealth(): Promise<boolean> {
  const response = await fetch(`${apiBase}/healthz`, { headers: { Accept: 'application/json' } })
  return response.ok
}

export function getGenerationJob(token: string, id: string): Promise<GenerationJob> {
  return request(`/api/v1/generation-jobs/${encodeURIComponent(id)}`, token)
}

export function cancelGenerationJob(token: string, id: string): Promise<GenerationJob> {
  return request(`/api/v1/generation-jobs/${encodeURIComponent(id)}/cancel`, token, {
    method: 'POST',
    headers: { 'Idempotency-Key': `cancel-${crypto.randomUUID()}` },
  })
}

export function getGenerationJobDraft(token: string, id: string): Promise<GenerationDraft> {
  return request(`/api/v1/generation-jobs/${encodeURIComponent(id)}/draft`, token)
}

export function getGenerationPackage(token: string, id: string): Promise<GenerationPackage> {
  return request(`/api/v1/generation-jobs/${encodeURIComponent(id)}/package`, token)
}

export function reviewGenerationJob(token: string, id: string, input: GenerationReviewRequest): Promise<GenerationJob> {
  return request(`/api/v1/generation-jobs/${encodeURIComponent(id)}/review`, token, {
    method: 'POST',
    headers: { 'Idempotency-Key': `review-${crypto.randomUUID()}` },
    body: JSON.stringify(input),
  })
}
