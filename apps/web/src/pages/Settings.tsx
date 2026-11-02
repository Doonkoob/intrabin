import { useEffect, useState } from 'react'
import { ApiError, patch, post } from '../api'
import { usePrefs, useSession, type Theme } from '../app'
import { useApi } from '../hooks'
import { Button, Card, ErrorBanner, Field, Input, PageHeader, Segmented, Select } from '../components/ui'
import type { Settings } from '../types'
import type { Lang } from '../i18n'

function Saved({ show }: { show: boolean }) {
  const { t } = usePrefs()
  return show ? <span role="status" className="text-sm text-up">{t('common.saved')}</span> : null
}

export function SettingsPage() {
  const { t, lang, setLang, theme, setTheme } = usePrefs()
  const { me, can, refresh } = useSession()
  const settings = useApi<{ settings: Settings }>('/settings')
  const [inst, setInst] = useState<Settings | null>(null)
  const [instState, setInstState] = useState<{ busy: boolean; saved: boolean; err?: string; fields: Record<string, string> }>({ busy: false, saved: false, fields: {} })
  const [displayName, setDisplayName] = useState(me?.user.display_name ?? '')
  const [profState, setProfState] = useState({ busy: false, saved: false })
  const [pw, setPw] = useState({ current_password: '', new_password: '' })
  const [pwState, setPwState] = useState<{ busy: boolean; saved: boolean; fields: Record<string, string>; err?: string }>({ busy: false, saved: false, fields: {} })
  useEffect(() => { if (settings.data) setInst(settings.data.settings) }, [settings.data])

  const saveInstance = async () => {
    if (!inst) return
    setInstState({ busy: true, saved: false, fields: {} })
    try {
      const r = await patch<{ settings: Settings }>('/settings', inst)
      setInst(r.settings)
      setInstState({ busy: false, saved: true, fields: {} })
    } catch (e) { const ae = e as ApiError; setInstState({ busy: false, saved: false, fields: ae.fields, err: Object.keys(ae.fields).length ? undefined : ae.message }) }
  }
  const saveProfile = async (language?: Lang) => {
    setProfState({ busy: true, saved: false })
    try {
      await patch('/auth/profile', { display_name: displayName, ...(language ? { language } : {}) })
      await refresh()
      setProfState({ busy: false, saved: true })
    } catch { setProfState({ busy: false, saved: false }) }
  }
  const changePassword = async () => {
    setPwState({ busy: true, saved: false, fields: {} })
    try {
      await post('/auth/password', pw)
      setPw({ current_password: '', new_password: '' })
      setPwState({ busy: false, saved: true, fields: {} })
    } catch (e) { const ae = e as ApiError; setPwState({ busy: false, saved: false, fields: ae.fields, err: Object.keys(ae.fields).length ? undefined : ae.message }) }
  }
  return (
    <>
      <PageHeader title={t('settings.title')} />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={t('settings.appearance')}>
          <div className="flex flex-col gap-4">
            <Field label={t('lang.label')}>
              <Segmented label={t('lang.label')} value={lang} onChange={(l: Lang) => { setLang(l); void saveProfile(l) }} options={[{ value: 'en', label: 'English' }, { value: 'fa', label: 'فارسی' }]} />
            </Field>
            <Field label={t('theme.label')}>
              <Segmented label={t('theme.label')} value={theme} onChange={(v: Theme) => setTheme(v)} options={(['light', 'dark', 'system'] as Theme[]).map((v) => ({ value: v, label: t(`theme.${v}`) }))} />
            </Field>
          </div>
        </Card>
        <Card title={t('settings.profile')}>
          <div className="flex flex-col gap-4">
            <Field label={t('field.username')}><Input value={me?.user.username ?? ''} disabled className="ltr" /></Field>
            <Field label={t('field.displayName')} htmlFor="dn"><Input id="dn" value={displayName} onChange={(e) => setDisplayName(e.target.value)} /></Field>
            <div className="flex items-center gap-3"><Button variant="primary" loading={profState.busy} onClick={() => saveProfile()}>{t('common.save')}</Button><Saved show={profState.saved} /></div>
          </div>
        </Card>
        <Card title={t('settings.password')}>
          <div className="flex flex-col gap-4">
            {pwState.err && <ErrorBanner error={pwState.err} />}
            <Field label={t('settings.currentPassword')} htmlFor="cp" error={pwState.fields.current_password}><Input id="cp" type="password" autoComplete="current-password" value={pw.current_password} onChange={(e) => setPw({ ...pw, current_password: e.target.value })} invalid={!!pwState.fields.current_password} /></Field>
            <Field label={t('settings.newPassword')} htmlFor="npw" help={t('field.passwordHelp')} error={pwState.fields.new_password}><Input id="npw" type="password" autoComplete="new-password" value={pw.new_password} onChange={(e) => setPw({ ...pw, new_password: e.target.value })} invalid={!!pwState.fields.new_password} /></Field>
            <div className="flex items-center gap-3"><Button variant="primary" loading={pwState.busy} onClick={changePassword} disabled={!pw.current_password || !pw.new_password}>{t('settings.password')}</Button>{pwState.saved && <span role="status" className="text-sm text-up">{t('settings.passwordChanged')}</span>}</div>
          </div>
        </Card>
        {inst && (
          <Card title={t('settings.instance')}>
            <div className="flex flex-col gap-4">
              {instState.err && <ErrorBanner error={instState.err} />}
              <Field label={t('settings.instanceName')} htmlFor="in" error={instState.fields.instance_name}><Input id="in" disabled={!can('settings.manage')} value={inst.instance_name} onChange={(e) => setInst({ ...inst, instance_name: e.target.value })} /></Field>
              <Field label={t('settings.defaultLanguage')} htmlFor="dl"><Select id="dl" disabled={!can('settings.manage')} value={inst.default_language} onChange={(e) => setInst({ ...inst, default_language: e.target.value as Lang })}><option value="en">English</option><option value="fa">فارسی</option></Select></Field>
              <h3 className="mt-2 text-sm font-semibold">{t('settings.retention')}</h3>
              <p className="-mt-2 text-xs text-muted">{t('settings.retentionHelp')}</p>
              <div className="grid gap-4 sm:grid-cols-3">
                <Field label={t('settings.rawDays')} htmlFor="rr" error={instState.fields.raw_retention_days}><Input id="rr" type="number" disabled={!can('settings.manage')} value={inst.raw_retention_days} onChange={(e) => setInst({ ...inst, raw_retention_days: Number(e.target.value) })} /></Field>
                <Field label={t('settings.roundDays')} htmlFor="rd" error={instState.fields.round_retention_days}><Input id="rd" type="number" disabled={!can('settings.manage')} value={inst.round_retention_days} onChange={(e) => setInst({ ...inst, round_retention_days: Number(e.target.value) })} /></Field>
                <Field label={t('settings.hourlyDays')} htmlFor="hd" error={instState.fields.hourly_retention_days}><Input id="hd" type="number" disabled={!can('settings.manage')} value={inst.hourly_retention_days} onChange={(e) => setInst({ ...inst, hourly_retention_days: Number(e.target.value) })} /></Field>
              </div>
              {can('settings.manage') && <div className="flex items-center gap-3"><Button variant="primary" loading={instState.busy} onClick={saveInstance}>{t('common.save')}</Button><Saved show={instState.saved} /></div>}
            </div>
          </Card>
        )}
        <Card title={t('settings.about')}>
          <dl className="grid grid-cols-2 gap-y-2 text-sm">
            <dt className="text-muted">{t('cluster.version')}</dt><dd className="ltr text-start">{me?.node.version}</dd>
            <dt className="text-muted">Commit</dt><dd className="ltr text-start font-mono text-xs">{me?.node.commit}</dd>
            <dt className="text-muted">API</dt><dd><a className="text-accent hover:underline" href="/api/v1/openapi.yaml">OpenAPI</a></dd>
          </dl>
        </Card>
      </div>
    </>
  )
}
