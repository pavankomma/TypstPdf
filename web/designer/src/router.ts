// Hand-rolled hash router (EasyScan convention): the Go binary serves one
// static bundle at /designer/, so hash routes need no server-side mapping.
import { ref, onMounted, onUnmounted, type Ref } from 'vue'

export type Route =
  | { name: 'templates'; template?: string }
  | { name: 'jobs' }

function parse(): Route {
  const hash = location.hash.replace(/^#\/?/, '')
  const [head, arg] = hash.split('/')
  if (head === 'jobs') return { name: 'jobs' }
  if (head === 'templates' && arg) return { name: 'templates', template: decodeURIComponent(arg) }
  return { name: 'templates' }
}

const current: Ref<Route> = ref(parse())
let listeners = 0
const onChange = () => { current.value = parse() }

export function useRoute(): Ref<Route> {
  onMounted(() => { if (listeners++ === 0) addEventListener('hashchange', onChange) })
  onUnmounted(() => { if (--listeners === 0) removeEventListener('hashchange', onChange) })
  return current
}

export function href(route: Route): string {
  if (route.name === 'jobs') return '#/jobs'
  return route.template ? `#/templates/${encodeURIComponent(route.template)}` : '#/templates'
}

export function navigate(route: Route): void {
  location.hash = href(route)
}
