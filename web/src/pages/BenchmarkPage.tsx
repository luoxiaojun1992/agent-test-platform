import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { getBenchmark, type RevisionItem } from '../api2'
import { Alert, Card, DataTable, Mono, Notice, PageHeader, Score } from '../admin'
import { RevisionSelect } from '../revision'

export function BenchmarkPage({ token }: { token: string }) {
  const [cases, setCases] = useState<RevisionItem[]>([])
  const ids = cases.map((item) => item.id)
  const benchmark = useQuery({ queryKey: ['benchmark', token, ids], enabled: token !== '' && ids.length > 0, queryFn: () => getBenchmark(token, ids) })
  return (
    <>
      <PageHeader title="基准对比" description="针对所选测试用例，对比各 Agent 版本的通过率与得分。仅统计已完成且真正触达 Agent 的运行。" />
      {token === '' && <Notice>请先输入 Platform API token。</Notice>}
      <Alert error={benchmark.error} />
      <Card>
        <RevisionSelect token={token} kind="test-cases" label="测试用例" multiple onChange={setCases} />
      </Card>
      <Card flush>
        <DataTable label="基准结果" rows={benchmark.data?.items ?? []} rowKey={(entry) => entry.agent_id} loading={benchmark.isPending && ids.length > 0}
          empty={ids.length > 0 ? '所选用例还没有已完成的运行' : '请先选择测试用例'}
          columns={[
            { key: 'agent', title: 'Agent', render: (entry) => <Mono>{entry.agent_id}</Mono> },
            { key: 'runs', title: '有效运行数', render: (entry) => entry.run_count },
            { key: 'rate', title: '通过率', render: (entry) => `${entry.pass_rate.toFixed(1)}%` },
            { key: 'score', title: '平均得分', render: (entry) => <Score value={entry.avg_score} /> },
          ]} />
      </Card>
    </>
  )
}
