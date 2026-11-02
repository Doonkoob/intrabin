export function fmtNumber(loc: string, n: number | null | undefined, digits = 0) {
  if (n === null || n === undefined || Number.isNaN(n)) return '—'
  return new Intl.NumberFormat(loc, { maximumFractionDigits: digits, minimumFractionDigits: digits }).format(n)
}

export function fmtPct(loc: string, n: number | null | undefined) {
  if (n === null || n === undefined) return '—'
  const digits = n >= 99 && n < 100 ? 2 : n === 100 ? 0 : 1
  return new Intl.NumberFormat(loc, { style: 'percent', maximumFractionDigits: digits, minimumFractionDigits: digits }).format(n / 100)
}

export function fmtMs(loc: string, n: number | null | undefined) {
  if (n === null || n === undefined) return '—'
  if (n >= 1000) return fmtNumber(loc, n / 1000, 2) + (loc.startsWith('fa') ? ' ث' : ' s')
  return fmtNumber(loc, n, 0) + (loc.startsWith('fa') ? ' م‌ث' : ' ms')
}

export function fmtDateTime(loc: string, iso?: string | null) {
  if (!iso) return '—'
  return new Intl.DateTimeFormat(loc, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(iso))
}

export function fmtTime(loc: string, iso: string) {
  return new Intl.DateTimeFormat(loc, { hour: '2-digit', minute: '2-digit', second: '2-digit' }).format(new Date(iso))
}

export function fmtDate(loc: string, iso: string) {
  return new Intl.DateTimeFormat(loc, { dateStyle: 'medium' }).format(new Date(iso))
}

export function fmtRelative(loc: string, iso?: string | null) {
  if (!iso) return '—'
  const diff = (new Date(iso).getTime() - Date.now()) / 1000
  const rtf = new Intl.RelativeTimeFormat(loc, { numeric: 'auto' })
  const abs = Math.abs(diff)
  if (abs < 60) return rtf.format(Math.round(diff), 'second')
  if (abs < 3600) return rtf.format(Math.round(diff / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), 'hour')
  return rtf.format(Math.round(diff / 86400), 'day')
}

export function fmtDuration(loc: string, seconds: number) {
  const s = Math.max(0, Math.round(seconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  const nf = (n: number) => fmtNumber(loc, n)
  const fa = loc.startsWith('fa')
  const u = fa ? { d: 'روز', h: 'ساعت', m: 'دقیقه', s: 'ثانیه' } : { d: 'd', h: 'h', m: 'm', s: 's' }
  const sep = fa ? ' ' : ''
  const parts: string[] = []
  if (d) parts.push(nf(d) + sep + u.d)
  if (h) parts.push(nf(h) + sep + u.h)
  if (m && !d) parts.push(nf(m) + sep + u.m)
  if (!d && !h) parts.push(nf(sec) + sep + u.s)
  return parts.slice(0, 2).join(' ')
}

export function fmtBytes(loc: string, b?: number) {
  if (b === undefined) return '—'
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let v = b
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return fmtNumber(loc, v, i ? 1 : 0) + ' ' + units[i]
}

/** Converts an ISO timestamp to a value for <input type="datetime-local"> in local time. */
export function toLocalInput(d: Date) {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}
