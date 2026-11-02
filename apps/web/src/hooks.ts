import { useCallback, useEffect, useRef, useState } from 'react'
import { get } from './api'

/** Fetches a GET endpoint with optional polling. */
export function useApi<T>(path: string | null, pollMs = 0) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<Error | null>(null)
  const [loading, setLoading] = useState(true)
  const seq = useRef(0)
  const load = useCallback(async () => {
    if (!path) return
    const n = ++seq.current
    try {
      const d = await get<T>(path)
      if (n === seq.current) {
        setData(d)
        setError(null)
      }
    } catch (e) {
      if (n === seq.current) setError(e as Error)
    } finally {
      if (n === seq.current) setLoading(false)
    }
  }, [path])
  useEffect(() => {
    setLoading(true)
    void load()
    if (!pollMs) return
    const id = window.setInterval(() => {
      if (document.visibilityState === 'visible') void load()
    }, pollMs)
    return () => window.clearInterval(id)
  }, [load, pollMs])
  return { data, error, loading, reload: load }
}
