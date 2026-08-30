import type { Download, Stats } from './types'

export interface EventsHandlers {
  onSnapshot: (downloads: { downloads: Download[] }) => void
  onDownload: (payload: { download: Download }) => void
  onRemoved: (payload: { id: string }) => void
  onStats: (stats: Stats) => void
  onConnected: () => void
  onDisconnected: () => void
  /** The stream was rejected because the session is gone (expired, logged out
   *  elsewhere, or password changed). The app should return to the login screen
   *  rather than retry forever. */
  onUnauthenticated?: () => void
}

/**
 * EventStream wraps EventSource with handler wiring. Reconnection is left to
 * the browser: correctness relies on the fresh snapshot sent after every
 * reconnect, never on replayed events.
 */
export class EventStream {
  private es: EventSource | null = null
  private handlers: EventsHandlers
  private closed = false
  private checking = false

  constructor(handlers: EventsHandlers) {
    this.handlers = handlers
  }

  connect(): void {
    if (this.es || this.closed) return
    const es = new EventSource('/api/v1/events')
    this.es = es

    es.addEventListener('open', () => this.handlers.onConnected())
    es.onerror = () => {
      // EventSource keeps retrying and cannot report the status of a failed
      // handshake, so a 401 looked exactly like a network blip: the tab showed
      // "Reconnecting…" forever and hammered the server every few seconds.
      // Ask the session endpoint (deliberately anonymous-friendly, so this
      // costs nothing and logs no error) which of the two it is.
      this.handlers.onDisconnected()
      void this.checkStillAuthenticated()
    }
    es.addEventListener('snapshot', (e) => {
      try {
        this.handlers.onSnapshot(JSON.parse((e as MessageEvent).data))
      } catch {
        /* ignore malformed frame; next snapshot heals state */
      }
    })
    es.addEventListener('download', (e) => {
      try {
        this.handlers.onDownload(JSON.parse((e as MessageEvent).data))
      } catch {
        /* ignore */
      }
    })
    es.addEventListener('removed', (e) => {
      try {
        this.handlers.onRemoved(JSON.parse((e as MessageEvent).data))
      } catch {
        /* ignore */
      }
    })
    es.addEventListener('stats', (e) => {
      try {
        this.handlers.onStats(JSON.parse((e as MessageEvent).data))
      } catch {
        /* ignore */
      }
    })
  }

  private async checkStillAuthenticated(): Promise<void> {
    if (this.checking || this.closed) return
    this.checking = true
    try {
      const resp = await fetch('/api/v1/session', { credentials: 'same-origin' })
      if (!resp.ok) return
      const session = await resp.json()
      if (!session?.authenticated && !this.closed) {
        this.close()
        this.handlers.onUnauthenticated?.()
      }
    } catch {
      // Genuinely offline: leave EventSource to keep retrying.
    } finally {
      this.checking = false
    }
  }

  close(): void {
    this.closed = true
    this.es?.close()
    this.es = null
  }
}
