<script setup lang="ts">
defineProps<{
  saving?: boolean
  disabled?: boolean
  label?: string
  showReload?: boolean
}>()

const emit = defineEmits<{
  save: []
  reload: []
}>()
</script>

<template>
  <div class="pointer-events-none fixed bottom-5 right-4 z-[70] flex items-center gap-2 sm:right-6">
    <button
      v-if="showReload !== false"
      type="button"
      class="pointer-events-auto btn btn-ghost shadow-card ui-surface ring-1"
      style="border-color: var(--app-border); background: var(--app-surface-solid)"
      @click="emit('reload')"
    >
      重新读取
    </button>
    <button
      type="button"
      class="pointer-events-auto btn btn-primary min-w-[7.5rem] shadow-glow"
      :disabled="disabled || saving"
      @click="emit('save')"
    >
      <svg v-if="!saving" class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
        <path stroke-linecap="round" stroke-linejoin="round" d="M4.5 12.75l6 6 9-13.5" />
      </svg>
      {{ saving ? '保存中…' : label || '保存' }}
    </button>
  </div>
</template>
