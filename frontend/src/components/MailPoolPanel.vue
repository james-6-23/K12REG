<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { apiJSON } from '../api'
import type { MailPoolReport, MailPoolBaseRow } from '../types'
import { toastError, toastSuccess } from '../toast'

const report = ref<MailPoolReport | null>(null)
const loading = ref(false)
const query = ref('')
const filter = ref<'all' | 'free' | 'partial' | 'exhausted'>('all')
const busyEmail = ref('')

const filteredBases = computed(() => {
  const rows = report.value?.bases || []
  const q = query.value.trim().toLowerCase()
  return rows.filter((r) => {
    if (filter.value !== 'all' && r.status !== filter.value) return false
    if (q && !r.base_email.toLowerCase().includes(q)) return false
    return true
  })
})

async function load() {
  loading.value = true
  try {
    report.value = await apiJSON<MailPoolReport>('/api/mail/pool')
  } catch (e) {
    report.value = null
    toastError((e as Error).message || '加载邮箱池失败')
  } finally {
    loading.value = false
  }
}

async function resetBase(row: MailPoolBaseRow) {
  if (
    !confirm(
      `重置 ${row.base_email} 的使用状态？\n将清除该主号及全部别名在 outlook_token_state 中的标记，下次运行可再次被捡起。`,
    )
  ) {
    return
  }
  busyEmail.value = row.base_email
  try {
    const res = await apiJSON<{ ok: boolean; cleared: number }>('/api/mail/pool', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'reset', email: row.base_email }),
    })
    toastSuccess(`已清除 ${res.cleared ?? 0} 条状态`)
    await load()
  } catch (e) {
    toastError((e as Error).message)
  } finally {
    busyEmail.value = ''
  }
}

function statusLabel(s: string) {
  switch (s) {
    case 'free':
      return '全部可用'
    case 'partial':
      return '部分可用'
    case 'exhausted':
      return '已用尽'
    default:
      return s
  }
}

function statusClass(s: string) {
  switch (s) {
    case 'free':
      return 'bg-emerald-500/12 text-emerald-700 ring-emerald-500/25 dark:text-emerald-300/90'
    case 'partial':
      return 'bg-amber-500/12 text-amber-800 ring-amber-500/25 dark:text-amber-200/90'
    case 'exhausted':
      return 'bg-rose-500/10 text-rose-700 ring-rose-500/20 dark:text-rose-300/90'
    default:
      return 'ui-surface ui-faint ring-1 ui-border'
  }
}

onMounted(load)
</script>

<template>
  <section class="animate-fade-in flex min-h-0 flex-1 flex-col gap-3">
    <!-- Summary -->
    <div class="card !p-3 sm:!p-4">
      <div class="flex flex-wrap items-start justify-between gap-2">
        <div class="min-w-0">
          <h2 class="text-sm font-semibold ui-heading">邮箱池用量</h2>
          <p class="mt-0.5 text-[11px] ui-faint">
            主号 × 别名 = 可注册槽位；used / failed / Graph 死号都会占用槽位
          </p>
        </div>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="load">
          {{ loading ? '加载中…' : '刷新' }}
        </button>
      </div>

      <div v-if="report?.error" class="mt-3 rounded-lg bg-rose-500/10 px-3 py-2 text-xs text-rose-600 ring-1 ring-rose-500/20 dark:text-rose-300">
        {{ report.error }}
      </div>

      <div v-if="report" class="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">
        <div class="stat-tile">
          <div class="stat-label">邮箱池文件</div>
          <div class="stat-value !text-xs font-mono truncate" :title="report.mailboxes_file">
            {{ report.mailboxes_file || '—' }}
          </div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">alias_count</div>
          <div class="stat-value">{{ report.alias_count }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">主号</div>
          <div class="stat-value">{{ report.base_total }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">总槽位</div>
          <div class="stat-value">{{ report.slot_total }}</div>
        </div>
        <div class="stat-tile stat-tile-ok">
          <div class="stat-label">剩余可用</div>
          <div class="stat-value text-emerald-600 dark:text-emerald-400">{{ report.free }}</div>
        </div>
        <div class="stat-tile">
          <div class="stat-label">已占用</div>
          <div class="stat-value">
            {{ report.used + report.failed + report.token_invalid + report.in_use }}
          </div>
        </div>
      </div>

      <div v-if="report" class="mt-2 flex flex-wrap gap-2 text-[11px] ui-faint">
        <span>used {{ report.used }}</span>
        <span>·</span>
        <span>failed {{ report.failed }}</span>
        <span>·</span>
        <span>token_invalid {{ report.token_invalid }}</span>
        <span>·</span>
        <span>in_use {{ report.in_use }}</span>
        <span>·</span>
        <span class="font-mono">state={{ report.state_file }}</span>
      </div>
    </div>

    <!-- Filters + table -->
    <div class="card flex min-h-0 flex-1 flex-col !p-0 overflow-hidden">
      <div class="flex shrink-0 flex-wrap items-center gap-2 border-b ui-border px-3 py-2.5">
        <input
          v-model="query"
          type="search"
          class="field !py-1.5 max-w-xs flex-1 text-xs"
          placeholder="搜索主号邮箱…"
        />
        <div class="flex flex-wrap gap-1">
          <button
            v-for="f in [
              { id: 'all', label: '全部' },
              { id: 'free', label: '全部可用' },
              { id: 'partial', label: '部分可用' },
              { id: 'exhausted', label: '已用尽' },
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
        <span class="ml-auto text-[11px] ui-faint">{{ filteredBases.length }} 条</span>
      </div>

      <div class="thin-scroll min-h-0 flex-1 overflow-auto">
        <table class="w-full min-w-[720px] border-collapse text-left text-xs">
          <thead class="sticky top-0 z-[1] backdrop-blur-md" style="background: color-mix(in srgb, var(--app-surface) 92%, transparent)">
            <tr class="border-b ui-border text-[10px] uppercase tracking-wide ui-faint">
              <th class="px-3 py-2 font-medium">主号</th>
              <th class="px-2 py-2 font-medium">状态</th>
              <th class="px-2 py-2 font-medium text-right">别名槽</th>
              <th class="px-2 py-2 font-medium text-right">剩余</th>
              <th class="px-2 py-2 font-medium text-right">used</th>
              <th class="px-2 py-2 font-medium text-right">failed</th>
              <th class="px-2 py-2 font-medium text-right">其它</th>
              <th class="px-3 py-2 font-medium text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!report && loading">
              <td colspan="8" class="px-3 py-8 text-center ui-faint">加载中…</td>
            </tr>
            <tr v-else-if="filteredBases.length === 0">
              <td colspan="8" class="px-3 py-8 text-center ui-faint">无匹配主号</td>
            </tr>
            <tr
              v-for="row in filteredBases"
              :key="row.base_email"
              class="border-b ui-border-soft transition hover:bg-[var(--app-hover)]"
            >
              <td class="px-3 py-2 font-mono text-[11px] ui-heading">{{ row.base_email }}</td>
              <td class="px-2 py-2">
                <span
                  class="inline-flex rounded-md px-1.5 py-0.5 text-[10px] font-medium ring-1"
                  :class="statusClass(row.status)"
                >
                  {{ statusLabel(row.status) }}
                </span>
              </td>
              <td class="px-2 py-2 text-right font-mono tabular-nums">{{ row.alias_count }}</td>
              <td class="px-2 py-2 text-right font-mono tabular-nums text-emerald-600 dark:text-emerald-400">
                {{ row.free }}
              </td>
              <td class="px-2 py-2 text-right font-mono tabular-nums">{{ row.used }}</td>
              <td class="px-2 py-2 text-right font-mono tabular-nums">{{ row.failed }}</td>
              <td class="px-2 py-2 text-right font-mono tabular-nums ui-faint">
                {{ row.token_invalid + row.in_use }}
              </td>
              <td class="px-3 py-2 text-right">
                <button
                  type="button"
                  class="btn btn-ghost btn-sm !py-0.5 !text-[11px]"
                  :disabled="busyEmail === row.base_email || row.status === 'free'"
                  :title="row.status === 'free' ? '无需重置' : '清除 used/failed 状态，重新可用'"
                  @click="resetBase(row)"
                >
                  {{ busyEmail === row.base_email ? '…' : '重置' }}
                </button>
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
