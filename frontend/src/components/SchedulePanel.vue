<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { apiJSON } from '../api'
import SaveDock from './SaveDock.vue'
import { toastError, toastSuccess } from '../toast'

export interface ScheduleConfig {
  enabled: boolean
  mode: 'interval' | 'daily'
  interval_minutes: number
  daily_time: string
  count: number | null
  skip_if_running: boolean
  last_run_at?: string
  last_run_ok?: boolean | null
  last_run_note?: string
  next_run_at?: string
  fire_count?: number
}

const schedule = ref<ScheduleConfig>({
  enabled: false,
  mode: 'interval',
  interval_minutes: 60,
  daily_time: '09:00',
  count: null,
  skip_if_running: true,
})
const countText = ref('')
const loading = ref(false)
const saving = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null

const intervalUnit = ref<'min' | 'hour'>('min')
const intervalValue = ref(60)

const nextLabel = computed(() => formatTime(schedule.value.next_run_at))
const lastLabel = computed(() => formatTime(schedule.value.last_run_at))
const intervalSummary = computed(() => {
  if (schedule.value.mode === 'daily') return `每天 ${schedule.value.daily_time || '—'}`
  const m = schedule.value.interval_minutes || 0
  if (m >= 60 && m % 60 === 0) return `每 ${m / 60} 小时`
  return `每 ${m} 分钟`
})

function formatTime(iso?: string) {
  if (!iso) return '—'
  try {
    const d = new Date(iso)
    if (Number.isNaN(d.getTime())) return iso
    return d.toLocaleString()
  } catch {
    return iso
  }
}

function syncIntervalFromCfg() {
  const m = schedule.value.interval_minutes || 60
  if (m >= 60 && m % 60 === 0) {
    intervalUnit.value = 'hour'
    intervalValue.value = m / 60
  } else {
    intervalUnit.value = 'min'
    intervalValue.value = m
  }
}

function applyIntervalToCfg() {
  let m = Number(intervalValue.value) || 1
  if (m < 1) m = 1
  if (intervalUnit.value === 'hour') m = m * 60
  if (m > 60 * 24 * 7) m = 60 * 24 * 7
  schedule.value.interval_minutes = m
}

async function load() {
  loading.value = true
  try {
    const s = await apiJSON<ScheduleConfig>('/api/schedule')
    schedule.value = {
      enabled: !!s.enabled,
      mode: s.mode === 'daily' ? 'daily' : 'interval',
      interval_minutes: s.interval_minutes || 60,
      daily_time: s.daily_time || '09:00',
      count: s.count ?? null,
      skip_if_running: s.skip_if_running !== false,
      last_run_at: s.last_run_at,
      last_run_ok: s.last_run_ok,
      last_run_note: s.last_run_note,
      next_run_at: s.next_run_at,
      fire_count: s.fire_count || 0,
    }
    countText.value = s.count != null && s.count > 0 ? String(s.count) : ''
    syncIntervalFromCfg()
  } catch (e) {
    toastError((e as Error).message)
  } finally {
    loading.value = false
  }
}

async function save() {
  applyIntervalToCfg()
  const t = countText.value.trim()
  let count: number | null = null
  if (t) {
    const n = parseInt(t, 10)
    if (!n || n < 1) {
      toastError('数量必须是 ≥ 1 的整数，或留空使用设置里的 total')
      return
    }
    count = n
  }
  saving.value = true
  try {
    const body = { ...schedule.value, count }
    const s = await apiJSON<ScheduleConfig>('/api/schedule', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    })
    schedule.value = { ...schedule.value, ...s }
    countText.value = s.count != null && s.count > 0 ? String(s.count) : ''
    syncIntervalFromCfg()
    toastSuccess(schedule.value.enabled ? '定时任务已保存并启用' : '定时任务已保存（未启用）')
  } catch (e) {
    toastError((e as Error).message)
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  load()
  pollTimer = setInterval(load, 15000)
})
onBeforeUnmount(() => {
  if (pollTimer) clearInterval(pollTimer)
})
</script>

<template>
  <section class="animate-fade-in w-full pb-24">
    <div class="mx-auto grid w-full max-w-5xl gap-4 lg:grid-cols-[1fr_280px]">
      <!-- Main config -->
      <div class="card space-y-5 !p-5 sm:!p-6">
        <div class="flex items-start justify-between gap-4">
          <div class="min-w-0">
            <h2 class="text-base font-semibold tracking-tight ui-heading">定时启动</h2>
            <p class="mt-1 text-xs leading-relaxed ui-muted">
              容器内自动调度流水线，无需外部 cron · 使用「设置」中的邮箱池 / 工作区 / 代理
            </p>
          </div>
          <div class="flex shrink-0 flex-col items-end gap-1">
            <label class="toggle" title="启用定时任务">
              <input v-model="schedule.enabled" type="checkbox" />
              <span class="toggle-track"><span class="toggle-thumb" /></span>
            </label>
            <span class="text-[11px] font-medium" :class="schedule.enabled ? 'text-emerald-600 dark:text-emerald-400' : 'ui-faint'">
              {{ schedule.enabled ? '已启用' : '未启用' }}
            </span>
          </div>
        </div>

        <!-- Segmented mode -->
        <div>
          <div class="label !mb-2">触发方式</div>
          <div
            class="inline-flex w-full max-w-md rounded-xl p-1 ring-1"
            style="background: var(--color-ink-800); box-shadow: inset 0 0 0 1px var(--app-border-soft)"
          >
            <button
              type="button"
              class="flex-1 rounded-lg px-3 py-2 text-sm font-medium transition"
              :class="
                schedule.mode === 'interval'
                  ? 'bg-white text-slate-900 shadow-sm dark:bg-ink-700 dark:text-white'
                  : 'ui-muted hover:ui-heading'
              "
              @click="schedule.mode = 'interval'"
            >
              固定间隔
            </button>
            <button
              type="button"
              class="flex-1 rounded-lg px-3 py-2 text-sm font-medium transition"
              :class="
                schedule.mode === 'daily'
                  ? 'bg-white text-slate-900 shadow-sm dark:bg-ink-700 dark:text-white'
                  : 'ui-muted hover:ui-heading'
              "
              @click="schedule.mode = 'daily'"
            >
              每天定时
            </button>
          </div>
        </div>

        <!-- Fields -->
        <div class="grid gap-4 sm:grid-cols-2">
          <template v-if="schedule.mode === 'interval'">
            <div>
              <label class="label">间隔数值</label>
              <input v-model.number="intervalValue" type="number" min="1" class="field w-full" />
            </div>
            <div>
              <label class="label">单位</label>
              <select v-model="intervalUnit" class="field w-full">
                <option value="min">分钟</option>
                <option value="hour">小时</option>
              </select>
            </div>
            <p class="hint sm:col-span-2">启用后会先等一个完整周期再首次触发，避免一打开就立刻跑。</p>
          </template>
          <template v-else>
            <div class="sm:col-span-2">
              <label class="label">每天触发时间（服务器本地时区）</label>
              <input v-model="schedule.daily_time" type="time" class="field w-full max-w-[14rem]" />
            </div>
          </template>

          <div>
            <label class="label">每次注册数量</label>
            <input
              v-model="countText"
              type="number"
              min="1"
              class="field w-full"
              placeholder="留空 = 设置 total"
            />
          </div>
          <div class="flex items-end pb-1">
            <label class="flex cursor-pointer items-center gap-2.5 text-sm ui-muted">
              <input v-model="schedule.skip_if_running" type="checkbox" class="h-4 w-4 rounded accent-blue-500" />
              流水线运行中则跳过本次
            </label>
          </div>
        </div>
      </div>

      <!-- Side summary -->
      <aside class="flex flex-col gap-3">
        <div class="card !p-4">
          <div class="mb-3 flex items-center gap-2">
            <div
              class="flex h-9 w-9 items-center justify-center rounded-xl ring-1"
              :class="
                schedule.enabled
                  ? 'bg-emerald-500/15 text-emerald-600 ring-emerald-400/25 dark:text-emerald-300'
                  : 'bg-ink-800 ui-faint'
              "
              style="box-shadow: inset 0 0 0 1px var(--app-border-soft)"
            >
              <svg class="h-4.5 w-4.5 h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
                <path stroke-linecap="round" stroke-linejoin="round" d="M12 6v6h4.5m4.5 0a9 9 0 11-18 0 9 9 0 0118 0z" />
              </svg>
            </div>
            <div>
              <div class="text-sm font-semibold ui-heading">{{ schedule.enabled ? '调度中' : '未启用' }}</div>
              <div class="text-[11px] ui-faint">{{ intervalSummary }}</div>
            </div>
          </div>

          <dl class="space-y-3 text-sm">
            <div>
              <dt class="text-[11px] font-medium ui-faint">下次触发</dt>
              <dd class="mt-0.5 font-medium leading-snug ui-heading">
                {{ schedule.enabled ? nextLabel : '—' }}
              </dd>
            </div>
            <div class="border-t pt-3" style="border-color: var(--app-border-soft)">
              <dt class="text-[11px] font-medium ui-faint">上次触发</dt>
              <dd class="mt-0.5 font-medium leading-snug ui-heading">{{ lastLabel }}</dd>
              <dd
                v-if="schedule.last_run_note"
                class="mt-1 text-[11px] leading-relaxed"
                :class="schedule.last_run_ok === false ? 'text-red-500' : 'ui-muted'"
              >
                {{ schedule.last_run_note }}
                <span v-if="schedule.fire_count" class="ui-faint"> · 累计 {{ schedule.fire_count }} 次</span>
              </dd>
            </div>
          </dl>
        </div>

        <div class="card !p-4">
          <div class="text-[11px] font-semibold uppercase tracking-wider ui-faint">提示</div>
          <ul class="mt-2 space-y-1.5 text-[11px] leading-relaxed ui-muted">
            <li>配置保存在 <code class="ui-chip">schedule.json</code></li>
            <li>触发日志前缀为 ⏰</li>
            <li>建议间隔勿过短，避免打满邮箱池</li>
          </ul>
        </div>
      </aside>
    </div>

    <SaveDock :saving="saving" :disabled="loading" @save="save" @reload="load" />
  </section>
</template>
