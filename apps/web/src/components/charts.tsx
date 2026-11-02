import { useMemo, useState } from 'react'
import { usePrefs } from '../app'
import { fmtDate, fmtMs, fmtNumber, fmtPct, fmtTime } from '../format'
import type { DailyBucket, LatencyPoint, RoundResult } from '../types'
import { verdictColor } from './ui'
import type { Key } from '../i18n'

/** Response-time area chart. Time always runs left→right (also in RTL) for readability. */
export function LatencyChart({ points, height = 180, showMax = false }: { points: LatencyPoint[]; height?: number; showMax?: boolean }) {
  const { loc, t } = usePrefs()
  const [hover, setHover] = useState<number | null>(null)
  const W = 800
  const H = height
  const pad = { l: 44, r: 8, t: 10, b: 22 }
  const data = points
  const maxY = useMemo(() => {
    const m = Math.max(1, ...data.map((p) => (showMax ? p.max_ms : p.avg_ms)))
    const step = Math.pow(10, Math.floor(Math.log10(m)))
    return Math.ceil(m / step) * step
  }, [data, showMax])
  if (data.length === 0) {
    return <div className="flex items-center justify-center text-sm text-muted" style={{ height }}>{t('common.noData')}</div>
  }
  const t0 = new Date(data[0].at).getTime()
  const t1 = Math.max(t0 + 1, new Date(data[data.length - 1].at).getTime())
  const x = (iso: string) => pad.l + ((new Date(iso).getTime() - t0) / (t1 - t0)) * (W - pad.l - pad.r)
  const y = (v: number) => pad.t + (1 - v / maxY) * (H - pad.t - pad.b)
  const valid = data.filter((p) => p.total > p.failures)
  const line = valid.map((p, i) => `${i ? 'L' : 'M'}${x(p.at).toFixed(1)},${y(p.avg_ms).toFixed(1)}`).join(' ')
  const area = valid.length ? `${line} L${x(valid[valid.length - 1].at).toFixed(1)},${y(0)} L${x(valid[0].at).toFixed(1)},${y(0)} Z` : ''
  const maxLine = showMax ? valid.map((p, i) => `${i ? 'L' : 'M'}${x(p.at).toFixed(1)},${y(p.max_ms).toFixed(1)}`).join(' ') : ''
  const ticks = [0, maxY / 2, maxY]
  const hp = hover !== null ? data[hover] : null
  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const rect = e.currentTarget.getBoundingClientRect()
    const px = ((e.clientX - rect.left) / rect.width) * W
    let best = 0
    let bd = Infinity
    data.forEach((p, i) => {
      const d = Math.abs(x(p.at) - px)
      if (d < bd) { bd = d; best = i }
    })
    setHover(best)
  }
  return (
    <div dir="ltr" className="relative">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height }} preserveAspectRatio="none" role="img"
        aria-label={t('mdetail.responseTime')} onMouseMove={onMove} onMouseLeave={() => setHover(null)}>
        {ticks.map((v) => (
          <g key={v}>
            <line x1={pad.l} x2={W - pad.r} y1={y(v)} y2={y(v)} stroke="var(--border)" strokeDasharray={v ? '3 4' : undefined} vectorEffect="non-scaling-stroke" />
            <text x={pad.l - 6} y={y(v) + 4} textAnchor="end" fontSize="11" fill="var(--muted)">{fmtNumber(loc, v)}</text>
          </g>
        ))}
        {data.filter((p) => p.failures > 0).map((p) => (
          <rect key={p.at} x={x(p.at) - 2} y={pad.t} width={4} height={H - pad.t - pad.b} fill="var(--down)" opacity={0.18} />
        ))}
        {area && <path d={area} fill="var(--accent)" opacity={0.12} />}
        {maxLine && <path d={maxLine} fill="none" stroke="var(--muted)" strokeWidth={1} strokeDasharray="4 3" vectorEffect="non-scaling-stroke" />}
        {line && <path d={line} fill="none" stroke="var(--accent)" strokeWidth={2} vectorEffect="non-scaling-stroke" strokeLinejoin="round" />}
        {valid.length <= 40 && valid.map((p) => <circle key={p.at} cx={x(p.at)} cy={y(p.avg_ms)} r={3} fill="var(--accent)" />)}
        {hp && <line x1={x(hp.at)} x2={x(hp.at)} y1={pad.t} y2={H - pad.b} stroke="var(--muted)" vectorEffect="non-scaling-stroke" />}
        <text x={pad.l} y={H - 4} fontSize="11" fill="var(--muted)">{fmtTime(loc, data[0].at)}</text>
        <text x={W - pad.r} y={H - 4} fontSize="11" fill="var(--muted)" textAnchor="end">{fmtTime(loc, data[data.length - 1].at)}</text>
      </svg>
      {hp && (
        <div className="pointer-events-none absolute top-1 rounded-md border border-line bg-surface px-2 py-1 text-xs shadow"
          style={{ left: `clamp(0px, calc(${(x(hp.at) / W) * 100}% - 70px), calc(100% - 150px))` }} dir={loc.startsWith('fa') ? 'rtl' : 'ltr'}>
          <div className="text-muted">{fmtDate(loc, hp.at)} {fmtTime(loc, hp.at)}</div>
          <div>{t('mdetail.avg')}: <b className="tabular">{fmtMs(loc, hp.total > hp.failures ? hp.avg_ms : null)}</b></div>
          {showMax && <div>{t('mdetail.max')}: <span className="tabular">{fmtMs(loc, hp.max_ms)}</span></div>}
          {hp.failures > 0 && <div className="text-down">{t('mdetail.failures')}: {fmtNumber(loc, hp.failures)}/{fmtNumber(loc, hp.total)}</div>}
        </div>
      )}
    </div>
  )
}

function barColor(b: DailyBucket) {
  if (b.availability === null) return b.maintenance_rounds > 0 ? 'var(--info)' : 'var(--border)'
  if (b.availability >= 99.9) return 'var(--up)'
  if (b.availability >= 98) return 'var(--warn)'
  return 'var(--down)'
}

/** Daily uptime bars (oldest → newest). */
export function UptimeBars({ days, height = 34 }: { days: DailyBucket[]; height?: number }) {
  const { loc, t } = usePrefs()
  const [hover, setHover] = useState<number | null>(null)
  const hb = hover !== null ? days[hover] : null
  return (
    <div className="relative" dir="ltr">
      <div className="flex items-end gap-[2px]" style={{ height }} onMouseLeave={() => setHover(null)}>
        {days.map((d, i) => (
          <div key={d.day} className="h-full flex-1 rounded-[2px] transition-opacity hover:opacity-70" style={{ background: barColor(d) }}
            onMouseEnter={() => setHover(i)} onFocus={() => setHover(i)} tabIndex={0}
            aria-label={`${fmtDate(loc, d.day)}: ${fmtPct(loc, d.availability)}`} role="img" />
        ))}
      </div>
      {hb && (
        <div className="pointer-events-none absolute -top-14 z-10 rounded-md border border-line bg-surface px-2 py-1 text-xs shadow"
          style={{ left: `clamp(0px, calc(${((hover! + 0.5) / days.length) * 100}% - 70px), calc(100% - 150px))` }} dir={loc.startsWith('fa') ? 'rtl' : 'ltr'}>
          <div className="font-medium">{fmtDate(loc, hb.day)}</div>
          <div>{t('mdetail.uptime')}: <span className="tabular">{fmtPct(loc, hb.availability)}</span> · {t('mdetail.coverage')}: <span className="tabular">{fmtPct(loc, hb.coverage)}</span></div>
        </div>
      )}
    </div>
  )
}

/** Strip of recent evaluation rounds. */
export function RoundStrip({ rounds }: { rounds: RoundResult[] }) {
  const { loc, t } = usePrefs()
  if (!rounds.length) return <div className="text-sm text-muted">{t('common.noData')}</div>
  return (
    <div className="flex flex-wrap gap-[3px]" dir="ltr">
      {rounds.map((r) => (
        <span key={r.round_start} className={`inline-block h-5 w-2 rounded-sm ${verdictColor[r.verdict]}`}
          title={`${fmtTime(loc, r.round_start)} — ${t(`verdict.${r.verdict}` as Key)} (${r.failing_probes}/${r.valid_probes})`} />
      ))}
    </div>
  )
}

export function Legend() {
  const { t } = usePrefs()
  const items: [string, Key][] = [['bg-up', 'verdict.ok'], ['bg-warn', 'verdict.partial'], ['bg-down', 'verdict.failed'], ['bg-line', 'verdict.inconclusive'], ['bg-info', 'verdict.maintenance']]
  return (
    <div className="flex flex-wrap gap-3 text-xs text-muted">
      {items.map(([c, k]) => <span key={k} className="inline-flex items-center gap-1"><span className={`size-2.5 rounded-sm ${c}`} />{t(k)}</span>)}
    </div>
  )
}
