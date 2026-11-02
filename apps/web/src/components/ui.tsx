import { forwardRef, useEffect, useId, useRef, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes, type TextareaHTMLAttributes } from 'react'
import { AlertTriangle, Loader2, X } from 'lucide-react'
import { useT } from '../app'
import type { IncidentStatus, MonitorStatus, Verdict } from '../types'
import type { Key } from '../i18n'

type Variant = 'primary' | 'secondary' | 'danger' | 'ghost'

const variants: Record<Variant, string> = {
  primary: 'bg-accent text-accent-fg hover:opacity-90 border border-transparent',
  secondary: 'bg-surface text-fg border border-line hover:bg-surface-2',
  danger: 'bg-down text-white hover:opacity-90 border border-transparent dark:text-black',
  ghost: 'bg-transparent text-fg hover:bg-surface-2 border border-transparent',
}

export const Button = forwardRef<HTMLButtonElement, ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; loading?: boolean; size?: 'sm' | 'md' }>(
  function Button({ variant = 'secondary', loading, size = 'md', className = '', children, disabled, ...rest }, ref) {
    const sz = size === 'sm' ? 'h-8 px-2.5 text-sm' : 'h-9 px-3.5 text-sm'
    return (
      <button ref={ref} type="button" {...rest} disabled={disabled || loading}
        className={`inline-flex items-center justify-center gap-1.5 rounded-lg font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed ${sz} ${variants[variant]} ${className}`}>
        {loading && <Loader2 className="size-4 animate-spin" aria-hidden />}
        {children}
      </button>
    )
  })

export function Card({ title, actions, children, className = '', bodyClass = 'p-4' }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string; bodyClass?: string }) {
  return (
    <section className={`rounded-xl border border-line bg-surface ${className}`}>
      {(title || actions) && (
        <header className="flex items-center justify-between gap-2 border-b border-line px-4 py-3">
          {title && <h2 className="text-sm font-semibold">{title}</h2>}
          {actions && <div className="flex items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={bodyClass}>{children}</div>
    </section>
  )
}

export function Field({ label, help, error, children, htmlFor, optional }: { label: ReactNode; help?: ReactNode; error?: string; children: ReactNode; htmlFor?: string; optional?: boolean }) {
  const t = useT()
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={htmlFor} className="text-sm font-medium">
        {label}
        {optional && <span className="ms-1 text-xs font-normal text-muted">({t('common.optional')})</span>}
      </label>
      {children}
      {help && !error && <p className="text-xs text-muted">{help}</p>}
      {error && <p className="text-xs text-down" role="alert">{error}</p>}
    </div>
  )
}

const inputCls = 'h-9 w-full rounded-lg border border-line bg-surface px-3 text-sm text-fg placeholder:text-muted/70 focus:border-focus aria-[invalid=true]:border-down'

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement> & { invalid?: boolean }>(function Input({ className = '', invalid, ...rest }, ref) {
  return <input ref={ref} aria-invalid={invalid || undefined} className={`${inputCls} ${className}`} {...rest} />
})

export function Textarea({ className = '', invalid, ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement> & { invalid?: boolean }) {
  return <textarea aria-invalid={invalid || undefined} className={`${inputCls} h-auto min-h-20 py-2 ${className}`} {...rest} />
}

export function Select({ className = '', children, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={`${inputCls} pe-8 ${className}`} {...rest}>{children}</select>
}

export function Checkbox({ label, checked, onChange, id }: { label: ReactNode; checked: boolean; onChange: (v: boolean) => void; id?: string }) {
  const auto = useId()
  const cid = id ?? auto
  return (
    <label htmlFor={cid} className="inline-flex cursor-pointer items-center gap-2 text-sm">
      <input id={cid} type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} className="size-4 accent-[var(--accent)]" />
      {label}
    </label>
  )
}

const statusStyle: Record<string, string> = {
  UP: 'bg-up-soft text-up',
  DOWN: 'bg-down-soft text-down',
  DEGRADED: 'bg-warn-soft text-warn',
  SUSPECTED_DOWN: 'bg-warn-soft text-warn',
  UNKNOWN: 'bg-neutral-soft text-muted',
  PAUSED: 'bg-neutral-soft text-muted',
  MAINTENANCE: 'bg-info-soft text-info',
}

const dotStyle: Record<string, string> = {
  UP: 'bg-up', DOWN: 'bg-down', DEGRADED: 'bg-warn', SUSPECTED_DOWN: 'bg-warn', UNKNOWN: 'bg-muted', PAUSED: 'bg-muted', MAINTENANCE: 'bg-info',
}

export function StatusDot({ status }: { status: string }) {
  return <span aria-hidden className={`inline-block size-2.5 shrink-0 rounded-full ${dotStyle[status] ?? 'bg-muted'} ${status === 'DOWN' ? 'animate-pulse' : ''}`} />
}

export function StatusBadge({ status }: { status: MonitorStatus | undefined }) {
  const t = useT()
  const s = status ?? 'UNKNOWN'
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium ${statusStyle[s]}`} data-status={s}>
      <StatusDot status={s} />
      {t(`status.${s}` as Key)}
    </span>
  )
}

const incStyle: Record<string, string> = {
  SUSPECTED: 'bg-warn-soft text-warn',
  CONFIRMED: 'bg-down-soft text-down',
  ACKNOWLEDGED: 'bg-info-soft text-info',
  RESOLVED: 'bg-up-soft text-up',
  MANUAL_INTERVENTION: 'bg-down-soft text-down',
}

export function IncidentBadge({ status }: { status: IncidentStatus }) {
  const t = useT()
  return <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${incStyle[status] ?? 'bg-neutral-soft text-muted'}`}>{t(`incident.${status}` as Key)}</span>
}

export const verdictColor: Record<Verdict, string> = {
  ok: 'bg-up',
  partial: 'bg-warn',
  failed: 'bg-down',
  inconclusive: 'bg-line',
  maintenance: 'bg-info',
}

export function Spinner({ label }: { label?: string }) {
  const t = useT()
  return (
    <div className="flex items-center justify-center gap-2 p-8 text-sm text-muted" role="status">
      <Loader2 className="size-4 animate-spin" aria-hidden />
      {label ?? t('common.loading')}
    </div>
  )
}

export function ErrorBanner({ error, onRetry }: { error: Error | string; onRetry?: () => void }) {
  const t = useT()
  return (
    <div role="alert" className="flex items-start gap-3 rounded-lg border border-down/30 bg-down-soft p-3 text-sm text-down">
      <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <div className="flex-1">{typeof error === 'string' ? error : error.message || t('common.error')}</div>
      {onRetry && <Button size="sm" onClick={onRetry}>{t('common.retry')}</Button>}
    </div>
  )
}

export function Empty({ icon, children, action }: { icon?: ReactNode; children: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 px-6 py-12 text-center text-sm text-muted">
      {icon && <div className="text-muted/70">{icon}</div>}
      <div className="max-w-md">{children}</div>
      {action}
    </div>
  )
}

export function PageHeader({ title, subtitle, actions }: { title: ReactNode; subtitle?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="mb-5 flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0">
        <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
        {subtitle && <div className="mt-1 text-sm text-muted">{subtitle}</div>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  )
}

export function Dialog({ open, onClose, title, children, footer, wide }: { open: boolean; onClose: () => void; title: ReactNode; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null)
  const t = useT()
  useEffect(() => {
    const d = ref.current
    if (!d) return
    if (open && !d.open) d.showModal()
    if (!open && d.open) d.close()
  }, [open])
  return (
    <dialog ref={ref} onClose={onClose} onCancel={(e) => { e.preventDefault(); onClose() }}
      className={`m-auto w-[calc(100%-2rem)] ${wide ? 'max-w-2xl' : 'max-w-md'} rounded-xl border border-line bg-surface p-0 text-fg shadow-xl`}>
      {open && (
        <div className="flex max-h-[85vh] flex-col">
          <div className="flex items-center justify-between border-b border-line px-5 py-3">
            <h2 className="text-base font-semibold">{title}</h2>
            <Button variant="ghost" size="sm" onClick={onClose} aria-label={t('common.close')}><X className="size-4" /></Button>
          </div>
          <div className="overflow-y-auto px-5 py-4">{children}</div>
          {footer && <div className="flex justify-end gap-2 border-t border-line px-5 py-3">{footer}</div>}
        </div>
      )}
    </dialog>
  )
}

export function ConfirmDialog({ open, onClose, onConfirm, title, message, confirmLabel, busy }: { open: boolean; onClose: () => void; onConfirm: () => void; title: ReactNode; message: ReactNode; confirmLabel?: string; busy?: boolean }) {
  const t = useT()
  return (
    <Dialog open={open} onClose={onClose} title={title}
      footer={<><Button onClick={onClose}>{t('common.cancel')}</Button><Button variant="danger" loading={busy} onClick={onConfirm}>{confirmLabel ?? t('common.confirm')}</Button></>}>
      <p className="text-sm">{message}</p>
    </Dialog>
  )
}

export function Table({ children }: { children: ReactNode }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">{children}</table>
    </div>
  )
}

export const th = 'whitespace-nowrap border-b border-line px-4 py-2.5 text-start text-xs font-medium uppercase tracking-wide text-muted'
export const td = 'border-b border-line px-4 py-2.5 align-middle'

export function Stat({ label, value, tone, sub }: { label: ReactNode; value: ReactNode; tone?: 'up' | 'down' | 'warn' | 'muted'; sub?: ReactNode }) {
  const color = tone === 'up' ? 'text-up' : tone === 'down' ? 'text-down' : tone === 'warn' ? 'text-warn' : 'text-fg'
  return (
    <div className="rounded-xl border border-line bg-surface p-4">
      <div className="text-xs font-medium text-muted">{label}</div>
      <div className={`mt-1 text-2xl font-semibold tabular ${color}`}>{value}</div>
      {sub && <div className="mt-1 text-xs text-muted">{sub}</div>}
    </div>
  )
}

export function Segmented<T extends string>({ value, onChange, options, label }: { value: T; onChange: (v: T) => void; options: { value: T; label: ReactNode }[]; label: string }) {
  return (
    <div role="radiogroup" aria-label={label} className="inline-flex rounded-lg border border-line bg-surface p-0.5">
      {options.map((o) => (
        <button key={o.value} type="button" role="radio" aria-checked={value === o.value} onClick={() => onChange(o.value)}
          className={`rounded-md px-2.5 py-1 text-xs font-medium ${value === o.value ? 'bg-accent-soft text-accent' : 'text-muted hover:text-fg'}`}>
          {o.label}
        </button>
      ))}
    </div>
  )
}
