import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Bot, Cpu, Database, FileText, FlaskConical, Gauge, KeyRound, Layers, PlayCircle, Table2, Trophy } from 'lucide-react'
import { Link, Navigate, NavLink, Route, Routes, useLocation } from 'react-router'
import { checkHealth } from './api'
import { AgentsPage } from './pages/AgentsPage'
import { AssetPage } from './pages/AssetPage'
import { BenchmarkPage } from './pages/BenchmarkPage'
import { Dashboard } from './pages/Dashboard'
import { ModelsPage } from './pages/ModelsPage'
import { RunDetailPage } from './pages/RunDetailPage'
import { RunsPage } from './pages/RunsPage'
import { JobPage } from './JobPage'
import { SecretsPage } from './SecretsPage'
import { ValidationDbPage } from './ValidationDbPage'
import { loadToken, saveToken } from './storage'
import './admin.css'

type NavItem = { to: string; label: string; icon: typeof Gauge; end?: boolean }

const nav: { group: string; items: NavItem[] }[] = [
  { group: '概览', items: [{ to: '/', label: '仪表盘', icon: Gauge, end: true }] },
  { group: '测试资产', items: [
    { to: '/scenarios', label: '业务场景', icon: Layers },
    { to: '/datasets', label: '数据集', icon: Table2 },
    { to: '/test-cases', label: '测试用例', icon: FileText },
  ] },
  { group: '评测执行', items: [
    { to: '/runs', label: '测试运行', icon: PlayCircle },
    { to: '/benchmark', label: '基准对比', icon: Trophy },
  ] },
  { group: '被测对象', items: [
    { to: '/agents', label: 'Agent 管理', icon: Bot },
  ] },
  { group: '系统', items: [
    { to: '/validation-db', label: '校验用 MySQL', icon: Database },
    { to: '/models', label: '模型配置', icon: Cpu },
    { to: '/secrets', label: '凭据管理', icon: KeyRound },
  ] },
]

const titles: Record<string, string> = {
  '': '仪表盘', scenarios: '业务场景', datasets: '数据集', 'test-cases': '测试用例', runs: '测试运行', benchmark: '基准对比',
  agents: 'Agent 管理', 'validation-db': '校验用 MySQL', models: '模型配置', secrets: '凭据管理', jobs: '生成任务',
}

export default function App() {
  const [token, setTokenState] = useState(loadToken)
  const location = useLocation()
  const health = useQuery({ queryKey: ['health'], queryFn: checkHealth, retry: false, refetchInterval: 15_000 })
  const section = location.pathname.split('/')[1] ?? ''

  function setToken(value: string) {
    setTokenState(value)
    saveToken(value)
  }

  return (
    <div className="admin">
      <aside className="sider">
        <Link to="/" className="sider-logo" aria-label="Agent Test Platform"><FlaskConical size={22} /><span>Agent Test Platform</span></Link>
        <nav aria-label="主导航">
          {nav.map((group) => (
            <div key={group.group}>
              <div className="nav-group-title">{group.group}</div>
              {group.items.map((item) => (
                <NavLink key={item.to} to={item.to} end={item.end} title={item.label} className={({ isActive }) => `nav-link${isActive ? ' is-active' : ''}`}>
                  <item.icon size={17} /><span>{item.label}</span>
                </NavLink>
              ))}
            </div>
          ))}
        </nav>
      </aside>
      <div className="admin-main">
        <header className="header">
          <div className="header-left"><span className="breadcrumb">Agent Test Platform / <strong>{titles[section] ?? ''}</strong></span></div>
          <div className="header-right">
            <span className={`health-dot${health.data ? ' is-up' : ''}`}>{health.isPending ? '检测中' : health.data ? 'API 在线' : 'API 离线'}</span>
            <input className="token-input" type="password" aria-label="Platform API token" placeholder="Platform API token" value={token}
              onChange={(event) => setToken(event.target.value)} maxLength={512} autoComplete="off" spellCheck={false} />
          </div>
        </header>
        <main className="content">
          <Routes>
            <Route path="/" element={<Dashboard token={token} />} />
            <Route path="/jobs/:jobId" element={<JobPage token={token} />} />
            <Route path="/scenarios" element={<AssetPage token={token} kind="scenarios" />} />
            <Route path="/datasets" element={<AssetPage token={token} kind="datasets" />} />
            <Route path="/test-cases" element={<AssetPage token={token} kind="test-cases" />} />
            <Route path="/runs" element={<RunsPage token={token} />} />
            <Route path="/runs/:runId" element={<RunDetailPage token={token} />} />
            <Route path="/benchmark" element={<BenchmarkPage token={token} />} />
            <Route path="/agents" element={<AgentsPage token={token} />} />
            <Route path="/validation-db" element={<ValidationDbPage token={token} />} />
            <Route path="/models" element={<ModelsPage token={token} />} />
            <Route path="/secrets" element={<SecretsPage token={token} />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </main>
      </div>
    </div>
  )
}
