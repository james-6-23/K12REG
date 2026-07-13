export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

type UnauthorizedHandler = () => void

let onUnauthorized: UnauthorizedHandler | null = null

export function setUnauthorizedHandler(fn: UnauthorizedHandler) {
  onUnauthorized = fn
}

export async function api(path: string, opts: RequestInit = {}): Promise<Response> {
  const res = await fetch(path, { credentials: 'same-origin', ...opts })
  if (res.status === 401) {
    onUnauthorized?.()
    throw new ApiError('未登录', 401)
  }
  return res
}

export async function apiJSON<T = unknown>(path: string, opts: RequestInit = {}): Promise<T> {
  const res = await api(path, opts)
  if (!res.ok) {
    let msg = res.statusText
    try {
      const body = await res.json()
      msg = (body as { detail?: string }).detail || msg
    } catch {
      /* ignore */
    }
    throw new ApiError(msg, res.status)
  }
  const ct = res.headers.get('content-type') || ''
  if (ct.includes('application/json')) return res.json() as Promise<T>
  return res.text() as Promise<T>
}

export function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1048576).toFixed(1)} MB`
}

/** Status pill classes with solid contrast in both light and dark themes. */
export function pillCls(v: string | null | undefined): string {
  if (!v) return 'pill-neutral'
  const s = v.toLowerCase()
  if (s === 'ok' || s === 'added' || s === 'updated' || s === 'duplicate' || s.startsWith('partial'))
    return 'pill-ok'
  if (s === 'failed' || s === 'error' || s.includes('fail')) return 'pill-fail'
  if (s === 'skipped') return 'pill-skip'
  return 'pill-warn'
}

export function planPillCls(plan: string | null | undefined): string {
  if (!plan) return 'pill-neutral'
  if (plan.toLowerCase() === 'k12') return 'pill-k12'
  return 'pill-neutral'
}

export function classifyLog(line: string): string {
  const s = line.toLowerCase()
  if (line.startsWith('▶') || line.startsWith('■') || line.startsWith('⏹') || line.startsWith('⏰'))
    return 'log-sys'
  if (line.startsWith('· ') || line.startsWith('──')) return 'log-meta'
  if (/\b(fail|error|✗|401|403|429|timeout|soft-fail)\b/.test(s)) return 'log-err'
  if (/\b(warn|warning|skip|retry|rotate)\b/.test(s)) return 'log-warn'
  if (/\b(ok|success|k12|imp|✓|registered|joined|done|got otp|matched)\b/.test(s)) return 'log-ok'
  if (/\botp\b/.test(s)) return 'log-otp'
  return 'log-info'
}

/** Strip server emit prefix `HH:MM:SS.mmm\\t` (see RunManager.emit). */
function splitServerLog(line: string): { time: string | null; body: string } {
  // 15:04:05.000\\tbody  or  15:04:05\\tbody
  const m = line.match(/^(\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?)\t(.*)$/s)
  if (m) return { time: m[1], body: m[2] }
  // Already-prefixed without tab (legacy paste)
  const m2 = line.match(/^(\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?)\s+(.*)$/s)
  if (m2 && !line.startsWith('[')) return { time: m2[1], body: m2[2] }
  return { time: null, body: line }
}

/** Build a structured log row DOM node for the console. */
export function buildLogRow(line: string, at = new Date()): HTMLElement {
  const { time: serverTime, body } = splitServerLog(line)
  const row = document.createElement('div')
  const level = classifyLog(body)
  row.className = `log-row ${level}`

  const time = document.createElement('span')
  time.className = 'log-time'
  // Prefer server stamp (true event time); fallback to client receive time.
  if (serverTime) {
    // Show HH:MM:SS.mmm when present so same-second lines still differ.
    time.textContent = serverTime.length > 8 ? serverTime : serverTime
    time.title = serverTime
  } else {
    time.textContent = at.toLocaleTimeString('zh-CN', {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    })
  }
  row.appendChild(time)

  // [n/m] worker tag
  const m = body.match(/^(\[(\d+)\/(\d+)\])\s*(.*)$/s)
  let rest = body
  if (m) {
    const badge = document.createElement('span')
    const n = parseInt(m[2], 10) || 0
    badge.className = `log-w log-w-${n % 6}`
    badge.textContent = m[1]
    row.appendChild(badge)
    rest = m[4]
  }

  const msg = document.createElement('span')
  msg.className = 'log-msg'
  msg.textContent = rest
  row.appendChild(msg)
  return row
}
