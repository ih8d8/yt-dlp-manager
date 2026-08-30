import type { Download, Stats } from '../api/types'

export interface AppState {
  downloads: Map<string, Download>
  selection: Set<string>
  connected: boolean
  everConnected: boolean
  stats: Stats | null
}

export type Action =
  | { type: 'snapshot'; downloads: Download[] }
  | { type: 'upsert'; download: Download }
  | { type: 'remove'; id: string }
  | { type: 'stats'; stats: Stats }
  | { type: 'connected'; connected?: boolean }
  | { type: 'toggleSelect'; id: string }
  | { type: 'selectMany'; ids: string[]; value?: boolean }
  | { type: 'clearSelection' }

export function initialState(): AppState {
  return {
    downloads: new Map(),
    selection: new Set(),
    connected: false,
    everConnected: false,
    stats: null
  }
}

/**
 * Pure reducer owning snapshot/upsert/remove plus selection.
 *
 * Removals are final, but that is enforced on the SERVER, not here: the
 * manager stamps every mutation with a sequence number and suppresses any
 * update whose stamp predates the item's removal, so a stale upsert never
 * reaches this reducer. There is deliberately no client-side tombstone —
 * don't read `upsert` as if one existed.
 *
 * Selection entries for removed rows are dropped so a stale selection cannot
 * act on ghosts.
 */
export function reduce(state: AppState, action: Action): AppState {
  switch (action.type) {
    case 'snapshot': {
      const downloads = new Map<string, Download>()
      const alive = new Set<string>()
      for (const d of action.downloads) {
        downloads.set(d.id, d)
        alive.add(d.id)
      }
      const selection = new Set([...state.selection].filter((id) => alive.has(id)))
      return { ...state, downloads, selection, connected: true, everConnected: true }
    }
    case 'upsert': {
      const downloads = new Map(state.downloads)
      downloads.set(action.download.id, action.download)
      return { ...state, downloads }
    }
    case 'remove': {
      if (!state.downloads.has(action.id)) return state
      const downloads = new Map(state.downloads)
      downloads.delete(action.id)
      const selection = new Set(state.selection)
      selection.delete(action.id)
      return { ...state, downloads, selection }
    }
    case 'stats':
      return { ...state, stats: action.stats }
    case 'connected':
      // Transport status only. Re-enabling mutations happens exclusively via
      // the fresh snapshot action, which sets connected/everConnected.
      return { ...state, connected: action.connected ?? state.connected }
    case 'toggleSelect': {
      const selection = new Set(state.selection)
      if (selection.has(action.id)) selection.delete(action.id)
      else selection.add(action.id)
      return { ...state, selection }
    }
    case 'selectMany': {
      const selection = new Set(state.selection)
      for (const id of action.ids) {
        if (action.value === undefined) {
          if (selection.has(id)) selection.delete(id)
          else selection.add(id)
        } else if (action.value) selection.add(id)
        else selection.delete(id)
      }
      return { ...state, selection }
    }
    case 'clearSelection':
      return { ...state, selection: new Set() }
    default:
      return state
  }
}

/**
 * The ids between `anchor` and `id` inclusive, in list order — the span a
 * shift-click asks for.
 *
 * Returns null when the range is not well defined: no anchor yet, the anchor
 * is the clicked row, or either end has left the list (rows come and go under
 * live updates, filtering and paging between two clicks). Callers fall back to
 * a plain toggle then, rather than selecting a span the user never pointed at.
 */
export function rangeBetween(
  ids: string[],
  anchor: string | null,
  id: string
): string[] | null {
  if (anchor === null || anchor === id) return null
  const from = ids.indexOf(anchor)
  const to = ids.indexOf(id)
  if (from < 0 || to < 0) return null
  return from < to ? ids.slice(from, to + 1) : ids.slice(to, from + 1)
}

/** Queue ordering mirrors the TUI: active, queued, paused, then the rest. */
const rank: Record<string, number> = {
  downloading: 0,
  queued: 1,
  paused: 2,
  failed: 3,
  completed: 4,
  deleted: 4
}

export function queueOrder(a: Download, b: Download): number {
  const ra = rank[a.state] ?? 9
  const rb = rank[b.state] ?? 9
  if (ra !== rb) return ra - rb
  return a.added_at.localeCompare(b.added_at)
}
