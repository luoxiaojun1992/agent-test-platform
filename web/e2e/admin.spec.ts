import { expect, test, type Page } from '@playwright/test'

const apiToken = 'e2e-api-token-00000000000000000001'
const shotDir = process.env.E2E_SHOTS ?? 'e2e-screenshots'
let shotNo = 0

// Full-page screenshot taken after a step has succeeded.
async function shot(page: Page, name: string) {
  shotNo += 1
  await page.screenshot({ path: `${shotDir}/${String(shotNo).padStart(2, '0')}-${name}.png`, fullPage: true })
}

async function open(page: Page, name: string) {
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name }).click()
  await expect(page.getByRole('heading', { level: 1, name })).toBeVisible()
}

async function saveSecret(page: Page, name: string, value: string) {
  await page.getByRole('button', { name: '保存凭据' }).click()
  await page.getByLabel('凭据名称').fill(name)
  await page.getByLabel('凭据值').fill(value)
  await page.getByRole('dialog').getByRole('button', { name: '保存', exact: true }).click()
  await expect(page.getByRole('table', { name: '凭据列表' }).getByText(name)).toBeVisible()
}

test('admin console: configure, run, evaluate, re-evaluate and benchmark with mock LLM and mock Agent', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('API 在线')).toBeVisible()
  await page.getByLabel('Platform API token').fill(apiToken)
  await expect(page.getByRole('heading', { level: 1, name: '仪表盘' })).toBeVisible()
  await shot(page, 'dashboard-empty')

  // The shell is a back-office layout: a left navigation with grouped sections, not a workbench.
  const sider = page.getByRole('navigation', { name: '主导航' })
  for (const group of ['概览', '测试资产', '评测执行', '被测对象', '系统']) await expect(sider.getByText(group, { exact: true })).toBeVisible()

  // Credentials
  await open(page, '凭据管理')
  await saveSecret(page, 'db-admin-password', 'e2eroot')
  await saveSecret(page, 'agent-token', 'mock-target-development-token')
  await shot(page, 'credentials-saved')

  // Validation MySQL (platform-side; the second container also plays the Agent's own database)
  await open(page, '校验用 MySQL')
  await page.getByRole('button', { name: '登记校验库' }).click()
  await page.getByLabel('校验库名称').fill('e2e-validation-db')
  await page.getByLabel('主机').fill('127.0.0.1')
  await page.getByLabel('端口').fill('3331')
  await page.getByLabel('管理员账号').fill('root')
  await page.getByLabel('密码凭据名').fill('db-admin-password')
  await page.getByRole('dialog').getByRole('button', { name: '登记', exact: true }).click()
  await expect(page.getByRole('table', { name: '校验库列表' }).getByText('e2e-validation-db')).toBeVisible()
  await shot(page, 'validation-db-registered')

  // Models: the active Judge and Generation model can be switched, one active per role.
  await open(page, '模型配置')
  const models = page.getByRole('table', { name: '模型列表' })
  await expect(models.getByRole('row', { name: /judge-b/ })).toBeVisible()
  await page.getByRole('button', { name: '启用 judge-b' }).click()
  await expect(page.getByRole('button', { name: '启用 judge-b' })).toBeDisabled()
  await expect(page.getByRole('button', { name: '启用 generation-primary' }).last()).toBeEnabled()
  await page.getByRole('button', { name: '启用 generation-alt' }).click()
  await expect(page.getByRole('button', { name: '启用 generation-alt' })).toBeDisabled()
  await shot(page, 'models-switched')

  // Test assets are generated one kind at a time, each from the previous one: scenario -> dataset -> test cases.
  await open(page, '业务场景')
  await page.getByRole('button', { name: '生成业务场景' }).click()
  await page.getByLabel('描述').fill('订单管理 Agent，覆盖订单状态查询')
  await shot(page, 'generate-scenario-dialog')
  await page.getByRole('dialog').getByRole('button', { name: '开始生成' }).click()
  await expect(page.getByText('generation job', { exact: false })).toBeVisible()
  await expect(page.locator('.status-badge')).toContainText(/Completed/i, { timeout: 90_000 })
  await shot(page, 'generate-scenario-completed')
  await open(page, '业务场景')
  await expect(page.getByRole('table', { name: '业务场景列表' }).getByText('Cancel an order')).toBeVisible()
  await shot(page, 'scenarios-listed')

  await open(page, '数据集')
  await page.getByRole('button', { name: '生成数据集' }).click()
  await expect(page.getByRole('dialog').getByRole('button', { name: '开始生成' })).toBeDisabled()
  await page.getByLabel('业务场景版本', { exact: true }).click()
  await expect(page.getByRole('listbox', { name: '业务场景版本选项' }).getByText('最新 10 条')).toBeVisible()
  await shot(page, 'dataset-pick-scenario')
  await page.getByRole('listbox', { name: '业务场景版本选项' }).getByRole('option').first().click()
  await page.getByLabel('描述').fill('订单表，6 行，状态均为 PAID')
  await page.getByLabel('校验库 e2e-validation-db').check()
  await expect(page.getByRole('dialog').getByRole('button', { name: '开始生成' })).toBeEnabled()
  await shot(page, 'dataset-dialog-ready')
  await page.getByRole('dialog').getByRole('button', { name: '开始生成' }).click()
  await expect(page.locator('.status-badge')).toContainText(/Completed/i, { timeout: 120_000 })
  await shot(page, 'generate-dataset-completed')
  await open(page, '数据集')
  await expect(page.getByRole('table', { name: '数据集列表' }).locator('tbody tr')).toHaveCount(1)
  await shot(page, 'datasets-listed')

  await open(page, '测试用例')
  await page.getByRole('button', { name: '生成测试用例' }).click()
  const generateCases = page.getByRole('dialog').getByRole('button', { name: '开始生成' })
  await expect(page.getByLabel('数据集版本', { exact: true })).toBeDisabled()
  await expect(generateCases).toBeDisabled()
  await page.getByLabel('业务场景版本', { exact: true }).click()
  await page.getByRole('listbox', { name: '业务场景版本选项' }).getByText('Cancel an order').click()
  await expect(page.getByLabel('数据集版本', { exact: true })).toBeEnabled()
  await page.getByLabel('数据集版本', { exact: true }).click()
  await page.getByRole('listbox', { name: '数据集版本选项' }).getByRole('option').first().click()
  await page.getByLabel('Agent 模式').selectOption('ANSWER')
  await page.getByLabel('用例数量', { exact: true }).fill('3')
  await page.getByLabel('描述').fill('订单状态查询')
  await shot(page, 'test-cases-dialog-ready')
  await generateCases.click()
  await expect(page.locator('.status-badge')).toContainText(/Completed/i, { timeout: 180_000 })
  await shot(page, 'generate-test-cases-completed')
  await open(page, '测试用例')
  await expect(page.getByRole('table', { name: '测试用例列表' }).getByText('status of order O1')).toBeVisible()
  await expect(page.getByRole('table', { name: '测试用例列表' }).locator('tbody tr')).toHaveCount(3)
  await shot(page, 'test-cases-listed')

  // Agent under test, including Tool discovery through the Agent's own GET /v1/tools.
  await open(page, 'Agent 管理')
  await page.getByRole('button', { name: '新建 Agent' }).click()
  await page.getByLabel('Agent 名称').fill('mock-agent')
  await page.getByLabel('Agent 地址').fill('http://127.0.0.1:18082')
  await page.getByLabel('Token 凭据名').fill('agent-token')
  await page.getByLabel('被测模型').fill('mock-agent-v1')
  await page.getByLabel('Tool 评估模式').selectOption('DETERMINISTIC_CHAIN')
  await page.getByRole('button', { name: '从 Agent 读取 Tool 列表' }).click()
  await expect(page.getByText(/共 [1-9]\d* 个 Tool/)).toBeVisible()
  await shot(page, 'agent-tools-discovered')
  await page.getByLabel('Tool 评估模式').selectOption('SESSION_LLM_ONLY')
  // The Agent's own database is configured on the Agent itself; it only receives the dataset's seed data and is never sent to the Agent.
  await page.getByLabel('数据库主机').fill('127.0.0.1')
  await page.getByLabel('数据库端口').fill('3331')
  await page.getByLabel('数据库库名').fill('agent_db')
  await page.getByLabel('数据库账号').fill('root')
  await page.getByLabel('数据库密码凭据名').fill('db-admin-password')
  await shot(page, 'agent-database')
  await page.getByRole('dialog').getByRole('button', { name: '创建', exact: true }).click()
  await expect(page.getByRole('row', { name: /mock-agent/ })).toBeVisible()
  await shot(page, 'agent-created')

  // Running tests is one action on the test run page: pick cases (newest 10 by default, searchable) and an Agent.
  await open(page, '测试运行')
  await page.getByRole('button', { name: '运行测试' }).click()
  const runDialog = page.getByRole('dialog', { name: '运行测试' })
  await expect(runDialog).toBeVisible()
  await expect(runDialog.getByRole('button', { name: /运行 0 个用例/ })).toBeDisabled()
  await runDialog.getByLabel('测试用例', { exact: true }).click()
  await expect(page.getByRole('listbox', { name: '测试用例选项' }).getByText('最新 10 条')).toBeVisible()
  await shot(page, 'run-tests-dropdown-latest')
  await runDialog.getByLabel('测试用例', { exact: true }).fill('order O2')
  await expect(page.getByRole('listbox', { name: '测试用例选项' }).getByText(/搜索“order O2”/)).toBeVisible()
  await expect(page.getByRole('listbox', { name: '测试用例选项' }).getByRole('option')).toHaveCount(1)
  await shot(page, 'run-tests-dropdown-search')
  await page.getByRole('listbox', { name: '测试用例选项' }).getByRole('option').first().click()
  await runDialog.getByLabel('选择 Agent', { exact: true }).selectOption({ index: 1 })
  await expect(runDialog.getByText('种子数据会写入该 Agent 配置的数据库')).toBeVisible()
  await expect(runDialog.getByRole('button', { name: /运行 1 个用例/ })).toBeEnabled()
  await shot(page, 'run-tests-dialog-ready')
  await runDialog.getByRole('button', { name: /运行 1 个用例/ }).click()

  // The Run list refreshes by itself until the worker and the Judge finish.
  await expect(page.getByRole('heading', { level: 1, name: '测试运行' })).toBeVisible()
  const firstRun = page.getByRole('table', { name: '运行列表' }).locator('tbody tr').first()
  await expect(firstRun.locator('[data-status="COMPLETED"]').first()).toBeVisible({ timeout: 90_000 })
  await shot(page, 'run-list-completed')
  await firstRun.getByRole('link').click()

  await expect(page.getByRole('heading', { level: 1, name: '运行详情' })).toBeVisible()
  await expect(page.getByText('Mock response from mock-agent-v1')).toBeVisible()
  await expect(page.getByText('What is the current status of order O2?')).toBeVisible()
  await expect(page.getByText(/Use the database named `run_/)).toBeVisible()
  await shot(page, 'run-detail-result')
  await page.getByRole('tab', { name: /评测尝试/ }).click()
  const attempts = page.getByRole('table', { name: '评测尝试' })
  await expect(attempts.getByText('judge-b')).toBeVisible()
  await expect(attempts.locator('[data-status="COMPLETED"]')).toBeVisible()
  await expect(attempts.getByText('通过', { exact: true })).toBeVisible()
  await shot(page, 'run-evaluation-attempts')

  // Re-evaluation creates a new Run judged by another model and leaves this Run untouched.
  const originalUrl = page.url()
  await page.getByRole('button', { name: '重新评测' }).click()
  await page.getByLabel('Judge 模型').selectOption('generation-primary')
  await shot(page, 'reevaluate-dialog')
  await page.getByRole('button', { name: '确认重新评测' }).click()
  await expect(page.getByText('已创建新的评测运行')).toBeVisible()
  await shot(page, 'reevaluate-created')
  await page.getByRole('status').getByRole('link').click()
  await expect(page).not.toHaveURL(originalUrl)
  await expect(page.locator('[data-status="COMPLETED"]').first()).toBeVisible({ timeout: 90_000 })
  await page.getByRole('tab', { name: /评测尝试/ }).click()
  await expect(page.getByRole('table', { name: '评测尝试' }).getByText('generation-primary')).toBeVisible()
  await shot(page, 'new-run-evaluated')
  await page.goto(originalUrl.replace(/^https?:\/\/[^/]+/, ''))
  await page.getByRole('tab', { name: /评测尝试/ }).click()
  await expect(page.getByRole('table', { name: '评测尝试' }).locator('tbody tr')).toHaveCount(1)
  await shot(page, 'original-run-untouched')

  // Benchmark compares Agents on the picked test cases.
  await open(page, '基准对比')
  await page.getByLabel('测试用例', { exact: true }).fill('order O2')
  await expect(page.getByRole('listbox', { name: '测试用例选项' }).getByRole('option')).toHaveCount(1)
  await page.getByRole('listbox', { name: '测试用例选项' }).getByRole('option').first().click()
  await expect(page.getByRole('table', { name: '基准结果' }).locator('tbody tr')).toHaveCount(1)
  await expect(page.getByRole('table', { name: '基准结果' })).toContainText('100.0%')
  await shot(page, 'benchmark')

  // Dashboard reflects the finished Runs.
  await open(page, '仪表盘')
  await expect(page.getByRole('table', { name: '最近运行' }).locator('[data-status="COMPLETED"]').first()).toBeVisible()
  // The dashboard analyses the results: one finished Run plus its re-evaluation, and the three generated asset kinds.
  await expect(page.locator('.stat', { hasText: '时间内运行数' })).toContainText('2')
  await expect(page.locator('.stat', { hasText: '测试用例（可用/全部版本）' })).toContainText('3 / 3')
  for (const chart of ['每日运行数与通过数', '每日通过率', '每日平均得分', '最终得分分布', '运行状态占比', '各 Agent 通过率', '测试资产版本数', '每日新增测试资产']) {
    await expect(page.getByRole('img', { name: chart }).or(page.getByRole('list', { name: chart }))).toBeVisible()
  }
  await expect(page.getByRole('list', { name: '测试资产版本数' })).toContainText('测试用例')
  await shot(page, 'dashboard-final')
  await page.getByRole('button', { name: '近 7 天' }).click()
  await expect(page.getByRole('button', { name: '近 7 天' })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByRole('img', { name: '每日通过率' })).toBeVisible()
  await shot(page, 'dashboard-7-days')
})

