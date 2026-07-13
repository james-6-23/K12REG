import { ref } from 'vue'

export type ToastKind = 'success' | 'error' | 'info'

export interface ToastItem {
  id: number
  kind: ToastKind
  message: string
}

const items = ref<ToastItem[]>([])
let seq = 0

export function useToasts() {
  return items
}

export function toast(message: string, kind: ToastKind = 'info', ms = 2800) {
  const id = ++seq
  items.value = [...items.value, { id, kind, message }]
  window.setTimeout(() => {
    items.value = items.value.filter((t) => t.id !== id)
  }, ms)
}

export function toastSuccess(message: string, ms = 2800) {
  toast(message, 'success', ms)
}

export function toastError(message: string, ms = 3600) {
  toast(message, 'error', ms)
}

export function toastInfo(message: string, ms = 2800) {
  toast(message, 'info', ms)
}

export function dismissToast(id: number) {
  items.value = items.value.filter((t) => t.id !== id)
}
