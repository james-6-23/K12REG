<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { apiJSON } from '../api'
import type { MailPoolReport, MailPoolBaseRow } from '../types'
import { toastError, toastSuccess } from '../toast'

const report = ref<MailPoolReport | null>(null)
const loading = ref(false)
const query = ref('')
const filter = ref<'all' | 'free' | 'partial' | 'exhausted'>('all')
const busyEmail = ref('')
const page = ref(1)
const pageSize = ref(50)

const filteredBases = computed(() => {
  const rows = report.value?.bases || []
  const q = query.value.trim().toLowerCase()
  return rows.filter((r) => {
    if (filter.value !== 'all' && r.status !== filter.value) return false
    if (q && !r.base_email.toLowerCase().includes(q)) return false
    return true
  })
})

const filterCounts = computed(() => {
  const bases = report.value?.bases || []
  const c = { all: bases.length, free: 0, partial: 0, exhausted: 0 }
  for (const r of bases) {
    if (r.status === 'free') c.free++
    else if (r.status === 'partial') c.partial++
    else if (r.status === 'exhausted') c.exhausted++
  }
  return c
})

const pages = computed(() => Math.max(1, Math.ceil(filteredBases.value.length / pageSize.value)))
const pageSafe = computed(() => Math.min(page.value, pages.value))

const pagedBases = computed(() => {
  const p = pageSafe.value
  const size = pageSize.value
  const start = (p - 1) * size
  return filteredBases.value.slice(start, start + size)
})

const rangeLabel = computed(() => {
  const total = filteredBases.value.length
  if (total === 0) return '0'
  const from = (pageSafe.value - 1) * pageSize.value + 1
  const to = Math.min(pageSafe.value * pageSize.value, total)
  return `${from}–${to} / ${total}`
})

const occupied = computed(() => {
  if (!report.value) return 0
  return report.value.used + report.value.failed + report.value.token_invalid + report.value.in_use
})

const freePct = computed(() => {
  const t = report.value?.slot_total || 0
  if (t <= 0) return 0
  return Math.round(((report.value?.free || 0) / t) * 1000) / 10
})

const usedPct = computed(() => {
  const t = report.value?.slot_total || 0
  if (t <= 0) return 0
  return Math.min(100, Math.round((occupied.value / t) * 1000) / 10)
})

watch([filter, query, pageSize], () => {
  page.value = 1
})

watch(pages, (n) => {
  if (page.value > n) page.value = n
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

function goPage(p: number) {
  page.value = Math.min(pages.value, Math.max(1, p))
}

function setPageSize(n: number) {
  pageSize.value = n
  page.value = 1
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
      return 'pill-ok'
    case 'partial':
      return 'pill-warn'
    case 'exhausted':
      return 'pill-fail'
    default:
      return 'pill-neutral'
  }
}

function fmtNum(n: number | undefined) {
  if (n == null) return '—'
  return n.toLocaleString('en-US')
}

onMounted(load)
</script>

<template>
  <section class="animate-fade-in flex min-h-0 flex-1 flex-col gap-3">
    <!-- Hero summary -->
    <div class="card pool-hero !p-0 overflow-hidden">
      <div class="pool-hero-bar" />
      <div class="relative p-3.5 sm:p-4">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <span class="pool-icon" aria-hidden="true">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <path d="M4 4h16c1.1 0 2 .9 2 2v12c0 1.1-.9 2-2 2H4c-1.1 0-2-.9-2-2V6c0-1.1.9-2 2-2z" />
                  <path d="m22 6-10 7L2 6" />
                </svg>
              </span>
              <div>
                <h2 class="text-sm font-semibold tracking-tight ui-heading">邮箱池用量</h2>
                <p class="mt-0.5 text-[11px] ui-faint">
                  主号 × 别名 = 可注册槽位 · used / failed / Graph 死号占用槽位
                </p>
              </div>
            </div>
          </div>
          <button type="button" class="btn btn-ghost btn-sm shrink-0" :disabled="loading" @click="load">
            <span v-if="loading" class="inline-flex items-center gap-1.5">
              <span class="pool-spin" />
              加载中…
            </span>
            <span v-else>刷新</span>
          </button>
        </div>

        <div
          v-if="report?.error"
          class="mt-3 rounded-xl bg-rose-500/10 px-3 py-2 text-xs text-rose-600 ring-1 ring-rose-500/20 dark:text-rose-300"
        >
          {{ report.error }}
        </div>

        <div v-if="report" class="mt-3.5 grid grid-cols-2 gap-2 sm:grid-cols-3 xl:grid-cols-6">
          <div class="stat-tile stat-file">
            <div class="stat-label">邮箱池文件</div>
            <div class="stat-value !text-[12px] font-mono truncate" :title="report.mailboxes_file">
              {{ report.mailboxes_file || '—' }}
            </div>
          </div>
          <div class="stat-tile">
            <div class="stat-label">别名数 / 主号</div>
            <div class="stat-value">
              <span class="text-sky-600 dark:text-sky-400">{{ report.alias_count }}</span>
              <span class="mx-1 text-[12px] font-normal ui-faint">×</span>
              <span>{{ fmtNum(report.base_total) }}</span>
            </div>
          </div>
          <div class="stat-tile">
            <div class="stat-label">总槽位</div>
            <div class="stat-value">{{ fmtNum(report.slot_total) }}</div>
          </div>
          <div class="stat-tile stat-ok">
            <div class="stat-label">剩余可用</div>
            <div class="stat-value text-emerald-600 dark:text-emerald-400">{{ fmtNum(report.free) }}</div>
            <div class="stat-sub">{{ freePct }}%</div>
          </div>
          <div class="stat-tile stat-used">
            <div class="stat-label">已占用</div>
            <div class="stat-value text-amber-700 dark:text-amber-300">{{ fmtNum(occupied) }}</div>
            <div class="stat-sub">{{ usedPct }}%</div>
          </div>
          <div class="stat-tile">
            <div class="stat-label">明细</div>
            <div class="flex flex-wrap gap-1 pt-0.5">
              <span class="mini-tag">u {{ report.used }}</span>
              <span class="mini-tag">f {{ report.failed }}</span>
              <span class="mini-tag">t {{ report.token_invalid }}</span>
              <span class="mini-tag">i {{ report.in_use }}</span>
            </div>
          </div>
        </div>

        <!-- Usage bar -->
        <div v-if="report && report.slot_total > 0" class="mt-3.5">
          <div class="mb-1 flex items-center justify-between text-[10px] ui-faint">
            <span>槽位占用</span>
            <span class="font-mono">{{ freePct }}% 可用 · {{ usedPct }}% 已用</span>
          </div>
          <div class="usage-track">
            <div class="usage-fill usage-used" :style="{ width: usedPct + '%' }" />
          </div>
        </div>
      </div>
    </div>

    <!-- Toolbar -->
    <div class="section-bar shrink-0 !gap-2 !py-2">
      <input
        v-model="query"
        type="search"
        class="field !py-1.5 max-w-[min(100%,280px)] flex-1 text-xs"
        placeholder="搜索主号邮箱…"
      />
      <div class="flex flex-wrap gap-1">
        <button
          v-for="f in [
            { id: 'all' as const, label: '全部', n: filterCounts.all },
            { id: 'free' as const, label: '全部可用', n: filterCounts.free },
            { id: 'partial' as const, label: '部分可用', n: filterCounts.partial },
            { id: 'exhausted' as const, label: '已用尽', n: filterCounts.exhausted },
          ]"
          :key="f.id"
          type="button"
          class="filter-chip"
          :class="filter === f.id && 'filter-chip-active'"
          @click="filter = f.id"
        >
          {{ f.label }}
          <span class="filter-n">{{ f.n }}</span>
        </button>
      </div>
      <div class="ml-auto flex flex-wrap items-center gap-1.5">
        <select
          class="field !w-auto !py-1 text-xs"
          :value="pageSize"
          @change="setPageSize(Number(($event.target as HTMLSelectElement).value))"
        >
          <option :value="30">30 / 页</option>
          <option :value="50">50 / 页</option>
          <option :value="100">100 / 页</option>
          <option :value="200">200 / 页</option>
        </select>
        <span class="pill pill-neutral ring-1 text-[11px]">匹配 {{ fmtNum(filteredBases.length) }}</span>
      </div>
    </div>

    <!-- Table -->
    <div class="card thin-scroll min-h-0 flex-1 overflow-auto !p-0">
      <table class="w-full min-w-[780px]">
        <thead
          class="sticky top-0 z-[1] backdrop-blur"
          style="background: color-mix(in srgb, var(--app-surface-solid) 92%, transparent)"
        >
          <tr>
            <th class="th w-10">#</th>
            <th class="th">主号</th>
            <th class="th">状态</th>
            <th class="th text-right">别名槽</th>
            <th class="th text-right">剩余</th>
            <th class="th text-right">used</th>
            <th class="th text-right">failed</th>
            <th class="th text-right">其它</th>
            <th class="th">用量</th>
            <th class="th text-right">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="loading && !report">
            <td class="td text-center ui-faint" colspan="10">
              <div class="py-14">加载中…</div>
            </td>
          </tr>
          <tr v-else-if="pagedBases.length === 0">
            <td class="td text-center" colspan="10">
              <div class="py-14">
                <p class="text-sm ui-muted">无匹配主号</p>
                <p class="mt-1 text-xs ui-faint">试试切换筛选，或清空搜索</p>
              </div>
            </td>
          </tr>
          <tr
            v-for="(row, i) in pagedBases"
            :key="row.base_email"
            class="transition ui-hover"
          >
            <td class="td font-mono text-[10px] ui-faint tabular-nums">
              {{ (pageSafe - 1) * pageSize + i + 1 }}
            </td>
            <td class="td font-mono text-[11px] font-medium ui-heading">
              {{ row.base_email }}
            </td>
            <td class="td">
              <span class="pill ring-1" :class="statusClass(row.status)">
                {{ statusLabel(row.status) }}
              </span>
            </td>
            <td class="td text-right font-mono tabular-nums">{{ row.alias_count }}</td>
            <td class="td text-right font-mono tabular-nums font-medium text-emerald-600 dark:text-emerald-400">
              {{ row.free }}
            </td>
            <td class="td text-right font-mono tabular-nums">{{ row.used }}</td>
            <td class="td text-right font-mono tabular-nums">{{ row.failed }}</td>
            <td class="td text-right font-mono tabular-nums ui-faint">
              {{ row.token_invalid + row.in_use }}
            </td>
            <td class="td min-w-[88px]">
              <div class="row-bar-track" :title="`${row.free}/${row.alias_count} free`">
                <div
                  class="row-bar-fill"
                  :class="{
                    'row-bar-ok': row.status === 'free',
                    'row-bar-warn': row.status === 'partial',
                    'row-bar-bad': row.status === 'exhausted',
                  }"
                  :style="{
                    width:
                      row.alias_count > 0
                        ? Math.round(((row.alias_count - row.free) / row.alias_count) * 100) + '%'
                        : '0%',
                  }"
                />
              </div>
            </td>
            <td class="td text-right">
              <button
                type="button"
                class="btn btn-ghost btn-sm !py-0.5 !text-[11px]"
                :disabled="busyEmail === row.base_email || row.status === 'free'"
                :title="row.status === 'free' ? '无需重置' : '清除 used/failed，重新可用'"
                @click="resetBase(row)"
              >
                {{ busyEmail === row.base_email ? '…' : '重置' }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- Pagination -->
    <div class="section-bar shrink-0 !justify-between !py-2">
      <span class="text-xs ui-muted">显示 {{ rangeLabel }}</span>
      <div class="flex flex-wrap items-center gap-1.5">
        <button type="button" class="btn btn-ghost btn-sm" :disabled="pageSafe <= 1 || loading" @click="goPage(1)">
          首页
        </button>
        <button
          type="button"
          class="btn btn-ghost btn-sm"
          :disabled="pageSafe <= 1 || loading"
          @click="goPage(pageSafe - 1)"
        >
          上一页
        </button>
        <span class="px-2 text-xs font-medium ui-heading tabular-nums">{{ pageSafe }} / {{ pages }}</span>
        <button
          type="button"
          class="btn btn-ghost btn-sm"
          :disabled="pageSafe >= pages || loading"
          @click="goPage(pageSafe + 1)"
        >
          下一页
        </button>
        <button
          type="button"
          class="btn btn-ghost btn-sm"
          :disabled="pageSafe >= pages || loading"
          @click="goPage(pages)"
        >
          末页
        </button>
      </div>
    </div>
  </section>
</template>

<style scoped>
.pool-hero {
  position: relative;
}
.pool-hero-bar {
  height: 3px;
  background: linear-gradient(90deg, #3b82f6, #6366f1 45%, #10b981);
  opacity: 0.9;
}
.pool-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 2rem;
  height: 2rem;
  border-radius: 0.65rem;
  color: #2563eb;
  background: color-mix(in srgb, #3b82f6 14%, transparent);
  ring: 1px solid color-mix(in srgb, #3b82f6 25%, transparent);
  box-shadow: 0 0 0 1px color-mix(in srgb, #3b82f6 18%, transparent);
  flex-shrink: 0;
}
html.dark .pool-icon {
  color: #93c5fd;
}

.stat-tile {
  border-radius: 0.75rem;
  border: 1px solid var(--app-border);
  background: color-mix(in srgb, var(--app-surface-solid) 70%, transparent);
  padding: 0.6rem 0.75rem;
  min-width: 0;
  transition: border-color 0.15s ease, box-shadow 0.15s ease;
}
.stat-tile:hover {
  border-color: color-mix(in srgb, #3b82f6 28%, var(--app-border));
}
.stat-ok {
  background: color-mix(in srgb, #10b981 8%, var(--app-surface-solid));
  border-color: color-mix(in srgb, #10b981 22%, var(--app-border));
}
.stat-used {
  background: color-mix(in srgb, #f59e0b 8%, var(--app-surface-solid));
  border-color: color-mix(in srgb, #f59e0b 20%, var(--app-border));
}
.stat-label {
  font-size: 10px;
  letter-spacing: 0.02em;
  color: var(--app-faint);
  margin-bottom: 0.2rem;
}
.stat-value {
  font-size: 1.1rem;
  font-weight: 650;
  font-variant-numeric: tabular-nums;
  color: var(--app-heading);
  line-height: 1.2;
}
.stat-sub {
  margin-top: 0.15rem;
  font-size: 10px;
  font-variant-numeric: tabular-nums;
  color: var(--app-faint);
}
.mini-tag {
  font-size: 10px;
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
  padding: 0.1rem 0.35rem;
  border-radius: 0.35rem;
  background: var(--app-hover);
  color: var(--app-muted);
  border: 1px solid var(--app-border-soft);
}

.usage-track {
  height: 6px;
  border-radius: 999px;
  background: var(--app-hover-strong);
  overflow: hidden;
  box-shadow: inset 0 1px 2px rgba(0, 0, 0, 0.06);
}
.usage-fill {
  height: 100%;
  border-radius: 999px;
  transition: width 0.35s ease;
}
.usage-used {
  background: linear-gradient(90deg, #f59e0b, #ef4444 85%);
}

.filter-chip {
  display: inline-flex;
  align-items: center;
  gap: 0.35rem;
  border-radius: 999px;
  border: 1px solid var(--app-border);
  background: var(--app-surface);
  padding: 0.2rem 0.55rem 0.2rem 0.65rem;
  font-size: 11px;
  color: var(--app-muted);
  transition: all 0.15s ease;
}
.filter-chip:hover {
  background: var(--app-hover);
  color: var(--app-heading);
}
.filter-chip-active {
  border-color: color-mix(in srgb, #3b82f6 45%, var(--app-border));
  background: color-mix(in srgb, #3b82f6 12%, transparent);
  color: var(--app-heading);
  box-shadow: 0 0 0 1px color-mix(in srgb, #3b82f6 15%, transparent);
}
.filter-n {
  font-size: 10px;
  font-variant-numeric: tabular-nums;
  font-family: var(--font-mono);
  min-width: 1.25rem;
  text-align: center;
  padding: 0.05rem 0.3rem;
  border-radius: 999px;
  background: var(--app-hover-strong);
  color: var(--app-faint);
}
.filter-chip-active .filter-n {
  background: color-mix(in srgb, #3b82f6 22%, transparent);
  color: #1d4ed8;
}
html.dark .filter-chip-active .filter-n {
  color: #93c5fd;
}

.row-bar-track {
  height: 5px;
  width: 72px;
  border-radius: 999px;
  background: var(--app-hover-strong);
  overflow: hidden;
}
.row-bar-fill {
  height: 100%;
  border-radius: 999px;
  min-width: 0;
  transition: width 0.2s ease;
}
.row-bar-ok {
  background: #10b981;
  width: 0 !important;
}
.row-bar-warn {
  background: #f59e0b;
}
.row-bar-bad {
  background: #f43f5e;
  width: 100% !important;
}

.pool-spin {
  width: 12px;
  height: 12px;
  border: 2px solid color-mix(in srgb, #3b82f6 30%, transparent);
  border-top-color: #3b82f6;
  border-radius: 50%;
  animation: pool-spin 0.7s linear infinite;
}
@keyframes pool-spin {
  to {
    transform: rotate(360deg);
  }
}
</style>
