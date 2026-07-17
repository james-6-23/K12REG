<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { apiJSON } from '../api'
import type { DataFile, Settings } from '../types'
import FileSelect from './FileSelect.vue'
import { toastError, toastSuccess } from '../toast'

const LS_KEY = 'k12reg_join_owner_manager_v1'
const LS_FILE_KEY = 'k12reg_join_owner_manager_file_v1'

interface SessionMeta {
  email?: string
  user_id?: string
  account_id?: string
  plan_type?: string
  expires?: string
  has_access_token?: boolean
}

interface LogLine {
  t: number
  level: string
  msg: string
}

interface JoinOwnerResult {
  ok: boolean
  error?: string
  workspace_id?: string
  manager?: SessionMeta
  target?: SessionMeta
  join?: { ok: boolean; status_code?: number; error?: string }
  approve?: { ok: boolean; invite_id?: string; error?: string }
  owner?: { ok: boolean; role?: string; error?: string }
  plan?: { plan?: string; account_id?: string; role?: string }
  session_after?: Record<string, unknown>
  accounts_check?: unknown
  logs?: LogLine[]
  proxy_used?: string
}

const dataFiles = ref<DataFile[]>([])
const managerFile = ref('')
const managerText = ref('')
const targetText = ref('')
const setOwner = ref(true)
const running = ref(false)
const loadingFile = ref(false)
const savingSession = ref(false)
const managerMeta = ref<SessionMeta | null>(null)
const targetMeta = ref<SessionMeta | null>(null)
const logs = ref<LogLine[]>([])
const logBox = ref<HTMLElement | null>(null)
const result = ref<JoinOwnerResult | null>(null)
const saveHint = ref('可从数据目录选择母号 session 文件，或直接粘贴 JSON')
const lastSavedFile = ref('')

/** session 类 json（排除注册结果） */
const sessionFiles = computed(() =>
  dataFiles.value.filter(
    (f) =>
      f.name.toLowerCase().endsWith('.json') &&
      !f.name.toLowerCase().startsWith('registered_accounts') &&
      f.name.toLowerCase() !== 'settings.json' &&
      f.name.toLowerCase() !== 'schedule.json',
  ),
)

function chipList(m: SessionMeta | null): [string, string][] {
  if (!m) return []
  const out: [string, string][] = []
  if (m.email) out.push(['email', m.email])
  if (m.user_id) out.push(['user', m.user_id])
  if (m.account_id) out.push(['workspace', m.account_id])
  if (m.plan_type) out.push(['plan', m.plan_type])
  if (m.expires) out.push(['expires', m.expires])
  if (m.has_access_token) out.push(['token', 'ok'])
  return out
}

async function loadDataFiles() {
  try {
    const data = await apiJSON<{ files: DataFile[] }>('/api/files')
    dataFiles.value = data.files || []
  } catch {
    dataFiles.value = []
  }
}

async function loadManagerFile(name: string, opts?: { silent?: boolean }) {
  const n = name.trim()
  if (!n) return
  loadingFile.value = true
  try {
    const text = await apiJSON<string>('/api/file?name=' + encodeURIComponent(n))
    managerText.value = typeof text === 'string' ? text : JSON.stringify(text, null, 2)
    try {
      localStorage.setItem(LS_FILE_KEY, n)
    } catch {
      /* ignore */
    }
    saveHint.value = `已从数据文件加载 · ${n}`
    await parseOne(managerText.value, 'manager')
    if (!opts?.silent) toastSuccess(`已加载 ${n}`)
  } catch (e) {
    toastError(e instanceof Error ? e.message : `加载 ${n} 失败`)
  } finally {
    loadingFile.value = false
  }
}

/** 启动阶段由 bootstrap 统一加载，避免与 watch 重复请求 */
let suppressFileWatch = true
watch(managerFile, (name, prev) => {
  if (suppressFileWatch) return
  if (!name || name === prev) return
  loadManagerFile(name)
})

function saveManager(showToast = false) {
  try {
    const v = managerText.value.trim()
    if (v) {
      localStorage.setItem(LS_KEY, v)
      if (managerFile.value) localStorage.setItem(LS_FILE_KEY, managerFile.value)
      saveHint.value = `已保存到浏览器 · ${new Date().toLocaleTimeString()}`
      if (showToast) toastSuccess('母号 session 已保存到浏览器')
    } else {
      localStorage.removeItem(LS_KEY)
      saveHint.value = '母号为空，已清除本地保存'
    }
  } catch {
    if (showToast) toastError('保存失败')
  }
}

function loadManagerFromBrowser() {
  try {
    const file = localStorage.getItem(LS_FILE_KEY)
    if (file) managerFile.value = file
    const v = localStorage.getItem(LS_KEY)
    if (v) {
      managerText.value = v
      saveHint.value = file
        ? `已从浏览器回填（上次文件: ${file}）`
        : '已从浏览器回填母号 session'
      return true
    }
  } catch {
    /* ignore */
  }
  return false
}

function clearManagerLocal() {
  try {
    localStorage.removeItem(LS_KEY)
    localStorage.removeItem(LS_FILE_KEY)
  } catch {
    /* ignore */
  }
  managerFile.value = ''
  managerText.value = ''
  managerMeta.value = null
  saveHint.value = '已清除本地母号缓存'
}

async function parseOne(raw: string, kind: 'manager' | 'target') {
  const t = raw.trim()
  if (!t) {
    if (kind === 'manager') managerMeta.value = null
    else targetMeta.value = null
    return
  }
  try {
    const data = await apiJSON<SessionMeta & { ok: boolean; error?: string }>(
      '/api/workspace/parse-session',
      {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ session: t.startsWith('{') ? JSON.parse(t) : t }),
      },
    )
    if (kind === 'manager') managerMeta.value = data
    else targetMeta.value = data
  } catch (e) {
    if (kind === 'manager') managerMeta.value = null
    else targetMeta.value = null
    toastError(e instanceof Error ? e.message : String(e))
  }
}

async function parseBoth() {
  await parseOne(managerText.value, 'manager')
  await parseOne(targetText.value, 'target')
}

function appendClientLog(level: string, msg: string) {
  logs.value = [...logs.value, { t: Date.now(), level, msg }]
  scrollLogsToBottom()
}

function scrollLogsToBottom() {
  nextTick(() => {
    const el = logBox.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

watch(
  () => logs.value.length,
  () => scrollLogsToBottom(),
)

async function runJoin() {
  const fileName = managerFile.value.trim()
  const manager_session = managerText.value.trim()
  if (!fileName && !manager_session) {
    toastError('请选择母号 session 文件，或粘贴 JSON')
    return
  }

  // 优先用数据目录文件：后端直接读 JSON 里的 account.id 作为空间 ID
  const body: Record<string, unknown> = {
    set_owner: setOwner.value,
  }
  if (fileName) {
    body.manager_session_file = fileName
  } else {
    let managerPayload: unknown = manager_session
    try {
      if (manager_session.startsWith('{')) managerPayload = JSON.parse(manager_session)
    } catch {
      toastError('母号 session 不是合法 JSON')
      return
    }
    body.manager_session = managerPayload
  }

  // 确保空间 ID 已从 JSON 解析出来（选文件后自动读 account.id）
  if (fileName && !managerMeta.value?.account_id) {
    await loadManagerFile(fileName, { silent: true })
  } else if (!managerMeta.value?.account_id && manager_session) {
    await parseOne(manager_session, 'manager')
  }
  if (!managerMeta.value?.account_id) {
    toastError('母号 JSON 中未找到空间 ID（account.id），请确认是完整 /api/auth/session')
    return
  }

  const target_session = targetText.value.trim()
  if (!target_session) {
    toastError('请粘贴目标账号的 /api/auth/session JSON')
    return
  }
  let targetPayload: unknown = target_session
  try {
    if (target_session.startsWith('{')) targetPayload = JSON.parse(target_session)
  } catch {
    toastError('目标 session 不是合法 JSON')
    return
  }
  body.target_session = targetPayload

  saveManager(false)
  running.value = true
  logs.value = []
  result.value = null
  lastSavedFile.value = ''
  const ws = managerMeta.value.account_id || ''
  appendClientLog(
    'info',
    `母号空间 ID（来自 JSON）· ${ws}${fileName ? ` · 文件=${fileName}` : ''}`,
  )
  appendClientLog('info', '开始执行 · 目标 session（/api/auth/session）…')

  try {
    // SSE 流式日志（注册/OTP 可能很长，边跑边看）
    const res = await fetch('/api/workspace/join-owner?stream=1', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'content-type': 'application/json',
        accept: 'text/event-stream',
      },
      body: JSON.stringify(body),
    })
    if (!res.ok && !res.headers.get('content-type')?.includes('text/event-stream')) {
      // non-stream error JSON
      const errBody = (await res.json().catch(() => ({}))) as { detail?: string; error?: string }
      throw new Error(errBody.detail || errBody.error || `HTTP ${res.status}`)
    }
    if (!res.body) {
      throw new Error('响应无 body，无法流式读日志')
    }

    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let buf = ''
    // Use a box so assignments inside handleEvent are visible to control-flow (not never).
    const streamState: { final: JoinOwnerResult | null } = { final: null }

    const handleEvent = (event: string, dataStr: string) => {
      let data: unknown
      try {
        data = JSON.parse(dataStr)
      } catch {
        return
      }
      if (event === 'log') {
        const line = data as LogLine
        if (line && typeof line.msg === 'string') {
          logs.value = [...logs.value, line]
        }
        return
      }
      if (event === 'result' || event === 'error') {
        streamState.final = data as JoinOwnerResult
      }
    }

    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      buf += decoder.decode(value, { stream: true })
      for (;;) {
        const sep = buf.indexOf('\n\n')
        if (sep < 0) break
        const block = buf.slice(0, sep)
        buf = buf.slice(sep + 2)
        let ev = 'message'
        const dataLines: string[] = []
        for (const rawLine of block.split('\n')) {
          const line = rawLine.replace(/\r$/, '')
          if (line.startsWith('event:')) ev = line.slice(6).trim()
          else if (line.startsWith('data:')) dataLines.push(line.slice(5).trimStart())
        }
        if (dataLines.length) handleEvent(ev, dataLines.join('\n'))
      }
    }

    const data = streamState.final
    if (!data) {
      throw new Error('流结束但未收到 result 事件')
    }
    result.value = data
    if (Array.isArray(data.logs) && data.logs.length && logs.value.length < data.logs.length) {
      logs.value = data.logs
    }
    if (data.manager) {
      managerMeta.value = {
        email: data.manager.email,
        user_id: data.manager.user_id,
        account_id: data.manager.account_id,
        plan_type: data.manager.plan_type,
        expires: data.manager.expires,
        has_access_token: true,
      }
    }
    if (data.target) {
      targetMeta.value = {
        email: data.target.email,
        user_id: data.target.user_id,
        account_id: data.target.account_id,
        plan_type: data.target.plan_type,
        expires: data.target.expires,
        has_access_token: true,
      }
    }
    if (data.session_after) {
      try {
        targetText.value = JSON.stringify(data.session_after, null, 2)
      } catch {
        /* ignore */
      }
    }
    if (data.ok) toastSuccess('完成')
    else toastError(data.error || '未完全成功')
  } catch (e) {
    appendClientLog('err', String(e))
    toastError(e instanceof Error ? e.message : String(e))
  } finally {
    running.value = false
  }
}

async function copyJSON(obj: unknown, label: string) {
  if (obj == null) {
    toastError(`暂无 ${label}`)
    return
  }
  const text = JSON.stringify(obj, null, 2)
  try {
    await navigator.clipboard.writeText(text)
    toastSuccess(`${label} 已复制`)
  } catch {
    toastError('复制失败')
  }
}

/** 从结果里解析邮箱，生成「邮箱.json」文件名 */
const sessionSaveFileName = computed(() => {
  const r = result.value
  if (!r?.session_after) return ''
  const after = r.session_after as Record<string, unknown>
  const user = (after.user && typeof after.user === 'object' ? after.user : {}) as Record<
    string,
    unknown
  >
  const email = String(
    r.target?.email ||
      after.email ||
      user.email ||
      targetMeta.value?.email ||
      '',
  )
    .trim()
    .toLowerCase()
  if (!email || !email.includes('@')) return ''
  // 禁止路径分隔符，其余保留（@ 在数据目录文件名中合法）
  const safe = email.replace(/[/\\]/g, '_').replace(/^\.+/, '')
  if (!safe) return ''
  return `${safe}.json`
})

async function saveSessionToDataFile() {
  const session = result.value?.session_after
  if (!session) {
    toastError('暂无 session_after')
    return
  }
  const name = sessionSaveFileName.value
  if (!name) {
    toastError('无法从结果中解析邮箱，无法命名文件')
    return
  }
  // 已存在时确认覆盖
  const exists = dataFiles.value.some((f) => f.name === name)
  if (exists && !confirm(`数据目录已有 ${name}，是否覆盖？`)) {
    return
  }
  savingSession.value = true
  try {
    const body = JSON.stringify(session, null, 2) + '\n'
    await apiJSON('/api/file?name=' + encodeURIComponent(name), {
      method: 'PUT',
      headers: { 'content-type': 'text/plain; charset=utf-8' },
      body,
    })
    lastSavedFile.value = name
    toastSuccess(`已写入数据文件 · ${name}`)
    appendClientLog('ok', `session_after 已保存为 ${name}`)
    await loadDataFiles()
  } catch (e) {
    toastError(e instanceof Error ? e.message : '保存失败')
  } finally {
    savingSession.value = false
  }
}

function clearTarget() {
  targetText.value = ''
  targetMeta.value = null
  result.value = null
  logs.value = []
}

function logClass(level: string) {
  if (level === 'ok') return 'text-emerald-600 dark:text-emerald-400'
  if (level === 'err') return 'text-rose-600 dark:text-rose-400'
  if (level === 'warn') return 'text-amber-600 dark:text-amber-400'
  return 'ui-faint'
}

function formatLogTime(t: number) {
  try {
    return new Date(t).toLocaleTimeString()
  } catch {
    return ''
  }
}

let saveTimer: ReturnType<typeof setTimeout> | null = null
function onManagerInput() {
  // 手动编辑后不再绑定到某个文件名（避免误覆盖认知）
  if (saveTimer) clearTimeout(saveTimer)
  saveTimer = setTimeout(() => saveManager(false), 600)
}

async function bootstrap() {
  suppressFileWatch = true
  await loadDataFiles()

  // 设置页里配置的母号 session
  let settingsFile = ''
  try {
    const st = await apiJSON<Settings>('/api/settings')
    settingsFile = (st.workspace?.manager_session_file || '').trim()
  } catch {
    /* ignore */
  }

  // 浏览器记住的文件名 / 正文
  const hadBrowser = loadManagerFromBrowser()

  // 优先级：上次选择的文件 → 设置默认 → session.json / hotsession.json → 列表第一个
  const preferred =
    managerFile.value ||
    settingsFile ||
    (sessionFiles.value.find((f) => f.name === 'session.json')?.name ?? '') ||
    (sessionFiles.value.find((f) => f.name === 'hotsession.json')?.name ?? '') ||
    sessionFiles.value[0]?.name ||
    ''

  if (preferred) {
    managerFile.value = preferred
    // 始终从数据目录读最新内容（覆盖可能过期的 localStorage 正文）
    await loadManagerFile(preferred, { silent: true })
  } else if (hadBrowser && managerText.value.trim()) {
    await parseOne(managerText.value, 'manager').catch(() => {})
  }

  suppressFileWatch = false
}

onMounted(() => {
  bootstrap().catch(() => {
    suppressFileWatch = false
  })
})
</script>

<template>
  <section class="animate-fade-in flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto">
    <div class="card p-4 sm:p-5">
      <p class="text-sm ui-muted leading-relaxed">
        选择母号 <strong class="ui-heading">session JSON</strong>（空间 ID 自动读
        <code class="text-xs">account.id</code>）。目标账号请自行打开
        <code class="text-xs">https://chatgpt.com/api/auth/session</code>
        复制完整 JSON 粘贴（本页<strong class="ui-heading">不再从邮箱池注册</strong>）。流程：join → 审批 →
        <code class="text-xs">session_after</code>。
      </p>
    </div>

    <!-- Manager -->
    <div class="card p-4 sm:p-5">
      <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
        <label class="text-xs font-semibold ui-muted tracking-wide">母号 Session *</label>
        <div class="flex flex-wrap gap-2">
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            :disabled="loadingFile || !managerFile"
            @click="managerFile && loadManagerFile(managerFile)"
          >
            {{ loadingFile ? '加载中…' : '重新加载' }}
          </button>
          <button type="button" class="btn btn-ghost btn-sm" @click="clearManagerLocal">清除</button>
        </div>
      </div>

      <div class="mb-3 grid grid-cols-1 gap-2 sm:grid-cols-[1fr_auto] sm:items-end">
        <div>
          <label class="label !mb-1">选择 session 文件（自动读空间 ID）</label>
          <FileSelect
            v-model="managerFile"
            :files="sessionFiles"
            empty-text="暂无 .json，请先到「数据文件」上传"
            placeholder="选择母号 session 文件"
          />
        </div>
        <button
          type="button"
          class="btn btn-ghost btn-sm h-[38px]"
          :disabled="loadingFile"
          @click="loadDataFiles"
        >
          刷新列表
        </button>
      </div>

      <!-- 空间 ID 等关键信息：选文件后自动解析 -->
      <div
        v-if="managerMeta"
        class="mb-3 rounded-lg border ui-border ui-surface px-3 py-2.5"
      >
        <div class="text-[10px] uppercase tracking-wider ui-faint">已从 JSON 解析</div>
        <div class="mt-1.5 flex flex-wrap gap-1.5">
          <span
            v-for="[k, v] in chipList(managerMeta)"
            :key="k"
            class="rounded-full px-2.5 py-0.5 text-[11px] ring-1 ui-border"
            :class="k === 'workspace' ? 'bg-sky-500/10 text-sky-700 dark:text-sky-300' : 'ui-surface'"
          >
            <span class="ui-faint">{{ k === 'workspace' ? '空间 ID' : k }}:</span>
            <strong class="ml-1 font-medium font-mono break-all">{{ v }}</strong>
          </span>
        </div>
        <p v-if="!managerMeta.account_id" class="mt-2 text-[11px] text-rose-600 dark:text-rose-400">
          未找到 account.id — 请确认文件是完整的 /api/auth/session JSON
        </p>
        <p v-else class="mt-1.5 text-[11px] ui-faint">
          空间 ID 已就绪，执行时将加入此工作区（无需在设置里再填）
        </p>
      </div>
      <p v-else-if="managerFile && !loadingFile" class="mb-2 text-[11px] ui-faint">
        已选文件，正在解析或 JSON 无效…
      </p>

      <details class="group">
        <summary class="cursor-pointer text-xs ui-faint select-none">
          高级：查看 / 粘贴原始 JSON
          <span v-if="managerFile" class="ml-1">（当前文件 {{ managerFile }}）</span>
        </summary>
        <textarea
          id="mgr-json"
          v-model="managerText"
          class="input font-mono text-xs mt-2 min-h-[120px] w-full resize-y"
          placeholder="选择上方文件后自动填入，或在此粘贴母号 session JSON…"
          @input="onManagerInput"
          @blur="parseOne(managerText, 'manager')"
        />
        <p class="mt-1.5 text-[11px] ui-faint">{{ saveHint }}</p>
      </details>
    </div>

    <!-- Target -->
    <div class="card p-4 sm:p-5">
      <label class="mb-2 block text-xs font-semibold ui-muted tracking-wide" for="tgt-json">
        目标账号 Session *
      </label>
      <p class="mb-2 text-xs ui-muted leading-relaxed">
        在目标账号浏览器打开
        <code class="text-[11px]">https://chatgpt.com/api/auth/session</code>
        ，复制完整 JSON 粘贴到下方（需含
        <code class="text-[11px]">accessToken</code> /
        <code class="text-[11px]">sessionToken</code>
        等）。
      </p>
      <textarea
        id="tgt-json"
        v-model="targetText"
        class="input font-mono text-xs min-h-[160px] w-full resize-y"
        placeholder="粘贴 /api/auth/session JSON…"
        @blur="parseOne(targetText, 'target')"
      />
      <div v-if="chipList(targetMeta).length" class="mt-2 flex flex-wrap gap-1.5">
        <span
          v-for="[k, v] in chipList(targetMeta)"
          :key="k"
          class="rounded-full px-2.5 py-0.5 text-[11px] ring-1 ui-border ui-surface"
        >
          <span class="ui-faint">{{ k }}:</span>
          <strong class="ml-1 font-medium ui-heading">{{ v }}</strong>
        </span>
      </div>
    </div>

    <!-- Actions -->
    <div class="card p-4 sm:p-5">
      <div class="flex flex-wrap items-center gap-3">
        <label class="flex cursor-pointer items-center gap-2 text-sm ui-heading">
          <input v-model="setOwner" type="checkbox" class="rounded border ui-border" />
          审批时设为 owner
        </label>
        <div class="flex-1" />
        <button type="button" class="btn btn-ghost btn-sm" :disabled="running" @click="parseBoth">
          解析两边
        </button>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="running" @click="clearTarget">
          清空目标
        </button>
        <button type="button" class="btn btn-primary" :disabled="running" @click="runJoin">
          {{ running ? '执行中…' : '加入空间并设 Owner' }}
        </button>
      </div>
    </div>

    <!-- Logs -->
    <div class="card p-4 sm:p-5">
      <div class="mb-2 flex items-center justify-between gap-2">
        <div class="text-xs font-semibold ui-muted tracking-wide">执行日志</div>
        <span v-if="running" class="text-[11px] text-sky-600 dark:text-sky-400">实时流式输出中…</span>
      </div>
      <div
        ref="logBox"
        class="max-h-[280px] min-h-[100px] overflow-auto rounded-lg border ui-border bg-[var(--app-input)] p-3 font-mono text-[11px] leading-relaxed"
      >
        <div v-if="!logs.length" class="ui-faint">
          {{ running ? '等待服务端日志…' : '尚无日志' }}
        </div>
        <div v-for="(line, i) in logs" :key="i" :class="logClass(line.level)">
          [{{ formatLogTime(line.t) }}] {{ line.msg }}
        </div>
      </div>

      <div v-if="result" class="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-3">
        <div class="rounded-lg border ui-border ui-surface p-3">
          <div class="text-[10px] uppercase tracking-wider ui-faint">Join</div>
          <div
            class="mt-1 text-sm font-semibold break-all"
            :class="result.join?.ok ? 'text-emerald-600 dark:text-emerald-400' : 'text-rose-600 dark:text-rose-400'"
          >
            {{ result.join?.ok ? `OK ${result.join?.status_code || ''}` : result.join?.error || '—' }}
          </div>
        </div>
        <div class="rounded-lg border ui-border ui-surface p-3">
          <div class="text-[10px] uppercase tracking-wider ui-faint">Approve</div>
          <div
            class="mt-1 text-sm font-semibold break-all"
            :class="
              result.approve == null
                ? 'ui-muted'
                : result.approve.ok
                  ? 'text-emerald-600 dark:text-emerald-400'
                  : 'text-rose-600 dark:text-rose-400'
            "
          >
            {{
              result.approve == null
                ? 'skipped'
                : result.approve.ok
                  ? 'OK'
                  : result.approve.error || 'fail'
            }}
          </div>
        </div>
        <div class="rounded-lg border ui-border ui-surface p-3">
          <div class="text-[10px] uppercase tracking-wider ui-faint">Owner</div>
          <div
            class="mt-1 text-sm font-semibold break-all"
            :class="
              result.owner == null
                ? 'ui-muted'
                : result.owner.ok
                  ? 'text-emerald-600 dark:text-emerald-400'
                  : 'text-rose-600 dark:text-rose-400'
            "
          >
            {{
              result.owner == null
                ? 'skipped'
                : result.owner.ok
                  ? `OK · ${result.owner.role || 'owner'}`
                  : result.owner.error || 'fail'
            }}
          </div>
        </div>
      </div>
      <p v-if="result?.proxy_used" class="mt-2 text-[11px] ui-faint">代理: {{ result.proxy_used }}</p>
    </div>

    <!-- session_after -->
    <div v-if="result?.session_after" class="card p-4 sm:p-5">
      <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
        <div class="text-xs font-semibold ui-muted tracking-wide">加入后的 JSON（session_after）</div>
        <div class="flex flex-wrap gap-2">
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            @click="copyJSON(result?.session_after, 'session_after')"
          >
            复制 JSON
          </button>
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            @click="copyJSON(result?.accounts_check, 'accounts_check')"
          >
            复制 accounts_check
          </button>
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="savingSession || !sessionSaveFileName"
            :title="sessionSaveFileName ? `写入 data/${sessionSaveFileName}` : '结果中无邮箱'"
            @click="saveSessionToDataFile"
          >
            {{
              savingSession
                ? '保存中…'
                : sessionSaveFileName
                  ? `写入数据文件 · ${sessionSaveFileName}`
                  : '写入数据文件（无邮箱）'
            }}
          </button>
        </div>
      </div>
      <p v-if="sessionSaveFileName" class="mb-2 text-[11px] ui-faint">
        将保存为数据目录下的
        <code class="text-xs">{{ sessionSaveFileName }}</code>
        <span v-if="lastSavedFile === sessionSaveFileName" class="ml-1 text-emerald-600 dark:text-emerald-400">
          · 已保存
        </span>
      </p>
      <textarea
        readonly
        class="input font-mono text-xs min-h-[200px] w-full resize-y"
        :value="JSON.stringify(result.session_after, null, 2)"
      />
      <details v-if="result.accounts_check" class="mt-3">
        <summary class="cursor-pointer text-xs ui-faint">accounts/check 原始响应</summary>
        <textarea
          readonly
          class="input font-mono text-xs mt-2 min-h-[120px] w-full resize-y"
          :value="JSON.stringify(result.accounts_check, null, 2)"
        />
      </details>
    </div>
  </section>
</template>
