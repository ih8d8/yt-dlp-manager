import { useState } from 'preact/hooks'
import type { Download } from '../api/types'

/**
 * Deterministic neutral placeholder: a tinted tile with the title's initial.
 * No network, no external assets — used when an item has no probed
 * thumbnail or the proxy fetch fails.
 */
function placeholderDataURI(id: string, title: string): string {
  let hash = 0
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) | 0
  const hue = Math.abs(hash) % 360
  const initial = ((title || '?').trim()[0] ?? '?').toUpperCase()
  const svg =
    `<svg xmlns='http://www.w3.org/2000/svg' width='192' height='108'>` +
    `<rect width='100%' height='100%' fill='hsl(${hue} 30% 22%)'/>` +
    `<text x='50%' y='54%' fill='hsl(${hue} 45% 62%)' font-family='sans-serif' ` +
    `font-size='44' text-anchor='middle' dominant-baseline='middle'>${initial}</text></svg>`
  return `data:image/svg+xml,${encodeURIComponent(svg)}`
}

export function Thumb({ download: d, large = false }: { download: Download; large?: boolean }) {
  const [failed, setFailed] = useState(false)
  const cls = large ? 'thumb thumb-large' : 'thumb'
  const src = !failed && d.thumbnail_url ? `/api/v1/downloads/${encodeURIComponent(d.id)}/thumbnail` : null
  return (
    <div class={cls}>
      {src ? (
        <img
          src={src}
          alt=""
          loading="lazy"
          onError={() => setFailed(true)}
        />
      ) : (
        <img src={placeholderDataURI(d.id, d.title)} alt="" aria-hidden="true" />
      )}
    </div>
  )
}
