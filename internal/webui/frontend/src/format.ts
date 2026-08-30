/** Centralized formatting: bytes, rates, ETA, percentage, timestamps. */

export function formatBytes(n: number | null | undefined): string {
  if (n === null || n === undefined || n < 0 || !Number.isFinite(n)) return '—'
  if (n === 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let value = n
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  const digits = unit === 0 ? 0 : 1
  return `${value.toFixed(digits)} ${units[unit]}`
}

export function formatSpeed(bytesPerSecond: number): string {
  if (!bytesPerSecond || bytesPerSecond <= 0) return '—'
  return `${formatBytes(bytesPerSecond)}/s`
}

export function formatEta(seconds: number): string {
  if (seconds === null || seconds === undefined || seconds <= 0 || !Number.isFinite(seconds)) {
    return '—'
  }
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = Math.floor(seconds % 60)
  if (h > 0) return `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
}

export function formatPercent(progress: number): string {
  const clamped = Math.min(100, Math.max(0, progress))
  return `${clamped.toFixed(1)}%`
}

/**
 * True when yt-dlp reported a usable total size. Streams (HLS/DASH) often
 * report none; percent progress would then be fabricated, so rows fall back
 * to showing downloaded bytes with an indeterminate bar instead.
 */
export function hasKnownTotal(totalBytes: number | null | undefined): boolean {
  return typeof totalBytes === 'number' && Number.isFinite(totalBytes) && totalBytes > 0
}

/**
 * Compact label above the progress bar: percent when the total is known,
 * otherwise the raw byte count (never a fabricated percent).
 *
 * A download that has started but not yet reported any bytes reads as
 * "Starting…" rather than "0.0%". yt-dlp spends real time resolving formats
 * and opening connections before the first byte, and a hard 0.0% during that
 * window looks like a stalled transfer rather than one that is spinning up.
 */
export function progressLabelText(
  progress: number,
  downloadedBytes: number,
  totalBytes: number,
  state = ''
): string {
  if (hasKnownTotal(totalBytes) || progress > 0) return formatPercent(progress)
  if (downloadedBytes > 0) return formatBytes(downloadedBytes)
  return state === 'downloading' ? 'Starting…' : '—'
}

export type ProgressTrackMode = 'determinate' | 'indeterminate' | 'static'

/**
 * How the progress track should render for this item:
 *   - determinate:   a real percentage is known — honest percent bar,
 *   - indeterminate: actively downloading but yt-dlp has not reported a size
 *                    or a percentage yet, so the bar animates to show work is
 *                    happening instead of sitting empty until it jumps,
 *   - static:        unknown total and not running — a neutral track that
 *                    never mimics a live download.
 */
export function progressTrackMode(
  state: string,
  totalBytes: number | null | undefined,
  progress = 0
): ProgressTrackMode {
  if (hasKnownTotal(totalBytes) || progress > 0) return 'determinate'
  if (state === 'downloading') return 'indeterminate'
  return 'static'
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short'
  }).format(d)
}

export function displayTitle(title: string, url: string): string {
  const t = (title || '').trim()
  if (t) return t
  try {
    const u = new URL(url)
    return u.pathname.split('/').filter(Boolean).pop() ?? url
  } catch {
    return url
  }
}

export function formatDateCompact(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit'
  }).format(d)
}

/** The shape of one entry in a batch-action response, as far as messaging cares. */
export interface BatchOutcome {
  ok: boolean
  error?: { message?: string }
}

/**
 * One honest sentence for a bulk action that was sent in chunks.
 *
 * `sendError` is set when a whole request failed (transport, auth, a 5xx) and
 * the remaining chunks were abandoned. Crucially, whatever earlier chunks
 * already accepted is still counted: reporting a bare failure after hundreds
 * of downloads were re-queued sends the user hunting for rows that are in fact
 * running again.
 */
export function summarizeBulkRetry(
  total: number,
  results: BatchOutcome[],
  sendError: string | null
): { message: string; ok: boolean } {
  const accepted = results.filter((r) => r.ok).length
  if (sendError) {
    return {
      message:
        accepted > 0
          ? `Retrying ${accepted} of ${total}; the rest failed: ${sendError}`
          : sendError,
      ok: false
    }
  }
  const firstFailure = results.find((r) => !r.ok)
  if (firstFailure) {
    const reason = firstFailure.error?.message ?? 'action failed'
    return {
      message: accepted > 0 ? `Retrying ${accepted} of ${total}; ${reason}` : reason,
      ok: false
    }
  }
  return {
    message: `Retrying ${accepted} download${accepted === 1 ? '' : 's'}`,
    ok: true
  }
}
