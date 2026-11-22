import type { ReactNode } from 'react'
import { CircleAlert } from 'lucide-react'
import { ApiError } from './api'
import type { GenerationJob } from './types'
import { titleCase } from './utils'

export function StatusBadge({ status }: { status: GenerationJob['status'] }) {
  return <span className={`status-badge status-${status.toLowerCase()}`}>{titleCase(status)}</span>
}

export function ErrorBanner({ error, action }: { error: unknown; action?: ReactNode }) {
  const message = error instanceof ApiError
    ? `${error.code}: ${error.message}`
    : error instanceof Error
      ? error.message
      : isJobFailure(error)
        ? `${error.code}: ${error.message}`
        : 'The request could not be completed.'
  return (
    <div className="error-banner" role="alert">
      <CircleAlert aria-hidden="true" size={19} />
      <span>{message}</span>
      {action}
    </div>
  )
}

export function EmptyState({ icon, title, text }: { icon: ReactNode; title: string; text: string }) {
  return <div className="empty-state page-enter"><span className="empty-icon">{icon}</span><h1>{title}</h1><p>{text}</p></div>
}

function isJobFailure(value: unknown): value is { code: string; message: string } {
  return Boolean(value && typeof value === 'object' && 'code' in value && 'message' in value)
}