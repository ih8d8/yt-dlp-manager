import { describe, expect, it } from 'vitest'
import {
  boundText,
  buildCurl,
  buildRequestURL,
  collectOperations,
  defaultBodyText,
  groupByTag,
  isImageContentType,
  resolveSchema,
  sanitizeBodyForEdit,
  schemaToText,
  shellQuote,
  type ExplorerOp,
  type OASpec
} from '../src/api/explorer'

const spec: OASpec = {
  info: { title: 'yt-dlp-manager API', version: 'v1' },
  paths: {
    '/healthz': { get: { summary: 'Liveness', tags: ['System'], responses: {} } },
    '/api/v1/downloads': {
      get: { summary: 'List', tags: ['Downloads'], security: [{ cookieAuth: [] }], responses: {} },
      post: {
        summary: 'Add',
        tags: ['Downloads'],
        security: [{ cookieAuth: [] }],
        requestBody: { content: { 'application/json': { example: { url: 'https://x', start_now: false } } } },
        responses: {}
      }
    },
    '/api/v1/downloads/{id}': {
      get: {
        summary: 'Get',
        tags: ['Downloads'],
        security: [{ cookieAuth: [] }],
        parameters: [{ name: 'id', in: 'path', required: true }],
        responses: {}
      }
    },
    '/api/v1/events': { get: { summary: 'Stream', tags: ['Events'], security: [{ cookieAuth: [] }], responses: {} } },
    '/api/v1/downloads/{id}/thumbnail': {
      get: { summary: 'Thumb', tags: ['Downloads'], security: [{ cookieAuth: [] }], responses: {} }
    },
    '/api/v1/session': {
      post: { summary: 'Login', tags: ['Session'], requestBody: { content: { 'application/json': { example: { password: 'real-secret' } } } }, responses: {} }
    }
  }
}

describe('api explorer helpers', () => {
  it('collects and groups operations by tag with metadata', () => {
    const ops = collectOperations(spec)
    expect(ops.length).toBe(7)
    const groups = groupByTag(ops)
    expect(groups.map((g) => g.tag)).toEqual(['Downloads', 'Events', 'Session', 'System'])
    const add = ops.find((o) => o.method === 'POST' && o.path === '/api/v1/downloads')
    expect(add).toBeTruthy()
    expect(add!.protected).toBe(true)
    expect(add!.mutation).toBe(true)
    expect(add!.hasBody).toBe(true)
    expect(add!.pathParams).toHaveLength(0)
    const health = ops.find((o) => o.path === '/healthz')!
    expect(health.protected).toBe(false)
    expect(health.mutation).toBe(false)
  })

  it('marks SSE and thumbnail operations for safe handling', () => {
    const ops = collectOperations(spec)
    expect(ops.find((o) => o.path === '/api/v1/events')!.isStream).toBe(true)
    expect(ops.find((o) => o.path.endsWith('/thumbnail'))!.isBinary).toBe(true)
    expect(ops.find((o) => o.path === '/healthz')!.isStream).toBe(false)
  })

  it('builds request URLs with encoded path params and non-empty query', () => {
    expect(buildRequestURL('/api/v1/downloads/{id}', { id: 'a b/c' }, {})).toBe(
      '/api/v1/downloads/a%20b%2Fc'
    )
    expect(
      buildRequestURL('/api/v1/downloads', {}, { state: 'failed', search: '' })
    ).toBe('/api/v1/downloads?state=failed')
    expect(buildRequestURL('/api/v1/downloads/{id}', {}, {})).toBe('/api/v1/downloads/{id}')
  })

  it('renders curl without cookies and with csrf/body for mutations', () => {
    const get = buildCurl('GET', '/api/v1/downloads', '', { mutation: false, protected: true })
    expect(get).toContain("curl -i -X GET '/api/v1/downloads'")
    expect(get).toContain('Cookie: <session cookie')
    expect(get).not.toContain('X-CSRF-Token')

    const post = buildCurl('POST', '/api/v1/downloads/clear', '{"scope":"all"}', {
      mutation: true,
      protected: true
    })
    expect(post).toContain('X-CSRF-Token')
    expect(post).toContain("Content-Type: application/json")
    expect(post).toContain(`'{"scope":"all"}'`)
  })

  it('bounds response text', () => {
    expect(boundText('short').truncated).toBe(false)
    const big = boundText('x'.repeat(150_000), 100_000)
    expect(big.truncated).toBe(true)
    expect(big.text.length).toBe(100_000)
  })

  it('POSIX-quotes every user-derived curl argument', () => {
    expect(shellQuote("a'b")).toBe("'a'\\''b'")
    expect(shellQuote('a;b|c$d`e`f g')).toBe("'a;b|c$d`e`f g'")
    // Apostrophe + shell metacharacters in a path value: the URL must remain
    // ONE single-quoted argument with the quote escaped, no raw break.
    const id = "x'; rm -rf /; echo 'pwned"
    const url = buildRequestURL('/api/v1/downloads/{id}', { id }, {})
    const curl = buildCurl('GET', url, '', { mutation: false, protected: true })
    const quotedURL = shellQuote(url)
    // buildCurl is multiline, so its first line deliberately ends in a shell
    // continuation after the fully quoted URL.
    expect(curl.split('\n')[0]).toBe(`curl -i -X GET ${quotedURL} \\`)
    expect(quotedURL).toContain("'\\''")
    expect(quotedURL.slice(1, -1).split("'\\''").join("'")).toBe(url)
    // Body is quoted with the same helper.
    const post = buildCurl('POST', '/api/v1/downloads/clear', '{"scope":"all"}', {
      mutation: true,
      protected: true
    })
    expect(post).toContain(`-d '{"scope":"all"}'`)
    const evilBody = buildCurl('POST', '/api/v1/downloads/clear', `{"scope":"a'b"}`, {
      mutation: true,
      protected: true
    })
    expect(evilBody).toContain(`-d '{"scope":"a'\\''b"}'`)
  })

  it('accepts only renderable image content types', () => {
    expect(isImageContentType('image/jpeg')).toBe(true)
    expect(isImageContentType('image/png')).toBe(true)
    expect(isImageContentType('image/webp')).toBe(true)
    expect(isImageContentType('image/jpeg; charset=binary')).toBe(true)
    expect(isImageContentType('application/json')).toBe(false)
    expect(isImageContentType('text/html')).toBe(false)
    expect(isImageContentType('')).toBe(false)
    expect(isImageContentType(null)).toBe(false)
    expect(isImageContentType(undefined)).toBe(false)
  })

  it('resolves local component refs for schema display', () => {
    const components = {
      ClearRequest: {
        type: 'object',
        properties: { scope: { type: 'string', enum: ['finished', 'all'] } },
        required: ['scope']
      }
    }
    const resolved = resolveSchema({ $ref: '#/components/schemas/ClearRequest' }, components) as Record<string, unknown>
    expect(resolved.type).toBe('object')
    expect(JSON.stringify(resolved)).toContain('"finished"')
    expect(JSON.stringify(resolved)).toContain('Referenced schema: ClearRequest')
    // Unknown refs stay untouched; nested items resolve too.
    expect(resolveSchema({ $ref: '#/components/schemas/Nope' }, components)).toEqual({
      $ref: '#/components/schemas/Nope'
    })
    const list = resolveSchema(
      { type: 'array', items: { $ref: '#/components/schemas/ClearRequest' } },
      components
    ) as Record<string, unknown>
    expect(JSON.stringify((list.items as Record<string, unknown>).properties)).toContain('scope')
    expect(schemaToText({ $ref: '#/components/schemas/ClearRequest' }, components)).toContain('"enum"')
  })

  it('never prefills a password into the editable login body', () => {
    const login = collectOperations(spec).find((o) => o.path === '/api/v1/session' && o.method === 'POST')!
    // The spec example deliberately contains secret-looking material; the
    // editor must replace it with an empty field.
    expect(defaultBodyText(login)).toContain('real-secret')
    expect(sanitizeBodyForEdit(login, defaultBodyText(login))).toBe('{\n  "password": ""\n}')
    // Other operations keep their example.
    const add = collectOperations(spec).find((o) => o.method === 'POST' && o.path === '/api/v1/downloads')!
    expect(sanitizeBodyForEdit(add, defaultBodyText(add))).not.toContain('password')
  })
})

describe('request bodies are sanitised before editing', () => {
  const op = (path: string, method = 'POST'): ExplorerOp => ({
    key: `${method} ${path}`,
    method,
    path,
    op: {},
    tag: 'Downloads',
    summary: '',
    protected: true,
    mutation: true,
    pathParams: [],
    queryParams: [],
    bodyExample: undefined,
    hasBody: true,
    isStream: false,
    isBinary: false
  })

  it('never prefills the login password', () => {
    const out = sanitizeBodyForEdit(op('/api/v1/session'), '{"password":"hunter2"}')
    expect(JSON.parse(out).password).toBe('')
  })

  it('leaves other bodies untouched', () => {
    const body = JSON.stringify({ action: 'pause', ids: ['a'] }, null, 2)
    expect(sanitizeBodyForEdit(op('/api/v1/downloads/actions'), body)).toBe(body)
  })
})
