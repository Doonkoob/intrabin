export class ApiError extends Error {
  status: number
  code: string
  fields: Record<string, string>
  constructor(status: number, code: string, message: string, fields?: Record<string, string>) {
    super(message)
    this.status = status
    this.code = code
    this.fields = fields ?? {}
  }
}

let csrfToken = ''
export function setCsrf(token: string) {
  csrfToken = token
}

let onUnauthorized: (() => void) | null = null
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn
}

export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (method !== 'GET' && csrfToken) headers['X-CSRF-Token'] = csrfToken
  let res: Response
  try {
    res = await fetch('/api/v1' + path, {
      method,
      headers,
      credentials: 'same-origin',
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
  } catch {
    throw new ApiError(0, 'network', 'Network error: the server could not be reached.')
  }
  const text = await res.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      throw new ApiError(res.status, 'bad_response', `Unexpected response (${res.status}).`)
    }
  }
  if (!res.ok) {
    const e = (data as { error?: { code: string; message: string; fields?: Record<string, string> } })?.error
    if (res.status === 401 && path !== '/auth/login' && onUnauthorized) onUnauthorized()
    throw new ApiError(res.status, e?.code ?? 'error', e?.message ?? `Request failed (${res.status}).`, e?.fields)
  }
  return data as T
}

export const get = <T,>(p: string) => api<T>('GET', p)
export const post = <T,>(p: string, b?: unknown) => api<T>('POST', p, b ?? {})
export const put = <T,>(p: string, b: unknown) => api<T>('PUT', p, b)
export const patch = <T,>(p: string, b: unknown) => api<T>('PATCH', p, b)
export const del = <T,>(p: string) => api<T>('DELETE', p)
