<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { fmtSize } from '../api'
import type { DataFile } from '../types'

const props = defineProps<{
  modelValue: string
  files: DataFile[]
  /** Extra names always listed even if missing from files */
  allowNames?: string[]
  emptyText?: string
  placeholder?: string
}>()

const emit = defineEmits<{
  'update:modelValue': [v: string]
}>()

const open = ref(false)
const root = ref<HTMLElement | null>(null)
const trigger = ref<HTMLElement | null>(null)
const menu = ref<HTMLElement | null>(null)

const menuStyle = ref<Record<string, string>>({})

const options = computed(() => {
  const map = new Map<string, number>()
  for (const f of props.files) {
    map.set(f.name, f.size)
  }
  for (const n of props.allowNames || []) {
    if (n && !map.has(n)) map.set(n, -1)
  }
  if (props.modelValue && !map.has(props.modelValue)) {
    map.set(props.modelValue, -1)
  }
  return [...map.entries()]
    .map(([name, size]) => ({ name, size }))
    .sort((a, b) => a.name.localeCompare(b.name))
})

const currentSize = computed(() => {
  const hit = options.value.find((o) => o.name === props.modelValue)
  return hit && hit.size >= 0 ? fmtSize(hit.size) : ''
})

function select(name: string) {
  emit('update:modelValue', name)
  open.value = false
}

function updateMenuPosition() {
  const el = trigger.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  const gap = 4
  const maxH = Math.min(240, window.innerHeight * 0.4)
  const spaceBelow = window.innerHeight - rect.bottom - gap - 8
  const spaceAbove = rect.top - gap - 8
  const openUp = spaceBelow < 120 && spaceAbove > spaceBelow
  const height = Math.min(maxH, openUp ? spaceAbove : spaceBelow)

  const width = Math.max(rect.width, 180)
  let left = rect.left
  if (left + width > window.innerWidth - 8) {
    left = Math.max(8, window.innerWidth - width - 8)
  }

  if (openUp) {
    menuStyle.value = {
      position: 'fixed',
      left: `${left}px`,
      width: `${width}px`,
      bottom: `${window.innerHeight - rect.top + gap}px`,
      top: 'auto',
      maxHeight: `${Math.max(100, height)}px`,
      zIndex: '100',
    }
  } else {
    menuStyle.value = {
      position: 'fixed',
      left: `${left}px`,
      width: `${width}px`,
      top: `${rect.bottom + gap}px`,
      bottom: 'auto',
      maxHeight: `${Math.max(100, height)}px`,
      zIndex: '100',
    }
  }
}

async function toggle() {
  open.value = !open.value
  if (open.value) {
    await nextTick()
    updateMenuPosition()
  }
}

function onDocClick(ev: MouseEvent) {
  const t = ev.target as Node
  if (root.value?.contains(t)) return
  if (menu.value?.contains(t)) return
  open.value = false
}

function onReposition() {
  if (open.value) updateMenuPosition()
}

onMounted(() => {
  document.addEventListener('click', onDocClick, true)
  window.addEventListener('resize', onReposition)
  window.addEventListener('scroll', onReposition, true)
})
onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick, true)
  window.removeEventListener('resize', onReposition)
  window.removeEventListener('scroll', onReposition, true)
})

watch(
  () => props.modelValue,
  () => {
    open.value = false
  },
)

watch(open, async (v) => {
  if (v) {
    await nextTick()
    updateMenuPosition()
  }
})
</script>

<template>
  <div ref="root" class="relative w-full">
    <button
      ref="trigger"
      type="button"
      class="field flex w-full items-center gap-2 !py-2 text-left"
      :aria-expanded="open"
      @click.stop="toggle"
    >
      <span
        class="min-w-0 flex-1 truncate font-mono text-xs"
        :class="modelValue ? 'ui-heading' : 'ui-faint'"
      >
        {{ modelValue || placeholder || emptyText || '请选择…' }}
      </span>
      <span v-if="currentSize" class="shrink-0 text-[10px] ui-faint">{{ currentSize }}</span>
      <svg
        class="h-4 w-4 shrink-0 ui-faint transition"
        :class="open && 'rotate-180'"
        fill="none"
        viewBox="0 0 24 24"
        stroke="currentColor"
        stroke-width="2"
      >
        <path stroke-linecap="round" stroke-linejoin="round" d="M19.5 8.25l-7.5 7.5-7.5-7.5" />
      </svg>
    </button>

    <Teleport to="body">
      <div
        v-if="open"
        ref="menu"
        class="thin-scroll overflow-y-auto rounded-xl border py-1 shadow-float"
        :style="{
          ...menuStyle,
          background: 'var(--app-surface-solid)',
          borderColor: 'var(--app-border)',
        }"
        @click.stop
      >
        <p v-if="!options.length" class="px-3 py-2 text-xs ui-faint">
          {{ emptyText || '暂无文件，请先到「数据文件」上传' }}
        </p>
        <button
          v-for="opt in options"
          :key="opt.name"
          type="button"
          class="flex w-full items-center gap-2 px-3 py-2 text-left text-xs transition"
          :class="
            opt.name === modelValue
              ? 'bg-blue-500/15 font-medium text-blue-700 dark:text-blue-300'
              : 'ui-heading hover:bg-[var(--app-hover)]'
          "
          @click="select(opt.name)"
        >
          <span
            class="flex h-4 w-4 shrink-0 items-center justify-center rounded text-[10px]"
            :class="opt.name === modelValue ? 'text-blue-600 dark:text-blue-400' : 'opacity-0'"
          >
            ✓
          </span>
          <span class="min-w-0 flex-1 truncate font-mono">{{ opt.name }}</span>
          <span v-if="opt.size >= 0" class="shrink-0 tabular-nums ui-faint">{{ fmtSize(opt.size) }}</span>
        </button>
      </div>
    </Teleport>
  </div>
</template>
