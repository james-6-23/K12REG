<script setup lang="ts">
import { ref } from 'vue'

const emit = defineEmits<{
  success: [dataDir: string]
}>()

const password = ref('')
const loginError = ref('')
const loading = ref(false)

async function login() {
  loginError.value = ''
  loading.value = true
  try {
    const fd = new FormData()
    fd.append('password', password.value)
    const res = await fetch('/api/login', {
      method: 'POST',
      body: fd,
      credentials: 'same-origin',
    })
    if (res.ok) {
      password.value = ''
      const me = await fetch('/api/me', { credentials: 'same-origin' }).then((r) => r.json())
      emit('success', me.data_dir || '')
    } else {
      let msg = '登录失败'
      try {
        msg = (await res.json()).detail || msg
      } catch {
        /* ignore */
      }
      loginError.value = msg
    }
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="relative flex h-full min-h-0 items-center justify-center overflow-y-auto p-4">
    <div class="pointer-events-none absolute inset-0 overflow-hidden">
      <div class="absolute -left-24 top-1/4 h-72 w-72 rounded-full bg-blue-600/20 blur-3xl" />
      <div class="absolute -right-16 bottom-1/4 h-64 w-64 rounded-full bg-indigo-500/15 blur-3xl" />
    </div>

    <form class="relative w-full max-w-[400px] animate-fade-in" @submit.prevent="login">
      <div class="card rounded-3xl !p-8 shadow-float">
        <div class="mb-8 text-center">
          <div
            class="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-gradient-to-br from-blue-500 to-indigo-600 text-xl font-bold text-white shadow-glow"
          >
            K
          </div>
          <h1 class="text-xl font-semibold tracking-tight ui-heading">K12REG 控制台</h1>
          <p class="mt-1.5 text-sm ui-muted">注册 · 加入 · 提权 · 导出</p>
        </div>

        <label class="label">访问密码</label>
        <div class="relative">
          <span class="pointer-events-none absolute inset-y-0 left-3 flex items-center text-slate-500">
            <svg class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.8">
              <path
                stroke-linecap="round"
                stroke-linejoin="round"
                d="M16.5 10.5V7a4.5 4.5 0 10-9 0v3.5M6.75 10.5h10.5a1.5 1.5 0 011.5 1.5v6a1.5 1.5 0 01-1.5 1.5H6.75a1.5 1.5 0 01-1.5-1.5v-6a1.5 1.5 0 011.5-1.5z"
              />
            </svg>
          </span>
          <input
            v-model="password"
            type="password"
            class="field w-full pl-10"
            placeholder="请输入密码"
            autocomplete="current-password"
          />
        </div>

        <button type="submit" class="btn btn-primary mt-5 w-full py-2.5" :disabled="loading">
          <span>{{ loading ? '登录中…' : '进入控制台' }}</span>
          <svg v-if="!loading" class="h-4 w-4 opacity-80" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
            <path stroke-linecap="round" stroke-linejoin="round" d="M13.5 4.5L21 12m0 0l-7.5 7.5M21 12H3" />
          </svg>
        </button>
        <p class="mt-3 min-h-[20px] text-center text-sm text-red-500 dark:text-red-400">{{ loginError }}</p>
      </div>
      <p class="mt-5 text-center text-xs ui-faint">本地数据目录 · Cookie 会话鉴权</p>
    </form>
  </div>
</template>
