// Session store: the API key used for every backend call. Persisted in
// localStorage so a designer session survives reloads; never sent
// anywhere except this service's own endpoints.
import { defineStore } from 'pinia'
import { ref } from 'vue'

const STORAGE = 'typstpdf.designer.key'

export const useSession = defineStore('session', () => {
  let initial = ''
  try { initial = localStorage.getItem(STORAGE) ?? '' } catch { /* private mode */ }
  const apiKey = ref(initial)
  // True once a request bounced with 401 — App.vue shows the key dialog.
  const needsKey = ref(false)

  function setKey(key: string) {
    apiKey.value = key.trim()
    needsKey.value = false
    try { localStorage.setItem(STORAGE, apiKey.value) } catch { /* private mode */ }
  }

  return { apiKey, needsKey, setKey }
})
