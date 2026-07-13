<script setup lang="ts">
import { dismissToast, useToasts } from '../toast'

const items = useToasts()

const kindCls: Record<string, string> = {
  success:
    'border-emerald-400/35 bg-emerald-50 text-emerald-900 dark:bg-emerald-500/15 dark:text-emerald-200',
  error: 'border-red-400/35 bg-red-50 text-red-900 dark:bg-red-500/15 dark:text-red-200',
  info: 'border-blue-400/35 bg-blue-50 text-blue-900 dark:bg-blue-500/15 dark:text-blue-200',
}
</script>

<template>
  <div
    class="pointer-events-none fixed top-4 right-4 z-[80] flex w-[min(22rem,calc(100vw-2rem))] flex-col gap-2 sm:top-5 sm:right-6"
  >
    <TransitionGroup name="toast">
      <div
        v-for="t in items"
        :key="t.id"
        class="pointer-events-auto flex items-start gap-2 rounded-xl border px-3.5 py-2.5 text-sm shadow-float backdrop-blur-md"
        :class="kindCls[t.kind] || kindCls.info"
        role="status"
      >
        <span class="mt-0.5 shrink-0 text-base leading-none">
          <template v-if="t.kind === 'success'">✓</template>
          <template v-else-if="t.kind === 'error'">!</template>
          <template v-else>·</template>
        </span>
        <span class="min-w-0 flex-1 leading-snug">{{ t.message }}</span>
        <button
          type="button"
          class="shrink-0 rounded-md px-1 text-xs opacity-60 hover:opacity-100"
          @click="dismissToast(t.id)"
        >
          ✕
        </button>
      </div>
    </TransitionGroup>
  </div>
</template>

<style scoped>
.toast-enter-active,
.toast-leave-active {
  transition: all 0.22s ease;
}
.toast-enter-from,
.toast-leave-to {
  opacity: 0;
  transform: translateY(-8px) scale(0.98);
}
.toast-move {
  transition: transform 0.22s ease;
}
</style>
