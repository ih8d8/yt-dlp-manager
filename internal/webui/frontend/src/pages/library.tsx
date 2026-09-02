import { h } from 'preact'
import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import type { AppState } from '../state/downloads'
import { rangeBetween } from '../state/downloads'
import type { Download } from '../api/types'
import { api } from '../api/client'
import { Dialog } from '../components/dialog'
import { DownloadRow } from '../components/download-row'
import { DetailsDrawer } from './queue'
import { summarizeBulkClear } from '../format'

interface PageProps {
  state: AppState
  pushToast: (message: string, ok?: boolean) => void
}

type LibFilter = 'all' | 'completed' | 'failed' | 'deleted'
type SortKey = 'newest' | 'oldest' | 'title' | 'size'

// Mirrors maxBatchIDs in internal/httpapi/downloads.go.
const MAX_BATCH_IDS = 500

/** What one "Clear …" button clears. 'history' is every finished state. */
type ClearTarget = 'completed' | 'failed' | 'deleted' | 'history'

const CLEAR_LABELS: Record<ClearTarget, string> = {
  completed: 'completed',
  failed: 'failed',
  deleted: 'files-deleted',
  history: 'all history'
}

/**
 * Library is a filtered view over completed/failed/deleted manager items.
 * It is not a filesystem index: entries disappear when cleared from the
 * queue history (e.g. clear-finished).
 */
export function LibraryPage({ state, pushToast }: PageProps): h.JSX.Element {
  const [filter, setFilter] = useState<LibFilter>('completed')
  const [query, setQuery] = useState('')
  const [sort, setSort] = useState<SortKey>('newest')
  const [page, setPage] = useState(0)
  const [busyIds, setBusyIds] = useState<Set<string>>(new Set())
  const [selection, setSelection] = useState<Set<string>>(new Set())
  const [confirmRemove, setConfirmRemove] = useState<string[] | null>(null)
  const [confirmClear, setConfirmClear] = useState<ClearTarget | null>(null)
  const [clearBusy, setClearBusy] = useState<ClearTarget | null>(null)
  const [detailsId, setDetailsId] = useState<string | null>(null)

  const connected = state.connected && state.everConnected
  const pageSize = 50

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    let list = [...state.downloads.values()].filter((d) =>
      filter === 'all' ? true : d.state === filter
    )
    if (q) {
      list = list.filter(
        (d) => d.title.toLowerCase().includes(q) || d.url.toLowerCase().includes(q)
      )
    }
    list.sort((a: Download, b: Download) => {
      switch (sort) {
        case 'newest':
          return b.completed_at?.localeCompare(a.completed_at ?? '') || b.added_at.localeCompare(a.added_at)
        case 'oldest':
          return a.completed_at?.localeCompare(b.completed_at ?? '') || a.added_at.localeCompare(b.added_at)
        case 'title':
          return (a.title || a.url).localeCompare(b.title || b.url)
        case 'size':
          return b.total_bytes - a.total_bytes
      }
    })
    return list
  }, [state.downloads, filter, query, sort])

  const pageCount = Math.max(1, Math.ceil(rows.length / pageSize))
  const current = Math.min(page, pageCount - 1)
  const visible = rows.slice(current * pageSize, (current + 1) * pageSize)
  const selectedRows = rows.filter((d) => selection.has(d.id))

  useEffect(() => {
    setSelection((previous) => {
      const next = new Set([...previous].filter((id) => state.downloads.has(id)))
      return next.size === previous.size ? previous : next
    })
  }, [state.downloads])

  // Shift-click range selection over the rows actually on screen. The anchor
  // is the last row clicked without shift and stays put across successive
  // shift-clicks; it is held by id because filtering, sorting and paging all
  // reshuffle the list between clicks.
  const anchorId = useRef<string | null>(null)

  const selectRow = (id: string, range: boolean) => {
    const span = range
      ? rangeBetween(visible.map((d) => d.id), anchorId.current, id)
      : null
    if (span) {
      // `value` is derived inside the updater, not from this render's closure,
      // so two clicks landing in one frame cannot both decide against the same
      // stale selection.
      setSelection((previous) => {
        const value = !previous.has(id)
        const next = new Set(previous)
        for (const rowID of span) {
          if (value) next.add(rowID)
          else next.delete(rowID)
        }
        return next
      })
      return
    }
    toggleSelection(id)
    anchorId.current = id
  }

  const toggleSelection = (id: string) => {
    setSelection((previous) => {
      const next = new Set(previous)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const reportResults = (
    results: { id: string; ok: boolean; error?: { message?: string }; note?: string }[]
  ) => {
    const failures = results.filter((result) => !result.ok)
    if (failures.length > 0) {
      pushToast(failures[0].error?.message ?? 'action failed')
      return
    }
    const note = results.find((r) => r.note)?.note
    if (note) pushToast(note)
  }

  const runBatch = async (action: 'retry' | 'remove', ids: string[]) => {
    if (!connected || ids.length === 0) return
    if (action === 'remove') {
      setConfirmRemove(ids)
      return
    }
    await sendBatch(action, ids)
  }

  const sendBatch = async (action: 'retry' | 'remove', ids: string[]) => {
    setBusyIds((previous) => new Set([...previous, ...ids]))
    try {
      const response = await api.batchAction(action, ids)
      reportResults(response.results)
      setSelection(new Set())
    } catch (error) {
      pushToast(error instanceof Error ? error.message : 'action failed')
    } finally {
      setBusyIds((previous) => {
        const next = new Set(previous)
        ids.forEach((id) => next.delete(id))
        return next
      })
    }
  }

  const act = async (id: string, action: 'retry' | 'remove') => {
    if (action === 'remove') {
      setConfirmRemove([id])
      return
    }
    setBusyIds((p) => new Set(p).add(id))
    try {
      const resp = await api.batchAction(action, [id])
      if (resp.results[0]?.ok === false) {
        pushToast(resp.results[0].error?.message ?? 'action failed')
      }
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'action failed')
    } finally {
      setBusyIds((p) => {
        const n = new Set(p)
        n.delete(id)
        return n
      })
    }
  }

  // Counts come from every tracked item, not the filtered page: a button that
  // says "Clear failed (12)" must mean the same thing whatever the view is
  // currently filtered or searched down to.
  const all = [...state.downloads.values()]
  const idsFor = (target: ClearTarget): string[] =>
    all
      .filter((d) =>
        target === 'history'
          ? d.state === 'completed' || d.state === 'failed' || d.state === 'deleted'
          : d.state === target
      )
      .map((d) => d.id)

  const clearCounts: Record<ClearTarget, number> = {
    completed: idsFor('completed').length,
    failed: idsFor('failed').length,
    deleted: idsFor('deleted').length,
    history: idsFor('history').length
  }

  const runClear = async (target: ClearTarget) => {
    setClearBusy(target)
    try {
      if (target === 'history') {
        // One server-side call rather than thousands of ids over the wire:
        // the clear endpoint's "finished" scope is exactly the three states
        // this page shows.
        const resp = await api.clearQueue('finished')
        pushToast(
          resp.removed > 0 ? `Cleared ${resp.removed} entries` : 'Nothing to clear',
          true
        )
      } else {
        const ids = idsFor(target)
        const results: { id: string; ok: boolean; error?: { message?: string } }[] = []
        let sendError: string | null = null
        for (let i = 0; i < ids.length; i += MAX_BATCH_IDS) {
          try {
            const resp = await api.batchAction('remove', ids.slice(i, i + MAX_BATCH_IDS))
            results.push(...resp.results)
          } catch (e) {
            sendError = e instanceof Error ? e.message : 'clear failed'
            break
          }
        }
        const verdict = summarizeBulkClear(ids.length, results, sendError)
        pushToast(verdict.message, verdict.ok)
      }
      setSelection(new Set())
      setPage(0)
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'clear failed')
    } finally {
      setClearBusy(null)
    }
  }

  if (!state.everConnected) {
    return (
      <div aria-hidden="true">
        <div class="skeleton" />
        <div class="skeleton" />
      </div>
    )
  }

  return (
    <div>
      <h1 class="page-title">Library</h1>
      <p class="page-sub">Completed and removed downloads</p>

      <div class="library-filters">
        <label for="lib-search" class="visually-hidden">Search</label>
        <input
          id="lib-search"
          class="input"
          placeholder="Search title or URL…"
          value={query}
          onInput={(e) => {
            setQuery((e.target as HTMLInputElement).value)
            setPage(0)
            setSelection(new Set())
          }}
        />
        <label for="lib-filter" class="visually-hidden">Filter</label>
        <select id="lib-filter" class="input input-auto" value={filter} onChange={(e) => { setFilter((e.target as HTMLSelectElement).value as LibFilter); setPage(0); setSelection(new Set()) }}>
          <option value="completed">Completed</option>
          <option value="failed">Failed</option>
          <option value="deleted">Files deleted</option>
          <option value="all">All history</option>
        </select>
        <label for="lib-sort" class="visually-hidden">Sort</label>
        <select id="lib-sort" class="input input-auto" value={sort} onChange={(e) => setSort((e.target as HTMLSelectElement).value as SortKey)}>
          <option value="newest">Newest first</option>
          <option value="oldest">Oldest first</option>
          <option value="title">Title A–Z</option>
          <option value="size">Largest first</option>
        </select>
      </div>

      <p class="hint hint-page">
        History from the running manager, not a separate archive — clearing
        finished items in the queue removes them here too.
      </p>

      <div class="queue-management" role="group" aria-labelledby="library-actions-title">
        <div class="queue-management-copy">
          <strong id="library-actions-title">Library actions</strong>
          <span>
            Clearing drops history entries only. Downloaded files are never
            deleted.
          </span>
        </div>
        <div class="global-actions library-actions" role="toolbar" aria-label="Library-wide actions">
          {(['completed', 'failed', 'deleted', 'history'] as ClearTarget[]).map((target) => (
            <button
              key={target}
              class="btn small"
              disabled={!connected || clearBusy !== null || clearCounts[target] === 0}
              onClick={() => setConfirmClear(target)}
              title={
                target === 'history'
                  ? 'Remove every completed, failed and files-deleted entry from the list'
                  : `Remove every ${CLEAR_LABELS[target]} entry from the list`
              }
            >
              {clearBusy === target
                ? 'Clearing…'
                : `Clear ${CLEAR_LABELS[target]}${clearCounts[target] > 0 ? ` (${clearCounts[target]})` : ''}`}
            </button>
          ))}
        </div>
      </div>

      {selection.size > 0 && (
        <div class="selection-bar" role="toolbar" aria-label="Library selection actions">
          <strong>{selection.size} selected</strong>
          <div class="spacer" />
          <button
            class="btn small"
            disabled={!selectedRows.some((d) => d.allowed_actions.includes('resume'))}
            onClick={() => void runBatch('retry', selectedRows.filter((d) => d.allowed_actions.includes('resume')).map((d) => d.id))}
          >
            Retry
          </button>
          <button class="btn small" onClick={() => void runBatch('remove', selectedRows.map((d) => d.id))}>
            Remove from history
          </button>
          <button class="btn small" onClick={() => setSelection(new Set())}>
            Deselect all
          </button>
        </div>
      )}

      {rows.length === 0 ? (
        <div class="empty-state">
          <p>Nothing here yet.</p>
          <p>Completed and failed downloads will appear in this library.</p>
        </div>
      ) : (
        <div class="queue-table table-spaced">
          <div class="row-main row-library queue-head" aria-hidden="true">
            <span />
            <span />
            <span>Name</span>
            <span>Status</span>
            <span>Size</span>
            <span>Completed</span>
            <span class="text-right">Actions</span>
          </div>
          <ul class="queue-list">
            {visible.map((d) => (
              <DownloadRow
                key={d.id}
                download={d}
                library
                selected={selection.has(d.id)}
                busy={busyIds.has(d.id) || !connected}
                onToggleSelect={(range) => selectRow(d.id, range)}
                onAction={(a) => void act(d.id, a as 'retry' | 'remove')}
                onShowDetails={() => setDetailsId(d.id)}
              />
            ))}
          </ul>
        </div>
      )}

      {pageCount > 1 && (
        <div class="pager">
          <button class="btn small" disabled={current === 0} onClick={() => setPage(current - 1)}>
            Previous
          </button>
          <span class="text-muted">
            Page {current + 1} / {pageCount} ({rows.length} items)
          </span>
          <button class="btn small" disabled={current >= pageCount - 1} onClick={() => setPage(current + 1)}>
            Next
          </button>
        </div>
      )}

      {confirmRemove && (
        <Dialog
          title={`Remove ${confirmRemove.length} item${confirmRemove.length > 1 ? 's' : ''} from history?`}
          onClose={() => setConfirmRemove(null)}
        >
          <p>
            This drops the entry from the manager list. Completed files stay on
            disk. Running partials would be removed.
          </p>
          <div class="dialog-actions">
            <button class="btn" onClick={() => setConfirmRemove(null)}>Cancel</button>
            <button
              class="btn danger"
              onClick={async () => {
                const ids = confirmRemove
                setConfirmRemove(null)
                try {
                  const response = await api.batchAction('remove', ids)
                  reportResults(response.results)
                  setSelection(new Set())
                } catch (e) {
                  pushToast(e instanceof Error ? e.message : 'remove failed')
                }
              }}
            >
              Remove from history
            </button>
          </div>
        </Dialog>
      )}

      {confirmClear && (
        <Dialog
          title={`Clear ${CLEAR_LABELS[confirmClear]}?`}
          onClose={() => clearBusy === null && setConfirmClear(null)}
        >
          <p>
            This removes {clearCounts[confirmClear]}{' '}
            {confirmClear === 'history'
              ? 'history entries'
              : `${CLEAR_LABELS[confirmClear]} entries`}{' '}
            from the manager's list. Downloaded files stay on disk — only the
            list entries go.
          </p>
          <div class="dialog-actions">
            <button
              class="btn"
              disabled={clearBusy !== null}
              onClick={() => setConfirmClear(null)}
            >
              Cancel
            </button>
            <button
              class="btn danger"
              disabled={clearBusy !== null}
              onClick={() => {
                const target = confirmClear
                setConfirmClear(null)
                void runClear(target)
              }}
            >
              Clear {CLEAR_LABELS[confirmClear]}
            </button>
          </div>
        </Dialog>
      )}

      {detailsId && state.downloads.get(detailsId) && (
        <DetailsDrawer
          download={state.downloads.get(detailsId)!}
          onClose={() => setDetailsId(null)}
        />
      )}
    </div>
  )
}
