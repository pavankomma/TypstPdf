<script setup lang="ts">
// Preview pane, Mermaid-Live style: drag anywhere to pan, Ctrl+wheel to
// zoom, floating zoom widget, a slim stats strip, and SVG/PDF export.
import { computed, ref } from 'vue'
import { useDesigner } from '../state/designer'

const designer = useDesigner()
const stage = ref<HTMLDivElement | null>(null)

// ---- zoom -------------------------------------------------------------

const ZOOM_STEPS = [50, 75, 100, 125, 150, 200, 300]

const zoomLabel = computed(() =>
  designer.zoom === 'fit' ? 'Fit' : `${designer.zoom}%`,
)

const sheetStyle = computed(() =>
  designer.zoom === 'fit'
    ? undefined
    : { width: `${(800 * (designer.zoom as number)) / 100}px`, maxWidth: 'none' },
)

function zoomStep(dir: 1 | -1) {
  const current = designer.zoom === 'fit' ? 100 : (designer.zoom as number)
  const idx = ZOOM_STEPS.findIndex((z) => z >= current)
  const at = idx === -1 ? ZOOM_STEPS.length - 1 : idx
  const next = ZOOM_STEPS[Math.min(ZOOM_STEPS.length - 1, Math.max(0, at + dir))]
  if (next !== undefined) designer.zoom = next
}

function onWheel(e: WheelEvent) {
  if (!e.ctrlKey) return
  e.preventDefault()
  const current = designer.zoom === 'fit' ? 100 : (designer.zoom as number)
  const factor = e.deltaY < 0 ? 1.1 : 1 / 1.1
  designer.zoom = Math.min(400, Math.max(25, Math.round(current * factor)))
}

// ---- hand tool (drag to pan) ------------------------------------------

const panning = ref(false)

function onPointerDown(e: PointerEvent) {
  const el = stage.value
  if (!el || e.button !== 0 || designer.previewMode !== 'html') return
  panning.value = true
  el.setPointerCapture(e.pointerId)
  let lastX = e.clientX
  let lastY = e.clientY
  const move = (ev: PointerEvent) => {
    el.scrollLeft -= ev.clientX - lastX
    el.scrollTop -= ev.clientY - lastY
    lastX = ev.clientX
    lastY = ev.clientY
  }
  const up = (ev: PointerEvent) => {
    panning.value = false
    el.releasePointerCapture(ev.pointerId)
    el.removeEventListener('pointermove', move)
    el.removeEventListener('pointerup', up)
  }
  el.addEventListener('pointermove', move)
  el.addEventListener('pointerup', up)
}

// ---- stats ------------------------------------------------------------

const stats = computed(() => {
  const s = designer.renderStats
  if (!s) return ''
  const size = s.bytes >= 1 << 20 ? `${(s.bytes / (1 << 20)).toFixed(1)} MB` : `${Math.round(s.bytes / 1024)} KB`
  const pages = s.pages > 0 ? `${s.pages} page${s.pages === 1 ? '' : 's'} · ` : ''
  return `${pages}${size} · ${s.ms} ms`
})
</script>

<template>
  <div class="pane-head">
    <div class="seg">
      <label class="seg-opt"><input v-model="designer.previewMode" type="radio" value="html" />HTML</label>
      <label class="seg-opt"><input v-model="designer.previewMode" type="radio" value="pdf" />PDF</label>
    </div>
    <select v-model="designer.dataMode" class="input select-compact" aria-label="Preview data">
      <option value="example">Example</option>
      <option value="empty">Defaults</option>
    </select>
    <span class="spacer"></span>
    <label class="radio">
      <input v-model="designer.autoPreview" type="checkbox" />
      <span class="dot" aria-hidden="true"></span>
      Auto
    </label>
    <button class="btn btn-secondary" :disabled="designer.busy" @click="designer.render()">Render</button>
  </div>

  <div class="stage-bar">
    <span class="stats">{{ stats }}</span>
    <span class="spacer"></span>
    <button
      class="btn btn-ghost"
      :disabled="designer.pageURLs.length === 0"
      title="Download the preview pages as SVG"
      @click="designer.exportSVG()"
    >SVG ↓</button>
    <button
      class="btn btn-ghost"
      :disabled="designer.busy || !designer.doc"
      title="Render and download as PDF"
      @click="designer.exportPDF()"
    >PDF ↓</button>
  </div>

  <div class="stage-wrap">
  <div
    ref="stage"
    class="stage"
    :class="{ paper: designer.previewMode === 'html', panning }"
    @wheel="onWheel"
    @pointerdown="onPointerDown"
  >
    <template v-if="!designer.diagnostics">
      <div v-if="designer.previewMode === 'html' && designer.pageURLs.length" class="sheets">
        <img
          v-for="(u, i) in designer.pageURLs"
          :key="u"
          :src="u"
          class="sheet"
          :style="sheetStyle"
          :alt="`Page ${i + 1}`"
          draggable="false"
        />
      </div>
      <embed
        v-else-if="designer.previewMode === 'pdf' && designer.pdfURL"
        :src="designer.pdfURL"
        type="application/pdf"
      />
      <div v-else class="stage-empty">
        <h4>No preview yet</h4>
        <p class="text-muted">Edit the source or press Render.</p>
      </div>
    </template>
    <pre v-if="designer.diagnostics" class="diag">{{ designer.diagnostics }}</pre>
  </div>

  <button
    v-if="designer.diagnostics && designer.diagLoc"
    class="btn btn-secondary diag-jump"
    @click="designer.jumpToDiag()"
  >
    Go to line {{ designer.diagLoc.line }}
  </button>

  <div v-if="designer.previewMode === 'html' && !designer.diagnostics" class="zoom-widget elev-md">
    <button class="btn btn-ghost" title="Zoom out" @click="zoomStep(-1)">−</button>
    <button class="btn btn-ghost zoom-label" title="Reset to fit" @click="designer.zoom = 'fit'">
      {{ zoomLabel }}
    </button>
    <button class="btn btn-ghost" title="Zoom in" @click="zoomStep(1)">+</button>
  </div>
  </div>
</template>
