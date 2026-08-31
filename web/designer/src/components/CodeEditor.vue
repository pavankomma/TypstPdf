<script setup lang="ts">
// CodeMirror 6 wrapper: v-model, a language prop (typst or json), and —
// for typst — payload-key completions fed from the sibling defaults/
// example documents. Chrome (line numbers, brackets, history) comes from
// codemirror's basicSetup; visual styling stays in app.css via tokens.
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { EditorView, basicSetup } from 'codemirror'
import { EditorState, Compartment, type Extension } from '@codemirror/state'
import { json } from '@codemirror/lang-json'
import { codeHighlight, typstExtensions } from '../editor/typst'

const props = defineProps<{
  modelValue: string
  lang: 'typst' | 'json'
  /** For typst: current defaults/example text, read at completion time. */
  docs?: () => { defaults: string; example: string }
  /** Jump request (1-based line/col); seq bumps to re-trigger. */
  jump?: { line: number; col: number; seq: number } | null
}>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const host = ref<HTMLDivElement | null>(null)
let view: EditorView | null = null
const langComp = new Compartment()

function langExtension(): Extension {
  if (props.lang === 'json') return json()
  return typstExtensions(props.docs ?? (() => ({ defaults: '', example: '' })))
}

onMounted(() => {
  view = new EditorView({
    parent: host.value!,
    state: EditorState.create({
      doc: props.modelValue,
      extensions: [
        codeHighlight, // before basicSetup so it outranks the default palette
        basicSetup,
        langComp.of(langExtension()),
        EditorView.lineWrapping,
        EditorView.updateListener.of((u) => {
          if (u.docChanged) emit('update:modelValue', u.state.doc.toString())
        }),
      ],
    }),
  })
})

onUnmounted(() => view?.destroy())

// External replacement (switching templates): reset the document.
watch(
  () => props.modelValue,
  (value) => {
    if (view && value !== view.state.doc.toString()) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } })
    }
  },
)

watch(
  () => props.lang,
  () => view?.dispatch({ effects: langComp.reconfigure(langExtension()) }),
)

// Jump to a diagnostic's line/col and center it.
watch(
  () => props.jump?.seq,
  () => {
    const j = props.jump
    if (!view || !j) return
    const line = view.state.doc.line(Math.min(Math.max(1, j.line), view.state.doc.lines))
    const pos = Math.min(line.from + Math.max(0, j.col - 1), line.to)
    view.dispatch({
      selection: { anchor: pos },
      effects: EditorView.scrollIntoView(pos, { y: 'center' }),
    })
    view.focus()
  },
)
</script>

<template>
  <div ref="host" class="code-editor"></div>
</template>
