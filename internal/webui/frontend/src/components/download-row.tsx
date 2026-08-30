import { h } from 'preact'
import { useState } from 'preact/hooks'
import type { Download } from '../api/types'
import { formatBytes, formatEta, formatSpeed, formatDateCompact, displayTitle, progressLabelText, progressTrackMode } from '../format'
import { Thumb } from './thumb'

const stateLabel: Record<string, string> = {
  queued: 'Queued',
  downloading: 'Downloading',
  paused: 'Paused',
  completed: 'Completed',
  failed: 'Failed',
  deleted: 'Files deleted'
}

export function StatePill({ state }: { state: string }) {
  return (
    <span class={`pill st-${state}`}>
      <span aria-hidden="true">{stateLabel[state] ?? state}</span>
      <span class="visually-hidden">state: {stateLabel[state] ?? state}</span>
    </span>
  )
}

const ACTION_LABELS: Record<string, string> = {
  pause: 'Pause',
  resume: 'Resume',
  start_now: 'Force start/resume',
  remove: 'Remove from list'
}

export interface RowProps {
  download: Download
  selected: boolean
  busy: boolean
  library?: boolean
  /**
   * `range` is true when the user held Shift, asking for every row between the
   * last plainly-clicked one and this one. The list owns the row order, so it
   * owns what "between" means; the row only reports the modifier.
   */
  onToggleSelect: (range: boolean) => void
  onAction: (action: string) => void
  onShowDetails: () => void
}

export function DownloadRow(props: RowProps): h.JSX.Element {
  const { download: d, selected, busy, library } = props
  const mode = progressTrackMode(d.state, d.total_bytes, d.progress)
  const [menuOpen, setMenuOpen] = useState(false)
  const title = displayTitle(d.title, d.url)
  // Explicit map with no fallback: an unknown action used to inherit the
  // "Delete files" label, which is the worst possible default for a value we
  // do not recognise. Unrecognised actions are simply not offered.
  const menuActions = d.allowed_actions.flatMap((a) => {
    const label = ACTION_LABELS[a]
    return label ? [{ a, label }] : []
  })
  return (
    <li>
      <div class={`row-main${library ? " row-library" : ""}`}>
        {/* Selection is driven from `click`, not `change`: only a MouseEvent
            carries shiftKey, and a checkbox's change event does not. Keyboard
            activation (Space) dispatches click too, so this stays accessible.

            Do NOT preventDefault here. The browser flips checkedness before
            dispatching click and undoes it afterwards when the event is
            canceled — and that undo lands AFTER the microtask in which Preact
            re-renders, so the tick would silently revert and only reappear on
            some later render (Preact re-syncs `checked` against the live DOM,
            which on an idle queue means the next 15s stats event). Letting the
            native toggle stand is also always correct: the clicked row's new
            value is `!selection.has(id)` in both the plain and the range case,
            which is exactly what the browser just did. */}
        <input
          type="checkbox"
          class="row-check"
          checked={selected}
          onClick={(e) => props.onToggleSelect(e.shiftKey)}
          disabled={busy}
          aria-label={`Select ${displayTitle(d.title, d.url)}`}
        />
        <Thumb download={d} />
        <div class="row-meta">
          <div class="row-title">
            <button
              class="row-title-btn"
              onClick={props.onShowDetails}
            >
              {displayTitle(d.title, d.url)}
            </button>
          </div>
          <div class="row-url">{d.url}</div>
          {library && d.state === 'failed' && d.error && (
            <div class="row-err">{firstLine(d.error)}</div>
          )}
        </div>
        <div class="cell-status">
          <StatePill state={d.state} />
        </div>
        {library ? (
          <>
            <div class="cell-num cell-size">{d.total_bytes > 0 ? formatBytes(d.total_bytes) : '—'}</div>
            <div class="cell-num cell-date">{formatDateCompact(d.completed_at ?? d.added_at)}</div>
          </>
        ) : (
          <>
            <div class="cell-progress">
              <span class="cell-progress-pct">
                {progressLabelText(d.progress, d.downloaded_bytes, d.total_bytes, d.state)}
              </span>
              {mode === 'determinate' ? (
                <progress
                  class={`progress${d.state === 'paused' ? ' progress-paused' : ''}`}
                  max={100}
                  value={d.progress}
                  aria-label={`${displayTitle(d.title, d.url)} progress`}
                />
              ) : mode === 'indeterminate' ? (
                <div
                  class="progress progress-indeterminate"
                  role="progressbar"
                  aria-label={`${displayTitle(d.title, d.url)}: starting`}
                />
              ) : (
                <div
                  class="progress progress-unknown"
                  role="img"
                  aria-label={`${displayTitle(d.title, d.url)}: ${formatBytes(
                    d.downloaded_bytes
                  )} downloaded, total size unknown`}
                >
                </div>
              )}
            </div>
            <div class="cell-num cell-speed" title="Speed">
              {d.state === 'downloading' ? formatSpeed(d.speed_bytes_per_second) : '—'}
            </div>
            <div class="cell-num cell-eta" title="ETA">
              {d.state === 'downloading' ? formatEta(d.eta_seconds) : '—'}
            </div>
          </>
        )}
        <div class="row-actions">
          <RowActions download={d} busy={busy} onAction={props.onAction} />
        </div>
        {/* Mobile kebab menu (queue-mobile mockup): one entry point for the
            row's allowed actions, keyboard and screen-reader accessible. */}
        <div
          class="row-kebab"
          onKeyDown={(e) => {
            if (e.key === 'Escape') setMenuOpen(false)
          }}
        >
          <button
            class="btn small icon-btn"
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            aria-label={`More actions for ${title}`}
            onClick={() => setMenuOpen((v) => !v)}
          >
            ⋮
          </button>
          {menuOpen && (
            <>
              <div class="menu-overlay" aria-hidden="true" onClick={() => setMenuOpen(false)} />
              <div class="menu" role="menu" aria-label={`Actions for ${title}`}>
                <button
                  role="menuitem"
                  autoFocus
                  onClick={() => {
                    setMenuOpen(false)
                    props.onShowDetails()
                  }}
                >
                  Details
                </button>
                {menuActions.map(({ a, label }) => (
                  <button
                    key={a}
                    role="menuitem"
                    class={a === 'remove' ? 'danger' : undefined}
                    disabled={busy}
                    onClick={() => {
                      setMenuOpen(false)
                      props.onAction(a)
                    }}
                  >
                    {label}
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
      </div>
    </li>
  )
}

export function RowActions({
  download: d,
  busy,
  onAction
}: {
  download: Download
  busy: boolean
  onAction: (action: string) => void
}): h.JSX.Element {
  const has = (a: string) => d.allowed_actions.includes(a as never)
  return (
    <>
      {has('pause') && (
        <button class="btn small icon-btn" disabled={busy} onClick={() => onAction('pause')} title="Pause" aria-label={`Pause ${displayTitle(d.title, d.url)}`}>
          ⏸
        </button>
      )}
      {has('resume') && (
        <button class="btn small icon-btn" disabled={busy} onClick={() => onAction('resume')} title="Resume" aria-label={`Resume ${displayTitle(d.title, d.url)}`}>
          ▶
        </button>
      )}
      {has('start_now') && (
        <button class="btn small icon-btn" disabled={busy} onClick={() => onAction('start_now')} title="Force start/resume" aria-label={`Force start or resume ${displayTitle(d.title, d.url)}`}>
          ⚡
        </button>
      )}
      {has('remove') && (
        <button class="btn small icon-btn" disabled={busy} onClick={() => onAction('remove')} title="Remove from list" aria-label={`Remove ${displayTitle(d.title, d.url)} from list`}>
          ✕
        </button>
      )}
    </>
  )
}

function firstLine(s: string): string {
  const i = s.indexOf('\n')
  return i >= 0 ? s.slice(0, i) : s
}
