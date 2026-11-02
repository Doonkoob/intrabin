import { Boxes, Server } from 'lucide-react'
import { usePrefs } from '../app'
import { useApi } from '../hooks'
import { fmtBytes, fmtDuration, fmtNumber } from '../format'
import { Card, ErrorBanner, PageHeader, Spinner, Table, td, th } from '../components/ui'
import type { NodeInfo } from '../types'

interface Cluster { mode: string; leader_id: string; quorum: string; members: { node_id: string; node_name: string; role: string; voter: boolean; version: string; status: string; self: boolean }[] }

function NodeCard({ node }: { node: NodeInfo }) {
  const { t, loc } = usePrefs()
  return (
    <dl className="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-[auto_1fr]">
      <dt className="text-muted">{t('cluster.nodeName')}</dt><dd className="font-medium">{node.node_name}</dd>
      <dt className="text-muted">{t('cluster.nodeId')}</dt><dd className="ltr break-all text-start font-mono text-xs">{node.node_id}</dd>
      <dt className="text-muted">{t('cluster.role')}</dt><dd>{node.role}</dd>
      <dt className="text-muted">{t('cluster.version')}</dt><dd className="ltr text-start">{node.version} ({node.commit.slice(0, 7)})</dd>
      <dt className="text-muted">{t('cluster.uptime')}</dt><dd>{fmtDuration(loc, node.uptime_seconds ?? 0)}</dd>
      <dt className="text-muted">{t('cluster.assigned')}</dt><dd className="tabular">{fmtNumber(loc, node.assigned_monitors ?? 0)}</dd>
      <dt className="text-muted">{t('cluster.db')}</dt><dd className="tabular">{fmtBytes(loc, node.database_bytes)}</dd>
    </dl>
  )
}

export function ClusterPage() {
  const { t } = usePrefs()
  const cluster = useApi<Cluster>('/cluster', 15000)
  const node = useApi<{ node: NodeInfo }>('/node', 15000)
  if ((cluster.loading && !cluster.data) || (node.loading && !node.data)) return <Spinner />
  if (cluster.error || node.error) return <ErrorBanner error={(cluster.error ?? node.error)!} onRetry={() => { cluster.reload(); node.reload() }} />
  return (
    <>
      <PageHeader title={t('cluster.title')} />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={<span className="inline-flex items-center gap-2"><Boxes className="size-4" />{t('cluster.mode')}: {t('cluster.standalone')}</span>}>
          <p className="text-sm text-muted">{t('cluster.standaloneHelp')}</p>
          <dl className="mt-4 grid grid-cols-2 gap-y-2 text-sm">
            <dt className="text-muted">{t('cluster.processHealthy')}</dt><dd className="text-up">✓</dd>
            <dt className="text-muted">{t('cluster.controlPlane')}</dt><dd className="text-up">{t('cluster.available')}</dd>
            <dt className="text-muted">{t('overview.quorum')}</dt><dd>{t('overview.quorumNA')}</dd>
          </dl>
        </Card>
        <Card title={<span className="inline-flex items-center gap-2"><Server className="size-4" />{t('overview.node')}</span>}><NodeCard node={node.data!.node} /></Card>
        <Card title={t('cluster.members')} className="lg:col-span-2" bodyClass="p-0">
          <Table>
            <thead><tr><th className={th}>{t('cluster.nodeName')}</th><th className={th}>{t('cluster.role')}</th><th className={th}>{t('cluster.voter')}</th><th className={th}>{t('common.status')}</th><th className={th}>{t('cluster.version')}</th></tr></thead>
            <tbody>
              {cluster.data!.members.map((m) => (
                <tr key={m.node_id}>
                  <td className={`${td} font-medium`}>{m.node_name}{m.self && <span className="ms-2 text-xs text-muted">({t('users.you')})</span>}</td>
                  <td className={td}>{m.role}</td>
                  <td className={td}>{m.voter ? t('common.yes') : t('common.no')}</td>
                  <td className={td}><span className="text-up">{m.status}</span></td>
                  <td className={`${td} ltr text-start`}>{m.version}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        </Card>
      </div>
    </>
  )
}

/** Restricted UI shown on nodes that are cluster members but not the active controller. */
export function FollowerStatusPage() {
  const { t } = usePrefs()
  const node = useApi<{ node: NodeInfo }>('/node', 10000)
  return (
    <>
      <PageHeader title={t('follower.title')} subtitle={t('follower.help')} />
      {node.error && <ErrorBanner error={node.error} onRetry={node.reload} />}
      {node.data ? <Card><NodeCard node={node.data.node} /></Card> : <Spinner />}
    </>
  )
}
