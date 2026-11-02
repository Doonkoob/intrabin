import { useState } from 'react'
import { KeyRound, Plus, Trash2, UserCheck, UserX } from 'lucide-react'
import { ApiError, del, patch, post } from '../api'
import { usePrefs, useSession } from '../app'
import { useApi } from '../hooks'
import { fmtRelative } from '../format'
import { Button, Card, ConfirmDialog, Dialog, ErrorBanner, Field, Input, PageHeader, Select, Spinner, Table, td, th } from '../components/ui'
import type { Role, User } from '../types'
import type { Key } from '../i18n'

const roles: Role[] = ['owner', 'admin', 'operator', 'viewer']

export function UsersPage() {
  const { t, loc } = usePrefs()
  const { me } = useSession()
  const { data, error, loading, reload } = useApi<{ users: User[] }>('/users')
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState({ username: '', display_name: '', password: '', role: 'viewer' as Role })
  const [pwUser, setPwUser] = useState<User | null>(null)
  const [newPw, setNewPw] = useState('')
  const [delUser, setDelUser] = useState<User | null>(null)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const assignable = roles.filter((r) => me?.user.role === 'owner' || r !== 'owner')
  const run = async (fn: () => Promise<unknown>, after?: () => void) => {
    setBusy(true); setErr(null); setFields({})
    try { await fn(); after?.(); await reload() } catch (e) { const ae = e as ApiError; setFields(ae.fields); if (!Object.keys(ae.fields).length) setErr(ae.message) } finally { setBusy(false) }
  }
  return (
    <>
      <PageHeader title={t('users.title')} actions={<Button variant="primary" onClick={() => { setForm({ username: '', display_name: '', password: '', role: 'viewer' }); setCreating(true) }}><Plus className="size-4" />{t('users.new')}</Button>} />
      {(error || (err && !creating && !pwUser)) && <div className="mb-3"><ErrorBanner error={error ?? err!} /></div>}
      <Card bodyClass="p-0">
        {loading && !data ? <Spinner /> : (
          <Table>
            <thead><tr><th className={th}>{t('field.username')}</th><th className={th}>{t('field.role')}</th><th className={th}>{t('common.status')}</th><th className={`${th} hidden md:table-cell`}>{t('users.lastLogin')}</th><th className={th}><span className="sr-only">{t('common.actions')}</span></th></tr></thead>
            <tbody>
              {data?.users.map((u) => {
                const self = u.id === me?.user.id
                const locked = (u.role === 'owner' && me?.user.role !== 'owner')
                return (
                  <tr key={u.id}>
                    <td className={td}><div className="font-medium">{u.display_name || u.username}{self && <span className="ms-2 text-xs text-muted">({t('users.you')})</span>}</div><div className="ltr text-start text-xs text-muted">{u.username}</div></td>
                    <td className={td}>
                      <Select className="w-auto" aria-label={t('field.role')} value={u.role} disabled={self || locked || busy}
                        onChange={(e) => run(() => patch(`/users/${u.id}`, { role: e.target.value }))}>
                        {(locked ? roles : assignable).map((r) => <option key={r} value={r}>{t(`role.${r}` as Key)}</option>)}
                      </Select>
                    </td>
                    <td className={td}>{u.disabled ? <span className="text-down">{t('users.disabled')}</span> : <span className="text-up">{t('users.active')}</span>}</td>
                    <td className={`${td} hidden text-muted md:table-cell`}>{u.last_login_at ? fmtRelative(loc, u.last_login_at) : t('common.never')}</td>
                    <td className={`${td} whitespace-nowrap text-end`}>
                      {!self && !locked && <>
                        <Button size="sm" variant="ghost" title={u.disabled ? t('users.enable') : t('users.disable')} aria-label={u.disabled ? t('users.enable') : t('users.disable')}
                          onClick={() => run(() => patch(`/users/${u.id}`, { disabled: !u.disabled }))}>{u.disabled ? <UserCheck className="size-4" /> : <UserX className="size-4" />}</Button>
                        <Button size="sm" variant="ghost" title={t('users.resetPassword')} aria-label={t('users.resetPassword')} onClick={() => { setNewPw(''); setPwUser(u) }}><KeyRound className="size-4" /></Button>
                        <Button size="sm" variant="ghost" title={t('common.delete')} aria-label={t('common.delete')} onClick={() => setDelUser(u)}><Trash2 className="size-4" /></Button>
                      </>}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </Table>
        )}
      </Card>
      <Card title={t('field.role')} className="mt-4">
        <ul className="grid gap-2 text-sm sm:grid-cols-2">
          {roles.map((r) => <li key={r}><span className="font-medium">{t(`role.${r}` as Key)}</span> — <span className="text-muted">{t(`role.${r}.desc` as Key)}</span></li>)}
        </ul>
      </Card>
      <Dialog open={creating} onClose={() => setCreating(false)} title={t('users.new')}
        footer={<><Button onClick={() => setCreating(false)}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} onClick={() => run(() => post('/users', form), () => setCreating(false))}>{t('common.create')}</Button></>}>
        <div className="flex flex-col gap-4">
          {err && <ErrorBanner error={err} />}
          <Field label={t('field.username')} htmlFor="nu" error={fields.username}><Input id="nu" className="ltr" autoComplete="off" value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} invalid={!!fields.username} /></Field>
          <Field label={t('field.displayName')} htmlFor="nd" optional error={fields.display_name}><Input id="nd" value={form.display_name} onChange={(e) => setForm({ ...form, display_name: e.target.value })} /></Field>
          <Field label={t('field.password')} htmlFor="np" help={t('field.passwordHelp')} error={fields.password}><Input id="np" type="password" autoComplete="new-password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} invalid={!!fields.password} /></Field>
          <Field label={t('field.role')} htmlFor="nr" error={fields.role}>
            <Select id="nr" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value as Role })}>{assignable.map((r) => <option key={r} value={r}>{t(`role.${r}` as Key)}</option>)}</Select>
          </Field>
        </div>
      </Dialog>
      <Dialog open={!!pwUser} onClose={() => setPwUser(null)} title={`${t('users.resetPassword')}: ${pwUser?.username ?? ''}`}
        footer={<><Button onClick={() => setPwUser(null)}>{t('common.cancel')}</Button><Button variant="primary" loading={busy} onClick={() => run(() => post(`/users/${pwUser!.id}/password`, { new_password: newPw }), () => setPwUser(null))}>{t('common.save')}</Button></>}>
        {err && <div className="mb-3"><ErrorBanner error={err} /></div>}
        <Field label={t('users.newPassword')} htmlFor="rp" help={t('field.passwordHelp')} error={fields.new_password}><Input id="rp" type="password" autoComplete="new-password" value={newPw} onChange={(e) => setNewPw(e.target.value)} invalid={!!fields.new_password} /></Field>
      </Dialog>
      <ConfirmDialog open={!!delUser} onClose={() => setDelUser(null)} busy={busy} title={t('common.delete')} confirmLabel={t('common.delete')}
        message={t('users.deleteConfirm', { name: delUser?.username ?? '' })} onConfirm={() => run(() => del(`/users/${delUser!.id}`), () => setDelUser(null))} />
    </>
  )
}
