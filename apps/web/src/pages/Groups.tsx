import { useState } from 'react'
import { FolderTree, Pencil, Plus, Trash2 } from 'lucide-react'
import { ApiError, del, patch, post } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtNumber } from '../format'
import { Button, Card, ConfirmDialog, Dialog, Empty, ErrorBanner, Field, Input, PageHeader, Spinner, Table, Textarea, td, th } from '../components/ui'
import type { Group } from '../types'

export function GroupsPage() {
  const { t, loc } = usePrefs()
  const { can } = useSession()
  const { data, error, loading, reload } = useApi<{ groups: Group[] }>('/monitor-groups')
  const [edit, setEdit] = useState<Partial<Group> | null>(null)
  const [delTarget, setDelTarget] = useState<Group | null>(null)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const manage = can('monitor.manage')
  const save = async () => {
    if (!edit) return
    setBusy(true); setFields({}); setErr(null)
    try {
      const body = { name: edit.name ?? '', description: edit.description ?? '' }
      if (edit.id) await patch(`/monitor-groups/${edit.id}`, body)
      else await post('/monitor-groups', body)
      setEdit(null); await reload()
    } catch (e) { const ae = e as ApiError; setFields(ae.fields); if (!Object.keys(ae.fields).length) setErr(ae.message) } finally { setBusy(false) }
  }
  const remove = async () => {
    if (!delTarget) return
    setBusy(true)
    try { await del(`/monitor-groups/${delTarget.id}`); setDelTarget(null); await reload() } catch (e) { setErr((e as Error).message) } finally { setBusy(false) }
  }
  return (
    <>
      <PageHeader title={t('groups.title')} actions={manage && <Button variant="primary" onClick={() => setEdit({})}><Plus className="size-4" />{t('groups.new')}</Button>} />
      {error && <ErrorBanner error={error} onRetry={reload} />}
      {err && !edit && <div className="mb-3"><ErrorBanner error={err} /></div>}
      <Card bodyClass="p-0">
        {loading && !data ? <Spinner /> : !data?.groups.length ? <Empty icon={<FolderTree className="size-8" />}>{t('groups.empty')}</Empty> : (
          <Table>
            <thead><tr><th className={th}>{t('common.name')}</th><th className={th}>{t('common.description')}</th><th className={th}>{t('groups.monitors')}</th>{manage && <th className={th}><span className="sr-only">{t('common.actions')}</span></th>}</tr></thead>
            <tbody>
              {data.groups.map((g) => (
                <tr key={g.id}>
                  <td className={`${td} font-medium`}>{g.name}</td>
                  <td className={`${td} text-muted`}>{g.description || '—'}</td>
                  <td className={`${td} tabular`}>{fmtNumber(loc, g.monitor_count ?? 0)}</td>
                  {manage && <td className={`${td} text-end`}>
                    <Button size="sm" variant="ghost" onClick={() => setEdit(g)} aria-label={t('common.edit')}><Pencil className="size-4" /></Button>
                    <Button size="sm" variant="ghost" onClick={() => setDelTarget(g)} aria-label={t('common.delete')}><Trash2 className="size-4" /></Button>
                  </td>}
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
      <Dialog open={!!edit} onClose={() => setEdit(null)} title={edit?.id ? t('common.edit') : t('groups.new')}
        footer={<><Button onClick={() => setEdit(null)}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} onClick={save}>{t('common.save')}</Button></>}>
        <div className="flex flex-col gap-4">
          {err && <ErrorBanner error={err} />}
          <Field label={t('common.name')} htmlFor="gname" error={fields.name}><Input id="gname" value={edit?.name ?? ''} onChange={(e) => setEdit({ ...edit, name: e.target.value })} invalid={!!fields.name} /></Field>
          <Field label={t('common.description')} htmlFor="gdesc" optional error={fields.description}><Textarea id="gdesc" value={edit?.description ?? ''} onChange={(e) => setEdit({ ...edit, description: e.target.value })} /></Field>
        </div>
      </Dialog>
      <ConfirmDialog open={!!delTarget} onClose={() => setDelTarget(null)} onConfirm={remove} busy={busy} title={t('common.delete')} confirmLabel={t('common.delete')}
        message={t('groups.deleteConfirm', { name: delTarget?.name ?? '' })} />
    </>
  )
}
