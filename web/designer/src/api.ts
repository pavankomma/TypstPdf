// Thin typed client for the TypstPdf designer + jobs APIs. Every call
// carries the session's bearer key; a 401 flips the session's needsKey
// flag so App.vue can prompt.
import { useSession } from './state/session'

export interface TemplateSummary {
  name: string
  version: string
  has_defaults: boolean
  has_example: boolean
}

export interface TemplateDoc {
  name: string
  version?: string
  source: string
  defaults: string
  example: string
}

export interface Job {
  id: string
  template: string
  template_version?: string
  status: 'queued' | 'running' | 'succeeded' | 'failed'
  sync?: boolean
  attempts: number
  max_attempts: number
  error?: string
  pdf_bytes?: number
  archival_fallback?: boolean
  created_at: string
  finished_at?: string
}

/** Compile diagnostics from a 422 — a normal outcome while designing. */
export class CompileFailure extends Error {
  constructor(public detail: string) { super('typst compile failed') }
}

export class ApiError extends Error {
  constructor(public status: number, message: string) { super(message) }
}

async function call(path: string, init: RequestInit = {}): Promise<Response> {
  const session = useSession()
  const headers = new Headers(init.headers)
  if (session.apiKey) headers.set('Authorization', `Bearer ${session.apiKey}`)
  const resp = await fetch(path, { ...init, headers })
  if (resp.status === 401) {
    session.needsKey = true
    throw new ApiError(401, 'API key required')
  }
  return resp
}

async function callJSON<T>(path: string, init: RequestInit = {}): Promise<T> {
  const resp = await call(path, init)
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({}) as Record<string, string>)
    throw new ApiError(resp.status, body.error ?? `HTTP ${resp.status}`)
  }
  return resp.json() as Promise<T>
}

export function listTemplates(): Promise<TemplateSummary[]> {
  return callJSON<{ templates: TemplateSummary[] }>('/v1/designer/templates')
    .then((r) => r.templates ?? [])
}

export function getTemplate(name: string): Promise<TemplateDoc> {
  return callJSON<TemplateDoc>(`/v1/designer/templates/${encodeURIComponent(name)}`)
}

export async function saveTemplate(doc: TemplateDoc): Promise<{ version: string }> {
  const resp = await call(`/v1/designer/templates/${encodeURIComponent(doc.name)}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ source: doc.source, defaults: doc.defaults, example: doc.example }),
  })
  if (resp.status === 422) {
    const body = await resp.json()
    throw new CompileFailure(body.detail ?? body.error)
  }
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({}) as Record<string, string>)
    throw new ApiError(resp.status, body.error ?? `HTTP ${resp.status}`)
  }
  return resp.json()
}

export function deleteTemplate(name: string): Promise<unknown> {
  return callJSON(`/v1/designer/templates/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

async function previewRequest(
  source: string,
  data: string,
  defaults: string,
  format: 'svg' | 'pdf',
): Promise<Response> {
  const resp = await call('/v1/designer/render', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      source,
      data: data.trim() ? JSON.parse(data) : {},
      defaults: defaults.trim() ? defaults : '',
      format,
    }),
  })
  if (resp.status === 422) {
    const body = await resp.json()
    throw new CompileFailure(body.detail ?? body.error)
  }
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({}) as Record<string, string>)
    throw new ApiError(resp.status, body.error ?? `HTTP ${resp.status}`)
  }
  return resp
}

/** Inline preview: one SVG markup string per page. */
export async function previewPages(source: string, data: string, defaults: string): Promise<string[]> {
  const resp = await previewRequest(source, data, defaults, 'svg')
  const body = (await resp.json()) as { pages: string[] }
  return body.pages ?? []
}

/** Full PDF preview; resolves to a blob URL (caller revokes) + size. */
export async function previewPDF(
  source: string,
  data: string,
  defaults: string,
): Promise<{ url: string; bytes: number }> {
  const resp = await previewRequest(source, data, defaults, 'pdf')
  const blob = await resp.blob()
  return { url: URL.createObjectURL(blob), bytes: blob.size }
}

export function listJobs(): Promise<Job[]> {
  return callJSON<{ jobs: Job[] }>('/v1/jobs?limit=100').then((r) => r.jobs ?? [])
}

export async function jobPDF(id: string): Promise<string> {
  const resp = await call(`/v1/jobs/${id}/pdf`)
  if (!resp.ok) throw new ApiError(resp.status, `HTTP ${resp.status}`)
  return URL.createObjectURL(await resp.blob())
}
