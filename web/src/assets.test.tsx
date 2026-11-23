import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as api2 from './api2'
import { GenerateAssetDialog } from './GenerateAssetDialog'
import { RevisionSelect, notDeprecated, revisionTitle, usableAsInput } from './revision'
import { RunTestsDialog } from './RunTestsDialog'

vi.mock('./api2', async (importOriginal) => ({ ...(await importOriginal<typeof import('./api2')>()), searchRevisions: vi.fn(), createGenerationJobRequest: vi.fn(),
  listAgents: vi.fn(), createRuns: vi.fn() }))
vi.mock('./api', async (importOriginal) => ({ ...(await importOriginal<typeof import('./api')>()), listValidationDbs: vi.fn(async () => ({ items: [{ id: 'T1', name: 'target-a' }], next_cursor: null })) }))

const mocked = vi.mocked(api2)
const item = (id: string, overrides: Partial<api2.RevisionItem> = {}): api2.RevisionItem => ({ id, revision_no: 1, created_at: '2026-01-01T00:00:00Z', lifecycle_status: 'ACTIVE', ...overrides })

function renderWith(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}><MemoryRouter>{ui}</MemoryRouter></QueryClientProvider>)
}

beforeEach(() => vi.clearAllMocks())

describe('revision helpers', () => {
  it('titles each asset kind from its content', () => {
    expect(revisionTitle('scenarios', item('S', { content: { name: 'Cancel an order' } }))).toBe('Cancel an order')
    expect(revisionTitle('datasets', item('D', { metadata: { tables: [{ name: 'orders' }, { name: 'payments' }] } }))).toBe('表：orders、payments')
    expect(revisionTitle('test-cases', item('C', { content: { prompt: 'x'.repeat(80) } }))).toHaveLength(61)
  })

  it('only allows ACTIVE or VALIDATED revisions as generation input', () => {
    expect(usableAsInput(item('a', { lifecycle_status: 'ACTIVE' }))).toBe(true)
    expect(usableAsInput(item('a', { lifecycle_status: 'VALIDATED' }))).toBe(true)
    expect(usableAsInput(item('a', { lifecycle_status: 'DEPRECATED' }))).toBe(false)
    expect(notDeprecated(item('a', { lifecycle_status: 'DEPRECATED' }))).toBe(false)
  })
})

describe('RevisionSelect', () => {
  it('lists the newest ten by default and searches with a debounce', async () => {
    mocked.searchRevisions.mockResolvedValue({ items: Array.from({ length: 15 }, (_, index) => item(`R${index}`, { content: { prompt: `case ${index}` } })), next_cursor: null })
    const onChange = vi.fn()
    renderWith(<RevisionSelect token="tok" kind="test-cases" label="测试用例" multiple onChange={onChange} />)
    await userEvent.click(screen.getByLabelText('测试用例'))
    const list = await screen.findByRole('listbox')
    expect(within(list).getByText('最新 10 条')).toBeInTheDocument()
    expect(within(list).getAllByRole('option')).toHaveLength(10)
    expect(mocked.searchRevisions).toHaveBeenCalledWith('tok', 'test-cases', '', '', 50)
    await userEvent.type(screen.getByLabelText('测试用例'), 'refund')
    await waitFor(() => expect(mocked.searchRevisions).toHaveBeenLastCalledWith('tok', 'test-cases', 'refund', '', 50))
  })

  it('supports multiple selection and removing a chip', async () => {
    mocked.searchRevisions.mockResolvedValue({ items: [item('R1'), item('R2')], next_cursor: null })
    const onChange = vi.fn()
    renderWith(<RevisionSelect token="tok" kind="test-cases" label="测试用例" multiple onChange={onChange} />)
    await userEvent.click(screen.getByLabelText('测试用例'))
    const options = within(await screen.findByRole('listbox')).getAllByRole('option')
    await userEvent.click(options[0])
    await userEvent.click(options[1])
    expect(onChange).toHaveBeenLastCalledWith([expect.objectContaining({ id: 'R1' }), expect.objectContaining({ id: 'R2' })])
    await userEvent.click(screen.getAllByRole('button', { name: /移除/ })[0])
    expect(onChange).toHaveBeenLastCalledWith([expect.objectContaining({ id: 'R2' })])
  })

  it('does not search while disabled and explains why', () => {
    renderWith(<RevisionSelect token="tok" kind="datasets" label="数据集版本" disabled disabledHint="请先选择业务场景版本" onChange={vi.fn()} />)
    expect(screen.getByLabelText('数据集版本')).toBeDisabled()
    expect(screen.getByPlaceholderText('请先选择业务场景版本')).toBeInTheDocument()
    expect(mocked.searchRevisions).not.toHaveBeenCalled()
  })
})

describe('GenerateAssetDialog', () => {
  it('generates a scenario from a description alone', async () => {
    mocked.createGenerationJobRequest.mockResolvedValue({ id: 'J1', job_kind: 'SCENARIO', status: 'PENDING' })
    renderWith(<GenerateAssetDialog token="tok" kind="scenarios" onClose={vi.fn()} />)
    const submit = screen.getByRole('button', { name: '开始生成' })
    expect(submit).toBeDisabled()
    await userEvent.type(screen.getByLabelText('描述'), '订单取消')
    await userEvent.click(submit)
    await waitFor(() => expect(mocked.createGenerationJobRequest).toHaveBeenCalledTimes(1))
    const body = mocked.createGenerationJobRequest.mock.calls[0][1]
    expect(body).toMatchObject({ job_kind: 'SCENARIO', description: '订单取消' })
    expect(body).not.toHaveProperty('input_scenario_revision_id')
    expect(body).not.toHaveProperty('validation_db_ids')
  })

  it('requires a scenario and a validation DB for a dataset', async () => {
    mocked.searchRevisions.mockResolvedValue({ items: [item('SCEN1', { content: { name: 'Cancel' } })], next_cursor: null })
    mocked.createGenerationJobRequest.mockResolvedValue({ id: 'J2', job_kind: 'DATASET', status: 'PENDING' })
    renderWith(<GenerateAssetDialog token="tok" kind="datasets" onClose={vi.fn()} />)
    await userEvent.type(screen.getByLabelText('描述'), '订单表')
    const submit = screen.getByRole('button', { name: '开始生成' })
    expect(submit).toBeDisabled()
    await userEvent.click(screen.getByLabelText('业务场景版本'))
    await userEvent.click(within(await screen.findByRole('listbox')).getByRole('option'))
    expect(submit).toBeDisabled()
    await userEvent.click(await screen.findByLabelText('校验库 target-a'))
    expect(submit).toBeEnabled()
    await userEvent.click(submit)
    await waitFor(() => expect(mocked.createGenerationJobRequest).toHaveBeenCalledTimes(1))
    expect(mocked.createGenerationJobRequest.mock.calls[0][1]).toMatchObject({ job_kind: 'DATASET', input_scenario_revision_id: 'SCEN1', validation_db_ids: ['T1'] })
  })

  it('locks the dataset picker until a scenario is chosen and only offers that scenario\'s datasets', async () => {
    mocked.searchRevisions.mockImplementation(async (_token, kind) => (kind === 'scenarios'
      ? { items: [item('SCEN1', { content: { name: 'Cancel' } })], next_cursor: null }
      : { items: [item('DS-OK', { scenario_revision_id: 'SCEN1' }), item('DS-OTHER', { scenario_revision_id: 'SCEN2' }), item('DS-OLD', { scenario_revision_id: 'SCEN1', lifecycle_status: 'DEPRECATED' })], next_cursor: null }))
    mocked.createGenerationJobRequest.mockResolvedValue({ id: 'J3', job_kind: 'TEST_CASES', status: 'PENDING' })
    renderWith(<GenerateAssetDialog token="tok" kind="test-cases" onClose={vi.fn()} />)
    expect(screen.getByLabelText('数据集版本')).toBeDisabled()
    await userEvent.click(screen.getByLabelText('业务场景版本'))
    await userEvent.click(within(await screen.findByRole('listbox')).getByRole('option'))
    expect(screen.getByLabelText('数据集版本')).toBeEnabled()
    await userEvent.click(screen.getByLabelText('数据集版本'))
    const options = within(await screen.findByRole('listbox')).getAllByRole('option')
    expect(options).toHaveLength(1)
    expect(options[0]).toHaveTextContent('DS-OK'.slice(-6))
    await userEvent.click(options[0])
    await userEvent.type(screen.getByLabelText('描述'), '退款边界')
    await userEvent.click(screen.getByRole('button', { name: '开始生成' }))
    await waitFor(() => expect(mocked.createGenerationJobRequest).toHaveBeenCalledTimes(1))
    expect(mocked.createGenerationJobRequest.mock.calls[0][1]).toMatchObject({
      job_kind: 'TEST_CASES', input_scenario_revision_id: 'SCEN1', input_dataset_revision_id: 'DS-OK', test_case_count: 5,
    })
  })
})

describe('RunTestsDialog', () => {
  const agent = { id: 'A1', name: 'mock-agent', database_profile_id: 'DB1', chat_endpoint: 'http://x', target_model: 'm', contract_version: '1.0', tool_evaluation_mode: 'SESSION_LLM_ONLY' as const, tool_catalog: null, created_at: '' }

  it('runs the picked cases on the chosen Agent without any suite or snapshot', async () => {
    mocked.searchRevisions.mockResolvedValue({ items: [item('CASE1', { content: { prompt: 'p1' } })], next_cursor: null })
    mocked.listAgents.mockResolvedValue({ items: [agent], next_cursor: null })
    mocked.createRuns.mockResolvedValue({ run_ids: ['R1'] })
    renderWith(<RunTestsDialog token="tok" onClose={vi.fn()} />)
    const start = screen.getByRole('button', { name: /运行 0 个用例/ })
    expect(start).toBeDisabled()
    await userEvent.click(screen.getByLabelText('测试用例'))
    await userEvent.click(within(await screen.findByRole('listbox')).getByRole('option'))
    await screen.findByRole('option', { name: /mock-agent/ })
    await userEvent.selectOptions(screen.getByLabelText('选择 Agent'), 'A1')
    expect(await screen.findByText(/种子数据会写入该 Agent 配置的数据库/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /运行 1 个用例/ }))
    await waitFor(() => expect(mocked.createRuns).toHaveBeenCalledWith('tok', { agent_id: 'A1', test_case_revision_ids: ['CASE1'], repetitions: 1 }))
  })

  it('warns when the Agent has no database configured', async () => {
    mocked.searchRevisions.mockResolvedValue({ items: [], next_cursor: null })
    mocked.listAgents.mockResolvedValue({ items: [{ ...agent, database_profile_id: null }], next_cursor: null })
    renderWith(<RunTestsDialog token="tok" initial={[item('C')]} onClose={vi.fn()} />)
    await screen.findByRole('option', { name: /mock-agent/ })
    await userEvent.selectOptions(screen.getByLabelText('选择 Agent'), 'A1')
    expect(await screen.findByRole('alert')).toHaveTextContent('尚未配置数据库')
    expect(screen.getByRole('button', { name: /运行 1 个用例/ })).toBeDisabled()
  })
})
