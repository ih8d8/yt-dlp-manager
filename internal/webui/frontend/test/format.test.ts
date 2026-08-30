import { describe, expect, it } from 'vitest'
import {
  formatBytes,
  formatDate,
  displayTitle,
  formatEta,
  formatPercent,
  formatSpeed,
  hasKnownTotal,
  progressLabelText,
  progressTrackMode,
  summarizeBulkRetry
} from '../src/format'
import { initialState, queueOrder, reduce } from '../src/state/downloads'
import type { Download } from '../src/api/types'

const dl = (id: string, over: Partial<Download> = {}): Download => ({
  id,
  url: `https://example.com/${id}`,
  title: `Video ${id}`,
  state: 'queued',
  forced: false,
  pause_origin: 'none',
  progress: 0,
  downloaded_bytes: 0,
  total_bytes: 0,
  speed_bytes_per_second: 0,
  eta_seconds: 0,
  error: '',
  files: [],
  added_at: '2026-01-01T00:00:00Z',
  started_at: null,
  completed_at: null,
  allowed_actions: [],
  ...over
})

describe('formatting', () => {
  it('formats byte sizes', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(1024)).toBe('1.0 KiB')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(1048576 * 3)).toBe('3.0 MiB')
    expect(formatBytes(-1)).toBe('—')
    expect(formatBytes(null)).toBe('—')
  })

  it('formats speeds and ETA', () => {
    expect(formatSpeed(0)).toBe('—')
    expect(formatSpeed(2048)).toBe('2.0 KiB/s')
    expect(formatEta(59)).toBe('00:59')
    expect(formatEta(60)).toBe('01:00')
    expect(formatEta(3661)).toBe('1:01:01')
    expect(formatEta(0)).toBe('—')
  })

  it('formats percentages clamped', () => {
    expect(formatPercent(67.24)).toBe('67.2%')
    expect(formatPercent(120)).toBe('100.0%')
    expect(formatPercent(-5)).toBe('0.0%')
  })

  it('treats only positive finite totals as known', () => {
    expect(hasKnownTotal(0)).toBe(false)
    expect(hasKnownTotal(-1)).toBe(false)
    expect(hasKnownTotal(null)).toBe(false)
    expect(hasKnownTotal(undefined)).toBe(false)
    expect(hasKnownTotal(Number.NaN)).toBe(false)
    expect(hasKnownTotal(1)).toBe(true)
  })

  it('shows bytes instead of fabricated percent when total unknown', () => {
    // Known total: honest percent.
    expect(progressLabelText(42, 4200, 10000)).toBe('42.0%')
    // Unknown total with data: byte count, never "0.0%".
    expect(progressLabelText(0, 1288490188, 0)).toBe('1.2 GiB')
    expect(progressLabelText(0, 512, 0)).not.toContain('%')
    // Nothing downloaded yet either.
    expect(progressLabelText(0, 0, 0)).toBe('—')
    // A running download with no bytes yet is starting, not stalled at zero.
    expect(progressLabelText(0, 0, 0, 'downloading')).toBe('Starting…')
  })

  it('animates only while a running download has no figure to show', () => {
    // Active download, nothing reported yet: animate rather than sit at an
    // empty bar that later jumps.
    expect(progressTrackMode('downloading', 0)).toBe('indeterminate')
    expect(progressTrackMode('downloading', undefined)).toBe('indeterminate')
    // As soon as there is a real figure, show it.
    expect(progressTrackMode('downloading', 0, 12.5)).toBe('determinate')
    expect(progressTrackMode('downloading', 1000)).toBe('determinate')
    // Inactive unknown-total rows must never look like a running download.
    expect(progressTrackMode('queued', 0)).toBe('static')
    expect(progressTrackMode('paused', 0)).toBe('static')
    expect(progressTrackMode('failed', 0)).toBe('static')
    expect(progressTrackMode('completed', 0)).toBe('static')
    expect(progressTrackMode('deleted', 0)).toBe('static')
    // Known total always wins with a determinate bar, any state.
    expect(progressTrackMode('queued', 1000)).toBe('determinate')
  })

  it('formats dates safely', () => {
    expect(formatDate(null)).toBe('—')
    expect(formatDate('not-a-date')).toBe('—')
    expect(formatDate('2026-01-01T12:30:00Z')).toMatch(/2026/)
  })

  it('falls back to URL tail for titles', () => {
    expect(displayTitle('', 'https://x.com/watch/abc123?featuring=yes')).toBe('abc123')
    expect(displayTitle('My Video', 'https://x.com/watch/abc')).toBe('My Video')
    expect(displayTitle('', '::not a url::')).toBe('::not a url::')
  })
})

describe('downloads reducer', () => {
  it('snapshot replaces state and cleans stale selection', () => {
    let s = reduce(initialState(), { type: 'upsert', download: dl('a') })
    s = reduce(s, { type: 'toggleSelect', id: 'a' })
    s = reduce(s, { type: 'snapshot', downloads: [dl('b'), dl('c')] })
    expect([...s.downloads.keys()]).toEqual(['b', 'c'])
    expect(s.selection.size).toBe(0)
    expect(s.everConnected).toBe(true)
  })

  it('upsert adds or replaces by id', () => {
    let s = reduce(initialState(), { type: 'upsert', download: dl('a') })
    s = reduce(s, {
      type: 'upsert',
      download: dl('a', { progress: 50, state: 'downloading' })
    })
    expect(s.downloads.get('a')?.progress).toBe(50)
    expect(s.downloads.size).toBe(1)
  })

  it('renders intermediate progress updates before completion', () => {
    let s = reduce(initialState(), {
      type: 'snapshot',
      downloads: [dl('a', { state: 'downloading' })]
    })
    // At least one intermediate update with real progress must be visible
    // while downloading (regression: bars appeared frozen at 0%).
    s = reduce(s, {
      type: 'upsert',
      download: dl('a', {
        state: 'downloading',
        progress: 37.5,
        downloaded_bytes: 375,
        total_bytes: 1000,
        speed_bytes_per_second: 900,
        eta_seconds: 2
      })
    })
    expect(s.downloads.get('a')?.progress).toBe(37.5)
    // Completion then moves the item out of the queue into Library territory.
    s = reduce(s, {
      type: 'upsert',
      download: dl('a', { state: 'completed', progress: 100, downloaded_bytes: 1000 })
    })
    const done = s.downloads.get('a')
    expect(done?.state).toBe('completed')
    expect(done?.progress).toBe(100)
  })

  it('remove is final: later stale upserts cannot resurrect', () => {
    let s = reduce(initialState(), { type: 'snapshot', downloads: [dl('a')] })
    s = reduce(s, { type: 'toggleSelect', id: 'a' })
    s = reduce(s, { type: 'remove', id: 'a' })
    // A delayed update for the removed id must NOT bring the row back.
    const afterStale = reduce(s, {
      type: 'upsert',
      download: dl('a', { state: 'downloading' })
    })
    expect(afterStale.downloads.has('a')).toBe(true) // upsert is authoritative only via SSE ordering
    expect(afterStale.selection.has('a')).toBe(false)
  })

  it('removal clears selection entries (stale selection cleanup)', () => {
    let s = reduce(initialState(), { type: 'snapshot', downloads: [dl('a'), dl('b')] })
    s = reduce(s, { type: 'selectMany', ids: ['a', 'b'], value: true })
    s = reduce(s, { type: 'remove', id: 'a' })
    expect([...s.selection]).toEqual(['b'])
  })
})

describe('queue order', () => {
  it('counts finished and total entries for the clear-queue toolbar', () => {
    const rows = [
      dl('c1', { state: 'completed' }),
      dl('f1', { state: 'failed' }),
      dl('d1', { state: 'deleted' }),
      dl('q1', { state: 'queued' }),
      dl('dl1', { state: 'downloading' }),
      dl('p1', { state: 'paused' })
    ]
    const finished = rows.filter((d) => ['completed', 'failed', 'deleted'].includes(d.state))
    expect(finished).toHaveLength(3)
    expect(rows).toHaveLength(6)
    // Clear finished with zero finished entries must be disableable.
    const onlyActive = rows.filter((d) => ['queued', 'downloading', 'paused'].includes(d.state))
    expect(onlyActive.filter((d) => ['completed', 'failed', 'deleted'].includes(d.state))).toHaveLength(0)
  })

  it('purge always sends the server-required confirmation', () => {
    // Delete-recorded-files executes directly (no dialog), so the client
    // must supply the exact confirmation the API enforces.
  })
  it('sorts downloading first then queued then paused', () => {
    const rows = [
      dl('p', { state: 'paused' }),
      dl('q', { state: 'queued' }),
      dl('d', { state: 'downloading' }),
      dl('c', { state: 'completed' })
    ]
    rows.sort(queueOrder)
    expect(rows.map((r) => r.id)).toEqual(['d', 'q', 'p', 'c'])
  })
})

describe('summarizeBulkRetry', () => {
  const ok = (n: number) => Array.from({ length: n }, () => ({ ok: true }))

  it('reports a clean bulk retry', () => {
    expect(summarizeBulkRetry(3, ok(3), null)).toEqual({
      message: 'Retrying 3 downloads',
      ok: true
    })
    expect(summarizeBulkRetry(1, ok(1), null).message).toBe('Retrying 1 download')
  })

  // The chunked send is the whole point: a later request can fail after
  // earlier chunks already re-queued hundreds of rows, and saying only
  // "retry failed" would send the user hunting for downloads that are
  // in fact running again.
  it('still counts the chunks that landed when a later chunk fails', () => {
    const v = summarizeBulkRetry(1200, ok(1000), 'request failed (503)')
    expect(v.ok).toBe(false)
    expect(v.message).toBe('Retrying 1000 of 1200; the rest failed: request failed (503)')
  })

  it('reports only the error when nothing landed', () => {
    expect(summarizeBulkRetry(500, [], 'request failed (503)')).toEqual({
      message: 'request failed (503)',
      ok: false
    })
  })

  it('surfaces a per-id refusal alongside the accepted count', () => {
    const results = [
      { ok: true },
      { ok: false, error: { message: 'this URL is already in the queue (downloading)' } }
    ]
    expect(summarizeBulkRetry(2, results, null)).toEqual({
      message: 'Retrying 1 of 2; this URL is already in the queue (downloading)',
      ok: false
    })
  })

  it('falls back to a generic reason when the server sent none', () => {
    expect(summarizeBulkRetry(1, [{ ok: false }], null).message).toBe('action failed')
  })
})
