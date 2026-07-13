<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { apiJSON } from '../api'
import {
  defaultSettings,
  emptyImportEndpoint,
  normalizeImportApi,
  type DataFile,
  type Settings,
} from '../types'
import FileSelect from './FileSelect.vue'
import SaveDock from './SaveDock.vue'
import { toastError, toastSuccess } from '../toast'

const settings = ref<Settings>(defaultSettings())
const idsText = ref('')
const dataFiles = ref<DataFile[]>([])
const saving = ref(false)

const idList = computed(() =>
  idsText.value
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean),
)

const enabledImportCount = computed(
  () => settings.value.import_api.endpoints.filter((e) => e.enabled && e.url.trim()).length,
)

function isTxt(name: string) {
  return name.toLowerCase().endsWith('.txt')
}

/** Mail pool: any .txt except known non-mail files (filename free-form, e.g. 1.txt). */
const mailPoolFiles = computed(() => {
  const skip = new Set(['access_token.txt', 'proxies.txt', 'proxy.txt'])
  return dataFiles.value.filter((f) => isTxt(f.name) && !skip.has(f.name.toLowerCase()))
})

/** Proxy pool: prefer proxy*.txt; still allow any .txt so custom names work. */
const proxyPoolFiles = computed(() => {
  const skip = new Set(['access_token.txt'])
  return dataFiles.value.filter((f) => isTxt(f.name) && !skip.has(f.name.toLowerCase()))
})

const sessionFiles = computed(() => {
  return dataFiles.value.filter(
    (f) => f.name.toLowerCase().endsWith('.json') && !f.name.startsWith('registered_accounts'),
  )
})

watch(idList, (list) => {
  if (!list.length) {
    settings.value.workspace.selected_id = ''
    return
  }
  const sel = settings.value.workspace.selected_id
  if (!sel || !list.some((id) => id.toLowerCase() === sel.toLowerCase())) {
    settings.value.workspace.selected_id = list[0]
  }
})

function addImportEndpoint() {
  const n = settings.value.import_api.endpoints.length + 1
  settings.value.import_api.endpoints.push(emptyImportEndpoint(n))
}

function removeImportEndpoint(i: number) {
  if (settings.value.import_api.endpoints.length <= 1) {
    settings.value.import_api.endpoints = [emptyImportEndpoint(1)]
    return
  }
  settings.value.import_api.endpoints.splice(i, 1)
}

async function loadDataFiles() {
  try {
    const data = await apiJSON<{ files: DataFile[] }>('/api/files')
    dataFiles.value = data.files || []
  } catch {
    dataFiles.value = []
  }
}

async function loadSettings() {
  try {
    await loadDataFiles()
    const s = await apiJSON<Settings>('/api/settings')
    s.registration.mode = 'protocol'
    s.import_api = normalizeImportApi(s.import_api)
    if (!s.mail) s.mail = { mailboxes_file: '', alias_count: 1 }
    if (!s.mail.alias_count || s.mail.alias_count < 1) s.mail.alias_count = 1
    if (s.mail.alias_count > 50) s.mail.alias_count = 50
    if (!s.workspace.selected_id && s.workspace.ids?.length) {
      s.workspace.selected_id = s.workspace.ids[0]
    }
    settings.value = s
    idsText.value = (s.workspace.ids || []).join('\n')
  } catch (e) {
    toastError((e as Error).message)
  }
}

async function saveSettings() {
  settings.value.workspace.ids = idList.value
  if (
    settings.value.workspace.selected_id &&
    !settings.value.workspace.ids.some(
      (id) => id.toLowerCase() === settings.value.workspace.selected_id.toLowerCase(),
    )
  ) {
    settings.value.workspace.ids = [settings.value.workspace.selected_id, ...settings.value.workspace.ids]
  }
  if (!settings.value.workspace.selected_id && settings.value.workspace.ids.length) {
    settings.value.workspace.selected_id = settings.value.workspace.ids[0]
  }
  settings.value.registration.mode = 'protocol'
  settings.value.import_api = normalizeImportApi(settings.value.import_api)
  let ac = Number(settings.value.mail.alias_count) || 1
  if (ac < 1) ac = 1
  if (ac > 50) ac = 50
  settings.value.mail.alias_count = ac
  saving.value = true
  try {
    await apiJSON('/api/settings', {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(settings.value),
    })
    toastSuccess('设置已保存，下次启动生效')
    await loadDataFiles()
  } catch (e) {
    toastError((e as Error).message)
  } finally {
    saving.value = false
  }
}

onMounted(loadSettings)
</script>

<template>
  <section class="animate-fade-in space-y-3 pb-24">
    <!-- Top: 注册 | Workspace | 代理 -->
    <div class="grid gap-3 xl:grid-cols-3">
      <!-- 注册 -->
      <div class="card !p-4 space-y-3">
        <div class="flex items-center gap-2.5">
          <div class="icon-box !h-8 !w-8 !rounded-lg">
            <svg class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
              <path stroke-linecap="round" stroke-linejoin="round" d="M9.594 3.94c.09-.542.56-.94 1.11-.94h2.593c.55 0 1.02.398 1.11.94l.213 1.281c.063.374.313.686.645.87.074.04.147.083.22.127.325.196.72.257 1.075.124l1.217-.456a1.125 1.125 0 011.37.49l1.296 2.247a1.125 1.125 0 01-.26 1.431l-1.003.827c-.293.241-.438.613-.43.992a7.723 7.723 0 010 .255c-.008.378.137.75.43.991l1.004.827c.424.35.534.955.26 1.43l-1.298 2.247a1.125 1.125 0 01-1.369.491l-1.217-.456c-.355-.133-.75-.072-1.076.124a6.47 6.47 0 01-.22.128c-.331.183-.581.495-.644.869l-.213 1.281c-.09.543-.56.94-1.11.94h-2.594c-.55 0-1.019-.398-1.11-.94l-.213-1.281c-.062-.374-.312-.686-.644-.87a6.52 6.52 0 01-.22-.127c-.325-.196-.72-.257-1.076-.124l-1.217.456a1.125 1.125 0 01-1.369-.49l-1.297-2.247a1.125 1.125 0 01.26-1.431l1.004-.827c.292-.24.437-.613.43-.991a6.932 6.932 0 010-.255c.007-.38-.138-.751-.43-.992l-1.004-.827a1.125 1.125 0 01-.26-1.43l1.297-2.247a1.125 1.125 0 011.37-.491l1.216.456c.356.133.751.072 1.076-.124.072-.044.146-.086.22-.128.332-.183.582-.495.644-.869l.214-1.28z" />
              <path stroke-linecap="round" stroke-linejoin="round" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
            </svg>
          </div>
          <div>
            <h3 class="card-title">注册</h3>
            <p class="hint">数量 · 并发 · 门控</p>
          </div>
        </div>
        <div class="grid grid-cols-2 gap-2.5">
          <div>
            <label class="label !mb-1">数量</label>
            <input v-model.number="settings.registration.total" type="number" min="1" class="field w-full !py-2" />
          </div>
          <div>
            <label class="label !mb-1">并发</label>
            <input v-model.number="settings.registration.threads" type="number" min="1" class="field w-full !py-2" />
          </div>
          <div>
            <label class="label !mb-1">模式</label>
            <div class="field flex w-full items-center gap-1.5 !py-2 text-sm text-slate-300">
              <span class="h-1.5 w-1.5 rounded-full bg-blue-400" />
              协议
            </div>
          </div>
          <div>
            <label class="label !mb-1">门控</label>
            <select v-model="settings.registration.pipeline_gate" class="field w-full !py-2">
              <option value="reg">reg</option>
              <option value="full">full</option>
              <option value="full_success">full_success</option>
            </select>
          </div>
        </div>
      </div>

      <!-- Workspace -->
      <div class="card !p-4 space-y-3">
        <div class="flex items-center justify-between gap-2">
          <div class="flex items-center gap-2.5">
            <div class="icon-box !h-8 !w-8 !rounded-lg">
              <svg class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
                <path stroke-linecap="round" stroke-linejoin="round" d="M2.25 12.75V12A2.25 2.25 0 014.5 9.75h15A2.25 2.25 0 0121.75 12v.75m-8.69-6.44l-2.12-2.12a1.5 1.5 0 00-1.061-.44H4.5A2.25 2.25 0 002.25 6v12a2.25 2.25 0 002.25 2.25h15A2.25 2.25 0 0021.75 18V9a2.25 2.25 0 00-2.25-2.25h-5.379a1.5 1.5 0 01-1.06-.44z" />
              </svg>
            </div>
            <div>
              <h3 class="card-title">Workspace</h3>
              <p class="hint">当前选用一个</p>
            </div>
          </div>
          <label class="toggle" title="启用">
            <input v-model="settings.workspace.enabled" type="checkbox" />
            <span class="toggle-track"><span class="toggle-thumb" /></span>
          </label>
        </div>
        <div>
          <label class="label !mb-1">当前工作区</label>
          <select
            v-model="settings.workspace.selected_id"
            class="field w-full !py-2 font-mono text-[11px]"
            :disabled="!idList.length"
          >
            <option v-if="!idList.length" value="">先填写下方编号池</option>
            <option v-for="id in idList" :key="id" :value="id">{{ id }}</option>
          </select>
        </div>
        <div>
          <label class="label !mb-1">编号池（每行一个）</label>
          <textarea
            v-model="idsText"
            spellcheck="false"
            class="field thin-scroll h-20 w-full resize-y font-mono text-[11px] leading-relaxed"
            placeholder="uuid-1&#10;uuid-2"
          />
        </div>
        <div class="grid grid-cols-1 gap-2.5 sm:grid-cols-[1fr_auto] sm:items-end">
          <div>
            <label class="label !mb-1">母号 session</label>
            <FileSelect
              v-model="settings.workspace.manager_session_file"
              :files="sessionFiles"
              empty-text="请先上传 .json"
              placeholder="选择 session 文件"
            />
          </div>
          <label class="flex h-[38px] cursor-pointer items-center gap-2 text-xs text-slate-400">
            <input
              v-model="settings.workspace.approve_requests"
              type="checkbox"
              class="h-3.5 w-3.5 rounded accent-blue-500"
            />
            自动批准
          </label>
        </div>
      </div>

      <!-- 代理 & 邮箱 -->
      <div class="card !p-4 space-y-3">
        <div class="flex items-center gap-2.5">
          <div class="icon-box !h-8 !w-8 !rounded-lg">
            <svg class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
              <path stroke-linecap="round" stroke-linejoin="round" d="M12 21a9.004 9.004 0 008.716-6.747M12 21a9.004 9.004 0 01-8.716-6.747M12 21c2.485 0 4.5-4.03 4.5-9S14.485 3 12 3m0 18c-2.485 0-4.5-4.03-4.5-9S9.515 3 12 3m0 0a8.997 8.997 0 017.843 4.582M12 3a8.997 8.997 0 00-7.843 4.582m15.686 0A11.953 11.953 0 0112 10.5c-2.998 0-5.74-1.1-7.843-2.918m15.686 0A8.959 8.959 0 0121 12c0 .778-.099 1.533-.284 2.253m0 0A17.919 17.919 0 0112 16.5c-3.162 0-6.133-.815-8.716-2.247m0 0A9.015 9.015 0 013 12c0-1.605.42-3.113 1.157-4.418" />
            </svg>
          </div>
          <div>
            <h3 class="card-title">代理 &amp; 邮箱</h3>
            <p class="hint">按文件名选择 · 内容格式正确即可</p>
          </div>
          <button type="button" class="btn btn-ghost btn-sm ml-auto" title="刷新文件列表" @click="loadDataFiles">
            刷新列表
          </button>
        </div>
        <div class="space-y-3">
          <div class="relative z-[2]">
            <label class="label !mb-1">邮箱池文件</label>
            <FileSelect
              v-model="settings.mail.mailboxes_file"
              :files="mailPoolFiles"
              empty-text="请先在「数据文件」上传邮箱池 .txt"
              placeholder="选择邮箱池文件"
            />
            <p class="hint mt-1.5 leading-relaxed">
              任意文件名均可（如 1.txt / outlook.txt）。行格式：
              <span class="font-mono text-[10px] ui-muted">email----password----refresh----client_id</span>
            </p>
          </div>

          <div>
            <label class="label !mb-1">邮箱别名数量 alias_count</label>
            <input
              v-model.number="settings.mail.alias_count"
              type="number"
              min="1"
              max="50"
              class="field w-full !py-2"
            />
            <p class="hint mt-1.5 leading-relaxed">
              1 = 不用别名；大于 1 时每个真实邮箱扩成多个
              <span class="font-mono text-[10px] ui-muted">local+xxxxN@domain</span>
              注册，OTP 仍进原邮箱。建议 1–5，过大易风控。
            </p>
          </div>

          <div class="relative z-[1] border-t pt-3" style="border-color: var(--app-border-soft)">
            <label class="label !mb-1">代理池文件</label>
            <FileSelect
              v-model="settings.proxy.proxies_file"
              :files="proxyPoolFiles"
              empty-text="请先上传代理 .txt"
              placeholder="选择代理池文件"
            />
          </div>

          <div class="grid grid-cols-2 gap-2.5">
            <div>
              <label class="label !mb-1">协议</label>
              <select v-model="settings.proxy.default_protocol" class="field w-full !py-2">
                <option value="socks5">socks5</option>
                <option value="http">http</option>
                <option value="https">https</option>
              </select>
            </div>
            <div>
              <label class="label !mb-1">FlareSolverr</label>
              <input
                v-model="settings.proxy.flaresolverr_url"
                class="field w-full !py-2 font-mono text-[11px]"
                placeholder="可选"
              />
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Bottom: 导入 API 紧凑列表 -->
    <div class="card !p-4">
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <div class="flex items-center gap-2.5">
          <div class="icon-box !h-8 !w-8 !rounded-lg">
            <svg class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
              <path stroke-linecap="round" stroke-linejoin="round" d="M7.5 21L3 16.5m0 0L7.5 12M3 16.5h13.5m0-13.5L21 7.5m0 0L16.5 12M21 7.5H7.5" />
            </svg>
          </div>
          <div>
            <h3 class="card-title">导入 API</h3>
            <p class="hint">
              已启用 <span class="text-slate-300">{{ enabledImportCount }}</span> /
              {{ settings.import_api.endpoints.length }} · 成功账号会推送到全部启用端点
            </p>
          </div>
        </div>
        <button type="button" class="btn btn-ghost btn-sm ml-auto" @click="addImportEndpoint">+ 添加</button>
      </div>

      <!-- Desktop header -->
      <div
        class="mb-1.5 hidden grid-cols-[auto_7rem_1fr_10rem_auto_auto] items-center gap-2 px-2 text-[10px] font-semibold uppercase tracking-wider text-slate-600 lg:grid"
      >
        <span class="w-10 text-center">开</span>
        <span>名称</span>
        <span>URL</span>
        <span>admin_key</span>
        <span class="w-16 text-center">k12</span>
        <span class="w-12" />
      </div>

      <div class="space-y-2">
        <div
          v-for="(ep, i) in settings.import_api.endpoints"
          :key="i"
          class="rounded-xl border border-white/[0.06] bg-ink-900/35 p-2.5 transition"
          :class="ep.enabled ? 'opacity-100' : 'opacity-55'"
        >
          <!-- Desktop row -->
          <div class="hidden items-center gap-2 lg:grid lg:grid-cols-[auto_7rem_1fr_10rem_auto_auto]">
            <label class="toggle mx-1" :title="ep.enabled ? '启用' : '禁用'">
              <input v-model="ep.enabled" type="checkbox" />
              <span class="toggle-track"><span class="toggle-thumb" /></span>
            </label>
            <input v-model="ep.name" class="field !rounded-lg !px-2 !py-1.5 text-sm" placeholder="名称" />
            <input
              v-model="ep.url"
              class="field !rounded-lg !px-2 !py-1.5 font-mono text-xs"
              placeholder="http://host:port"
            />
            <input
              v-model="ep.admin_key"
              class="field !rounded-lg !px-2 !py-1.5 font-mono text-xs"
              placeholder="密钥"
            />
            <label class="flex w-16 cursor-pointer items-center justify-center gap-1 text-xs text-slate-400">
              <input v-model="ep.require_k12" type="checkbox" class="h-3.5 w-3.5 rounded accent-blue-500" />
              k12
            </label>
            <button
              type="button"
              class="btn btn-ghost btn-sm !px-2 text-red-400/80 hover:text-red-300"
              title="删除"
              @click="removeImportEndpoint(i)"
            >
              ✕
            </button>
          </div>

          <!-- Mobile stacked -->
          <div class="space-y-2 lg:hidden">
            <div class="flex items-center gap-2">
              <label class="toggle shrink-0">
                <input v-model="ep.enabled" type="checkbox" />
                <span class="toggle-track"><span class="toggle-thumb" /></span>
              </label>
              <input v-model="ep.name" class="field min-w-0 flex-1 !py-1.5 text-sm" placeholder="名称" />
              <button
                type="button"
                class="btn btn-ghost btn-sm !px-2 text-red-400/80"
                @click="removeImportEndpoint(i)"
              >
                删
              </button>
            </div>
            <input
              v-model="ep.url"
              class="field w-full !py-1.5 font-mono text-xs"
              placeholder="URL http://host:port"
            />
            <div class="flex gap-2">
              <input
                v-model="ep.admin_key"
                class="field min-w-0 flex-1 !py-1.5 font-mono text-xs"
                placeholder="admin_key"
              />
              <label class="flex shrink-0 items-center gap-1.5 text-xs text-slate-400">
                <input v-model="ep.require_k12" type="checkbox" class="h-3.5 w-3.5 rounded accent-blue-500" />
                仅 k12
              </label>
            </div>
          </div>
        </div>
      </div>
    </div>

    <SaveDock :saving="saving" @save="saveSettings" @reload="loadSettings" />
  </section>
</template>
