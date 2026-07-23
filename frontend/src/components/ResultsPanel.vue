<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { apiJSON, pillCls, planPillCls } from '../api'
import type { AccountRow } from '../types'
import { toastError, toastSuccess } from '../toast'

const accounts = ref<AccountRow[]>([])
const total = ref(0)
const offset = ref(0)
const limit = ref(100)
const loading = ref(false)
const order = ref<'desc' | 'asc'>('desc')
const codexBusy = ref(false)
const importBusy = ref(false)
const codexMsg = ref('')
const codexFiles = ref<{ name: string; size: number }[]>([])
const codexOutDir = ref('codex_auth')

const page = computed(() => Math.floor(offset.value / limit.value) + 1)
const pages = computed(() => Math.max(1, Math.ceil(total.value / limit.value)))
const k12OnPage = computed(
  () => accounts.value.filter((a) => (a.plan_type || '').toLowerCase() === 'k12').length,
)
const rangeLabel = computed(() => {
  if (total.value === 0) return '0'
  const from = offset.value + 1
  const to = Math.min(offset.value + accounts.value.length, total.value)
  return `${from}–${to} / ${total.value}`
})

async function loadAccounts() {
  loading.value = true
  try {
    const q = new URLSearchParams({
      limit: String(limit.value),
      offset: String(offset.value),
      order: order.value,
    })
    const data = await apiJSON<{
      accounts: AccountRow[]
      total: number
      offset: number
      limit: number
    }>('/api/accounts?' + q.toString())
    accounts.value = data.accounts || []
    total.value = data.total || 0
    if (data.offset != null) offset.value = data.offset
    if (data.limit != null) limit.value = data.limit
  } finally {
    loading.value = false
  }
}

function goPage(p: number) {
  const max = pages.value
  const next = Math.min(max, Math.max(1, p))
  offset.value = (next - 1) * limit.value
  loadAccounts()
}

function setLimit(n: number) {
  limit.value = n
  offset.value = 0
  loadAccounts()
}

function toggleOrder() {
  order.value = order.value === 'desc' ? 'asc' : 'desc'
  offset.value = 0
  loadAccounts()
}

function download(name: string) {
  window.open('/api/download?name=' + encodeURIComponent(name), '_blank')
}

async function loadCodexFiles() {
  try {
    const data = await apiJSON<{
      output_dir: string
      files: { name: string; size: number }[]
    }>('/api/codex-agent')
    codexOutDir.value = data.output_dir || 'codex_auth'
    codexFiles.value = (data.files || []).filter((f) => f.name.endsWith('.json') && f.name !== 'agents.jsonl')
  } catch {
    codexFiles.value = []
  }
}

/** Batch: access_token.txt → Codex Agent Identity auth.json under data/codex_auth/ */
async function generateCodexAgents(source: 'token_file' | 'accounts') {
  if (codexBusy.value) return
  codexBusy.value = true
  codexMsg.value = ''
  try {
    const body =
      source === 'token_file'
        ? { from_access_token_file: true }
        : { from_accounts: true }
    const data = await apiJSON<{
      total: number
      success: number
      failed: number
      output_dir: string
    }>('/api/codex-agent', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    })
    const msg = `Codex Agent：成功 ${data.success}/${data.total}，失败 ${data.failed} → data/${data.output_dir}/`
    codexMsg.value = msg
    if (data.success > 0) toastSuccess(msg)
    else toastError(msg)
    await loadCodexFiles()
  } catch (e) {
    const msg = (e as Error).message || 'Codex Agent 注册失败'
    codexMsg.value = msg
    toastError(msg)
  } finally {
    codexBusy.value = false
  }
}

/** Push data/codex_auth/*.json → codex2api agent-identity/import endpoints */
async function importCodexToAPI() {
  if (importBusy.value) return
  importBusy.value = true
  codexMsg.value = ''
  try {
    const data = await apiJSON<{
      file_count: number
      import?: {
        ok?: boolean
        error?: string
        results?: {
          name: string
          imported?: number
          failed?: number
          total?: number
          error?: string
          ok?: boolean
        }[]
      }
    }>('/api/codex-agent/import', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
    })
    const imp = data.import
    if (!imp) {
      toastError('无导入结果')
      return
    }
    if (imp.error) {
      codexMsg.value = imp.error
      toastError(imp.error)
      return
    }
    const parts = (imp.results || []).map((r) => {
      if (!r.ok) return `${r.name}: fail ${r.error || ''}`
      return `${r.name}: +${r.imported ?? 0}/${r.total ?? 0}`
    })
    const msg = `Agent Identity 导入 ${data.file_count} 个文件 · ${parts.join(' · ') || 'ok'}`
    codexMsg.value = msg
    if (imp.ok) toastSuccess(msg)
    else toastError(msg)
  } catch (e) {
    const msg = (e as Error).message || '导入失败'
    codexMsg.value = msg
    toastError(msg)
  } finally {
    importBusy.value = false
  }
}

/** Format created_at for the results table. */
function formatTime(raw?: string | null) {
  if (!raw) return '—'
  const s = String(raw).trim()
  if (!s) return '—'
  try {
    const d = new Date(s)
    if (Number.isNaN(d.getTime())) {
      // already local-ish string
      return s.length > 19 ? s.slice(0, 19).replace('T', ' ') : s
    }
    // YYYY-MM-DD HH:mm:ss local
    const pad = (n: number) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  } catch {
    return s
  }
}

onMounted(() => {
  loadAccounts()
  loadCodexFiles()
})
</script>

<template>
  <section class="animate-fade-in flex min-h-0 flex-1 flex-col gap-3">
    <div class="section-bar shrink-0 !gap-2 !py-2">
      <button type="button" class="btn btn-ghost btn-sm" :disabled="loading" @click="loadAccounts">
        {{ loading ? '加载中…' : '刷新' }}
      </button>
      <button type="button" class="btn btn-ghost btn-sm" @click="download('access_token.txt')">
        access_token.txt
      </button>
      <button type="button" class="btn btn-ghost btn-sm" @click="download('registered_accounts.json')">
        导出 JSON
      </button>
      <button type="button" class="btn btn-ghost btn-sm" @click="download('registered_accounts.jsonl')">
        导出 JSONL
      </button>
      <button
        type="button"
        class="btn btn-ghost btn-sm"
        :disabled="codexBusy"
        title="从 access_token.txt 批量注册 Codex Agent Identity"
        @click="generateCodexAgents('token_file')"
      >
        {{ codexBusy ? 'Codex…' : 'Codex Agent (token 文件)' }}
      </button>
      <button
        type="button"
        class="btn btn-ghost btn-sm"
        :disabled="codexBusy"
        title="从已保存账号的 access_token 批量注册"
        @click="generateCodexAgents('accounts')"
      >
        Codex Agent (账号库)
      </button>
      <button
        v-if="codexFiles.length"
        type="button"
        class="btn btn-ghost btn-sm"
        title="下载 agents.jsonl 汇总"
        @click="download(codexOutDir + '/agents.jsonl')"
      >
        下载 agents.jsonl ({{ codexFiles.length }})
      </button>
      <button
        type="button"
        class="btn btn-ghost btn-sm"
        :disabled="importBusy || !codexFiles.length"
        title="将 codex_auth/*.json 批量推送到 mode=agent_identity 的导入 API"
        @click="importCodexToAPI"
      >
        {{ importBusy ? '导入中…' : '推送到 codex2api' }}
      </button>

      <div class="ml-auto flex flex-wrap items-center gap-1.5">
        <span class="pill pill-neutral ring-1">共 <span class="font-semibold ui-heading">{{ total }}</span></span>
        <span class="pill pill-k12 ring-1">本页 k12 {{ k12OnPage }}</span>
        <select
          class="field !w-auto !py-1 text-xs"
          :value="limit"
          @change="setLimit(Number(($event.target as HTMLSelectElement).value))"
        >
          <option :value="50">50 / 页</option>
          <option :value="100">100 / 页</option>
          <option :value="200">200 / 页</option>
          <option :value="500">500 / 页</option>
        </select>
        <button type="button" class="btn btn-ghost btn-sm" @click="toggleOrder">
          {{ order === 'desc' ? '最新优先' : '最早优先' }}
        </button>
      </div>
    </div>

    <div class="card thin-scroll min-h-0 flex-1 overflow-auto !p-0">
      <table class="w-full min-w-[860px]">
        <thead
          class="sticky top-0 z-[1] backdrop-blur"
          style="background: color-mix(in srgb, var(--app-surface-solid) 92%, transparent)"
        >
          <tr>
            <th class="th">邮箱</th>
            <th class="th whitespace-nowrap">时间</th>
            <th class="th">plan</th>
            <th class="th">join</th>
            <th class="th">approve</th>
            <th class="th">elevate</th>
            <th class="th">import</th>
            <th class="th">account_id</th>
            <th class="th">AT</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(a, i) in accounts" :key="a.email || a.chatgpt_account_id || i" class="transition ui-hover">
            <td class="td font-mono text-xs font-medium ui-heading">{{ a.email || '-' }}</td>
            <td
              class="td whitespace-nowrap font-mono text-[11px] ui-muted"
              :title="a.created_at || ''"
            >
              {{ formatTime(a.created_at) }}
            </td>
            <td class="td">
              <span class="pill ring-1" :class="planPillCls(a.plan_type)">{{ a.plan_type || '-' }}</span>
            </td>
            <td class="td">
              <span class="pill ring-1" :class="pillCls(a.join_status)">{{ a.join_status || '-' }}</span>
            </td>
            <td class="td">
              <span class="pill ring-1" :class="pillCls(a.approve_status)">{{ a.approve_status || '-' }}</span>
            </td>
            <td class="td">
              <span class="pill ring-1" :class="pillCls(a.elevate_status)">{{ a.elevate_status || '-' }}</span>
            </td>
            <td class="td">
              <span class="pill ring-1" :class="pillCls(a.import_status)">{{ a.import_status || '-' }}</span>
            </td>
            <td class="td font-mono text-xs ui-muted">
              {{ (a.chatgpt_account_id || '').slice(0, 12) || '-' }}
            </td>
            <td class="td">
              <span class="pill ring-1" :class="a.has_access_token ? 'pill-ok' : 'pill-fail'">
                {{ a.has_access_token ? 'yes' : 'no' }}
              </span>
            </td>
          </tr>
          <tr v-if="!accounts.length && !loading">
            <td class="td text-center" colspan="9">
              <div class="py-14">
                <p class="text-sm ui-muted">暂无账号</p>
                <p class="mt-1 text-xs ui-faint">去「运行」页启动流水线后在此查看结果</p>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- Pagination -->
    <div class="section-bar shrink-0 !justify-between !py-2">
      <span class="text-xs ui-muted">显示 {{ rangeLabel }}</span>
      <div class="flex items-center gap-1.5">
        <button type="button" class="btn btn-ghost btn-sm" :disabled="page <= 1 || loading" @click="goPage(1)">
          首页
        </button>
        <button type="button" class="btn btn-ghost btn-sm" :disabled="page <= 1 || loading" @click="goPage(page - 1)">
          上一页
        </button>
        <span class="px-2 text-xs font-medium ui-heading">{{ page }} / {{ pages }}</span>
        <button
          type="button"
          class="btn btn-ghost btn-sm"
          :disabled="page >= pages || loading"
          @click="goPage(page + 1)"
        >
          下一页
        </button>
        <button
          type="button"
          class="btn btn-ghost btn-sm"
          :disabled="page >= pages || loading"
          @click="goPage(pages)"
        >
          末页
        </button>
      </div>
    </div>
  </section>
</template>
