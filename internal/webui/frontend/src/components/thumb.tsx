import { useState } from 'preact/hooks'
import type { Download } from '../api/types'

/**
 * Deterministic neutral placeholder: a tinted tile with the title's initial.
 * No network, no external assets — used when an item has no probed
 * thumbnail or the proxy fetch fails.
 */
function placeholderDataURI(id: string, title: string): string {
  const hue = hashString(id) % 360
  const initial = ((title || '?').trim()[0] ?? '?').toUpperCase()
  const svg =
    `<svg xmlns='http://www.w3.org/2000/svg' width='192' height='108'>` +
    `<rect width='100%' height='100%' fill='hsl(${hue} 30% 22%)'/>` +
    `<text x='50%' y='54%' fill='hsl(${hue} 45% 62%)' font-family='sans-serif' ` +
    `font-size='44' text-anchor='middle' dominant-baseline='middle'>${escapeXML(initial)}</text></svg>`
  return `data:image/svg+xml,${encodeURIComponent(svg)}`
}

/**
 * A small deterministic string hash. Not cryptographic and not required to
 * agree with anything the server computes — it only has to be stable, and to
 * change when its input does.
 */
function hashString(s: string): number {
  let hash = 0
  for (let i = 0; i < s.length; i++) hash = (hash * 31 + s.charCodeAt(i)) | 0
  return Math.abs(hash)
}

/**
 * The initial is pasted into SVG markup, so a title starting with "<" or "&"
 * would otherwise produce a document the browser refuses to parse — a broken
 * image icon exactly where the fallback was supposed to prevent one.
 */
function escapeXML(s: string): string {
  return s.replace(/[<>&"']/g, (c) =>
    c === '<' ? '&lt;' : c === '>' ? '&gt;' : c === '&' ? '&amp;' : c === '"' ? '&quot;' : '&apos;'
  )
}

/**
 * What one thumbnail attempt is identified by: the probed URL, and the run
 * that asked for it.
 *
 * A plain "it failed" boolean latched for the life of the component, so a
 * preview that only became available later was never requested again. Keying
 * on the probed URL alone is not enough either: a retry usually re-probes to
 * the SAME url, and a transient timeout or 502 would still leave the
 * placeholder up forever. `started_at` changes once per download attempt, so
 * including it gives each attempt one fresh try at the image and no more —
 * never a request per render.
 */
export function thumbKey(d: Pick<Download, 'thumbnail_url' | 'started_at'>): string {
  return `${d.thumbnail_url ?? ''}\n${d.started_at ?? ''}`
}

type ThumbFields = Pick<Download, 'id' | 'thumbnail_url' | 'started_at'>

/**
 * The proxy URL to request for an item, or null to fall back to the
 * placeholder. `failedFor` is the thumbKey of the attempt that failed.
 */
export function thumbSource(d: ThumbFields, failedFor: string | null): string | null {
  if (!d.thumbnail_url) return null
  if (failedFor === thumbKey(d)) return null
  // ?v= is a cache buster, not a parameter the server reads. The proxy path is
  // keyed by item id alone, so after a re-probe changes an item's thumbnail
  // the src string would be byte-identical: Preact would leave the attribute
  // untouched, and even a fresh request would be answered from the browser's
  // own cache for the 24 hours the response advertises. Deriving it from the
  // probed URL makes a new image a new URL. The value is a hash of our own
  // probe output, and query strings are kept out of the access log.
  const version = hashString(d.thumbnail_url).toString(36)
  return `/api/v1/downloads/${encodeURIComponent(d.id)}/thumbnail?v=${version}`
}

export function Thumb({ download: d, large = false }: { download: Download; large?: boolean }) {
  const [failedFor, setFailedFor] = useState<string | null>(null)
  const cls = large ? 'thumb thumb-large' : 'thumb'
  const src = thumbSource(d, failedFor)
  return (
    <div class={cls}>
      {src ? (
        <img
          src={src}
          alt=""
          loading="lazy"
          onError={() => setFailedFor(thumbKey(d))}
        />
      ) : (
        <img src={placeholderDataURI(d.id, d.title)} alt="" aria-hidden="true" />
      )}
    </div>
  )
}
