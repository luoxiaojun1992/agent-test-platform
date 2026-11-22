import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ColumnChart, DonutChart, HBarChart, LineChart, StackedBarChart } from './charts'

const labels = ['2026-11-18', '2026-11-19', '2026-11-20']

describe('charts', () => {
  it('shows an empty state instead of an empty plot when there is no data', () => {
    render(<LineChart label="趋势" labels={labels} series={[{ name: 'a', color: '#000', values: [null, null, null] }]} />)
    expect(screen.getByText('所选时间内暂无数据')).toBeInTheDocument()
    render(<StackedBarChart label="柱" labels={labels} series={[{ name: 'a', color: '#000', values: [0, 0, 0] }]} />)
    expect(screen.getAllByText('所选时间内暂无数据')).toHaveLength(2)
  })

  it('breaks a line at days without data rather than drawing them as zero', () => {
    const { container } = render(<LineChart label="通过率" labels={labels} max={100} unit="%" series={[{ name: '通过率', color: '#2f9e6f', values: [80, null, 60] }]} />)
    expect(screen.getByRole('img', { name: '通过率' })).toBeInTheDocument()
    expect(container.querySelectorAll('path')).toHaveLength(2)
    expect(container.querySelectorAll('circle')).toHaveLength(2)
    expect(container.querySelector('circle title')?.textContent).toBe('通过率 2026-11-18: 80%')
  })

  it('stacks bars and skips zero-height segments', () => {
    const { container } = render(<StackedBarChart label="运行" labels={labels} series={[
      { name: '通过', color: '#2f9e6f', values: [2, 0, 1] }, { name: '未通过', color: '#d9534f', values: [1, 0, 0] },
    ]} />)
    expect(container.querySelectorAll('rect')).toHaveLength(3)
    expect(screen.getByText('通过')).toBeInTheDocument()
  })

  it('draws horizontal bars scaled to the largest value', () => {
    const { container } = render(<HBarChart label="版本" unit="%" items={[{ name: 'a · v1', value: 100 }, { name: 'b · v2', value: 50 }]} />)
    const fills = Array.from(container.querySelectorAll<HTMLElement>('.hbar-fill')).map((el) => el.style.width)
    expect(fills).toEqual(['100%', '50%'])
    expect(screen.getByText('50%')).toBeInTheDocument()
  })

  it('draws one column per bucket and labels non-empty ones', () => {
    const { container } = render(<ColumnChart label="分布" items={[{ name: '0-20', value: 0 }, { name: '80-100', value: 7 }]} />)
    expect(container.querySelectorAll('rect')).toHaveLength(2)
    expect(screen.getByText('7')).toBeInTheDocument()
  })

  it('draws donut slices that add up to the total and ignores empty ones', () => {
    const { container } = render(<DonutChart label="状态" items={[{ name: '已完成', value: 3, color: '#2f9e6f' }, { name: '排队中', value: 0, color: '#aaa' }, { name: '运行中', value: 1, color: '#4f8cff' }]} />)
    expect(container.querySelectorAll('circle')).toHaveLength(2)
    expect(screen.getAllByText('4').length).toBeGreaterThan(0)
  })
})

const overview = {
  days: 14,
  assets: [
    { kind: 'scenarios', resources: 1, revisions: 2, usable_revisions: 1 },
    { kind: 'datasets', resources: 1, revisions: 1, usable_revisions: 1 },
    { kind: 'test_cases', resources: 3, revisions: 3, usable_revisions: 3 },
  ],
  agents: { agents: 2, versions: 3 },
  run_statuses: [{ status: 'COMPLETED', count: 5 }, { status: 'RUNNING', count: 1 }],
  generation_jobs: [{ kind: 'TEST_CASES', status: 'COMPLETED', count: 2 }],
  daily_runs: [
    { date: '2026-11-19', total: 2, evaluated: 2, passed: 1, pass_rate: 50, avg_score: 60 },
    { date: '2026-11-20', total: 4, evaluated: 2, passed: 2, pass_rate: 100, avg_score: 90 },
  ],
  daily_assets: [{ date: '2026-11-19', scenarios: 0, datasets: 0, test_cases: 0 }, { date: '2026-11-20', scenarios: 1, datasets: 1, test_cases: 3 }],
  score_distribution: [{ from: 0, to: 20, count: 0 }, { from: 20, to: 40, count: 0 }, { from: 40, to: 60, count: 1 }, { from: 60, to: 80, count: 0 }, { from: 80, to: 100, count: 3 }],
  by_agent_version: [{ agent_name: 'bot', version: 'v1', agent_version_id: 'V', runs: 6, evaluated: 4, passed: 3, pass_rate: 75, avg_score: 80 }],
}

const mocked = vi.hoisted(() => ({ getAnalytics: vi.fn(), listModels: vi.fn(), listRuns: vi.fn() }))
vi.mock('./api2', async (importOriginal) => ({ ...(await importOriginal<typeof import('./api2')>()), ...mocked }))

describe('Dashboard', () => {
  it('summarises the window and reloads when the range changes', async () => {
    mocked.getAnalytics.mockResolvedValue(overview)
    mocked.listModels.mockResolvedValue({ items: [] })
    mocked.listRuns.mockResolvedValue({ items: [], next_cursor: null })
    const { Dashboard } = await import('./pages/Dashboard')
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <MemoryRouter><Dashboard token="tok" /></MemoryRouter>
      </QueryClientProvider>,
    )
    expect(await screen.findByText('时间内运行数')).toBeInTheDocument()
    const stat = (name: string) => screen.getAllByText(name).map((el) => el.closest('.stat')).find(Boolean) as HTMLElement
    expect(stat('时间内运行数')).toHaveTextContent('6')
    expect(stat('通过率')).toHaveTextContent('75%')
    expect(stat('测试用例（可用/全部版本）')).toHaveTextContent('3 / 3')
    expect(stat('Agent / 版本')).toHaveTextContent('2 / 3')
    expect(screen.getByRole('img', { name: '每日通过率' })).toBeInTheDocument()
    expect(screen.getByRole('img', { name: '每日运行数与通过数' })).toBeInTheDocument()
    expect(mocked.getAnalytics).toHaveBeenLastCalledWith('tok', 14)
    await userEvent.click(screen.getByRole('button', { name: '近 30 天' }))
    await vi.waitFor(() => expect(mocked.getAnalytics).toHaveBeenLastCalledWith('tok', 30))
  })
})
