import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { activateModel, listModels, type ModelEntry } from '../api2'
import { Alert, Badge, Card, DataTable, Mono, Notice, PageHeader } from '../admin'

const roleText = { generation: '生成模型', judge: 'Judge 评判模型' } as const

export function ModelsPage({ token }: { token: string }) {
  const client = useQueryClient()
  const models = useQuery({ queryKey: ['models', token], enabled: token !== '', queryFn: () => listModels(token) })
  const activate = useMutation({
    mutationFn: (model: ModelEntry) => activateModel(token, model.role, model.alias),
    onSuccess: () => client.invalidateQueries({ queryKey: ['models'] }),
  })
  const rows = models.data?.items ?? []
  return (
    <>
      <PageHeader title="模型配置" description="生成模型与 Judge 模型各自只能启用一个。切换后，新的生成任务和新的评测使用新启用的模型（缓存约 5 秒）。" />
      {token === '' && <Notice>请先输入 Platform API token。</Notice>}
      <Alert error={models.error ?? activate.error} />
      <Card flush>
        <DataTable label="模型列表" rows={rows} rowKey={(model) => `${model.role}/${model.alias}`} loading={models.isPending && token !== ''}
          empty="尚未注册任何模型。worker 启动时会按环境变量自动注册。"
          columns={[
            { key: 'role', title: '角色', render: (model) => roleText[model.role] },
            { key: 'alias', title: '别名', render: (model) => <Mono>{model.alias}</Mono> },
            { key: 'provider', title: '提供方', render: (model) => model.provider },
            { key: 'model', title: '模型名', render: (model) => <Mono>{model.model}</Mono> },
            { key: 'active', title: '状态', render: (model) => (model.active ? <Badge value="ACTIVE" /> : <Badge value="未启用" tone="muted" />) },
            { key: 'op', title: '操作', align: 'right', render: (model) => (
              <button type="button" className="btn" disabled={model.active || activate.isPending} onClick={() => activate.mutate(model)} aria-label={`启用 ${model.alias}`}>启用</button>
            ) },
          ]} />
      </Card>
    </>
  )
}
