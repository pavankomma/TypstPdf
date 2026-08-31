// Designer workspace store: the open template document, preview state,
// and every action the workbench components share. Components stay thin
// (EasyScan convention: shared reactive logic lives in Pinia stores, not
// prop-drilled through SFCs).
import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'
import * as api from '../api'

export const useDesigner = defineStore('designer', () => {
  const templates = ref<api.TemplateSummary[]>([])
  const doc = ref<api.TemplateDoc | null>(null)
  const savedSnapshot = ref('')

  const tab = ref<'source' | 'defaults' | 'example' | 'page'>('source')
  const dataMode = ref<'example' | 'empty'>('example')
  const previewMode = ref<'html' | 'pdf'>('html')
  const autoPreview = ref(true)

  const pageURLs = ref<string[]>([])
  const pdfURL = ref('')
  const diagnostics = ref('')
  const busy = ref(false)
  const notice = ref('')

  /** Facts about the last successful render, for the preview status strip. */
  const renderStats = ref<{ pages: number; bytes: number; ms: number } | null>(null)
  /** Preview zoom: 'fit' (pane width) or a percentage of the 800px base. */
  const zoom = ref<'fit' | number>('fit')

  /** Editor jump request (from a diagnostic's main.typ:line:col). */
  const jump = ref<{ line: number; col: number; seq: number } | null>(null)

  const dirty = computed(() => doc.value !== null && snapshot() !== savedSnapshot.value)

  /** First main.typ:line:col location in the current diagnostics. */
  const diagLoc = computed(() => {
    const m = /main\.typ:(\d+):(\d+)/.exec(diagnostics.value)
    return m ? { line: Number(m[1]), col: Number(m[2]) } : null
  })

  /** Methods on `d`, not payload keys. */
  const D_METHODS = new Set(['at', 'join', 'map', 'len', 'keys', 'values', 'pairs', 'filter'])

  /** Keys the source reads (`d.key`, `d.at("key")`) that the defaults JSON
   *  does not supply — the exact gap the save gate rejects. */
  const missingKeys = computed(() => {
    const d = doc.value
    if (!d) return []
    let have: Set<string>
    try {
      const obj = JSON.parse(d.defaults || '{}')
      if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return []
      have = new Set(Object.keys(obj))
    } catch {
      return [] // defaults mid-edit; don't nag
    }
    const used = new Set<string>()
    for (const m of d.source.matchAll(/\bd\.([A-Za-z_][A-Za-z0-9_]*)/g)) {
      const key = m[1]!
      if (!D_METHODS.has(key)) used.add(key)
    }
    for (const m of d.source.matchAll(/\bd\.at\(\s*"([^"]+)"/g)) used.add(m[1]!)
    return [...used].filter((k) => !have.has(k)).sort()
  })

  /** One-click fix: add every missing key to the defaults as a placeholder. */
  function addMissingDefaults() {
    const d = doc.value
    if (!d || missingKeys.value.length === 0) return
    let obj: Record<string, unknown>
    try {
      obj = JSON.parse(d.defaults || '{}')
    } catch {
      return
    }
    for (const k of missingKeys.value) obj[k] = '—'
    d.defaults = JSON.stringify(obj, null, 2) + '\n'
    notice.value = 'placeholders added to defaults'
  }

  function jumpToDiag() {
    const loc = diagLoc.value
    if (!loc) return
    tab.value = 'source'
    jump.value = { ...loc, seq: (jump.value?.seq ?? 0) + 1 }
  }

  // ---- page setup (header/footer via components/page.typ) -------------

  /** The `page` object inside the defaults JSON — the source of truth the
   *  Page tab edits. Header/footer are payload data like everything else:
   *  defaultable per template, overridable per request. */
  const pageCfg = computed<Record<string, unknown>>(() => {
    try {
      const obj = JSON.parse(doc.value?.defaults || '{}')
      const page = obj?.page
      return page && typeof page === 'object' && !Array.isArray(page) ? page : {}
    } catch {
      return {}
    }
  })

  function setPage(key: string, value: unknown) {
    const d = doc.value
    if (!d) return
    let obj: Record<string, unknown>
    try {
      obj = JSON.parse(d.defaults || '{}')
      if (!obj || typeof obj !== 'object' || Array.isArray(obj)) obj = {}
    } catch {
      return // defaults mid-edit; the form is read-only until it parses
    }
    const page = { ...(pageCfg.value as object), [key]: value }
    obj.page = page
    d.defaults = JSON.stringify(obj, null, 2) + '\n'
  }

  /** Whether the source applies the shared page component. */
  const usesBranded = computed(
    () => doc.value?.source.includes('components/page.typ') ?? false,
  )

  /** Wire the shared page component into the source (after the data load). */
  function applyBrandedToSource() {
    const d = doc.value
    if (!d || usesBranded.value) return
    const wire = '#import "components/page.typ": branded\n#show: branded.with(d)\n'
    const anchor = /#let d = json\("data\.json"\)\s*\r?\n/
    if (anchor.test(d.source)) {
      d.source = d.source.replace(anchor, (m) => m + wire)
    } else {
      d.source = '#let d = json("data.json")\n' + wire + d.source
    }
    notice.value = 'page component applied to source'
  }

  // ---- export (Mermaid-Live-style test exports) -----------------------

  function triggerDownload(url: string, filename: string) {
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    a.click()
  }

  function previewData(): string {
    const d = doc.value!
    return dataMode.value === 'example' && d.example.trim() ? d.example : '{}'
  }

  /** Download the current preview pages as SVG files. */
  function exportSVG() {
    const d = doc.value
    if (!d || pageURLs.value.length === 0) return
    pageURLs.value.forEach((u, i) => triggerDownload(u, `${d.name}-page-${i + 1}.svg`))
    notice.value = `exported ${pageURLs.value.length} SVG page(s)`
  }

  /** Render + download the current document as a PDF. */
  async function exportPDF() {
    const d = doc.value
    if (!d) return
    busy.value = true
    try {
      const { url } = await api.previewPDF(d.source, previewData(), d.defaults)
      triggerDownload(url, `${d.name}.pdf`)
      setTimeout(() => URL.revokeObjectURL(url), 30_000)
      notice.value = 'exported PDF'
    } catch (e) {
      if (e instanceof api.CompileFailure) diagnostics.value = e.detail
    } finally {
      busy.value = false
    }
  }

  function snapshot(): string {
    const d = doc.value
    return d ? JSON.stringify([d.source, d.defaults, d.example]) : ''
  }

  /** Read live by the source editor's completion source, so `d.` completes
   *  payload keys even while defaults/example are being edited. */
  function editorDocs() {
    return { defaults: doc.value?.defaults ?? '', example: doc.value?.example ?? '' }
  }

  async function refreshList() {
    templates.value = await api.listTemplates()
  }

  async function open(name: string) {
    doc.value = await api.getTemplate(name)
    savedSnapshot.value = snapshot()
    diagnostics.value = ''
    notice.value = ''
    render()
  }

  // ---- preview --------------------------------------------------------

  function releasePreviews() {
    for (const u of pageURLs.value) URL.revokeObjectURL(u)
    pageURLs.value = []
    if (pdfURL.value) {
      URL.revokeObjectURL(pdfURL.value)
      pdfURL.value = ''
    }
  }

  async function render() {
    const d = doc.value
    if (!d) return
    busy.value = true
    const t0 = performance.now()
    try {
      const data = dataMode.value === 'example' && d.example.trim() ? d.example : '{}'
      if (previewMode.value === 'html') {
        const pages = await api.previewPages(d.source, data, d.defaults)
        releasePreviews()
        pageURLs.value = pages.map((svg) =>
          URL.createObjectURL(new Blob([svg], { type: 'image/svg+xml' })),
        )
        renderStats.value = {
          pages: pages.length,
          bytes: pages.reduce((n, p) => n + p.length, 0),
          ms: Math.round(performance.now() - t0),
        }
      } else {
        const { url, bytes } = await api.previewPDF(d.source, data, d.defaults)
        releasePreviews()
        pdfURL.value = url
        renderStats.value = { pages: 0, bytes, ms: Math.round(performance.now() - t0) }
      }
      diagnostics.value = ''
    } catch (e) {
      if (e instanceof api.CompileFailure) diagnostics.value = e.detail
      else if (e instanceof SyntaxError) diagnostics.value = `data is not valid JSON:\n${e.message}`
      else if (!(e instanceof api.ApiError && e.status === 401)) diagnostics.value = String(e)
    } finally {
      busy.value = false
    }
  }

  let debounce: ReturnType<typeof setTimeout> | undefined
  watch(
    [() => doc.value?.source, () => doc.value?.defaults, () => doc.value?.example, dataMode],
    () => {
      if (!autoPreview.value || !doc.value) return
      clearTimeout(debounce)
      debounce = setTimeout(render, 700)
    },
  )
  // Switching HTML ↔ PDF re-renders immediately: the PDF is only ever
  // produced when the PDF view is asked for.
  watch(previewMode, render)

  // ---- save / create / delete ----------------------------------------

  async function save() {
    const d = doc.value
    if (!d) return
    busy.value = true
    try {
      const { version } = await api.saveTemplate(d)
      d.version = version
      savedSnapshot.value = snapshot()
      diagnostics.value = ''
      notice.value = `saved · ${version}`
      await refreshList()
    } catch (e) {
      if (e instanceof api.CompileFailure) {
        diagnostics.value = `SAVE REJECTED — the template must compile against its defaults:\n\n${e.detail}`
      } else if (!(e instanceof api.ApiError && e.status === 401)) {
        notice.value = String(e)
      }
    } finally {
      busy.value = false
    }
  }

  /** Seed a new unsaved template document. Caller navigates to it. */
  function create(name: string) {
    doc.value = {
      name,
      source:
        `#let d = json("data.json")\n` +
        `#import "components/page.typ": branded\n` +
        `#show: branded.with(d)\n\n` +
        `= ${name}\n\nHello #d.at("name", default: "—")\n`,
      defaults: JSON.stringify(
        {
          name: '—',
          page: {
            paper: 'a4',
            margin_cm: 2,
            header_left: '',
            header_right: '',
            footer_left: '',
            footer_right: '',
            page_numbers: true,
          },
        },
        null,
        2,
      ) + '\n',
      example: `{\n  "name": "World"\n}\n`,
    }
    savedSnapshot.value = '' // unsaved until first save
    render()
  }

  async function remove() {
    const d = doc.value
    if (!d) return
    await api.deleteTemplate(d.name)
    doc.value = null
    releasePreviews()
    await refreshList()
  }

  return {
    templates, doc, tab, dataMode, previewMode, autoPreview,
    pageURLs, pdfURL, diagnostics, busy, notice, dirty,
    renderStats, zoom, jump, diagLoc, missingKeys,
    pageCfg, usesBranded,
    editorDocs, refreshList, open, render, save, create, remove, releasePreviews,
    addMissingDefaults, jumpToDiag, exportSVG, exportPDF, setPage, applyBrandedToSource,
  }
})
