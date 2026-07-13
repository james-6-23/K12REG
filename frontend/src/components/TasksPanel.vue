<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { apiJSON } from '../api'
import type { TaskRecord, TaskListResponse } from '../types'
import { toastError, toastSuccess } from '../toast'

const tasks = ref<TaskRecord[]>([])
const summary = ref<TaskListResponse['summary'] | null>(null)
const loading = ref(false)
const filter = ref<'all' | 'manual' | 'schedule'>('all')
const query = ref('')

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  return tasks.value.filter((t) => {
    if (filter.value !== 'all' && t.source !== filter.value) return false
    if (!q) return true
    return (
      (t.note || '').toLowerCase().includes(q) ||
      (t.mailboxes_file || '').toLowerCase().includes(q) ||
      (t.workspace_id || '').toLowerCase().includes(q) ||
      (t.id || '').toLowerCase().includes(q)
    )
  })
})

async function load() {
  loading.value = true
  try {
    const data = await apiJSON<TaskListResponse>('/api/tasks?limit=200')
    tasks.value = data.tasks || []
    summary.value = data.summary || null
  } catch (e) {
    tasks.value = []
    summary.value = null
    toastError((e as Error).message || '加载任务记录失败')
  } finally {
    loading.value = false
  }
}

async function clearAll() {
  if (!confirm('清空全部任务记录？此操作不可恢复。')) return
  try {
    await apiJSON('/api/tasks', { method: 'DELETE' })
    toastSuccess('已清空任务记录')
    await load()
  } catch (e) {
    toastError((e as Error).message)
  }
}

function sourceLabel(s: string) {
  return s === 'schedule' ? '定时' : '手动'
}

function statusLabel(s: string) {
  switch (s) {
    case 'ok':
      return '成功'
    case 'fail':
      return '失败'
    case 'cancelled':
      return '已取消'
    case 'skipped':
      return '跳过'
    case 'error':
      return '错误'
    default:
      return s || '—'
  }
}

function statusClass(s: string) {
  switch (s) {
    case 'ok':
      return 'bg-emerald-500/12 text-emerald-700 ring-emerald-500/25 dark:text-emerald-300/90'
    case 'fail':
    case 'error':
      return 'bg-rose-500/10 text-rose-700 ring-rose-500/20 dark:text-rose-300/90'
    case 'cancelled':
      return 'bg-slate-500/10 text-slate-700 ring-slate-500/20 dark:text-slate-300/90'
    case 'skipped':
      return 'bg-amber-500/12 text-amber-800 ring-amber-500/25 dark:text-amber-200/90'
    default:
      return 'ui-surface ui-faint ring-1 ui-border'
  }
}

function fmtTime(iso?: string) {
  if (!iso) return '—'
  try {
    const d = new Date(iso)
    if (Number.isNaN(d.getTime())) return iso
    const p = (n: number) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
  } catch {
    return iso
  }
}

function fmtDur(sec?: number) {
  if (sec == null || Number.isNaN(sec)) return '—'
  if (sec < 60) return `${sec.toFixed(0)}s`
  const m = Math.floor(sec / 60)
  const s = Math.round(sec % 60)
  if (m < 60) return `${m}m ${s}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m`
}

onMounted(load)
</script>

<template>
  <section class="animate-fade-in flex min-h-0 flex-1 flex-col gap-3">
    <div class="card !p-3 sm:!p-4">
      <div class="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h2 class="text-sm font-semibold ui-heading">任务记录</h2>
          <p class="mt-0.5 text-[11px] ui-faint">
            手动运行与定时触发的流水线历史 · 注册 / 成功 / 失败与耗时
          </p>
        </div>
        <div class="flex gap-1.5">
          <button type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="load">
            {{ loading ? '加载中…' : '刷新' }}
          </button>
          <button type="button" class="btn btn-ghost btn-sm text-rose-600 dark:text-rose-300" @click="clearAll">
            清空
          </button>
        </div>
      </div>

      <div v-if="summary" class="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">
        <div class="stat-tile">
          <div class="stat-label">记录数</div>
          <div class="stat-value">{{ summary.runs }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">运行成功</div>
          <div class="stat-value text-emerald-600 dark:text-emerald-400">{{ summary.runs_ok }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">运行失败</div>
          <div class="stat-value text-rose-600 dark:text-rose-400">{{ summary.runs_fail }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">累计注册</div>
          <div class="stat-value">{{ summary.total_registered }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">累计失败项</div>
          <div class="stat-value">{{ summary.total_fail }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">累计耗时</div>
          <div class="stat-value !text-sm">{{ fmtDur(summary.total_elapsed_sec) }}</div>
        </div>
      </div>
    </div>

    <div class="card flex min-h-0 flex-1 flex-col !p-0 overflow-hidden">
      <div class="flex shrink-0 flex-wrap items-center gap-2 border-b ui-border px-3 py-2.5">
        <input
          v-model="query"
          type="search"
          class="field !py-1.5 max-w-xs flex-1 text-xs"
          placeholder="搜索备注 / 邮箱池 / workspace…"
        />
        <div class="flex flex-wrap gap-1">
          <button
            v-for="f in [
              { id: 'all', label: '全部' },
              { id: 'manual', label: '手动' },
              { id: 'schedule', label: '定时' },
            ] as const"
            :key="f.id"
            type="button"
            class="ui-chip transition"
            :class="filter === f.id && 'ring-1 ring-blue-400/40 bg-blue-500/10'"
            @click="filter = f.id"
          >
            {{ f.label }}
          </button>
        </div>
        <span class="ml-auto text-[11px] ui-faint">{{ filtered.length }} 条</span>
      </div>

      <div class="thin-scroll min-h-0 flex-1 overflow-auto">
        <table class="w-full min-w-[960px] border-collapse text-left text-xs">
          <thead
            class="sticky top-0 z-[1] backdrop-blur-md"
            style="background: color-mix(in srgb, var(--app-surface) 92%, transparent)"
          >
            <tr class="border-b ui-border text-[10px] uppercase tracking-wide ui-faint">
              <th class="px-3 py-2 font-medium">开始时间</th>
              <th class="px-2 py-2 font-medium">来源</th>
              <th class="px-2 py-2 font-medium">状态</th>
              <th class="px-2 py-2 font-medium text-right">目标</th>
              <th class="px-2 py-2 font-medium text-right">注册</th>
              <th class="px-2 py-2 font-medium text-right">失败</th>
              <th class="px-2 py-2 font-medium text-right">Join</th>
              <th class="px-2 py-2 font-medium text-right">K12</th>
              <th class="px-2 py-2 font-medium text-right">Import</th>
              <th class="px-2 py-2 font-medium text-right">耗时</th>
              <th class="px-3 py-2 font-medium">备注</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading && !tasks.length">
              <td colspan="11" class="px-3 py-8 text-center ui-faint">加载中…</td>
            </tr>
            <tr v-else-if="filtered.length === 0">
              <td colspan="11" class="px-3 py-8 text-center ui-faint">
                暂无任务记录。运行一次流水线或等待定时触发后会出现在这里。
              </td>
            </tr>
            <tr
              v-for="t in filtered"
              :key="t.id"
              class="border-b ui-border-soft transition hover:bg-[var(--app-hover)]"
            >
              <td class="px-3 py-2 font-mono text-[11px] ui-heading whitespace-nowrap">
                {{ fmtTime(t.started_at) }}
              </td>
              <td class="px-2 py-2">
                <span
                  class="inline-flex rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1"
                  :class="
                    t.source === 'schedule'
                      ? 'bg-indigo-500/12 text-indigo-700 ring-indigo-500/25 dark:text-indigo-300/90'
                      : 'bg-sky-500/12 text-sky-800 ring-sky-500/25 dark:text-sky-200/90'
                  "
                >
                  {{ sourceLabel(t.source) }}
                </span>
              </td>
              <td class="px-2 py-2">
                <span
                  class="inline-flex rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1"
                  :class="statusClass(t.status)"
                >
                  {{ statusLabel(t.status) }}
                </span>
              </td>
              <td class="px-2 py-2 text-right font-mono tabular-nums">{{ t.requested || '—' }}</td>
              <td class="px-2 py-2 text-right font-mono tabular-nums text-emerald-600 dark:text-emerald-400">
                {{ t.registered }}
              </td>
              <td class="px-2 py-2 text-right font-mono tabular-nums text-rose-600 dark:text-rose-400">
                {{ t.fail }}
              </td>
              <td class="px-2 py-2 text-right font-mono tabular-nums">{{ t.join_ok }}</td>
              <td class="px-2 py-2 text-right font-mono tabular-nums">{{ t.k12 }}</td>
              <td class="px-2 py-2 text-right font-mono tabular-nums">{{ t.import_ok }}</td>
              <td class="px-2 py-2 text-right font-mono tabular-nums whitespace-nowrap">
                {{ fmtDur(t.elapsed_sec) }}
              </td>
              <td class="px-3 py-2 max-w-[220px] truncate ui-faint" :title="t.note || t.mailboxes_file">
                {{ t.note || t.mailboxes_file || '—' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </section>
</template>

<style scoped>
.stat-tile {
  border-radius: 0.65rem;
  border: 1px solid var(--app-border);
  background: var(--app-hover);
  padding: 0.55rem 0.7rem;
  min-width: 0;
}
.stat-label {
  font-size: 10px;
  color: var(--app-faint);
  margin-bottom: 0.15rem;
}
.stat-value {
  font-size: 1.05rem;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  color: var(--app-heading);
  line-height: 1.2;
}
</style>
