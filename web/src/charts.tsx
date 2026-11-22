// Small dependency-free SVG charts. Each one has a text alternative (role="img" + aria-label) and a visible empty state.

export interface Series { name: string; color: string; values: (number | null)[] }

const W = 560
const H = 200
const PAD = { top: 12, right: 12, bottom: 24, left: 36 }

function niceMax(value: number): number {
  if (value <= 0) return 1
  const exponent = 10 ** Math.floor(Math.log10(value))
  const fraction = value / exponent
  const step = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 5 ? 5 : 10
  return step * exponent
}

export function shortDate(date: string): string {
  return date.slice(5)
}

function Axes({ labels, max, unit }: { labels: string[]; max: number; unit?: string }) {
  const ticks = [0, 0.25, 0.5, 0.75, 1]
  const every = Math.max(1, Math.ceil(labels.length / 7))
  const x = (index: number) => PAD.left + ((W - PAD.left - PAD.right) * (labels.length === 1 ? 0.5 : index / (labels.length - 1)))
  return (
    <g className="chart-axes">
      {ticks.map((tick) => {
        const y = PAD.top + (H - PAD.top - PAD.bottom) * (1 - tick)
        return (
          <g key={tick}>
            <line x1={PAD.left} x2={W - PAD.right} y1={y} y2={y} />
            <text x={PAD.left - 6} y={y + 4} textAnchor="end">{Math.round(max * tick)}{unit ?? ''}</text>
          </g>
        )
      })}
      {labels.map((label, index) => (index % every === 0 ? <text key={label} x={x(index)} y={H - 6} textAnchor="middle">{shortDate(label)}</text> : null))}
    </g>
  )
}

export function Legend({ items }: { items: { name: string; color: string }[] }) {
  return (
    <ul className="chart-legend">
      {items.map((item) => <li key={item.name}><i style={{ background: item.color }} />{item.name}</li>)}
    </ul>
  )
}

export function Empty({ text = '所选时间内暂无数据' }: { text?: string }) {
  return <div className="chart-empty">{text}</div>
}

// Line chart over a shared x axis of dates. A null value breaks the line, so a day without data is not drawn as zero.
export function LineChart({ labels, series, max, unit, label }: { labels: string[]; series: Series[]; max?: number; unit?: string; label: string }) {
  const has = series.some((item) => item.values.some((value) => value !== null))
  if (!has) return <Empty />
  const top = max ?? niceMax(Math.max(...series.flatMap((item) => item.values.filter((value): value is number => value !== null))))
  const x = (index: number) => PAD.left + ((W - PAD.left - PAD.right) * (labels.length === 1 ? 0.5 : index / (labels.length - 1)))
  const y = (value: number) => PAD.top + (H - PAD.top - PAD.bottom) * (1 - Math.min(value, top) / top)
  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={label} className="chart">
        <Axes labels={labels} max={top} unit={unit} />
        {series.map((item) => {
          const segments: string[] = []
          let current = ''
          item.values.forEach((value, index) => {
            if (value === null) {
              if (current) segments.push(current)
              current = ''
            } else {
              current += `${current ? 'L' : 'M'}${x(index).toFixed(1)},${y(value).toFixed(1)}`
            }
          })
          if (current) segments.push(current)
          return (
            <g key={item.name}>
              {segments.map((path, index) => <path key={index} d={path} fill="none" stroke={item.color} strokeWidth={2} strokeLinejoin="round" />)}
              {item.values.map((value, index) => (value === null ? null : (
                <circle key={index} cx={x(index)} cy={y(value)} r={3} fill={item.color}><title>{`${item.name} ${labels[index]}: ${Math.round(value * 10) / 10}${unit ?? ''}`}</title></circle>
              )))}
            </g>
          )
        })}
      </svg>
      <Legend items={series} />
    </div>
  )
}

// Stacked vertical bars over dates.
export function StackedBarChart({ labels, series, label }: { labels: string[]; series: Series[]; label: string }) {
  const totals = labels.map((_, index) => series.reduce((sum, item) => sum + (item.values[index] ?? 0), 0))
  if (totals.every((total) => total === 0)) return <Empty />
  const top = niceMax(Math.max(...totals))
  const inner = W - PAD.left - PAD.right
  const slot = inner / labels.length
  const bar = Math.min(28, slot * 0.7)
  const y = (value: number) => PAD.top + (H - PAD.top - PAD.bottom) * (1 - value / top)
  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={label} className="chart">
        <Axes labels={labels} max={top} />
        {labels.map((date, index) => {
          let base = 0
          const cx = PAD.left + slot * index + slot / 2
          return (
            <g key={date}>
              {series.map((item) => {
                const value = item.values[index] ?? 0
                if (value === 0) return null
                const rect = <rect key={item.name} x={cx - bar / 2} y={y(base + value)} width={bar} height={y(base) - y(base + value)} fill={item.color}><title>{`${item.name} ${date}: ${value}`}</title></rect>
                base += value
                return rect
              })}
            </g>
          )
        })}
      </svg>
      <Legend items={series} />
    </div>
  )
}

// Horizontal bars for a few categories, with the value printed at the end.
export function HBarChart({ items, label, unit }: { items: { name: string; value: number; color?: string }[]; label: string; unit?: string }) {
  if (items.length === 0 || items.every((item) => item.value === 0)) return <Empty />
  const max = Math.max(...items.map((item) => item.value), 1)
  return (
    <ul className="hbars" aria-label={label}>
      {items.map((item) => (
        <li key={item.name}>
          <span className="hbar-name" title={item.name}>{item.name}</span>
          <span className="hbar-track"><span className="hbar-fill" style={{ width: `${(item.value / max) * 100}%`, background: item.color ?? 'var(--coral)' }} /></span>
          <span className="hbar-value">{Math.round(item.value * 10) / 10}{unit ?? ''}</span>
        </li>
      ))}
    </ul>
  )
}

// Column chart for a distribution (score buckets).
export function ColumnChart({ items, label, color = 'var(--coral)' }: { items: { name: string; value: number }[]; label: string; color?: string }) {
  const total = items.reduce((sum, item) => sum + item.value, 0)
  if (total === 0) return <Empty />
  const top = niceMax(Math.max(...items.map((item) => item.value)))
  const inner = W - PAD.left - PAD.right
  const slot = inner / items.length
  const bar = slot * 0.6
  return (
    <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={label} className="chart">
      {[0, 0.5, 1].map((tick) => {
        const y = PAD.top + (H - PAD.top - PAD.bottom) * (1 - tick)
        return <g key={tick} className="chart-axes"><line x1={PAD.left} x2={W - PAD.right} y1={y} y2={y} /><text x={PAD.left - 6} y={y + 4} textAnchor="end">{Math.round(top * tick)}</text></g>
      })}
      {items.map((item, index) => {
        const cx = PAD.left + slot * index + slot / 2
        const height = (H - PAD.top - PAD.bottom) * (item.value / top)
        return (
          <g key={item.name}>
            <rect x={cx - bar / 2} y={H - PAD.bottom - height} width={bar} height={height} fill={color}><title>{`${item.name}: ${item.value}`}</title></rect>
            {item.value > 0 && <text x={cx} y={H - PAD.bottom - height - 4} textAnchor="middle" className="chart-value">{item.value}</text>}
            <text x={cx} y={H - 6} textAnchor="middle" className="chart-axes-text">{item.name}</text>
          </g>
        )
      })}
    </svg>
  )
}

// Donut for shares of a whole.
export function DonutChart({ items, label }: { items: { name: string; value: number; color: string }[]; label: string }) {
  const total = items.reduce((sum, item) => sum + item.value, 0)
  if (total === 0) return <Empty />
  const radius = 54
  const circumference = 2 * Math.PI * radius
  const slices = items.filter((item) => item.value > 0).reduce<{ item: (typeof items)[number]; length: number; offset: number }[]>((all, item) => {
    const offset = all.length ? all[all.length - 1].offset + all[all.length - 1].length : 0
    return [...all, { item, length: (item.value / total) * circumference, offset }]
  }, [])
  return (
    <div className="donut">
      <svg viewBox="0 0 140 140" role="img" aria-label={label} className="donut-svg">
        <g transform="rotate(-90 70 70)">
          {slices.map(({ item, length, offset }) => (
            <circle key={item.name} cx={70} cy={70} r={radius} fill="none" stroke={item.color} strokeWidth={20} strokeDasharray={`${length} ${circumference - length}`} strokeDashoffset={-offset}><title>{`${item.name}: ${item.value}`}</title></circle>
          ))}
        </g>
        <text x={70} y={68} textAnchor="middle" className="donut-total">{total}</text>
        <text x={70} y={86} textAnchor="middle" className="chart-axes-text">总计</text>
      </svg>
      <ul className="chart-legend vertical">
        {items.map((item) => <li key={item.name}><i style={{ background: item.color }} />{item.name}<b>{item.value}</b></li>)}
      </ul>
    </div>
  )
}
