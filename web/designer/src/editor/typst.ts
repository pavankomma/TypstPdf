// Typst editor support: a StreamLanguage highlighting mode plus an
// autocomplete source. There is no official CodeMirror Typst mode, so
// this is a pragmatic hand-rolled one covering the constructs the
// templates actually use. The completion source is template-aware: it
// completes `d.<key>` / `d.at("<key>")` from the template's own defaults
// and example JSON — intellisense no external Typst editor can offer,
// because the payload schema lives in this service.
import {
  HighlightStyle,
  StreamLanguage,
  syntaxHighlighting,
  type StringStream,
} from '@codemirror/language'
import { tags as t } from '@lezer/highlight'
import {
  autocompletion,
  snippetCompletion,
  type Completion,
  type CompletionContext,
  type CompletionResult,
} from '@codemirror/autocomplete'
import type { Extension } from '@codemirror/state'

// ---- highlighting -----------------------------------------------------

const KEYWORDS = new Set([
  'let', 'set', 'show', 'import', 'include', 'if', 'else', 'for', 'in',
  'while', 'break', 'continue', 'return', 'none', 'auto', 'true', 'false',
  'not', 'and', 'or', 'as', 'context',
])

interface TypstState {
  inBlockComment: boolean
}

const typstMode = StreamLanguage.define<TypstState>({
  name: 'typst',
  startState: () => ({ inBlockComment: false }),
  token(stream: StringStream, state: TypstState): string | null {
    if (state.inBlockComment) {
      if (stream.match(/^.*?\*\//)) state.inBlockComment = false
      else stream.skipToEnd()
      return 'comment'
    }
    if (stream.match('//')) {
      stream.skipToEnd()
      return 'comment'
    }
    if (stream.match('/*')) {
      state.inBlockComment = true
      return 'comment'
    }
    if (stream.match(/^"(?:[^"\\]|\\.)*"?/)) return 'string'
    if (stream.match(/^\$[^$]*\$?/)) return 'string.special' // math
    if (stream.sol() && stream.match(/^=+\s/)) {
      stream.skipToEnd()
      return 'heading'
    }
    // #keyword / #function( — code-mode entry
    if (stream.match(/^#[A-Za-z_][A-Za-z0-9_-]*/)) {
      const word = stream.current().slice(1)
      return KEYWORDS.has(word) ? 'keyword' : 'variableName.function'
    }
    // bare keywords inside code blocks
    if (stream.match(/^[A-Za-z_][A-Za-z0-9_-]*/)) {
      return KEYWORDS.has(stream.current()) ? 'keyword' : null
    }
    // numbers with optional units (12pt, 2.5cm, 40%, 1fr)
    if (stream.match(/^\d+(\.\d+)?(pt|mm|cm|in|em|fr|%|deg|rad)?/)) return 'number'
    if (stream.match(/^[{}[\]()]/)) return 'bracket'
    if (stream.match(/^[*_`]/)) return 'emphasis'
    stream.next()
    return null
  },
  languageData: {
    commentTokens: { line: '//', block: { open: '/*', close: '*/' } },
  },
})

// Doclerk-token-aligned code colors (replaces CodeMirror's default
// red-leaning palette). Shared by the typst and JSON editors.
export const codeHighlight: Extension = syntaxHighlighting(
  HighlightStyle.define([
    { tag: t.comment, color: 'var(--muted-2)', fontStyle: 'italic' },
    { tag: t.string, color: 'var(--p-green-txt)' },
    { tag: t.special(t.string), color: 'var(--p-purple-txt)' },
    { tag: t.keyword, color: 'var(--brand)', fontWeight: '600' },
    { tag: t.function(t.variableName), color: 'var(--color-accent-800)' },
    { tag: t.number, color: 'var(--p-purple-txt)' },
    { tag: t.bool, color: 'var(--p-purple-txt)' },
    { tag: t.null, color: 'var(--p-purple-txt)' },
    { tag: t.propertyName, color: 'var(--color-accent-800)' },
    { tag: t.heading, color: 'var(--ink)', fontWeight: '700' },
    { tag: t.emphasis, color: 'var(--muted)' },
    { tag: t.bracket, color: 'var(--muted)' },
  ]),
)

// ---- completions ------------------------------------------------------

function fn(label: string, detail: string, snippet: string): Completion {
  return snippetCompletion(snippet, { label, detail, type: 'function' })
}

/** Typst built-ins the bundled templates lean on, as snippets. */
const BUILTINS: Completion[] = [
  fn('#let', 'binding', '#let ${name} = ${value}'),
  fn('#set page', 'page setup', '#set page(paper: "${a4}", margin: ${2cm})'),
  fn('#set text', 'text style', '#set text(size: ${10pt})'),
  fn('#set par', 'paragraph style', '#set par(justify: ${true})'),
  fn('#set align', 'alignment', '#set align(${center})'),
  fn('#show', 'show rule', '#show ${selector}: ${transform}'),
  fn('#import', 'import module', '#import "${path}": ${items}'),
  fn('#text', 'styled text', '#text(size: ${10pt}, weight: "${bold}")[${content}]'),
  fn('#table', 'table', '#table(\n  columns: (${auto, 1fr}),\n  ${cells}\n)'),
  fn('#table.header', 'table header row', 'table.header(${cells})'),
  fn('#grid', 'grid layout', '#grid(\n  columns: (${1fr, auto}),\n  ${cells}\n)'),
  fn('#block', 'block container', '#block[${content}]'),
  fn('#box', 'inline container', '#box(${options})[${content}]'),
  fn('#stack', 'stacked layout', '#stack(dir: ${ttb}, ${items})'),
  fn('#align', 'aligned content', '#align(${right})[${content}]'),
  fn('#pad', 'padding', '#pad(left: ${1cm})[${content}]'),
  fn('#v', 'vertical space', '#v(${8pt})'),
  fn('#h', 'horizontal space', '#h(${1fr})'),
  fn('#line', 'line', '#line(length: ${100%}, stroke: ${0.5pt + gray})'),
  fn('#rect', 'rectangle', '#rect(width: ${100%}, inset: ${10pt})[${content}]'),
  fn('#image', 'image', '#image("${path}", width: ${50%})'),
  fn('#json', 'read JSON input', '#json("${data.json}")'),
  fn(
    '#show: branded',
    'shared header/footer (components/page.typ)',
    '#import "components/page.typ": branded\n#show: branded.with(d)',
  ),
  fn('#counter', 'counter', '#counter(${page})'),
  fn('#numbering', 'numbering format', '#numbering("${1.}", ${n})'),
  fn('#pagebreak', 'page break', '#pagebreak()'),
  fn('#if', 'conditional', '#if ${condition} [\n  ${content}\n]'),
  fn('#for', 'loop', '#for ${item} in ${collection} [\n  ${content}\n]'),
  fn('#upper', 'uppercase', '#upper(${value})'),
  fn('#lower', 'lowercase', '#lower(${value})'),
  fn('#str', 'to string', '#str(${value})'),
  fn('#raw', 'raw/monospace text', '#raw(${value})'),
  fn('#context', 'contextual expression', '#context ${expr}'),
  { label: '.at', detail: 'safe key access', type: 'method', apply: '.at("${key}", default: "—")' } as Completion,
  { label: '.join', detail: 'join array', type: 'method' },
  { label: '.map', detail: 'map array', type: 'method' },
  { label: '.len', detail: 'length', type: 'method' },
]

/** Collect the top-level keys of the template's defaults + example JSON. */
function dataKeys(defaults: string, example: string): string[] {
  const keys = new Set<string>()
  for (const raw of [defaults, example]) {
    try {
      const obj = JSON.parse(raw)
      if (obj && typeof obj === 'object' && !Array.isArray(obj)) {
        for (const k of Object.keys(obj)) keys.add(k)
      }
    } catch {
      /* mid-edit JSON; skip */
    }
  }
  return [...keys].sort()
}

/**
 * The completion source. `getDocs` is read at completion time so the key
 * list always reflects the current defaults/example editors.
 */
export function typstCompletions(
  getDocs: () => { defaults: string; example: string },
): (ctx: CompletionContext) => CompletionResult | null {
  return (ctx) => {
    const { defaults, example } = getDocs()

    // d.<key> and d.at("<key>  → payload keys from defaults/example.
    const keyRef = ctx.matchBefore(/\bd\.(?:at\(\s*")?[\w-]*/)
    if (keyRef) {
      const prefixMatch = /^d\.(?:at\(\s*")?/.exec(keyRef.text)
      const from = keyRef.from + (prefixMatch?.[0].length ?? 2)
      const options = dataKeys(defaults, example).map(
        (k): Completion => ({ label: k, type: 'property', detail: 'payload key' }),
      )
      if (options.length === 0) return null
      return { from, options, validFor: /^[\w-]*$/ }
    }

    // #partial or bare word → built-ins.
    const word = ctx.matchBefore(/#?[\w.]*/)
    if (!word || (word.from === word.to && !ctx.explicit)) return null
    return { from: word.from, options: BUILTINS, validFor: /^#?[\w.]*$/ }
  }
}

export function typstExtensions(
  getDocs: () => { defaults: string; example: string },
): Extension[] {
  return [typstMode, autocompletion({ override: [typstCompletions(getDocs)], activateOnTyping: true })]
}
