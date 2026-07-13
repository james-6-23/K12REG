<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, apiJSON, fmtSize } from '../api'
import type { DataFile } from '../types'
import { toastError, toastSuccess } from '../toast'

const files = ref<DataFile[]>([])
const currentFile = ref<string | null>(null)
const fileText = ref('')
const query = ref('')
const dirty = ref(false)
const openError = ref('')
const blockedFile = ref<DataFile | null>(null)

const MAX_EDITOR = 512 * 1024

function isAccountsDump(name: string) {
  return name.startsWith('registered_accounts')
}

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return files.value
  return files.value.filter((f) => f.name.toLowerCase().includes(q))
})

const commonHints = ['hotmail.txt', 'proxies.txt', 'hotsession.json']

async function loadFiles() {
  const data = await apiJSON<{ files: DataFile[] }>('/api/files')
  files.value = data.files
}

async function openFile(name: string) {
  if (dirty.value && currentFile.value && currentFile.value !== name) {
    if (!confirm('当前文件未保存，切换后修改将丢失。继续？')) return
  }
  openError.value = ''
  blockedFile.value = null
  const meta = files.value.find((f) => f.name === name)
  if (meta && (!meta.editable || meta.size > MAX_EDITOR || isAccountsDump(name))) {
    currentFile.value = name
    fileText.value = ''
    dirty.value = false
    blockedFile.value = meta
    openError.value = isAccountsDump(name)
      ? '账号库文件很大，请到「结果」页分页浏览，或点下载导出。'
      : `文件过大（${fmtSize(meta.size)}），在线编辑上限 512 KB，请下载后用本地编辑器打开。`
    return
  }
  try {
    fileText.value = await apiJSON<string>('/api/file?name=' + encodeURIComponent(name))
    currentFile.value = name
    dirty.value = false
  } catch (e) {
    currentFile.value = name
    fileText.value = ''
    dirty.value = false
    blockedFile.value = meta || null
    openError.value = (e as Error).message || '无法打开文件'
  }
}

async function saveFile() {
  if (!currentFile.value) return
  try {
    await apiJSON('/api/file?name=' + encodeURIComponent(currentFile.value), {
      method: 'PUT',
      headers: { 'content-type': 'text/plain' },
      body: fileText.value,
    })
    dirty.value = false
    toastSuccess('文件已保存')
    await loadFiles()
  } catch (e) {
    toastError((e as Error).message)
  }
}

async function deleteFile(name: string) {
  if (!confirm('删除 ' + name + ' ?')) return
  await apiJSON('/api/file?name=' + encodeURIComponent(name), { method: 'DELETE' })
  if (currentFile.value === name) {
    currentFile.value = null
    fileText.value = ''
    dirty.value = false
  }
  await loadFiles()
}

async function uploadFile(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  const fd = new FormData()
  fd.append('file', file)
  fd.append('name', file.name)
  await api('/api/upload', { method: 'POST', body: fd })
  input.value = ''
  await loadFiles()
}

function download(name: string) {
  window.open('/api/download?name=' + encodeURIComponent(name), '_blank')
}

function onEdit(text: string) {
  fileText.value = text
  if (currentFile.value) dirty.value = true
}

function pickHint(name: string) {
  const hit = files.value.find((f) => f.name === name)
  if (hit?.editable) openFile(name)
}

onMounted(loadFiles)
</script>

<template>
  <section
    class="animate-fade-in grid min-h-0 flex-1 gap-3 overflow-y-auto lg:grid-cols-[minmax(300px,360px)_1fr] lg:overflow-hidden"
  >
    <!-- File list -->
    <div class="card flex max-h-[46vh] min-h-[220px] flex-col !p-0 lg:max-h-none lg:min-h-0">
      <div class="flex shrink-0 flex-col gap-2 border-b ui-border px-3 py-2.5">
        <div class="flex items-center gap-2">
          <button type="button" class="btn btn-ghost btn-sm" @click="loadFiles">刷新</button>
          <label class="btn btn-ghost btn-sm cursor-pointer">
            上传
            <input type="file" class="hidden" @change="uploadFile" />
          </label>
          <span class="ml-auto text-[11px] ui-faint">{{ files.length }} 个</span>
        </div>
        <input
          v-model="query"
          type="search"
          class="field !py-1.5 text-xs"
          placeholder="搜索文件名…"
        />
        <div class="flex flex-wrap gap-1">
          <button
            v-for="h in commonHints"
            :key="h"
            type="button"
            class="ui-chip transition hover:ring-blue-400/30"
            @click="pickHint(h)"
          >
            {{ h }}
          </button>
        </div>
      </div>

      <div class="thin-scroll min-h-0 flex-1 overflow-y-auto p-1.5">
        <div
          v-for="f in filtered"
          :key="f.name"
          class="file-item group"
          :class="currentFile === f.name && 'is-active'"
          @click="f.editable && openFile(f.name)"
        >
          <div class="min-w-0 flex-1">
            <div class="truncate font-mono text-[12px] leading-snug ui-heading">{{ f.name }}</div>
            <div class="mt-0.5 text-[10px] ui-faint">{{ fmtSize(f.size) }}</div>
          </div>
          <div class="flex shrink-0 items-center gap-0.5" @click.stop>
            <button
              type="button"
              class="file-action file-action-primary"
              :disabled="!f.editable && !f.text && !isAccountsDump(f.name)"
              :class="!f.editable && 'opacity-70'"
              :title="
                f.editable
                  ? '编辑'
                  : isAccountsDump(f.name) || (f.size > MAX_EDITOR && f.text)
                    ? '过大，请下载 / 结果页查看'
                    : '不可在线编辑'
              "
              @click="openFile(f.name)"
            >
              {{ f.editable ? '编辑' : f.text || isAccountsDump(f.name) ? '查看' : '二进制' }}
            </button>
            <button type="button" class="file-action" title="下载" @click="download(f.name)">下载</button>
            <button type="button" class="file-action file-action-danger" title="删除" @click="deleteFile(f.name)">
              删除
            </button>
          </div>
        </div>

        <div v-if="!filtered.length" class="px-3 py-12 text-center">
          <div
            class="mx-auto mb-2 flex h-10 w-10 items-center justify-center rounded-xl ui-surface ring-1"
            style="border-color: var(--app-border); color: var(--app-faint)"
          >
            <svg class="h-5 w-5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5">
              <path
                stroke-linecap="round"
                stroke-linejoin="round"
                d="M19.5 14.25v-2.625a3.375 3.375 0 00-3.375-3.375h-1.5A1.125 1.125 0 0113.5 7.125v-1.5a3.375 3.375 0 00-3.375-3.375H8.25m0 12.75h7.5m-7.5 3H12M10.5 2.25H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 00-9-9z"
              />
            </svg>
          </div>
          <p class="text-sm ui-muted">{{ query ? '无匹配文件' : '暂无文件' }}</p>
        </div>
      </div>
    </div>

    <!-- Editor -->
    <div class="card flex min-h-[280px] flex-col !p-0 lg:min-h-0">
      <div class="flex shrink-0 items-center gap-3 border-b ui-border px-3 py-2.5 sm:px-4">
        <div class="min-w-0 flex-1">
          <div class="text-[10px] font-medium uppercase tracking-wider ui-faint">当前文件</div>
          <div class="flex min-w-0 items-center gap-2">
            <div class="truncate font-mono text-sm" :class="currentFile ? 'ui-heading' : 'ui-faint'">
              {{ currentFile || '未选择文件' }}
            </div>
            <span
              v-if="dirty"
              class="shrink-0 rounded-full bg-amber-500/15 px-1.5 py-0.5 text-[10px] font-medium text-amber-600 ring-1 ring-amber-500/25 dark:text-amber-300"
            >未保存</span>
          </div>
        </div>
        <button
          v-if="currentFile"
          type="button"
          class="btn btn-ghost btn-sm"
          @click="download(currentFile)"
        >
          下载
        </button>
        <button type="button" class="btn btn-primary btn-sm" :disabled="!currentFile || !dirty || !!openError" @click="saveFile">
          保存
        </button>
      </div>

      <!-- Large / blocked file notice -->
      <div
        v-if="openError"
        class="flex min-h-[200px] flex-1 flex-col items-center justify-center gap-3 px-6 text-center lg:min-h-0"
      >
        <div class="max-w-md space-y-2">
          <p class="text-sm font-medium ui-heading">{{ currentFile }}</p>
          <p class="text-sm ui-muted">{{ openError }}</p>
          <p v-if="blockedFile" class="text-xs ui-faint">大小 {{ fmtSize(blockedFile.size) }}</p>
          <div class="flex flex-wrap items-center justify-center gap-2 pt-2">
            <button
              v-if="currentFile"
              type="button"
              class="btn btn-primary btn-sm"
              @click="download(currentFile!)"
            >
              下载文件
            </button>
            <button
              v-if="currentFile && isAccountsDump(currentFile)"
              type="button"
              class="btn btn-ghost btn-sm"
              @click="download('registered_accounts.json')"
            >
              导出 JSON 数组
            </button>
          </div>
        </div>
      </div>

      <textarea
        v-else
        :value="fileText"
        spellcheck="false"
        :disabled="!currentFile"
        class="field thin-scroll min-h-[200px] w-full flex-1 resize-none rounded-none border-0 font-mono text-[13px] leading-relaxed focus:ring-0 disabled:opacity-40 lg:min-h-0"
        placeholder="从左侧选择一个文本文件进行编辑"
        @input="onEdit(($event.target as HTMLTextAreaElement).value)"
      />
    </div>
  </section>
</template>
