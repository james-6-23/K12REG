export type TabId =
  | 'run'
  | 'settings'
  | 'files'
  | 'mail'
  | 'tasks'
  | 'results'
  | 'schedule'
  | 'advanced'

export interface TaskRecord {
  id: string
  source: 'manual' | 'schedule' | string
  status: 'ok' | 'fail' | 'cancelled' | 'error' | 'skipped' | string
  started_at: string
  finished_at: string
  elapsed_sec: number
  requested: number
  registered: number
  fail: number
  join_ok: number
  approve_ok: number
  k12: number
  import_ok: number
  exit_code: number
  workspace_id?: string
  mailboxes_file?: string
  threads?: number
  note?: string
}

export interface TaskListResponse {
  tasks: TaskRecord[]
  summary: {
    runs: number
    runs_ok: number
    runs_fail: number
    runs_cancelled: number
    total_registered: number
    total_fail: number
    total_elapsed_sec: number
  }
  file?: string
}

export interface MailPoolBaseRow {
  base_email: string
  alias_count: number
  free: number
  used: number
  failed: number
  token_invalid: number
  in_use: number
  status: 'free' | 'partial' | 'exhausted' | string
  base_key_state: string
}

export interface MailPoolReport {
  mailboxes_file: string
  state_file: string
  alias_count: number
  base_total: number
  slot_total: number
  free: number
  used: number
  failed: number
  token_invalid: number
  in_use: number
  bases: MailPoolBaseRow[]
  error?: string
}

export interface TabItem {
  id: TabId
  label: string
}

export interface RunStatus {
  running: boolean
  pid: number | null
  elapsed: number | null
  exit_code: number | null
}

export interface ImportEndpoint {
  name: string
  enabled: boolean
  url: string
  admin_key: string
  require_k12: boolean
}

export interface ImportApiSettings {
  /** Default for new endpoints / legacy fallback */
  require_k12: boolean
  endpoints: ImportEndpoint[]
}

export interface RegistrationSettings {
  total: number
  threads: number
  mode: string
  pipeline_gate: string
}

export interface WorkspaceSettings {
  enabled: boolean
  /** Pool of workspace UUIDs (for switching). */
  ids: string[]
  /** The one used for join/plan on each run. */
  selected_id: string
  manager_session_file: string
  approve_requests: boolean
}

export interface ProxySettings {
  proxies_file: string
  default_protocol: string
  flaresolverr_url: string
}

export interface MailSettings {
  mailboxes_file: string
  /** Plus-aliases per real mailbox (1 = no alias expansion). */
  alias_count: number
}

export interface Settings {
  import_api: ImportApiSettings
  registration: RegistrationSettings
  workspace: WorkspaceSettings
  proxy: ProxySettings
  mail: MailSettings
}

export interface DataFile {
  name: string
  size: number
  mtime: number
  editable: boolean
  /** True for text-like extensions even when not editable (e.g. huge dumps). */
  text?: boolean
}

export interface AccountRow {
  email: string | null
  plan_type: string | null
  join_status: string | null
  approve_status: string | null
  elevate_status: string | null
  import_status: string | null
  chatgpt_account_id: string | null
  has_access_token: boolean
  has_refresh_token?: boolean
}

export function emptyImportEndpoint(i = 1): ImportEndpoint {
  return {
    name: `api-${i}`,
    enabled: true,
    url: '',
    admin_key: '',
    require_k12: true,
  }
}

export function defaultSettings(): Settings {
  return {
    import_api: {
      require_k12: true,
      endpoints: [emptyImportEndpoint(1)],
    },
    registration: { total: 1, threads: 1, mode: 'protocol', pipeline_gate: 'reg' },
    workspace: {
      enabled: true,
      ids: [],
      selected_id: '',
      manager_session_file: 'session.json',
      approve_requests: true,
    },
    proxy: { proxies_file: '', default_protocol: 'socks5', flaresolverr_url: '' },
    mail: { mailboxes_file: '', alias_count: 1 },
  }
}

export function defaultRunStatus(): RunStatus {
  return { running: false, pid: null, elapsed: null, exit_code: null }
}

/** Normalize API payload that may still be legacy single-url shape. */
export function normalizeImportApi(raw: unknown): ImportApiSettings {
  const m = (raw || {}) as Record<string, unknown>
  const reqK12 = m.require_k12 !== false
  let endpoints: ImportEndpoint[] = []
  if (Array.isArray(m.endpoints) && m.endpoints.length) {
    endpoints = m.endpoints.map((item, i) => {
      const e = (item || {}) as Record<string, unknown>
      return {
        name: String(e.name || `api-${i + 1}`),
        enabled: e.enabled !== false,
        url: String(e.url || ''),
        admin_key: String(e.admin_key || ''),
        require_k12: e.require_k12 !== undefined ? !!e.require_k12 : reqK12,
      }
    })
  } else if (m.url) {
    endpoints = [
      {
        name: 'default',
        enabled: m.enabled !== false,
        url: String(m.url || ''),
        admin_key: String(m.admin_key || ''),
        require_k12: reqK12,
      },
    ]
  }
  if (!endpoints.length) endpoints = [emptyImportEndpoint(1)]
  return { require_k12: reqK12, endpoints }
}
