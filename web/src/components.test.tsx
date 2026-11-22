import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactElement } from 'react'
import { MemoryRouter, Route, Routes } from 'react-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Alert, Badge, DataTable, LoadMore, Modal, PassTag, Score } from './admin'
import { ApiError } from './api'
import * as api2 from './api2'
import { ModelsPage } from './pages/ModelsPage'
import { RunDetailPage } from './pages/RunDetailPage'
import { RunsPage } from './pages/RunsPage'

vi.mock('./api2', async (importOriginal) => ({ ...(await importOriginal<typeof import('./api2')>()), listRuns: vi.fn(), getRun: vi.fn(), listToolCalls: vi.fn(),
  listRunEvaluations: vi.fn(), cancelRun: vi.fn(), reevaluateRun: vi.fn(), listModels: vi.fn(), activateModel: vi.fn() }))

const mocked = vi.mocked(api2)

function renderWith(ui: ReactElement, path = '/') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}>{ui}</MemoryRouter></QueryClientProvider>)
}

const run = (overrides: Partial<api2.TestRun> = {}): api2.TestRun => ({
  id: '01HZZZZZZZZZZZZZZZZZZZZZZ1', status: 'COMPLETED', execution_outcome: 'COMPLETED', test_case_revision_id: 'T', dataset_revision_id: 'D', agent_version_id: 'A',
  final_output: 'the answer', precondition_results: null, postcondition_results: null, state_diff: null, deterministic_details: null, error: null,
  deterministic_score: 100, final_score: 92.5, pass: true, created_at: '2026-11-19T10:00:00Z', ...overrides,
})

beforeEach(() => vi.clearAllMocks())
afterEach(() => vi.useRealTimers())

describe('shared components', () => {
  it('renders status badges, pass tags and scores', () => {
    render(<><Badge value="EVALUATING" /><PassTag pass={true} /><PassTag pass={false} /><PassTag pass={null} /><Score value={92.456} /><Score value={null} /></>)
    expect(screen.getByText('评测中')).toBeInTheDocument()
    expect(screen.getByText('通过')).toBeInTheDocument()
    expect(screen.getByText('未通过')).toBeInTheDocument()
    expect(screen.getByText('92.5')).toBeInTheDocument()
  })

  it('shows API errors with their code', () => {
    render(<Alert error={new ApiError(409, { code: 'CONFLICT', message: 'nope', retryable: false })} />)
    expect(screen.getByRole('alert')).toHaveTextContent('CONFLICT：nope')
  })

  it('renders table rows, the empty state and row clicks', async () => {
    const onClick = vi.fn()
    const { rerender } = render(<DataTable label="t" rows={[{ id: 'a' }]} rowKey={(row) => row.id} onRowClick={onClick} columns={[{ key: 'id', title: 'ID', render: (row) => row.id }]} />)
    await userEvent.click(screen.getByText('a'))
    expect(onClick).toHaveBeenCalledWith({ id: 'a' })
    rerender(<DataTable label="t" rows={[]} rowKey={(row: { id: string }) => row.id} empty="没有数据" columns={[{ key: 'id', title: 'ID', render: (row: { id: string }) => row.id }]} />)
    expect(screen.getByText('没有数据')).toBeInTheDocument()
  })

  it('closes a modal with Escape and offers load more only when there is a next page', async () => {
    const onClose = vi.fn()
    render(<Modal title="标题" onClose={onClose}><p>内容</p></Modal>)
    expect(screen.getByRole('dialog', { name: '标题' })).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalled()
    const fetchNextPage = vi.fn()
    const { rerender } = render(<LoadMore query={{ hasNextPage: true, isFetchingNextPage: false, fetchNextPage }} />)
    await userEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(fetchNextPage).toHaveBeenCalled()
    rerender(<LoadMore query={{ hasNextPage: false, isFetchingNextPage: false, fetchNextPage }} />)
    expect(screen.queryByRole('button', { name: '加载更多' })).toBeNull()
  })
})

describe('RunsPage', () => {
  it('lists runs and filters by status', async () => {
    mocked.listRuns.mockResolvedValue({ items: [run()], next_cursor: null })
    renderWith(<RunsPage token="tok" />)
    expect((await screen.findAllByText('已完成')).length).toBeGreaterThan(0)
    await userEvent.selectOptions(screen.getByLabelText('状态筛选'), 'RUNNING')
    await waitFor(() => expect(mocked.listRuns).toHaveBeenLastCalledWith('tok', '', { status: 'RUNNING' }))
  })

  it('asks for a token instead of calling the API', () => {
    renderWith(<RunsPage token="" />)
    expect(screen.getByText(/请先输入 Platform API token/)).toBeInTheDocument()
    expect(mocked.listRuns).not.toHaveBeenCalled()
  })
})

describe('RunDetailPage', () => {
  function detail() {
    return renderWith(<Routes><Route path="/runs/:runId" element={<RunDetailPage token="tok" />} /></Routes>, '/runs/01HZZZZZZZZZZZZZZZZZZZZZZ1')
  }

  it('offers re-evaluation for a completed run and reports the new run', async () => {
    mocked.getRun.mockResolvedValue(run())
    mocked.listToolCalls.mockResolvedValue({ items: [] })
    mocked.listRunEvaluations.mockResolvedValue({ items: [] })
    mocked.listModels.mockResolvedValue({ items: [
      { alias: 'judge-a', role: 'judge', provider: 'openai', model: 'm', active: true, created_at: '' },
      { alias: 'gen-a', role: 'generation', provider: 'openai', model: 'm', active: true, created_at: '' },
    ] })
    mocked.reevaluateRun.mockResolvedValue({ run_ids: ['NEWRUN'] })
    detail()
    expect(screen.queryByRole('button', { name: '取消运行' })).toBeNull()
    await userEvent.click(await screen.findByRole('button', { name: '重新评测' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).queryByText(/gen-a/)).toBeNull()
    await userEvent.click(within(dialog).getByRole('button', { name: '确认重新评测' }))
    await waitFor(() => expect(mocked.reevaluateRun).toHaveBeenCalledWith('tok', '01HZZZZZZZZZZZZZZZZZZZZZZ1', 'judge-a'))
    expect(await screen.findByText(/已创建新的评测运行/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'NEWRUN' })).toHaveAttribute('href', '/runs/NEWRUN')
  })

  it('offers cancel, not re-evaluation, for a running run', async () => {
    mocked.getRun.mockResolvedValue(run({ status: 'RUNNING', pass: null, final_score: null }))
    mocked.listToolCalls.mockResolvedValue({ items: [] })
    mocked.listRunEvaluations.mockResolvedValue({ items: [] })
    mocked.cancelRun.mockResolvedValue(run({ status: 'CANCELLED' }))
    detail()
    await userEvent.click(await screen.findByRole('button', { name: '取消运行' }))
    await waitFor(() => expect(mocked.cancelRun).toHaveBeenCalledWith('tok', '01HZZZZZZZZZZZZZZZZZZZZZZ1'))
    expect(screen.queryByRole('button', { name: '重新评测' })).toBeNull()
  })

  it('lists Tool calls and evaluation attempts on their tabs', async () => {
    mocked.getRun.mockResolvedValue(run())
    mocked.listToolCalls.mockResolvedValue({ items: [{ sequence: 1, call_id: 'c1', tool_name: 'run_query', arguments: { q: 1 }, result: [1], error: null, status: 'SUCCESS', duration_ms: 5, trace_source: 'AGENT_REPORTED' }] })
    mocked.listRunEvaluations.mockResolvedValue({ items: [{ id: 'e1', test_run_id: 'r', attempt_no: 1, status: 'COMPLETED', judge_model_alias: 'judge-a', llm_score: 90, final_score: 92, pass: true, created_at: '2026-11-19T10:00:00Z' }] })
    detail()
    await userEvent.click(await screen.findByRole('tab', { name: /Tool 调用/ }))
    expect(await screen.findByText('run_query')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: /评测尝试/ }))
    expect(await screen.findByText('judge-a')).toBeInTheDocument()
  })
})

describe('ModelsPage', () => {
  it('enables a non-active model and disables the button for the active one', async () => {
    mocked.listModels.mockResolvedValue({ items: [
      { alias: 'judge-a', role: 'judge', provider: 'openai', model: 'm1', active: true, created_at: '' },
      { alias: 'judge-b', role: 'judge', provider: 'openai', model: 'm2', active: false, created_at: '' },
    ] })
    mocked.activateModel.mockResolvedValue({ role: 'judge', alias: 'judge-b' })
    renderWith(<ModelsPage token="tok" />)
    expect(await screen.findByRole('button', { name: '启用 judge-a' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: '启用 judge-b' }))
    await waitFor(() => expect(mocked.activateModel).toHaveBeenCalledWith('tok', 'judge', 'judge-b'))
  })

  it('shows the API error when activation fails', async () => {
    mocked.listModels.mockResolvedValue({ items: [{ alias: 'judge-b', role: 'judge', provider: 'openai', model: 'm', active: false, created_at: '' }] })
    mocked.activateModel.mockRejectedValue(new ApiError(404, { code: 'NOT_FOUND', message: 'model alias is not registered', retryable: false }))
    renderWith(<ModelsPage token="tok" />)
    await userEvent.click(await screen.findByRole('button', { name: '启用 judge-b' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('NOT_FOUND')
  })
})
