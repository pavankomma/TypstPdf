<script setup lang="ts">
// The template workbench: composes rail + editor + preview around the
// designer store, and owns only layout (collapsible rail, draggable
// split) and the delete confirmation.
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useDesigner } from '../state/designer'
import { useRoute, navigate } from '../router'
import TemplateRail from '../components/TemplateRail.vue'
import EditorPane from '../components/EditorPane.vue'
import PreviewPane from '../components/PreviewPane.vue'

const route = useRoute()
const designer = useDesigner()
const deleteDialog = ref<HTMLDialogElement | null>(null)

const selected = computed(() =>
  route.value.name === 'templates' ? route.value.template : undefined,
)

// ---- layout: collapsible rail + draggable editor/preview split --------

function readPct(): number {
  try {
    const n = Number(localStorage.getItem('typstpdf.designer.split'))
    if (n >= 30 && n <= 75) return n
  } catch { /* private mode */ }
  return 60 // editor 60 / preview 40 by default
}
const editorPct = ref(readPct())
const railCollapsed = ref(false)
try { railCollapsed.value = localStorage.getItem('typstpdf.designer.rail') === 'collapsed' } catch { /* private mode */ }

function toggleRail() {
  railCollapsed.value = !railCollapsed.value
  try {
    localStorage.setItem('typstpdf.designer.rail', railCollapsed.value ? 'collapsed' : 'open')
  } catch { /* private mode */ }
}

function startSplitDrag(e: PointerEvent) {
  const panes = (e.currentTarget as HTMLElement).parentElement
  if (!panes) return
  const rect = panes.getBoundingClientRect()
  const move = (ev: PointerEvent) => {
    editorPct.value = Math.min(68, Math.max(32, ((ev.clientX - rect.left) / rect.width) * 100))
  }
  const up = () => {
    removeEventListener('pointermove', move)
    removeEventListener('pointerup', up)
    try {
      localStorage.setItem('typstpdf.designer.split', String(Math.round(editorPct.value)))
    } catch { /* private mode */ }
  }
  addEventListener('pointermove', move)
  addEventListener('pointerup', up)
}

// ---- lifecycle --------------------------------------------------------

async function confirmDelete() {
  await designer.remove()
  deleteDialog.value?.close()
  navigate({ name: 'templates' })
}

// Keyboard shortcuts: Ctrl/Cmd+S save, Ctrl/Cmd+Enter render.
function onKeydown(e: KeyboardEvent) {
  if (!(e.ctrlKey || e.metaKey)) return
  if (e.key === 's') {
    e.preventDefault()
    if (designer.dirty && !designer.busy) designer.save()
  } else if (e.key === 'Enter') {
    e.preventDefault()
    designer.render()
  }
}
onMounted(() => addEventListener('keydown', onKeydown))
onUnmounted(() => removeEventListener('keydown', onKeydown))

// Unsaved-changes guard: switching templates with a dirty document asks
// before discarding.
const discardDialog = ref<HTMLDialogElement | null>(null)
const pendingOpen = ref('')

watch(selected, (name) => {
  if (!name || name === designer.doc?.name) return
  if (designer.dirty) {
    pendingOpen.value = name
    // Put the URL back on the open document until the user decides.
    if (designer.doc) navigate({ name: 'templates', template: designer.doc.name })
    discardDialog.value?.showModal()
    return
  }
  designer.open(name).catch(() => {})
})

function confirmDiscard() {
  discardDialog.value?.close()
  const name = pendingOpen.value
  pendingOpen.value = ''
  if (name) {
    navigate({ name: 'templates', template: name })
    designer.open(name).catch(() => {})
  }
}

onMounted(async () => {
  await designer.refreshList().catch(() => {})
  const name = selected.value ?? designer.templates[0]?.name
  if (name) {
    if (!selected.value) navigate({ name: 'templates', template: name })
    else await designer.open(name).catch(() => {})
  }
})

onUnmounted(() => designer.releasePreviews())
</script>

<template>
  <div class="workspace">
    <TemplateRail v-if="!railCollapsed" @collapse="toggleRail" />

    <div v-if="designer.doc" class="panes">
      <section class="pane" :style="{ flex: `0 0 ${editorPct}%` }">
        <EditorPane
          :rail-collapsed="railCollapsed"
          @expand="toggleRail"
          @delete="deleteDialog?.showModal()"
        />
      </section>

      <div
        class="divider"
        role="separator"
        aria-orientation="vertical"
        title="Drag to resize"
        @pointerdown.prevent="startSplitDrag"
      ></div>

      <section class="pane">
        <PreviewPane />
      </section>
    </div>

    <div v-else class="panes">
      <div class="pane">
        <div class="stage">
          <div class="stage-empty">
            <h4>Pick a template</h4>
            <p class="text-muted">or create one from the rail.</p>
          </div>
        </div>
      </div>
    </div>
  </div>

  <dialog ref="discardDialog" class="dialog">
    <div class="dialog-title">Discard unsaved changes?</div>
    <div class="dialog-body">
      “{{ designer.doc?.name }}” has unsaved edits. Switching to
      “{{ pendingOpen }}” will discard them.
    </div>
    <div class="dialog-actions">
      <button class="btn btn-secondary" @click="discardDialog?.close(); pendingOpen = ''">
        Keep editing
      </button>
      <button class="btn btn-danger" @click="confirmDiscard">Discard</button>
    </div>
  </dialog>

  <dialog ref="deleteDialog" class="dialog">
    <div class="dialog-title">Delete “{{ designer.doc?.name }}”?</div>
    <div class="dialog-body">
      Removes the template, its defaults, and its example from disk.
      Rendered documents and job history are not touched.
    </div>
    <div class="dialog-actions">
      <button class="btn btn-secondary" @click="deleteDialog?.close()">Cancel</button>
      <button class="btn btn-danger" @click="confirmDelete">Delete</button>
    </div>
  </dialog>
</template>
