import { h } from 'preact'
import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { getCsrfToken } from '../api/client'
import {
  boundText,
  buildCurl,
  buildRequestURL,
  collectOperations,
  defaultBodyText,
  groupByTag,
  isImageContentType,
  sanitizeBodyForEdit,
  schemaToText,
  type ExplorerOp,
  type OASpec
} from '../api/explorer'

interface TryResult {
  status: number | null
  contentType?: string
  text: string
  truncated: boolean
  binaryUrl?: string
  note?: string
}

/**
 * API Explorer: renders the compiled OpenAPI 3.1 document with native Preact.
 * No CDN, no Swagger assets. Every operation can be executed explicitly with
 * same-origin credentials; nothing auto-runs and no secret is prefilled,
 * stored or logged.
 */
export function ExplorerPage(): h.JSX.Element {
  const [spec, setSpec] = useState<OASpec | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [openKey, setOpenKey] = useState<string | null>(null)
  const [pathValues, setPathValues] = useState<Record<string, Record<string, string>>>({})
  const [queryValues, setQueryValues] = useState<Record<string, Record<string, string>>>({})
  const [bodyTexts, setBodyTexts] = useState<Record<string, string>>({})
  const [results, setResults] = useState<Record<string, TryResult | 'loading'>>({})
  const [copied, setCopied] = useState<string | null>(null)
  const copyFailed = copied !== null && copied.startsWith('!')
  const objectUrls = useRef<Map<string, string>>(new Map())
  const mounted = useRef(true)
  useEffect(() => {
    return () => {
      mounted.current = false
      for (const u of objectUrls.current.values()) URL.revokeObjectURL(u)
      objectUrls.current.clear()
    }
  }, [])

  useEffect(() => {
    let alive = true
    fetch('/api/v1/openapi.json', { credentials: 'same-origin' })
      .then(async (r) => {
        if (!r.ok) throw new Error(`could not load API documentation (${r.status})`)
        return r.json()
      })
      .then((doc: OASpec) => {
        if (!alive) return
        setSpec(doc)
        setLoading(false)
      })
      .catch((e: unknown) => {
        if (!alive) return
        setLoadError(e instanceof Error ? e.message : 'could not load API documentation')
        setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [])

  const groups = useMemo(() => (spec ? groupByTag(collectOperations(spec)) : []), [spec])

  const setPathValue = (key: string, name: string, v: string) =>
    setPathValues((m) => ({ ...m, [key]: { ...(m[key] ?? {}), [name]: v } }))
  const setQueryValue = (key: string, name: string, v: string) =>
    setQueryValues((m) => ({ ...m, [key]: { ...(m[key] ?? {}), [name]: v } }))
  const setBodyText = (key: string, v: string) => setBodyTexts((m) => ({ ...m, [key]: v }))

  const bodyFor = (op: ExplorerOp) => {
    const existing = bodyTexts[op.key]
    if (existing !== undefined) return existing
    return sanitizeBodyForEdit(op, defaultBodyText(op))
  }

  const execute = async (op: ExplorerOp) => {
    const pv = pathValues[op.key] ?? {}
    const qv = queryValues[op.key] ?? {}
    for (const p of op.pathParams) {
      if (!(pv[p.name] ?? '').trim()) {
        setResults((m) => ({
          ...m,
          [op.key]: { status: null, text: `path parameter "${p.name}" is required`, truncated: false }
        }))
        return
      }
    }
    const url = buildRequestURL(op.path, pv, qv)
    const headers: Record<string, string> = {}
    let body: string | undefined
    if (op.mutation) {
      const token = getCsrfToken()
      if (token) headers['X-CSRF-Token'] = token
    }
    if (op.hasBody && op.method !== 'GET' && op.method !== 'HEAD') {
      headers['Content-Type'] = 'application/json'
      body = bodyFor(op)
    }

    // Release an earlier thumbnail result before replacing it. Waiting until
    // unmount would retain one Blob per repeated Execute click.
    const previousObjectURL = objectUrls.current.get(op.key)
    if (previousObjectURL) {
      URL.revokeObjectURL(previousObjectURL)
      objectUrls.current.delete(op.key)
    }
    setResults((m) => ({ ...m, [op.key]: 'loading' }))

    // SSE: read a small bounded sample, then abort — never an endless capture.
    if (op.isStream) {
      const ctrl = new AbortController()
      const timer = window.setTimeout(() => ctrl.abort(), 2500)
      try {
        const resp = await fetch(url, { ...{ method: op.method }, headers, credentials: 'same-origin', signal: ctrl.signal })
        const reader = resp.body?.getReader()
        let text = ''
        if (reader) {
          const { value } = await reader.read()
          if (value) text = new TextDecoder().decode(value.slice(0, 4096))
        }
        ctrl.abort()
        setResults((m) => ({
          ...m,
          [op.key]: {
            status: resp.status,
            contentType: resp.headers.get('Content-Type') ?? undefined,
            text: boundText(text || '(stream opened; sample aborted)').text,
            truncated: true,
            note: 'Live stream: only a bounded sample was read, then the request was aborted.'
          }
        }))
      } catch {
        setResults((m) => ({
          ...m,
          [op.key]: { status: null, text: '(stream sample aborted after timeout — this is expected)', truncated: false }
        }))
      } finally {
        window.clearTimeout(timer)
      }
      return
    }

    try {
      const resp = await fetch(url, {
        method: op.method,
        headers,
        credentials: 'same-origin',
        body
      })
      if (op.isBinary) {
        const ct = resp.headers.get('Content-Type')
        if (resp.ok && isImageContentType(ct)) {
          const blob = await resp.blob()
          const url2 = URL.createObjectURL(blob)
          // The request can finish after navigating away from the explorer.
          // Revoke immediately instead of retaining an unreachable Blob URL.
          if (!mounted.current) {
            URL.revokeObjectURL(url2)
            return
          }
          objectUrls.current.set(op.key, url2)
          setResults((m) => ({
            ...m,
            [op.key]: {
              status: resp.status,
              contentType: ct ?? undefined,
              text: '(binary image response)',
              truncated: false,
              binaryUrl: url2,
              note: 'Binary response rendered as an image; no bytes are dumped as text.'
            }
          }))
        } else {
          const raw = await resp.text()
          const b = boundText(raw)
          setResults((m) => ({
            ...m,
            [op.key]: {
              status: resp.status,
              contentType: ct ?? undefined,
              text: b.text,
              truncated: b.truncated,
              note: 'Not a renderable image response; showing the bounded body text.'
            }
          }))
        }
        return
      }
      const raw = await resp.text()
      const { text, truncated } = boundText(raw)
      setResults((m) => ({
        ...m,
        [op.key]: { status: resp.status, contentType: resp.headers.get('Content-Type') ?? undefined, text, truncated }
      }))
    } catch (e) {
      setResults((m) => ({
        ...m,
        [op.key]: {
          status: null,
          text: e instanceof Error ? e.message : 'request failed',
          truncated: false
        }
      }))
    }
  }

  if (loading) {
    return <div class="empty-state">Loading API documentation…</div>
  }
  if (loadError || !spec) {
    return (
      <div>
        <h1 class="page-title-sm">API</h1>
        <p class="page-sub">OpenAPI explorer</p>
        <div class="empty-state" role="alert">
          {loadError ?? 'API documentation unavailable.'}
        </div>
      </div>
    )
  }

  return (
    <div>
      <h1 class="page-title-sm">API</h1>
      <p class="page-sub">
        {spec.info?.title ?? 'yt-dlp-manager API'} v{spec.info?.version ?? '1'} — OpenAPI explorer.
        Requests run same-origin with your session; mutations send your CSRF token. Nothing
        executes until you press Execute.
      </p>

      {groups.map((g) => (
        <section key={g.tag} aria-label={g.tag} class="api-group">
          <h2 class="api-group-title">
            {g.tag}
          </h2>
          <div class="card api-group">
            {g.ops.map((op) => {
              const open = openKey === op.key
              const result = results[op.key]
              const curl = buildCurl(op.method, buildRequestURL(op.path, pathValues[op.key] ?? {}, queryValues[op.key] ?? {}), bodyFor(op), {
                mutation: op.mutation,
                protected: op.protected
              })
              return (
                <div key={op.key} class="api-op">
                  <button
                    class="api-op-head"
                    aria-expanded={open}
                    onClick={() => setOpenKey(open ? null : op.key)}
                  >
                    <span class={`api-method m-${op.method}`}>{op.method}</span>
                    <code class="api-path">{op.path}</code>
                    <span class="api-summary">{op.summary}</span>
                  </button>
                  {open && (
                    <div class="api-detail">
                      {op.op.description && <p class="hint">{op.op.description}</p>}
                      <p class="hint">
                        {op.protected ? 'Requires the ytdlp_session cookie.' : 'Public endpoint.'}
                        {op.mutation && ' Mutations require the X-CSRF-Token header (sent automatically from your session).'}
                        {op.path === '/api/v1/downloads/clear' &&
                          ' Note: scope "all" stops active downloads and removes every queue/history row. Downloaded files are never deleted.'}
                      </p>

                      {op.pathParams.length > 0 && (
                        <div class="api-fields">
                          <h4>Path parameters</h4>
                          {op.pathParams.map((p) => (
                            <label key={p.name} class="api-field">
                              <span>{p.name}{p.required ? ' *' : ''}</span>
                              <input
                                class="input"
                                value={pathValues[op.key]?.[p.name] ?? ''}
                                placeholder={p.name}
                                onInput={(e) => setPathValue(op.key, p.name, (e.target as HTMLInputElement).value)}
                              />
                            </label>
                          ))}
                        </div>
                      )}

                      {op.queryParams.length > 0 && (
                        <div class="api-fields">
                          <h4>Query parameters</h4>
                          {op.queryParams.map((p) => (
                            <label key={p.name} class="api-field">
                              <span>
                                {p.name}
                                {p.schema?.enum ? ` (${p.schema.enum.join(' | ')})` : ''}
                              </span>
                              <input
                                class="input"
                                value={queryValues[op.key]?.[p.name] ?? ''}
                                placeholder="optional"
                                onInput={(e) => setQueryValue(op.key, p.name, (e.target as HTMLInputElement).value)}
                              />
                            </label>
                          ))}
                        </div>
                      )}

                      {op.hasBody && (
                        <div class="api-fields">
                          <h4>Request body (JSON)</h4>
                          {op.path === '/api/v1/session' && op.method === 'POST' && (
                            <p class="hint text-warning">
                              Enter the admin password yourself; it is sent only to this server
                              and is never stored, prefilled or logged here.
                            </p>
                          )}
                          <textarea
                            class="input api-body"
                            spellcheck={false}
                            rows={Math.min(12, bodyFor(op).split('\n').length + 1)}
                            value={bodyFor(op)}
                            onInput={(e) => setBodyText(op.key, (e.target as HTMLTextAreaElement).value)}
                          />
                        </div>
                      )}

                      <div class="api-fields">
                        <h4>Documentation</h4>
                        {op.op.requestBody?.content && (
                          <>
                            <h5>Request body</h5>
                            {Object.entries(op.op.requestBody.content).map(([ct, media]) => (
                              <div key={ct}>
                                <p class="hint hint-media-type">{ct}</p>
                                <pre class="api-code">{schemaToText(media.schema, spec.components?.schemas)}</pre>
                              </div>
                            ))}
                          </>
                        )}
                        <h5>Responses</h5>
                        {Object.entries(op.op.responses ?? {}).map(([code, resp]) => {
                          const r = resp as { description?: string; content?: Record<string, { schema?: unknown }> }
                          return (
                            <div key={code} class="api-response">
                              <p class="hint hint-response">
                                <b>{code}</b> — {r.description ?? ''}
                              </p>
                              {r.content &&
                                Object.entries(r.content).map(([ct, media]) => (
                                  <div key={ct}>
                                    <p class="hint hint-media-type-tight">{ct}</p>
                                    <pre class="api-code">{schemaToText(media.schema, spec.components?.schemas)}</pre>
                                  </div>
                                ))}
                            </div>
                          )
                        })}
                      </div>

                      <div class="api-actions">
                        <button
                          class="btn small primary"
                          disabled={result === 'loading'}
                          onClick={() => void execute(op)}
                        >
                          {result === 'loading' ? 'Executing…' : 'Execute'}
                        </button>
                        <button
                          class="btn small"
                          onClick={() => {
                            const mark = (ok: boolean) => {
                              setCopied(ok ? op.key : `!${op.key}`)
                              window.setTimeout(() => setCopied(null), 1500)
                            }
                            const clip = navigator.clipboard
                            if (!clip?.writeText) {
                              mark(false)
                              return
                            }
                            clip.writeText(curl).then(
                              () => mark(true),
                              () => mark(false)
                            )
                          }}
                        >
                          {copied === op.key
                            ? 'Copied'
                            : copyFailed && copied === `!${op.key}`
                              ? 'Copy failed'
                              : 'Copy curl'}
                        </button>
                      </div>

                      <h4>curl</h4>
                      <pre class="api-code">{curl}</pre>

                      {result && result !== 'loading' && (
                        <div class="api-result" role="region" aria-label="Response">
                          <h4>
                            Response{' '}
                            {result.status !== null && (
                              <span class={`api-status ${result.status < 400 ? 'ok' : 'err'}`}>{result.status}</span>
                            )}
                          </h4>
                          {result.note && <p class="hint">{result.note}</p>}
                          {result.binaryUrl ? (
                            <img class="api-img" src={result.binaryUrl} alt="thumbnail response" />
                          ) : (
                            <pre class="api-code">{result.text}</pre>
                          )}
                        </div>
                      )}
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        </section>
      ))}
    </div>
  )
}
