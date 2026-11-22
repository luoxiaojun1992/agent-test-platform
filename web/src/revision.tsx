import { useEffect, useId, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { X } from 'lucide-react'
import { searchRevisions, type AssetKind, type RevisionItem } from './api2'
import { shortID } from './utils'

const topN = 10

function text(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

export function revisionTitle(kind: AssetKind, item: RevisionItem): string {
  if (kind === 'scenarios') return text(item.content?.name) || '（未命名场景）'
  if (kind === 'datasets') {
    const tables = Array.isArray(item.metadata?.tables) ? (item.metadata?.tables as { name?: string }[]).map((table) => table.name).filter(Boolean) : []
    return tables.length > 0 ? `表：${tables.join('、')}` : '（数据集）'
  }
  const prompt = text(item.content?.prompt)
  return prompt.length > 60 ? `${prompt.slice(0, 60)}…` : prompt || '（测试用例）'
}

export function revisionLabel(kind: AssetKind, item: RevisionItem): string {
  return `r${item.revision_no} · ${revisionTitle(kind, item)} · ${shortID(item.id)}`
}

export const usableAsInput = (item: RevisionItem) => item.lifecycle_status === 'ACTIVE' || item.lifecycle_status === 'VALIDATED'
export const notDeprecated = (item: RevisionItem) => item.lifecycle_status !== 'DEPRECATED'

interface Props {
  token: string
  kind: AssetKind
  label: string
  multiple?: boolean
  required?: boolean
  disabled?: boolean
  disabledHint?: string
  initial?: RevisionItem[]
  filter?: (item: RevisionItem) => boolean
  onChange: (items: RevisionItem[]) => void
}

// Searchable dropdown over published asset revisions. With an empty search it lists the newest N revisions.
export function RevisionSelect({ token, kind, label, multiple, required, disabled, disabledHint, initial, filter, onChange }: Props) {
  const listId = useId()
  const box = useRef<HTMLDivElement>(null)
  const [selected, setSelected] = useState<RevisionItem[]>(initial ?? [])
  const [input, setInput] = useState('')
  const [search, setSearch] = useState('')
  const [open, setOpen] = useState(false)

  useEffect(() => {
    const timer = setTimeout(() => setSearch(input.trim()), 250)
    return () => clearTimeout(timer)
  }, [input])

  const results = useQuery({
    queryKey: ['revision-search', token, kind, search],
    enabled: token !== '' && open && !disabled,
    queryFn: () => searchRevisions(token, kind, search, '', 50),
  })
  const options = (results.data?.items ?? []).filter((item) => (filter ? filter(item) : true)).slice(0, topN)
  const chosen = new Set(selected.map((item) => item.id))

  function commit(next: RevisionItem[]) {
    setSelected(next)
    onChange(next)
  }

  function toggle(item: RevisionItem) {
    if (multiple) {
      commit(chosen.has(item.id) ? selected.filter((value) => value.id !== item.id) : [...selected, item])
    } else {
      commit([item])
      setOpen(false)
      setInput('')
    }
  }

  return (
    <div className="revision-select" ref={box} onBlur={(event) => { if (!box.current?.contains(event.relatedTarget as Node | null)) setOpen(false) }}>
      <span className="form-label">{label}{required && <em className="required-mark"> *</em>}</span>
      <div className={`rs-control${disabled ? ' is-disabled' : ''}`} onClick={() => { if (!disabled) setOpen(true) }}>
        {selected.map((item) => (
          <span key={item.id} className="rs-chip" title={revisionLabel(kind, item)}>
            {revisionLabel(kind, item)}
            {!disabled && (
              <button type="button" aria-label={`移除 ${shortID(item.id)}`} onClick={(event) => { event.stopPropagation(); commit(selected.filter((value) => value.id !== item.id)) }}><X size={12} /></button>
            )}
          </span>
        ))}
        <input
          role="combobox"
          aria-expanded={open}
          aria-controls={listId}
          aria-label={label}
          disabled={disabled}
          value={input}
          placeholder={selected.length === 0 ? (disabled ? disabledHint ?? '' : '点击选择，或输入关键字搜索') : ''}
          onFocus={() => setOpen(true)}
          onChange={(event) => { setInput(event.target.value); setOpen(true) }}
        />
      </div>
      {open && !disabled && (
        <ul className="rs-list" id={listId} role="listbox" aria-label={`${label}选项`} aria-multiselectable={multiple}>
          <li className="rs-head" role="presentation">{search ? `搜索“${search}”（最多 ${topN} 条）` : `最新 ${topN} 条`}</li>
          {results.isPending && <li className="rs-empty" role="presentation">加载中…</li>}
          {results.isError && <li className="rs-empty" role="presentation">加载失败</li>}
          {!results.isPending && !results.isError && options.length === 0 && <li className="rs-empty" role="presentation">没有可选项{search ? '，换个关键字试试' : ''}</li>}
          {options.map((item) => (
            <li key={item.id} role="option" aria-selected={chosen.has(item.id)} className={chosen.has(item.id) ? 'rs-option is-selected' : 'rs-option'}
              tabIndex={-1} onMouseDown={(event) => event.preventDefault()} onClick={() => toggle(item)}>
              <span>{revisionLabel(kind, item)}</span>
              {item.lifecycle_status && <small>{item.lifecycle_status}</small>}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
