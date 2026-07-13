<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import type { RunStatus, TabId, TabItem } from '../types'
import { getStoredTheme, toggleTheme, type ThemeMode } from '../theme'

const props = defineProps<{
  tabs: TabItem[]
  tab: TabId
  runStatus: RunStatus
  dataDir: string
}>()

const emit = defineEmits<{
  'update:tab': [id: TabId]
  logout: []
}>()

const theme = ref<ThemeMode>('dark')

onMounted(() => {
  theme.value = getStoredTheme()
})

function onToggleTheme() {
  theme.value = toggleTheme()
}

const icons: Record<string, string> = {
  run: 'M5.25 5.653c0-.856.917-1.398 1.667-.986l11.54 6.347a1.125 1.125 0 010 1.972l-11.54 6.347a1.125 1.125 0 01-1.667-.986V5.653z',
  settings:
    'M9.594 3.94c.09-.542.56-.94 1.11-.94h2.593c.55 0 1.02.398 1.11.94l.213 1.281c.063.374.313.686.645.87.074.04.147.083.22.127.325.196.72.257 1.075.124l1.217-.456a1.125 1.125 0 011.37.49l1.296 2.247a1.125 1.125 0 01-.26 1.431l-1.003.827c-.293.241-.438.613-.43.992a7.723 7.723 0 010 .255c-.008.378.137.75.43.991l1.004.827c.424.35.534.955.26 1.43l-1.298 2.247a1.125 1.125 0 01-1.369.491l-1.217-.456c-.355-.133-.75-.072-1.076.124a6.47 6.47 0 01-.22.128c-.331.183-.581.495-.644.869l-.213 1.281c-.09.543-.56.94-1.11.94h-2.594c-.55 0-1.019-.398-1.11-.94l-.213-1.281c-.062-.374-.312-.686-.644-.87a6.52 6.52 0 01-.22-.127c-.325-.196-.72-.257-1.076-.124l-1.217.456a1.125 1.125 0 01-1.369-.49l-1.297-2.247a1.125 1.125 0 01.26-1.431l1.004-.827c.292-.24.437-.613.43-.991a6.932 6.932 0 010-.255c.007-.38-.138-.751-.43-.992l-1.004-.827a1.125 1.125 0 01-.26-1.43l1.297-2.247a1.125 1.125 0 011.37-.491l1.216.456c.356.133.751.072 1.076-.124.072-.044.146-.086.22-.128.332-.183.582-.495.644-.869l.214-1.28z',
  files:
    'M2.25 12.75V12A2.25 2.25 0 014.5 9.75h15A2.25 2.25 0 0121.75 12v.75m-8.69-6.44l-2.12-2.12a1.5 1.5 0 00-1.061-.44H4.5A2.25 2.25 0 002.25 6v12a2.25 2.25 0 002.25 2.25h15A2.25 2.25 0 0021.75 18V9a2.25 2.25 0 00-2.25-2.25h-5.379a1.5 1.5 0 01-1.06-.44z',
  results:
    'M3 13.125C3 12.504 3.504 12 4.125 12h2.25c.621 0 1.125.504 1.125 1.125v6.75C7.5 20.496 6.996 21 6.375 21h-2.25A1.125 1.125 0 013 19.875v-6.75zM9.75 8.625c0-.621.504-1.125 1.125-1.125h2.25c.621 0 1.125.504 1.125 1.125v11.25c0 .621-.504 1.125-1.125 1.125h-2.25a1.125 1.125 0 01-1.125-1.125V8.625zM16.5 4.125c0-.621.504-1.125 1.125-1.125h2.25C20.496 3 21 3.504 21 4.125v15.75c0 .621-.504 1.125-1.125 1.125h-2.25a1.125 1.125 0 01-1.125-1.125V4.125z',
  schedule:
    'M12 6v6h4.5m4.5 0a9 9 0 11-18 0 9 9 0 0118 0z',
  advanced:
    'M17.25 6.75L22.5 12l-5.25 5.25m-10.5 0L1.5 12l5.25-5.25m7.5-3l-4.5 16.5',
}

const statusBadge = computed(() => {
  const s = props.runStatus
  if (s.running)
    return {
      text: '运行中',
      cls: 'bg-emerald-500/15 text-emerald-700 ring-1 ring-emerald-400/30 dark:text-emerald-300 dark:ring-emerald-400/25',
      dot: 'bg-emerald-400 animate-pulse',
    }
  if (s.exit_code !== null && s.exit_code !== undefined)
    return {
      text: `结束 · ${s.exit_code}`,
      cls: 'bg-blue-500/15 text-blue-700 ring-1 ring-blue-400/30 dark:text-blue-300 dark:ring-blue-400/25',
      dot: 'bg-blue-400',
    }
  return {
    text: '空闲',
    cls: 'bg-ink-800 ui-muted ring-1 ui-border',
    dot: 'bg-slate-400',
  }
})
</script>

<template>
  <!-- Desktop sidebar -->
  <aside class="ui-sidebar hidden h-full w-[200px] shrink-0 flex-col border-r backdrop-blur-xl lg:flex xl:w-[220px]">
    <div class="flex items-center gap-2.5 px-4 py-4">
      <div
        class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-blue-500 to-indigo-600 text-sm font-bold text-white shadow-glow"
      >
        K
      </div>
      <div class="min-w-0 leading-tight">
        <div class="truncate text-sm font-semibold tracking-tight ui-heading">K12REG</div>
        <div class="truncate text-[11px] ui-faint">Pipeline</div>
      </div>
    </div>

    <nav class="flex flex-1 flex-col gap-0.5 overflow-y-auto px-2.5 pb-2">
      <button
        v-for="t in tabs"
        :key="t.id"
        type="button"
        class="group flex items-center gap-2.5 rounded-xl px-2.5 py-2 text-left text-[13px] font-medium transition ui-hover"
        :class="
          tab === t.id
            ? 'bg-blue-500/15 ui-heading ring-1 ring-blue-400/25'
            : 'ui-muted'
        "
        @click="emit('update:tab', t.id)"
      >
        <svg
          class="h-4 w-4 shrink-0"
          :class="tab === t.id ? 'text-blue-500 dark:text-blue-400' : 'ui-faint'"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          stroke-width="1.7"
        >
          <path stroke-linecap="round" stroke-linejoin="round" :d="icons[t.id] || icons.run" />
        </svg>
        <span class="truncate">{{ t.label }}</span>
        <span
          v-if="t.id === 'run' && runStatus.running"
          class="ml-auto h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-400 pulse-live"
        />
      </button>
    </nav>

    <div class="space-y-2 border-t ui-border p-3">
      <div class="pill w-full justify-center" :class="statusBadge.cls">
        <span
          class="h-1.5 w-1.5 rounded-full"
          :class="[statusBadge.dot, runStatus.running && 'pulse-live']"
        />
        <span>{{ statusBadge.text }}</span>
      </div>
      <p class="truncate px-0.5 font-mono text-[10px] ui-faint" :title="dataDir">{{ dataDir }}</p>
      <div class="grid grid-cols-2 gap-1.5">
        <button
          type="button"
          class="btn btn-ghost btn-sm justify-center"
          :title="theme === 'dark' ? '切换浅色' : '切换深色'"
          @click="onToggleTheme"
        >
          <!-- sun -->
          <svg v-if="theme === 'dark'" class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
            <path stroke-linecap="round" stroke-linejoin="round" d="M12 3v2.25m6.364.386l-1.591 1.591M21 12h-2.25m-.386 6.364l-1.591-1.591M12 18.75V21m-4.773-4.227l-1.591 1.591M5.25 12H3m4.227-4.773L5.636 5.636M15.75 12a3.75 3.75 0 11-7.5 0 3.75 3.75 0 017.5 0z" />
          </svg>
          <!-- moon -->
          <svg v-else class="h-3.5 w-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
            <path stroke-linecap="round" stroke-linejoin="round" d="M21.752 15.002A9.718 9.718 0 0118 15.75c-5.385 0-9.75-4.365-9.75-9.75 0-1.33.266-2.597.748-3.752A9.753 9.753 0 003 11.25C3 16.635 7.365 21 12.75 21a9.753 9.753 0 009.002-5.998z" />
          </svg>
          {{ theme === 'dark' ? '浅色' : '深色' }}
        </button>
        <button type="button" class="btn btn-ghost btn-sm justify-center" @click="emit('logout')">
          退出
        </button>
      </div>
    </div>
  </aside>

  <!-- Mobile top bar + tabs -->
  <header class="ui-sidebar shrink-0 border-b backdrop-blur-xl lg:hidden">
    <div class="flex items-center gap-2.5 px-3 py-2.5">
      <div
        class="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-gradient-to-br from-blue-500 to-indigo-600 text-xs font-bold text-white shadow-glow"
      >
        K
      </div>
      <div class="min-w-0 flex-1 leading-tight">
        <div class="text-sm font-semibold ui-heading">K12REG</div>
        <div class="truncate font-mono text-[10px] ui-faint" :title="dataDir">{{ dataDir }}</div>
      </div>
      <button type="button" class="btn btn-ghost btn-sm !px-2" :title="theme === 'dark' ? '浅色' : '深色'" @click="onToggleTheme">
        <svg v-if="theme === 'dark'" class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
          <path stroke-linecap="round" stroke-linejoin="round" d="M12 3v2.25m6.364.386l-1.591 1.591M21 12h-2.25m-.386 6.364l-1.591-1.591M12 18.75V21m-4.773-4.227l-1.591 1.591M5.25 12H3m4.227-4.773L5.636 5.636M15.75 12a3.75 3.75 0 11-7.5 0 3.75 3.75 0 017.5 0z" />
        </svg>
        <svg v-else class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
          <path stroke-linecap="round" stroke-linejoin="round" d="M21.752 15.002A9.718 9.718 0 0118 15.75c-5.385 0-9.75-4.365-9.75-9.75 0-1.33.266-2.597.748-3.752A9.753 9.753 0 003 11.25C3 16.635 7.365 21 12.75 21a9.753 9.753 0 009.002-5.998z" />
        </svg>
      </button>
      <span class="pill shrink-0" :class="statusBadge.cls">
        <span
          class="h-1.5 w-1.5 rounded-full"
          :class="[statusBadge.dot, runStatus.running && 'pulse-live']"
        />
        <span class="max-w-[5.5rem] truncate">{{ statusBadge.text }}</span>
      </span>
      <button type="button" class="btn btn-ghost btn-sm shrink-0 px-2" @click="emit('logout')">退出</button>
    </div>
    <nav class="flex gap-0.5 overflow-x-auto px-2 pb-2 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      <button
        v-for="t in tabs"
        :key="t.id"
        type="button"
        class="tab-btn shrink-0"
        :class="tab === t.id && 'tab-btn-active'"
        @click="emit('update:tab', t.id)"
      >
        {{ t.label }}
      </button>
    </nav>
  </header>
</template>
