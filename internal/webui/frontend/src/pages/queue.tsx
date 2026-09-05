import { h } from 'preact'
import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import type { AppState } from '../state/downloads'
import { queueOrder, rangeBetween, reduce } from '../state/downloads'
import { api } from '../api/client'
import type { ClearScope } from '../api/client'
import type { BatchResult, Download, DownloadOptions } from '../api/types'
import { Dialog } from '../components/dialog'
import { AddOptionsDialog } from '../components/add-options-dialog'
import { DownloadRow, StatePill } from '../components/download-row'
import { Thumb } from '../components/thumb'
import { formatBytes, formatEta, formatPercent, formatSpeed, clearedMessage, displayTitle, hasKnownTotal, summarizeBulkRetry } from '../format'

interface PageProps {
  state: AppState
  setState: (updater: (s: AppState) => AppState) => void
  pushToast: (message: string, ok?: boolean) => void
  onTheme: (t: 'dark' | 'light' | 'system') => void
}

const URL_MAX = 4096
// Mirrors maxBatchIDs in internal/httpapi/downloads.go.
const MAX_BATCH_IDS = 500

/**
 * The clear scopes offered as their own button, narrowest first, so the
 * broader "Clear finished" sits to their right and "Clear all" — which also
 * stops running work — stays last. None of them deletes a downloaded file.
 */
const NARROW_CLEAR_SCOPES = ['completed', 'failed', 'finished'] as const
type NarrowClearScope = (typeof NARROW_CLEAR_SCOPES)[number]

const CLEAR_HINTS: Record<NarrowClearScope, string> = {
  completed: 'Remove completed entries from the list (files are kept)',
  failed: 'Remove failed entries from the list',
  finished: 'Remove completed, failed and deleted entries from the list (files are kept)'
}

// How the toast names what went. "all" spans queued and running rows too, so
// it gets a bare count rather than a state name it would only half fit.
const CLEAR_KINDS: Record<ClearScope, string> = {
  completed: 'completed',
  failed: 'failed',
  deleted: 'files-deleted',
  finished: 'finished',
  all: ''
}

export function QueuePage({ state, setState, pushToast }: PageProps): h.JSX.Element {
  const [url, setUrl] = useState('')
  const [addError, setAddError] = useState<string | null>(null)
  const [busyIds, setBusyIds] = useState<Set<string>>(new Set())
  const [detailsId, setDetailsId] = useState<string | null>(null)
  const [confirmRemove, setConfirmRemove] = useState<{
    ids: string[]
    clearSelection: boolean
  } | null>(null)
  const [confirmClearAll, setConfirmClearAll] = useState(false)
  // The URL the options dialog is open for. Held separately from `url` so the
  // field keeps its value if the dialog is cancelled.
  const [optionsFor, setOptionsFor] = useState<string | null>(null)
  const [clearBusy, setClearBusy] = useState<ClearScope | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [retryAllBusy, setRetryAllBusy] = useState(false)
  const [resumeAllBusy, setResumeAllBusy] = useState(false)
  const [selectionActionBusy, setSelectionActionBusy] = useState(false)
  const anchorId = useRef<string | null>(null)

  const connected = state.connected && state.everConnected
  const loading = !state.everConnected

  const downloads = useMemo(
    () => [...state.downloads.values()].sort(queueOrder),
    [state.downloads]
  )
  const queueRows = downloads.filter((d) =>
    ['queued', 'downloading', 'paused'].includes(d.state)
  )
  const failedRows = downloads.filter((d) => d.state === 'failed')
  const selection = state.selection
  const selectedRows = queueRows.filter((d) => selection.has(d.id))
  const selectedTargetsFor = (action: string) => selectedRows
    .filter((d) => d.allowed_actions.includes(action as never))
    .map((d) => d.id)
  const pausedTargets = queueRows
    .filter((d) => d.state === 'paused' && d.allowed_actions.includes('resume'))
    .map((d) => d.id)
  // "retry" is the server's resume action, so only rows whose state actually
  // allows resuming are sent; anything else would come back as invalid_state
  // and turn a bulk retry into a wall of per-id errors.
  const retryableFailed = failedRows
    .filter((d) => d.allowed_actions.includes('resume'))
    .map((d) => d.id)

  // Live summary line. Every figure is derived from the same rows the table
  // renders, so the headline speed can never disagree with the sum of the
  // progress bars. The server's periodic "stats" event is NOT used here: it
  // only arrives on the 15-second heartbeat, while row updates arrive every
  // 100ms, so preferring it made the total lag far behind the bars.
  const active = queueRows.filter((d) => d.state === 'downloading').length
  const queued = queueRows.filter((d) => d.state === 'queued').length
  const paused = queueRows.filter((d) => d.state === 'paused').length
  const completed = downloads.filter((d) => d.state === 'completed').length
  const totalSpeed = queueRows.reduce(
    (acc, d) => (d.state === 'downloading' ? acc + Math.max(0, d.speed_bytes_per_second) : acc),
    0
  )

  const validateUrl = (raw: string): string | null => {
    if (!raw.trim()) return 'enter a URL'
    if (raw.length > URL_MAX) return `URL exceeds ${URL_MAX} bytes`
    if (/^[\s]*-/.test(raw)) return 'URL may not start with "-"'
    if (/[\u0000-\u001f\u007f]/.test(raw)) return 'URL contains control characters'
    return null
  }

  const addDownload = async () => {
    const err = validateUrl(url)
    if (err) {
      setAddError(err)
      return
    }
    setSubmitting(true)
    try {
      await api.addDownload(url.trim())
      setUrl('')
      setAddError(null)
      pushToast('Added to queue', true)
    } catch (e) {
      setAddError(e instanceof Error ? e.message : 'could not add download')
    } finally {
      setSubmitting(false)
    }
  }

  // Opening the options dialog validates the URL first, so a typo is reported
  // at the field rather than as a failed format probe inside the dialog.
  const openOptions = () => {
    const err = validateUrl(url)
    if (err) {
      setAddError(err)
      return
    }
    setAddError(null)
    setOptionsFor(url.trim())
  }

  const addWithOptions = async (options: DownloadOptions | undefined, startNow: boolean) => {
    const target = optionsFor
    if (!target) return
    await api.addDownload(target, options, startNow)
    setOptionsFor(null)
    setUrl('')
    pushToast(startNow ? 'Added and starting now' : 'Added to queue', true)
  }

  // Queue-wide actions. The server owns the semantics (manager.ClearStates /
  // ClearAll); SSE reconciles the rows — nothing is claimed locally. The
  // counts here are only for the labels: the request names a scope, never a
  // list of ids, so what actually goes is whatever is terminal when it lands.
  const clearCounts: Record<NarrowClearScope, number> = {
    completed,
    failed: failedRows.length,
    finished: downloads.filter((d) => ['completed', 'failed', 'deleted'].includes(d.state)).length
  }
  const totalCount = downloads.length

  const clearQueue = async (scope: ClearScope) => {
    setClearBusy(scope)
    try {
      const resp = await api.clearQueue(scope)
      pushToast(
        resp.removed > 0 ? clearedMessage(resp.removed, CLEAR_KINDS[scope]) : 'Nothing to clear',
        true
      )
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'clear failed')
    } finally {
      setClearBusy(null)
    }
  }

  const act = async (action: string, id: string) => {
    markBusy(id, true)
    try {
      await runBatch(action, [id])
    } finally {
      markBusy(id, false)
    }
  }

  const clearSelection = () => {
    setState((s) => reduce(s, { type: 'clearSelection' }))
    anchorId.current = null
  }

  const runBatch = async (action: string, ids: string[], clearAfter = false) => {
    if (!connected || ids.length === 0) return
    // Removing a row stops an active download and clears its partial data, so
    // it confirms first. It never touches a finished file.
    if (action === 'remove') {
      setConfirmRemove({ ids, clearSelection: clearAfter })
      return
    }
    if (clearAfter) setSelectionActionBusy(true)
    try {
      const resp = await api.batchAction(action, ids)
      reportBatch(resp.results)
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'action failed')
    } finally {
      if (clearAfter) {
        clearSelection()
        setSelectionActionBusy(false)
      }
    }
  }

  const sendConfirmed = async (action: string, ids: string[], clearAfter = false) => {
    if (clearAfter) setSelectionActionBusy(true)
    try {
      const resp = await api.batchAction(action, ids)
      reportBatch(resp.results)
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'action failed')
    } finally {
      if (clearAfter) {
        clearSelection()
        setSelectionActionBusy(false)
      }
    }
  }

  // Typed as the API's own BatchResult rather than an inline shape, so a
  // field the server stops sending — or never sent — is a compile error here
  // instead of a branch that silently never runs.
  const reportBatch = (results: BatchResult[]) => {
    const failures = results.filter((r) => !r.ok)
    if (failures.length === 0) return
    const firstMsg = failures[0].error?.message ?? 'action failed'
    pushToast(
      failures.length === results.length
        ? firstMsg
        : `${results.length - failures.length}/${results.length} succeeded; ${firstMsg}`
    )
  }

  // Bulk retry of the whole failed section. The API caps one batch at 500 ids,
  // so a long backlog goes out in chunks — which means a later chunk can fail
  // after earlier ones already re-queued hundreds of downloads. Each chunk is
  // therefore caught on its own and the work that DID land is counted into the
  // verdict; reporting a bare "retry failed" there would send the user looking
  // for rows that are already running again.
  const retryAllFailed = async () => {
    if (!connected || retryableFailed.length === 0) return
    setRetryAllBusy(true)
    const total = retryableFailed.length
    const results: BatchResult[] = []
    let sendError: string | null = null
    try {
      for (let i = 0; i < total; i += MAX_BATCH_IDS) {
        try {
          const resp = await api.batchAction('retry', retryableFailed.slice(i, i + MAX_BATCH_IDS))
          results.push(...resp.results)
        } catch (e) {
          sendError = e instanceof Error ? e.message : 'retry failed'
          break
        }
      }
      const verdict = summarizeBulkRetry(total, results, sendError)
      pushToast(verdict.message, verdict.ok)
    } finally {
      setRetryAllBusy(false)
    }
  }

  const resumeAllPaused = async () => {
    if (!connected || pausedTargets.length === 0) return
    setResumeAllBusy(true)
    const ids = [...pausedTargets]
    const results: BatchResult[] = []
    let sendError: string | null = null
    try {
      for (let i = 0; i < ids.length; i += MAX_BATCH_IDS) {
        try {
          const resp = await api.batchAction('resume', ids.slice(i, i + MAX_BATCH_IDS))
          results.push(...resp.results)
        } catch (e) {
          sendError = e instanceof Error ? e.message : 'resume failed'
          break
        }
      }
      const failures = results.filter((r) => !r.ok)
      const succeeded = results.length - failures.length
      if (sendError || failures.length > 0) {
        const reason = failures[0]?.error?.message ?? sendError ?? 'resume failed'
        pushToast(`${succeeded}/${ids.length} resumed; ${reason}`)
      } else {
        pushToast(`Resumed ${succeeded} paused download${succeeded === 1 ? '' : 's'}`, true)
      }
    } finally {
      setResumeAllBusy(false)
    }
  }

  // Shift-click range selection. The anchor is the last row selected WITHOUT
  // shift; it deliberately stays put across successive shift-clicks so the
  // range can be widened or narrowed from one fixed end, the way a file
  // manager behaves. Rows are addressed by id rather than index because the
  // list re-sorts under live SSE updates between clicks.
  const selectRow = (id: string, range: boolean) => {
    const span = range
      ? rangeBetween(queueRows.map((d) => d.id), anchorId.current, id)
      : null
    if (span) {
      // The whole span takes the value the clicked row is moving to, so a
      // shift-click extends a selection and shift-clicking back over it
      // clears the same stretch. The value is read inside the updater rather
      // than from this render's closure: two clicks in one frame would
      // otherwise both decide against the same stale selection.
      setState((s) =>
        reduce(s, { type: 'selectMany', ids: span, value: !s.selection.has(id) })
      )
      return
    }
    setState((s) => toggleSel(s, id))
    anchorId.current = id
  }

  const markBusy = (id: string, busy: boolean) => {
    setBusyIds((prev) => {
      const next = new Set(prev)
      if (busy) next.add(id)
      else next.delete(id)
      return next
    })
  }

  // Announce terminal transitions politely through the live region, and
  // surface probe/download failures as toasts. Without this, an item that
  // fails during its metadata probe would flip queued→failed and silently
  // vanish from the queue view (failed rows are not active work).
  const prevStates = useRef(new Map<string, string>())
  useEffect(() => {
    for (const d of state.downloads.values()) {
      const before = prevStates.current.get(d.id)
      if (before !== undefined && before !== d.state) {
        if (d.state === 'completed') {
          announce(`${d.title || d.url} completed`)
        } else if (d.state === 'failed') {
          const reason = firstLine(d.error || '')
          announce(`${d.title || d.url} failed: ${reason}`)
          pushToast(`${displayTitle(d.title, d.url)} failed — ${reason}`, false)
        }
      }
      prevStates.current.set(d.id, d.state)
    }
    // Prune tracked ids so the map cannot grow unbounded across long sessions.
    for (const id of [...prevStates.current.keys()]) {
      if (!state.downloads.has(id)) prevStates.current.delete(id)
    }
  }, [state.downloads])

  function announce(msg: string) {
    const el = document.getElementById('live-region')
    if (el) el.textContent = msg
  }

  const details = detailsId ? state.downloads.get(detailsId) : undefined

  return (
    <div>
      <h1 class="page-title page-title-spaced">Queue</h1>

      <form
        class="add-download-bar"
        onSubmit={(e) => {
          e.preventDefault()
          void addDownload()
        }}
      >
        <label for="add-url" class="visually-hidden">
          Video or playlist URL
        </label>
        <input
          id="add-url"
          class="input"
          placeholder="Paste a video or playlist URL…"
          value={url}
          maxlength={URL_MAX}
          onInput={(e) => {
            setUrl((e.target as HTMLInputElement).value)
            setAddError(null)
          }}
          onKeyDown={(e) => {
            // Enter adds with the configured defaults; Ctrl/Cmd+Enter opens
            // the picker, so neither path costs the other a click.
            if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
              e.preventDefault()
              openOptions()
            }
          }}
          disabled={!connected || submitting}
        />
        <div class="add-actions">
          <button class="btn primary" type="submit" disabled={!connected || submitting}>
            {submitting ? 'Adding…' : 'Add download'}
          </button>
          <button
            class="btn"
            type="button"
            disabled={!connected || submitting}
            title="Choose quality, format and subtitles for this download"
            onClick={openOptions}
          >
            Add with options…
          </button>
        </div>
      </form>
      {addError && <p class="field-error add-download-error" role="alert">{addError}</p>}

      <div class="summary-line" aria-live="off">
        <span>
          Active <b>{active}</b>
        </span>
        <span>
          Queued <b>{queued}</b>
        </span>
        <span>
          Completed <b>{completed}</b>
        </span>
        <span>
          Speed <b>{formatSpeed(totalSpeed)}</b>
        </span>
        {paused > 0 ? (
          <span>
            Paused <b>{paused}</b>
          </span>
        ) : null}
      </div>

      <div class="queue-management" role="group" aria-labelledby="queue-management-title">
        <div class="queue-management-copy">
          <strong id="queue-management-title">Queue actions</strong>
          <span>Resume paused work or clear entries. Downloaded files are never touched.</span>
        </div>
        <div class="global-actions" role="toolbar" aria-label="Queue-wide actions">
          <button class="btn small" disabled={!connected || resumeAllBusy || pausedTargets.length === 0}
            onClick={() => void resumeAllPaused()}
            title="Resume every paused download while respecting the concurrency limit">
            {resumeAllBusy ? 'Resuming…' : `Resume all paused${pausedTargets.length > 0 ? ` (${pausedTargets.length})` : ''}`}
          </button>
          {NARROW_CLEAR_SCOPES.map((scope) => (
            <button
              key={scope}
              class="btn small"
              disabled={!connected || clearBusy !== null || clearCounts[scope] === 0}
              onClick={() => void clearQueue(scope)}
              title={CLEAR_HINTS[scope]}
            >
              {clearBusy === scope
                ? 'Clearing…'
                : `Clear ${scope}${clearCounts[scope] > 0 ? ` (${clearCounts[scope]})` : ''}`}
            </button>
          ))}
          <button class="btn small" disabled={!connected || clearBusy !== null || totalCount === 0}
            onClick={() => setConfirmClearAll(true)}
            title="Stop active downloads and remove every entry; downloaded files are kept">
            {clearBusy === 'all' ? 'Clearing…' : 'Clear all'}
          </button>
        </div>
      </div>

      {selection.size > 0 && (
        <div class="selection-bar" role="toolbar" aria-label="Selection actions">
          <strong>{selection.size} selected</strong>
          <div class="spacer" />
          <button class="btn small" disabled={!connected || busyIds.size > 0 || selectionActionBusy || selectedTargetsFor('pause').length === 0} onClick={() => void runBatch('pause', selectedTargetsFor('pause'), true)}>
            Pause
          </button>
          <button class="btn small" disabled={!connected || busyIds.size > 0 || selectionActionBusy || selectedTargetsFor('resume').length === 0} onClick={() => void runBatch('resume', selectedTargetsFor('resume'), true)}>
            Resume
          </button>
          <button class="btn small" disabled={!connected || busyIds.size > 0 || selectionActionBusy || selectedTargetsFor('start_now').length === 0} onClick={() => void runBatch('start_now', selectedTargetsFor('start_now'), true)}>
            Force start/resume
          </button>
          {/* Distinct from "Deselect all" below: this one acts on the queue
              (stop and drop the chosen rows), that one only empties the
              selection. The danger styling and the wording keep them apart. */}
          <button class="btn small danger" disabled={!connected || busyIds.size > 0 || selectionActionBusy || selectedTargetsFor('remove').length === 0} onClick={() => void runBatch('remove', selectedTargetsFor('remove'), true)}>
            Remove from queue
          </button>
          <button class="btn small" onClick={clearSelection}>
            Deselect all
          </button>
        </div>
      )}

      {loading ? (
        <div aria-hidden="true">
          <div class="skeleton" />
          <div class="skeleton" />
          <div class="skeleton" />
        </div>
      ) : queueRows.length === 0 && failedRows.length === 0 ? (
        <div class="empty-state">
          <p>Queue is empty.</p>
          <p>Paste a video or playlist URL above to get started.</p>
        </div>
      ) : (
        <>
          {queueRows.length > 0 && (
            <div class="queue-table">
              <div class="row-main queue-head" aria-hidden="true">
                <span />
                <span />
                <span>Name</span>
                <span>Status</span>
                <span>Progress</span>
                <span>Speed</span>
                <span>ETA</span>
                <span class="text-right">Actions</span>
              </div>
              <ul class="queue-list">
                {queueRows.map((d) => (
                  <DownloadRow
                    key={d.id}
                    download={d}
                    selected={selection.has(d.id)}
                    busy={busyIds.has(d.id) || !connected}
                    onToggleSelect={(range) => selectRow(d.id, range)}
                    onAction={(a) => void act(a, d.id)}
                    onShowDetails={() => setDetailsId(d.id)}
                  />
                ))}
              </ul>
            </div>
          )}
          {failedRows.length > 0 && (
            <section aria-label="Failed downloads" class="section-spaced">
              <div class="section-head">
                <h2 class="failed-title">
                  Failed ({failedRows.length})
                </h2>
                {retryableFailed.length > 0 && (
                  <button
                    class="btn small"
                    disabled={!connected || retryAllBusy}
                    onClick={() => void retryAllFailed()}
                    title="Re-queue every failed download"
                  >
                    {retryAllBusy ? 'Retrying…' : `↻ Retry all failed (${retryableFailed.length})`}
                  </button>
                )}
              </div>
              <ul class="queue-list">
                {failedRows.map((d) => (
                  <FailedRow
                    key={d.id}
                    download={d}
                    busy={busyIds.has(d.id) || !connected}
                    onAction={(a) => void act(a, d.id)}
                  />
                ))}
              </ul>
            </section>
          )}
        </>
      )}

      {confirmRemove && confirmRemove.ids.length > 0 && (
        <Dialog title={`Remove ${confirmRemove.ids.length} download${confirmRemove.ids.length > 1 ? 's' : ''}?`} onClose={() => setConfirmRemove(null)}>
          <p>
            Removing stops active downloads and clears partial data. Completed
            files already on disk are kept.
          </p>
          <div class="dialog-actions">
            <button class="btn" onClick={() => setConfirmRemove(null)}>
              Cancel
            </button>
            <button
              class="btn danger"
              onClick={() => {
                const { ids, clearSelection: clearAfter } = confirmRemove
                setConfirmRemove(null)
                void sendConfirmed('remove', ids, clearAfter)
              }}
            >
              Remove
            </button>
          </div>
        </Dialog>
      )}

      {confirmClearAll && (
        <Dialog title="Clear the entire queue?" onClose={() => !clearBusy && setConfirmClearAll(false)}>
          <p>
            This stops all active downloads and removes every queue and history
            entry. Downloaded files are never deleted — only yt-dlp's own
            partial data for downloads that were still running.
          </p>
          <div class="dialog-actions">
            <button class="btn" onClick={() => setConfirmClearAll(false)} disabled={clearBusy !== null}>
              Cancel
            </button>
            <button
              class="btn danger"
              disabled={clearBusy !== null}
              onClick={() => {
                setConfirmClearAll(false)
                void clearQueue('all')
              }}
            >
              {clearBusy === 'all' ? 'Clearing…' : 'Clear everything'}
            </button>
          </div>
        </Dialog>
      )}

      {optionsFor && (
        <AddOptionsDialog
          url={optionsFor}
          onCancel={() => setOptionsFor(null)}
          onAdd={addWithOptions}
        />
      )}

      {details && (
        <DetailsDrawer download={details} onClose={() => setDetailsId(null)} />
      )}
    </div>
  )
}

function firstLine(s: string): string {
  const i = s.indexOf('\n')
  return i >= 0 ? s.slice(0, i) : s
}

/**
 * Failed rows stay visible right under the active queue so a fast metadata
 * failure can never look like a silent disappearance. Retry maps to the
 * server's resume/retry action; the error's first line is shown inline.
 */
function FailedRow({
  download: d,
  busy,
  onAction
}: {
  download: Download
  busy: boolean
  onAction: (action: string) => void
}): h.JSX.Element {
  const reason = firstLine(d.error || 'download failed')
  const writeProblem = /read-only|unable to open for writing|permission denied/i.test(
    d.error || ''
  )
  return (
    <li>
      <div class="row-main row-failed">
        <div class="row-meta">
          <div class="row-title">{displayTitle(d.title, d.url)}</div>
          <div class="row-sub">
            <span class="pill st-failed">Failed</span>
            <span class="text-danger">{reason}</span>
          </div>
          {writeProblem && (
            <p class="hint text-warning">
              yt-dlp could not write its output. Set your download directory
              under Settings → Downloads (e.g. /downloads).
            </p>
          )}
        </div>
        <div class="row-actions">
          {d.allowed_actions.includes('resume') && (
            <button
              class="btn small"
              disabled={busy}
              onClick={() => onAction('retry')}
              title="Retry this download"
            >
              ↻ Retry
            </button>
          )}
          {d.allowed_actions.includes('remove') && (
            <button
              class="btn small icon-btn"
              disabled={busy}
              onClick={() => onAction('remove')}
              aria-label={`Remove failed item ${displayTitle(d.title, d.url)} from list`}
              title="Remove from list"
            >
              ✕
            </button>
          )}
        </div>
      </div>
    </li>
  )
}

const FOCUSABLE =
  'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'

export function DetailsDrawer({ download: d, onClose }: { download: Download; onClose: () => void }) {
  const panel = useRef<HTMLElement | null>(null)

  useEffect(() => {
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    const bodyAlreadyLocked = document.body.classList.contains('modal-open')
    document.body.classList.add('modal-open')
    window.addEventListener('keydown', closeOnEscape)
    return () => {
      window.removeEventListener('keydown', closeOnEscape)
      if (!bodyAlreadyLocked) document.body.classList.remove('modal-open')
    }
  }, [onClose])

  // aria-modal="true" tells assistive technology the rest of the page is
  // inert. That is only true if focus actually moves here and stays here, so
  // take focus on open, trap Tab inside, and hand it back on close.
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    const first = panel.current?.querySelector<HTMLElement>(FOCUSABLE)
    ;(first ?? panel.current)?.focus()
    return () => {
      requestAnimationFrame(() => {
        if (previous?.isConnected) previous.focus()
      })
    }
  }, [])

  const trapTab = (event: KeyboardEvent) => {
    if (event.key !== 'Tab' || !panel.current) return
    const items = Array.from(
      panel.current.querySelectorAll<HTMLElement>(FOCUSABLE)
    ).filter((el) => !el.hasAttribute('disabled'))
    if (items.length === 0) return
    const first = items[0]
    const last = items[items.length - 1]
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
  }

  return (
    <>
      <div class="drawer-overlay" aria-hidden="true" onClick={onClose} />
      <aside
        class="drawer"
        role="dialog"
        aria-modal="true"
        aria-label="Download details"
        tabIndex={-1}
        ref={(el) => {
          panel.current = el as HTMLElement | null
        }}
        onKeyDown={trapTab}
      >
        <div class="detail-head">
          <h2>Details</h2>
          <button class="btn small icon-btn" onClick={onClose} aria-label="Close details">
            ✕
          </button>
        </div>
        <Thumb download={d} large />
        <dl class="detail-list">
          <dt>Title</dt>
          <dd>{displayTitle(d.title, d.url)}</dd>
          <dt>URL</dt>
          <dd>
            {d.url}{' '}
            <button
              class="btn small"
              onClick={() => void navigator.clipboard?.writeText(d.url).catch(() => undefined)}
            >
              Copy
            </button>
          </dd>
          <dt>ID</dt>
          <dd class="kv">{d.id}</dd>
          <dt>State</dt>
          <dd>
            <StatePill state={d.state} /> {d.forced ? '(forced)' : ''}
          </dd>
          <dt>Pause origin</dt>
          <dd>{d.pause_origin}</dd>
          <dt>Options</dt>
          <dd>
            {d.options_invalid
              ? 'unreadable after a restart — remove this row and add the URL again'
              : d.options_summary || 'defaults from Settings'}
          </dd>
          <dt>Progress</dt>
          <dd>
            {hasKnownTotal(d.total_bytes) ? (
              <>
                <progress class="progress" max={100} value={d.progress} aria-label="Progress" />{' '}
                {formatPercent(d.progress)}
              </>
            ) : (
              <span>{formatBytes(d.downloaded_bytes)} downloaded, total size unknown</span>
            )}
          </dd>
          <dt>Bytes</dt>
          <dd>
            {formatBytes(d.downloaded_bytes)} / {formatBytes(d.total_bytes)}
          </dd>
          <dt>Speed</dt>
          <dd>{formatSpeed(d.speed_bytes_per_second)}</dd>
          <dt>ETA</dt>
          <dd>{formatEta(d.eta_seconds)}</dd>
          <dt>Added</dt>
          <dd>{new Date(d.added_at).toLocaleString()}</dd>
          {d.started_at && (
            <>
              <dt>Started</dt>
              <dd>{new Date(d.started_at).toLocaleString()}</dd>
            </>
          )}
          {d.completed_at && (
            <>
              <dt>Completed</dt>
              <dd>{new Date(d.completed_at).toLocaleString()}</dd>
            </>
          )}
          {d.error && (
            <>
              <dt>Error</dt>
              <dd class="detail-error">{d.error}</dd>
            </>
          )}
          {d.files.length > 0 && (
            <>
              <dt>Files</dt>
              <dd>
                <ul class="file-list">
                  {d.files.map((f) => (
                    <li key={f} class="kv">
                      {f}
                    </li>
                  ))}
                </ul>
              </dd>
            </>
          )}
        </dl>
      </aside>
    </>
  )
}

function toggleSel(s: AppState, id: string): AppState {
  return reduce(s, { type: 'toggleSelect', id })
}
