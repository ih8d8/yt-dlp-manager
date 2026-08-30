/**
 * Pure helpers for the built-in API Explorer. Everything here is unit-testable
 * without a DOM: URL building, curl rendering, response bounding and the
 * special-casing for SSE/binary operations.
 */

export interface OAParam {
  name: string
  in: 'path' | 'query' | 'header'
  required?: boolean
  description?: string
  schema?: { type?: string; enum?: string[]; maxLength?: number; default?: unknown }
}

export interface OAOperation {
  summary?: string
  description?: string
  tags?: string[]
  operationId?: string
  security?: unknown[]
  parameters?: OAParam[]
  requestBody?: {
    content?: Record<string, { schema?: unknown; example?: unknown }>
  }
  responses?: Record<string, unknown>
}

export interface OASpec {
  info?: { title?: string; version?: string; description?: string }
  paths?: Record<string, Record<string, OAOperation>>
  components?: { schemas?: Record<string, unknown> }
}

export interface ExplorerOp {
  key: string
  method: string
  path: string
  op: OAOperation
  tag: string
  summary: string
  /** true when the operation requires the session cookie + CSRF token */
  protected: boolean
  /** GET/HEAD: no CSRF; mutations always need the token */
  mutation: boolean
  pathParams: OAParam[]
  queryParams: OAParam[]
  bodyExample: unknown
  hasBody: boolean
  /** SSE stream: never read unbounded */
  isStream: boolean
  /** binary image response: render, don't dump */
  isBinary: boolean
}

const MUTATING = new Set(['post', 'put', 'delete', 'patch'])

export function isProtected(op: OAOperation): boolean {
  // Operations carry security: [{cookieAuth: []}] when authenticated.
  // healthz/readyz and login/logout intentionally have none.
  return Array.isArray(op.security) && op.security.length > 0
}

export function collectOperations(spec: OASpec): ExplorerOp[] {
  const ops: ExplorerOp[] = []
  for (const [path, item] of Object.entries(spec.paths ?? {})) {
    for (const [method, op] of Object.entries(item)) {
      if (typeof op !== 'object' || op === null) continue
      const o = op as OAOperation
      const mutation = MUTATING.has(method)
      const params = o.parameters ?? []
      ops.push({
        key: `${method.toUpperCase()} ${path}`,
        method: method.toUpperCase(),
        path,
        op: o,
        tag: o.tags?.[0] ?? 'Other',
        summary: o.summary ?? path,
        protected: isProtected(o),
        mutation,
        pathParams: params.filter((p) => p.in === 'path'),
        queryParams: params.filter((p) => p.in === 'query'),
        bodyExample: o.requestBody?.content?.['application/json']?.example,
        hasBody: Boolean(o.requestBody?.content?.['application/json']),
        isStream: path.endsWith('/events'),
        isBinary: path.endsWith('/thumbnail'),
      })
    }
  }
  ops.sort((a, b) => a.tag.localeCompare(b.tag) || a.path.localeCompare(b.path) || a.method.localeCompare(b.method))
  return ops
}

export function groupByTag(ops: ExplorerOp[]): { tag: string; ops: ExplorerOp[] }[] {
  const groups = new Map<string, ExplorerOp[]>()
  for (const op of ops) {
    const list = groups.get(op.tag) ?? []
    list.push(op)
    groups.set(op.tag, list)
  }
  return [...groups.entries()].map(([tag, list]) => ({ tag, ops: list }))
}

/** Substitute {param} placeholders and append query string. */
export function buildRequestURL(
  pathTemplate: string,
  pathValues: Record<string, string>,
  queryValues: Record<string, string>
): string {
  let p = pathTemplate
  for (const [name, value] of Object.entries(pathValues)) {
    p = p.split(`{${name}}`).join(encodeURIComponent(value))
  }
  const qs = new URLSearchParams()
  for (const [name, value] of Object.entries(queryValues)) {
    if (value !== '') qs.set(name, value)
  }
  const q = qs.toString()
  return q ? `${p}?${q}` : p
}

/** Build a copyable curl example. Never includes Cookie headers. Every
 *  user-derived argument is POSIX-quoted so metacharacters cannot break it. */
export function buildCurl(
  method: string,
  url: string,
  bodyText: string,
  opts: { mutation: boolean; protected: boolean }
): string {
  const parts = [`curl -i -X ${method} ${shellQuote(url)}`]
  if (opts.protected) {
    parts.push("     -H 'Cookie: <session cookie, sent automatically by the browser>'")
  }
  if (opts.mutation) {
    parts.push("     -H 'X-CSRF-Token: <token from GET /api/v1/session>'")
  }
  if (bodyText.trim() !== '' && method !== 'GET' && method !== 'HEAD') {
    parts.push("     -H 'Content-Type: application/json'")
    parts.push(`     -d ${shellQuote(bodyText)}`)
  }
  return parts.join(' \\\n')
}

/** Bound response text so a huge or endless body can never flood the page. */
export function boundText(text: string, max = 100_000): { text: string; truncated: boolean } {
  if (text.length <= max) return { text, truncated: false }
  return { text: text.slice(0, max), truncated: true }
}

/** Default JSON body text for an operation: its example, or a safe empty object. */
export function defaultBodyText(op: ExplorerOp): string {
  if (!op.hasBody) return ''
  if (op.bodyExample !== undefined) {
    try {
      return JSON.stringify(op.bodyExample, null, 2)
    } catch {
      /* fall through */
    }
  }
  return '{\n  \n}'
}

/**
 * Login must never be prefilled with password material. (There is deliberately
 * no destructive endpoint left to guard: the API cannot delete media.)
 */
export function sanitizeBodyForEdit(op: ExplorerOp, text: string): string {
  if (op.path === '/api/v1/session' && op.method === 'POST') {
    return '{\n  "password": ""\n}'
  }
  return text
}

/**
 * POSIX-safe single-quote for shell arguments: closes the quote, escapes the
 * apostrophe as '\'' , and reopens. Every user-derived curl argument (URL,
 * body) must pass through this so metacharacters can never terminate the
 * quoted string.
 */
export function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

/** True only for the image content types the thumbnail endpoint may return. */
export function isImageContentType(contentType: string | null | undefined): boolean {
  if (!contentType) return false
  const mt = contentType.split(';')[0].trim().toLowerCase()
  return mt === 'image/jpeg' || mt === 'image/png' || mt === 'image/webp'
}

/**
 * Resolve a schema object enough for display: local #/components/schemas
 * references are replaced by the referenced schema (one hop, with a marker
 * so the reader knows it was a ref). Non-local refs are left as-is.
 */
export function resolveSchema(
  schema: unknown,
  components: Record<string, unknown> | undefined,
  depth = 0
): unknown {
  if (depth > 4 || schema === null || typeof schema !== 'object') return schema
  const s = schema as Record<string, unknown>
  const ref = s.$ref
  if (typeof ref === 'string' && ref.startsWith('#/components/schemas/') && components) {
    const name = ref.slice('#/components/schemas/'.length)
    const target = components[name]
    if (target !== undefined) {
      return { description: `Referenced schema: ${name}`, ...(resolveSchema(target, components, depth + 1) as object) }
    }
    return schema
  }
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(s)) {
    if (k === 'items' && v !== null && typeof v === 'object') {
      out[k] = resolveSchema(v, components, depth + 1)
    } else if ((k === 'properties' || k === 'additionalProperties') && v && typeof v === 'object') {
      const inner: Record<string, unknown> = {}
      for (const [pk, pv] of Object.entries(v as Record<string, unknown>)) {
        inner[pk] = resolveSchema(pv, components, depth + 1)
      }
      out[k] = inner
    } else {
      out[k] = v
    }
  }
  return out
}

/** Pretty-print a schema for the documentation code blocks. */
export function schemaToText(schema: unknown, components: Record<string, unknown> | undefined): string {
  try {
    return JSON.stringify(resolveSchema(schema, components), null, 2)
  } catch {
    return String(schema)
  }
}
