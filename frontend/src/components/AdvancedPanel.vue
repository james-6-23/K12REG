<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { apiJSON } from '../api'
import SaveDock from './SaveDock.vue'
import { toastError, toastSuccess } from '../toast'

const rawText = ref('')
const saving = ref(false)

async function loadRaw() {
  try {
    rawText.value = await apiJSON<string>('/api/settings/raw')
  } catch (e) {
    toastError((e as Error).message)
  }
}

async function saveRaw() {
  try {
    JSON.parse(rawText.value)
  } catch (e) {
    toastError('JSON 格式错误: ' + (e as Error).message)
    return
  }
  saving.value = true
  try {
    await apiJSON('/api/settings/raw', {
      method: 'PUT',
      headers: { 'content-type': 'text/plain' },
      body: rawText.value,
    })
    toastSuccess('已保存，下次启动生效')
  } catch (e) {
    toastError((e as Error).message)
  } finally {
    saving.value = false
  }
}

onMounted(loadRaw)
</script>

<template>
  <section class="animate-fade-in flex min-h-0 flex-1 flex-col gap-3 pb-20">
    <div class="flex min-h-0 flex-1 flex-col overflow-hidden rounded-2xl border shadow-card ui-border">
      <div class="flex shrink-0 items-center gap-2 border-b ui-border px-3 py-2 sm:px-4 ui-surface">
        <span class="pill pill-warn">JSON</span>
        <span class="text-xs ui-muted">settings.json 原始编辑</span>
        <span class="ml-auto text-[11px] ui-faint">approve_max_attempts · mail.wait_timeout …</span>
      </div>
      <textarea
        v-model="rawText"
        spellcheck="false"
        class="field thin-scroll min-h-0 w-full flex-1 resize-none rounded-none border-0 font-mono text-[13px] leading-relaxed focus:ring-0"
        style="background: var(--app-log-to)"
        placeholder='{ "registration": { "total": 1 } }'
      />
    </div>

    <SaveDock :saving="saving" @save="saveRaw" @reload="loadRaw" />
  </section>
</template>
