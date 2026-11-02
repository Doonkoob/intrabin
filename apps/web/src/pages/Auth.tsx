import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import { ApiError, get, post } from '../api'
import { usePrefs, useSession } from '../app'
import { Button, ErrorBanner, Field, Input, Select } from '../components/ui'
import { Logo, PrefControls } from '../components/Layout'

function Shell({ title, subtitle, children }: { title: string; subtitle?: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-screen flex-col">
      <div className="flex justify-end p-3"><PrefControls /></div>
      <div className="flex flex-1 items-start justify-center px-4 pb-16 pt-6 sm:items-center">
        <div className="w-full max-w-md">
          <div className="mb-6 flex items-center gap-2"><Logo /><span className="font-semibold">Intrabin</span></div>
          <div className="rounded-2xl border border-line bg-surface p-6 shadow-sm">
            <h1 className="text-lg font-semibold">{title}</h1>
            {subtitle && <p className="mt-1 text-sm text-muted">{subtitle}</p>}
            <div className="mt-5">{children}</div>
          </div>
        </div>
      </div>
    </div>
  )
}

export function LoginPage({ instanceName }: { instanceName: string }) {
  const { t } = usePrefs()
  const { refresh } = useSession()
  const navigate = useNavigate()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await post('/auth/login', { username, password })
      await refresh()
      navigate('/')
    } catch (err) {
      const ae = err as ApiError
      setError(ae.code === 'invalid_credentials' ? t('login.failed') : ae.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Shell title={t('login.title')} subtitle={t('login.subtitle', { name: instanceName || 'Intrabin' })}>
      <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
        {error && <ErrorBanner error={error} />}
        <Field label={t('login.username')} htmlFor="username">
          <Input id="username" name="username" autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required autoFocus />
        </Field>
        <Field label={t('login.password')} htmlFor="password">
          <Input id="password" name="password" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
        </Field>
        <Button type="submit" variant="primary" loading={busy} className="w-full">{t('login.submit')}</Button>
      </form>
    </Shell>
  )
}

export function SetupPage({ onDone }: { onDone: () => void }) {
  const { t, lang, setLang } = usePrefs()
  const { refresh } = useSession()
  const navigate = useNavigate()
  const [form, setForm] = useState({ setup_token: '', instance_name: 'Intrabin', username: '', display_name: '', password: '' })
  const [fields, setFields] = useState<Record<string, string>>({})
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value })
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    setFields({})
    try {
      await post('/setup', { ...form, language: lang })
      await refresh()
      onDone()
      navigate('/')
    } catch (err) {
      const ae = err as ApiError
      setFields(ae.fields)
      if (!Object.keys(ae.fields).length) setError(ae.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Shell title={t('setup.title')} subtitle={t('setup.subtitle')}>
      <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
        {error && <ErrorBanner error={error} />}
        <Field label={t('setup.token')} htmlFor="setup_token" help={t('setup.tokenHelp')} error={fields.setup_token}>
          <Input id="setup_token" className="ltr font-mono" value={form.setup_token} onChange={set('setup_token')} invalid={!!fields.setup_token} autoComplete="off" required />
        </Field>
        <Field label={t('setup.instance')} htmlFor="instance_name" error={fields.instance_name}>
          <Input id="instance_name" value={form.instance_name} onChange={set('instance_name')} />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t('field.username')} htmlFor="username" error={fields.username}>
            <Input id="username" className="ltr" value={form.username} onChange={set('username')} invalid={!!fields.username} autoComplete="username" required />
          </Field>
          <Field label={t('field.displayName')} htmlFor="display_name" optional>
            <Input id="display_name" value={form.display_name} onChange={set('display_name')} />
          </Field>
        </div>
        <Field label={t('field.password')} htmlFor="password" help={t('field.passwordHelp')} error={fields.password}>
          <Input id="password" type="password" value={form.password} onChange={set('password')} invalid={!!fields.password} autoComplete="new-password" required />
        </Field>
        <Field label={t('field.language')} htmlFor="language">
          <Select id="language" value={lang} onChange={(e) => setLang(e.target.value as 'en' | 'fa')}>
            <option value="en">English</option>
            <option value="fa">فارسی</option>
          </Select>
        </Field>
        <p className="rounded-lg bg-surface-2 p-3 text-xs text-muted">{t('setup.mode')}</p>
        <Button type="submit" variant="primary" loading={busy} className="w-full">{t('setup.submit')}</Button>
      </form>
    </Shell>
  )
}

export function useSetupStatus() {
  const [state, setState] = useState<{ needs_setup: boolean; instance_name: string; default_language: string } | null>(null)
  const [error, setError] = useState<Error | null>(null)
  const load = async () => {
    try {
      setState(await get('/setup/status'))
      setError(null)
    } catch (e) {
      setError(e as Error)
    }
  }
  useEffect(() => { void load() }, [])
  return { state, error, reload: load }
}
