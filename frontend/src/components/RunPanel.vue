<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { apiJSON, buildLogRow } from '../api'
import type { RunStatus, Settings } from '../types'

const props = defineProps<{
  runStatus: RunStatus
}>()

const emit = defineEmits<{
  refresh: []
}>()

const count = ref('')
const workspaceId = ref('')
const workspaceEnabled = ref(true)
const managerSessionFile = ref('')
const mailBinding = ref<'shared' | 'per_manager'>('shared')
const managerSlots = ref<
  { session_file: string; quota: number; workspace_id: string; email: string; domain: string; enabled: boolean }[]
>([])
const autoscroll = ref(true)
const logEl = ref<HTMLDivElement | null>(null)
let logSource: EventSource | null = null

const showWorkspace = computed(
  () => workspaceEnabled.value && (managerSlots.value.length > 0 || !!workspaceId.value),
)
const totalQuota = computed(() =>
  managerSlots.value.filter((m) => m.enabled).reduce((s, m) => s + (m.quota || 0), 0),
)

async function loadWorkspace() {
  try {
    const s = await apiJSON<Settings>('/api/settings')
    workspaceEnabled.value = !!s.workspace?.enabled
    managerSessionFile.value = (s.workspace?.manager_session_file || '').trim()
    mailBinding.value = s.workspace?.mail_binding === 'per_manager' ? 'per_manager' : 'shared'
    const mgrs = s.workspace?.managers || []
    managerSlots.value = mgrs.map((m) => ({
      enabled: m.enabled !== false,
      session_file: m.session_file || '',
      quota: m.quota || 20,
      workspace_id: m.workspace_id || '',
      email: m.email || '',
      domain: m.domain || '',
    }))
    workspaceId.value = (s.workspace?.selected_id || managerSlots.value[0]?.workspace_id || '').trim()
  } catch {
    /* ignore */
  }
}

function appendLog(line: string) {
  const el = logEl.value
  if (!el) return
  const near = el.scrollHeight - el.scrollTop - el.clientHeight < 80
  el.appendChild(buildLogRow(line))
  while (el.childElementCount > 4000) el.removeChild(el.firstChild!)
  if (autoscroll.value && near) el.scrollTop = el.scrollHeight
}

function startLogStream() {
  if (logSource) return
  logSource = new EventSource('/api/run/logs')
  logSource.onmessage = (ev) => {
    try {
      appendLog(JSON.parse(ev.data))
    } catch {
      /* ignore */
    }
  }
}

async function clearLog() {
  try {
    // Clear server buffer first; otherwise refresh / SSE reconnect replays old lines.
    await apiJSON('/api/run/logs/clear', { method: 'POST' })
  } catch (e) {
    console.warn('clear server logs failed', e)
  }
  if (logEl.value) logEl.value.innerHTML = ''
}

async function startRun() {
  const v = String(count.value).trim()
  try {
    await apiJSON('/api/run/start', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        count: v ? parseInt(v, 10) : null,
        workspace_id: workspaceId.value || undefined,
      }),
    })
    emit('refresh')
  } catch (e) {
    alert('启动失败: ' + (e as Error).message)
  }
}

async function stopRun() {
  try {
    await apiJSON('/api/run/stop', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ force: false }),
    })
    emit('refresh')
  } catch (e) {
    alert('停止失败: ' + (e as Error).message)
  }
}

onMounted(() => {
  startLogStream()
  loadWorkspace()
  nextTick(() => {
    if (logEl.value && autoscroll.value) logEl.value.scrollTop = logEl.value.scrollHeight
  })
})

onBeforeUnmount(() => {
  if (logSource) {
    logSource.close()
    logSource = null
  }
})

watch(
  () => props.runStatus.running,
  () => emit('refresh'),
)
</script>

<template>
  <section class="animate-fade-in flex min-h-0 flex-1 flex-col gap-2.5">
    <!-- Toolbar: actions row + optional full-width workspace -->
    <div class="section-bar shrink-0 flex-col !items-stretch !gap-2 !py-2.5 sm:!py-2">
      <div class="flex flex-wrap items-center gap-2">
        <div class="flex items-center gap-1.5">
          <label class="text-xs text-slate-500">数量</label>
          <input
            v-model="count"
            type="number"
            min="1"
            class="field w-20 !py-1.5 text-sm sm:w-24"
            placeholder="total"
          />
        </div>

        <div class="flex items-center gap-1.5">
          <button type="button" class="btn btn-primary" :disabled="runStatus.running" @click="startRun">
            <svg class="h-4 w-4" fill="currentColor" viewBox="0 0 20 20">
              <path d="M6.3 2.84A1.5 1.5 0 004 4.11v11.78a1.5 1.5 0 002.3 1.27l9.344-5.891a1.5 1.5 0 000-2.538L6.3 2.841z" />
            </svg>
            启动
          </button>
          <button type="button" class="btn btn-danger" :disabled="!runStatus.running" @click="stopRun">
            <svg class="h-4 w-4" fill="currentColor" viewBox="0 0 20 20">
              <path d="M5.25 3A2.25 2.25 0 003 5.25v9.5A2.25 2.25 0 005.25 17h9.5A2.25 2.25 0 0017 14.75v-9.5A2.25 2.25 0 0014.75 3h-9.5z" />
            </svg>
            停止
          </button>
          <button type="button" class="btn btn-ghost btn-sm" @click="clearLog">清屏</button>
        </div>

        <div class="ml-auto flex flex-wrap items-center gap-2 text-xs text-slate-500">
          <span
            v-if="runStatus.running && runStatus.elapsed != null"
            class="rounded-lg bg-emerald-500/10 px-2 py-0.5 font-mono text-emerald-300/90 ring-1 ring-emerald-500/20"
          >{{ Math.round(runStatus.elapsed) }}s</span>
          <span
            v-if="!runStatus.running && runStatus.exit_code !== null"
            class="rounded-lg bg-ink-900/80 px-2 py-0.5 font-mono ring-1 ring-white/5"
          >
            exit <span class="text-slate-300">{{ runStatus.exit_code }}</span>
          </span>
          <label class="flex cursor-pointer items-center gap-1.5 hover:text-slate-300">
            <input v-model="autoscroll" type="checkbox" class="h-3.5 w-3.5 rounded accent-blue-500" />
            自动滚动
          </label>
        </div>
      </div>

      <!-- 多母号工作区摘要 -->
      <div
        v-if="showWorkspace"
        class="min-w-0 space-y-1.5 border-t border-white/[0.05] pt-2"
      >
        <div class="flex flex-wrap items-center gap-2 text-xs text-slate-500">
          <span>工作区</span>
          <span class="rounded bg-white/5 px-1.5 py-0.5 font-mono text-[10px]">
            {{ managerSlots.filter((m) => m.enabled).length || 1 }} 个 · 配额
            {{ totalQuota || '—' }} ·
            {{ mailBinding === 'per_manager' ? '每母号邮箱池' : '共用邮箱池' }}
          </span>
        </div>
        <div
          v-if="managerSlots.length"
          class="max-h-24 overflow-y-auto space-y-1 text-[11px] font-mono leading-snug"
        >
          <div
            v-for="(m, i) in managerSlots.filter((x) => x.enabled)"
            :key="i"
            class="flex min-w-0 flex-wrap gap-x-2 gap-y-0.5 text-slate-400"
          >
            <span class="text-slate-500">#{{ i + 1 }}</span>
            <span class="ui-heading">{{ m.session_file || '—' }}</span>
            <span>×{{ m.quota }}</span>
            <span v-if="m.domain">@{{ m.domain }}</span>
            <span v-if="m.workspace_id" class="truncate opacity-80" :title="m.workspace_id">
              {{ m.workspace_id.slice(0, 8) }}…
            </span>
          </div>
        </div>
        <code
          v-else-if="workspaceId"
          class="field block min-w-0 !py-1.5 font-mono text-[12px] leading-snug break-all ui-heading"
          :title="workspaceId"
        >{{ workspaceId }}</code>
      </div>
    </div>

    <!-- Log console -->
    <div class="flex min-h-0 flex-1 flex-col overflow-hidden rounded-2xl border shadow-card ui-border">
      <div class="flex shrink-0 items-center gap-2 border-b ui-border px-3 py-1.5 sm:px-4 ui-surface">
        <span class="flex gap-1">
          <span class="h-2 w-2 rounded-full bg-red-400/70" />
          <span class="h-2 w-2 rounded-full bg-amber-400/70" />
          <span class="h-2 w-2 rounded-full bg-emerald-400/70" />
        </span>
        <span class="text-xs font-medium ui-muted">实时日志</span>
        <span
          v-if="runStatus.running"
          class="inline-flex items-center gap-1 rounded-full bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-medium text-emerald-700 ring-1 ring-emerald-500/25 dark:text-emerald-300"
        >
          <span class="h-1 w-1 rounded-full bg-emerald-400 pulse-live" />
          LIVE
        </span>
        <span class="ml-auto text-[11px] ui-faint">SSE</span>
      </div>
      <div
        ref="logEl"
        class="log-scroll log-panel min-h-0 flex-1 overflow-y-auto px-2 py-2 font-mono text-[12px] sm:px-3 sm:text-[12.5px]"
      />
    </div>
  </section>
</template>
