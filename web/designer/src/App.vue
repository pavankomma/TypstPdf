<script setup lang="ts">
import { ref, watch, computed } from 'vue'
import { useRoute, href } from './router'
import { useSession } from './state/session'
import TemplatesView from './views/TemplatesView.vue'
import JobsView from './views/JobsView.vue'

const route = useRoute()
const session = useSession()
const keyDialog = ref<HTMLDialogElement | null>(null)
const keyDraft = ref('')

const view = computed(() => (route.value.name === 'jobs' ? JobsView : TemplatesView))

watch(
  () => session.needsKey,
  (needs) => {
    if (needs) { keyDraft.value = session.apiKey; keyDialog.value?.showModal() }
  },
)

function saveKey() {
  session.setKey(keyDraft.value)
  keyDialog.value?.close()
}
</script>

<template>
  <header class="topbar">
    <nav class="nav">
      <span class="nav-brand"><span class="brand-text">TypstPdf</span> Designer</span>
      <a :href="href({ name: 'templates' })" :aria-current="route.name === 'templates' ? 'page' : undefined">Templates</a>
      <a :href="href({ name: 'jobs' })" :aria-current="route.name === 'jobs' ? 'page' : undefined">Jobs</a>
      <button class="btn btn-ghost" @click="session.needsKey = true">API key</button>
    </nav>
  </header>

  <component :is="view" />

  <dialog ref="keyDialog" class="dialog">
    <form method="dialog" @submit.prevent="saveKey">
      <div class="dialog-title">API key</div>
      <div class="dialog-body">
        <p class="text-muted">
          Paste a <code>tp_…</code> key (mint one with <code>server keys create</code>).
          Not needed when the server runs with <code>-no-auth</code>.
        </p>
        <label class="field">
          <input v-model="keyDraft" class="input mono" type="password" placeholder="tp_…" autofocus />
        </label>
      </div>
      <div class="dialog-actions">
        <button type="button" class="btn btn-secondary" @click="keyDialog?.close()">Cancel</button>
        <button type="submit" class="btn btn-primary">Save</button>
      </div>
    </form>
  </dialog>
</template>
