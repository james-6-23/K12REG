<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { apiJSON, setUnauthorizedHandler } from './api'
import type { RunStatus, TabId, TabItem } from './types'
import { defaultRunStatus } from './types'
import LoginView from './components/LoginView.vue'
import AppHeader from './components/AppHeader.vue'
import RunPanel from './components/RunPanel.vue'
import SettingsPanel from './components/SettingsPanel.vue'
import AdvancedPanel from './components/AdvancedPanel.vue'
import FilesPanel from './components/FilesPanel.vue'
import ResultsPanel from './components/ResultsPanel.vue'
import SchedulePanel from './components/SchedulePanel.vue'
import ToastHost from './components/ToastHost.vue'

const authed = ref(false)
const dataDir = ref('')
const tab = ref<TabId>('run')
const runStatus = ref<RunStatus>(defaultRunStatus())
const tabs: TabItem[] = [
  { id: 'run', label: '运行' },
  { id: 'settings', label: '设置' },
  { id: 'files', label: '数据文件' },
  { id: 'results', label: '结果' },
  { id: 'schedule', label: '定时' },
  { id: 'advanced', label: '高级' },
]

const pageMeta: Record<TabId, { title: string; desc: string }> = {
  run: { title: '运行', desc: '启动 / 停止流水线，查看实时日志' },
  settings: { title: '设置', desc: '注册、Workspace、代理与导入 API' },
  files: { title: '数据文件', desc: '上传、编辑邮箱池 / 代理 / session' },
  results: { title: '结果', desc: '已注册账号与下载产物' },
  schedule: { title: '定时任务', desc: '按间隔或每天固定时间自动启动流水线' },
  advanced: { title: '高级', desc: '直接编辑 settings.json 覆盖层' },
}

const meta = computed(() => pageMeta[tab.value])
/** 需要铺满视口、内部自滚动的页面 */
const fillPage = computed(() => tab.value === 'run' || tab.value === 'files' || tab.value === 'results' || tab.value === 'advanced')

let statusTimer: ReturnType<typeof setInterval> | null = null

setUnauthorizedHandler(() => {
  authed.value = false
  stopStatusTimer()
})

function stopStatusTimer() {
  if (statusTimer) {
    clearInterval(statusTimer)
    statusTimer = null
  }
}

async function refreshStatus() {
  try {
    runStatus.value = await apiJSON<RunStatus>('/api/run/status')
  } catch {
    /* ignore */
  }
}

function onAuthed(dir: string) {
  dataDir.value = dir
  authed.value = true
  tab.value = 'run'
  refreshStatus()
  stopStatusTimer()
  statusTimer = setInterval(refreshStatus, 3000)
}

async function logout() {
  await fetch('/api/logout', { method: 'POST', credentials: 'same-origin' })
  stopStatusTimer()
  authed.value = false
  dataDir.value = ''
}

async function bootstrap() {
  try {
    const me = await apiJSON<{ authed: boolean; data_dir?: string }>('/api/me')
    if (me.authed) onAuthed(me.data_dir || '')
  } catch {
    /* stay on login */
  }
}

onMounted(bootstrap)
onBeforeUnmount(stopStatusTimer)
</script>

<template>
  <LoginView v-if="!authed" @success="onAuthed" />

  <div v-else class="flex h-full min-h-0 flex-col lg:flex-row">
    <AppHeader
      :tabs="tabs"
      :tab="tab"
      :run-status="runStatus"
      :data-dir="dataDir"
      @update:tab="tab = $event"
      @logout="logout"
    />

    <!-- Main column -->
    <div class="flex min-h-0 min-w-0 flex-1 flex-col">
      <!-- Desktop page header (compact) -->
      <header
        class="hidden shrink-0 items-center justify-between gap-3 border-b ui-border px-5 py-2.5 backdrop-blur-md lg:flex"
        style="background: color-mix(in srgb, var(--app-bg) 70%, transparent)"
      >
        <div class="min-w-0">
          <h1 class="text-sm font-semibold tracking-tight ui-heading">{{ meta.title }}</h1>
          <p class="truncate text-[11px] ui-faint">{{ meta.desc }}</p>
        </div>
        <div class="flex shrink-0 items-center gap-2 text-xs ui-faint">
          <span
            v-if="runStatus.running && runStatus.elapsed != null"
            class="rounded-md bg-emerald-500/10 px-2 py-0.5 font-mono text-emerald-600 ring-1 ring-emerald-500/20 dark:text-emerald-300/90"
          >{{ Math.round(runStatus.elapsed) }}s</span>
          <span
            v-if="runStatus.pid"
            class="rounded-md px-2 py-0.5 font-mono ring-1 ui-border ui-surface"
          >PID {{ runStatus.pid }}</span>
        </div>
      </header>

      <main
        class="flex min-h-0 flex-1 flex-col"
        :class="fillPage ? 'overflow-hidden p-2.5 sm:p-3.5 lg:p-4' : 'overflow-y-auto p-2.5 sm:p-3.5 lg:p-4'"
      >
        <div
          class="mx-auto flex w-full max-w-[1600px] flex-col"
          :class="fillPage ? 'min-h-0 flex-1' : ''"
        >
          <RunPanel v-if="tab === 'run'" :run-status="runStatus" @refresh="refreshStatus" />
          <SettingsPanel v-else-if="tab === 'settings'" />
          <FilesPanel v-else-if="tab === 'files'" />
          <ResultsPanel v-else-if="tab === 'results'" />
          <SchedulePanel v-else-if="tab === 'schedule'" />
          <AdvancedPanel v-else-if="tab === 'advanced'" />
        </div>
      </main>
    </div>

    <ToastHost />
  </div>
</template>
