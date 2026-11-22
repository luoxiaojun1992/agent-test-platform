import { useInfiniteQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { useEffect, useId, useRef } from 'react'
import { LoaderCircle, X } from 'lucide-react'
import { ApiError } from './api'

export interface Page<T> { items: T[]; next_cursor: string | null }

const statusTone: Record<string, string> = {
  COMPLETED: 'success', ACTIVE: 'success', SUCCESS: 'success', SUCCEEDED: 'success',
  RUNNING: 'info', PREPARING: 'info', EVALUATING: 'info', QUEUED: 'default', PENDING: 'default',
  FAILED: 'danger', EVALUATION_FAILED: 'danger', AGENT_ERROR: 'danger', INFRA_ERROR: 'danger', INVALID_TEST_DATA: 'warning', TIMEOUT: 'warning',
  CANCELLED: 'muted', AWAITING_REVIEW: 'warning',
}

const statusText: Record<string, string> = {
  COMPLETED: '已完成', QUEUED: '排队中', PREPARING: '准备中', RUNNING: '执行中', EVALUATING: '评测中', FAILED: '失败',
  EVALUATION_FAILED: '评测失败', INVALID_TEST_DATA: '测试数据无效', CANCELLED: '已取消', PENDING: '待处理', AGENT_ERROR: 'Agent 错误',
  TIMEOUT: '超时', INFRA_ERROR: '基础设施错误', AWAITING_REVIEW: '待审核', SUCCESS: '成功', SUCCEEDED: '成功',
}

export function Badge({ value, tone }: { value: string; tone?: string }) {
  return <span className={`tag tag-${tone ?? statusTone[value] ?? 'default'}`} data-status={value}>{statusText[value] ?? value}</span>
}

export function PassTag({ pass }: { pass: boolean | null | undefined }) {
  if (pass === null || pass === undefined) return <span className="muted">—</span>
  return <span className={`tag ${pass ? 'tag-success' : 'tag-danger'}`}>{pass ? '通过' : '未通过'}</span>
}

export function PageHeader({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return (
    <div className="page-header">
      <div>
        <h1>{title}</h1>
        {description && <p>{description}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  )
}

export function Card({ title, extra, children, flush }: { title?: ReactNode; extra?: ReactNode; children: ReactNode; flush?: boolean }) {
  return (
    <section className="card">
      {(title || extra) && <header className="card-head"><h2>{title}</h2><div>{extra}</div></header>}
      <div className={flush ? 'card-body flush' : 'card-body'}>{children}</div>
    </section>
  )
}

export interface Column<T> { key: string; title: string; render: (row: T) => ReactNode; width?: number | string; align?: 'right' }

export function DataTable<T>({ columns, rows, rowKey, loading, empty = '暂无数据', onRowClick, label }: {
  columns: Column<T>[]; rows: T[]; rowKey: (row: T) => string; loading?: boolean; empty?: string; onRowClick?: (row: T) => void; label?: string
}) {
  return (
    <div className="table-wrap">
      <table className="data-table" aria-label={label}>
        <thead>
          <tr>{columns.map((column) => <th key={column.key} style={{ width: column.width, textAlign: column.align }}>{column.title}</th>)}</tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={rowKey(row)} className={onRowClick ? 'clickable' : undefined} onClick={onRowClick ? () => onRowClick(row) : undefined}>
              {columns.map((column) => <td key={column.key} style={{ textAlign: column.align }}>{column.render(row)}</td>)}
            </tr>
          ))}
          {!loading && rows.length === 0 && <tr><td className="table-empty" colSpan={columns.length}>{empty}</td></tr>}
          {loading && rows.length === 0 && <tr><td className="table-empty" colSpan={columns.length}><LoaderCircle className="spin" size={16} /> 加载中…</td></tr>}
        </tbody>
      </table>
    </div>
  )
}

export function useCursorList<T>(key: unknown[], enabled: boolean, fetcher: (cursor: string) => Promise<Page<T>>, refetchInterval?: number) {
  const query = useInfiniteQuery({
    queryKey: key,
    enabled,
    initialPageParam: '',
    queryFn: ({ pageParam }) => fetcher(pageParam),
    getNextPageParam: (last: Page<T>) => last.next_cursor ?? undefined,
    refetchInterval,
  })
  const rows = query.data?.pages.flatMap((page) => page.items) ?? []
  return { query, rows }
}

export function LoadMore({ query }: { query: { hasNextPage: boolean; isFetchingNextPage: boolean; fetchNextPage: () => unknown } }) {
  if (!query.hasNextPage) return null
  return (
    <div className="load-more">
      <button type="button" className="btn" onClick={() => query.fetchNextPage()} disabled={query.isFetchingNextPage}>
        {query.isFetchingNextPage ? '加载中…' : '加载更多'}
      </button>
    </div>
  )
}

export function Modal({ title, onClose, children, footer, wide }: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  const titleId = useId()
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    ref.current?.focus()
    const onKey = (event: KeyboardEvent) => { if (event.key === 'Escape') onClose() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])
  return (
    <div className="modal-mask" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}>
      <div className={wide ? 'modal modal-wide' : 'modal'} role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1} ref={ref}>
        <header><h2 id={titleId}>{title}</h2><button type="button" className="icon-btn" onClick={onClose} aria-label="关闭"><X size={16} /></button></header>
        <div className="modal-body">{children}</div>
        {footer && <footer>{footer}</footer>}
      </div>
    </div>
  )
}

export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return <label className="form-item"><span className="form-label">{label}</span>{children}{hint && <span className="form-hint">{hint}</span>}</label>
}

export function Tabs({ tabs, active, onChange }: { tabs: { key: string; label: string }[]; active: string; onChange: (key: string) => void }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map((tab) => (
        <button key={tab.key} type="button" role="tab" aria-selected={tab.key === active} className={tab.key === active ? 'tab is-active' : 'tab'} onClick={() => onChange(tab.key)}>{tab.label}</button>
      ))}
    </div>
  )
}

export function JsonView({ value }: { value: unknown }) {
  if (value === null || value === undefined) return <span className="muted">—</span>
  return <pre className="json-view">{typeof value === 'string' ? value : JSON.stringify(value, null, 2)}</pre>
}

export function Descriptions({ items }: { items: { label: string; value: ReactNode }[] }) {
  return <dl className="descriptions">{items.map((item) => <div key={item.label}><dt>{item.label}</dt><dd>{item.value}</dd></div>)}</dl>
}

export function Alert({ error }: { error: unknown }) {
  if (!error) return null
  const message = error instanceof ApiError ? `${error.code}：${error.message}` : error instanceof Error ? error.message : '请求失败'
  return <div className="alert alert-error" role="alert">{message}</div>
}

export function Notice({ children }: { children: ReactNode }) {
  return <div className="alert alert-info" role="status">{children}</div>
}

export function Mono({ children }: { children: ReactNode }) {
  return <code className="mono">{children}</code>
}

export function Score({ value }: { value: number | null | undefined }) {
  return value === null || value === undefined ? <span className="muted">—</span> : <span className="mono">{value.toFixed(1)}</span>
}
