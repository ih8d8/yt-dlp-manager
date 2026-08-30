import { useCallback, useEffect, useRef, useState } from 'preact/hooks'
import { api, setCsrfToken } from './api/client'
import { EventStream } from './api/events'
import type { Stats } from './api/types'
import { initialState, reduce } from './state/downloads'
import { LoginPage, SetupPasswordPage } from './pages/login'
import { QueuePage } from './pages/queue'
import { LibraryPage } from './pages/library'
import { SettingsPage } from './pages/settings'
import { ExplorerPage } from './pages/explorer'
import { Logo } from './components/logo'

export interface Toast {
  id: number
  message: string
  ok: boolean
}

type Route = 'queue' | 'library' | 'settings' | 'api'

function parseHash(): Route {
  const raw = location.hash.replace(/^#\/?/, '').split('/')[0]
  if (raw === 'library' || raw === 'settings' || raw === 'api') return raw
  return 'queue'
}

/**
 * App owns authentication state, the SSE stream lifecycle, and the shared
 * downloads store. All mutations reconcile exclusively through SSE events —
 * nothing is optimistically claimed.
 */

const THEME_ORDER = ['system', 'light', 'dark'] as const
const THEME_LABEL: Record<string, string> = {
  system: 'Match system',
  light: 'Light',
  dark: 'Dark'
}
const THEME_GLYPH: Record<string, string> = { system: '◐', light: '☀', dark: '☾' }

export function App() {
  const [authed, setAuthed] = useState<boolean | null>(null)
  const [setupRequired, setSetupRequired] = useState(false)
  const [setupTokenRequired, setSetupTokenRequired] = useState(false)
  const [state, setState] = useState(initialState)
  const [toasts, setToasts] = useState<Toast[]>([])
  const [route, setRoute] = useState(parseHash)
  const [theme, setTheme] = useState<'dark' | 'light' | 'system'>('dark')
  const [compact, setCompact] = useState(false)
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false)
  const toastSeq = useRef(0)
  const streamRef = useRef<EventStream | null>(null)

  const pushToast = useCallback((message: string, ok = false) => {
    const id = ++toastSeq.current
    setToasts((ts) => [...ts.slice(-4), { id, message, ok }])
    window.setTimeout(() => {
      setToasts((ts) => ts.filter((t) => t.id !== id))
    }, 5000)
  }, [])

  useEffect(() => {
    const onHash = () => setRoute(parseHash())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
  }, [theme])

  useEffect(() => {
    document.documentElement.setAttribute('data-compact', compact ? 'true' : 'false')
  }, [compact])

  // Boot: determine session; wire SSE once authenticated.
  useEffect(() => {
    let alive = true
    api
      .session()
      .then((s) => {
        if (!alive) return
        if (s.csrf_token) setCsrfToken(s.csrf_token)
        setAuthed(s.authenticated)
        setSetupRequired(Boolean(s.setup_required))
        setSetupTokenRequired(Boolean(s.setup_token_required))
      })
      .catch(() => alive && setAuthed(false))
    return () => {
      alive = false
    }
  }, [])

  // Appearance is loaded whenever the session becomes usable, not just on the
  // first page load. Fetching it only during boot meant that signing in left
  // the UI on the built-in default until the next refresh — which made a saved
  // light theme look like it had not been applied at all.
  //
  // Settings is an authenticated endpoint, so this deliberately waits for a
  // real session: requesting it behind the login screen only produces a 401
  // and a noisy console entry on every signed-out visit.
  useEffect(() => {
    if (!authed || setupRequired) return
    let alive = true
    api
      .settings()
      .then((v) => {
        if (!alive) return
        const t = v.ui.theme as typeof theme
        if (t === 'dark' || t === 'light' || t === 'system') setTheme(t)
        setCompact(Boolean(v.ui.compact))
      })
      .catch(() => undefined)
    return () => {
      alive = false
    }
  }, [authed, setupRequired])

  // "system" has no fixed value: it tracks the OS. Re-render on that change so
  // the choice keeps meaning something after the page has loaded.
  useEffect(() => {
    if (theme !== 'system' || !window.matchMedia) return
    const mq = window.matchMedia('(prefers-color-scheme: light)')
    const sync = () => document.documentElement.setAttribute('data-theme', 'system')
    mq.addEventListener('change', sync)
    return () => mq.removeEventListener('change', sync)
  }, [theme])

  useEffect(() => {
    if (!authed || setupRequired) return

    const stream = new EventStream({
      onSnapshot: (payload) =>
        setState((s) => reduce(s, { type: 'snapshot', downloads: payload.downloads })),
      onDownload: (payload) =>
        setState((s) => reduce(s, { type: 'upsert', download: payload.download })),
      onRemoved: (payload) => setState((s) => reduce(s, { type: 'remove', id: payload.id })),
      onStats: (stats: Stats) => setState((s) => reduce(s, { type: 'stats', stats })),
      // Transport-open alone must NOT re-enable mutations: only the fresh
      // snapshot that follows reconnection does that (reducer sets it).
      onConnected: () => undefined,
      onDisconnected: () => setState((s) => reduce(s, { type: "connected", connected: false })),
      onUnauthenticated: () => {
        setCsrfToken(null)
        setState(initialState())
        setAuthed(false)
      }
    })
    streamRef.current = stream
    stream.connect()
    return () => {
      stream.close()
      streamRef.current = null
    }
  }, [authed, setupRequired])

  const logout = useCallback(async () => {
    try {
      await api.logout()
    } finally {
      setCsrfToken(null)
      streamRef.current?.close()
      setState(initialState())
      setAuthed(false)
      setSetupRequired(false)
    }
  }, [])

  if (authed === null) {
    return <div class="empty-state">Loading…</div>
  }
  if (setupRequired) {
    return (
      <SetupPasswordPage
        tokenRequired={setupTokenRequired}
        onSuccess={(session) => {
          if (session.csrf_token) setCsrfToken(session.csrf_token)
          setSetupRequired(false)
          setAuthed(true)
        }}
      />
    )
  }
  if (!authed) {
    return (
      <LoginPage
        onSuccess={(session) => {
          if (session.csrf_token) setCsrfToken(session.csrf_token)
          setAuthed(true)
          setSetupRequired(Boolean(session.setup_required))
        }}
      />
    )
  }

  const connected = state.connected && state.everConnected

  return (
    <div class="shell">
      <header class="topbar">
        <div class="brand">
          <Logo />
          yt-dlp-manager
        </div>
        <div class="spacer" />
        <button
          class="btn small icon-btn theme-toggle"
          title={`Theme: ${THEME_LABEL[theme]} — click to change`}
          aria-label={`Theme: ${THEME_LABEL[theme]}. Click to change.`}
          onClick={() => {
            const next = THEME_ORDER[(THEME_ORDER.indexOf(theme) + 1) % THEME_ORDER.length]
            setTheme(next)
            // Persist through the same endpoint the Settings page uses so the
            // two controls can never disagree.
            void api.saveSettings({ ui: { theme: next } }).catch(() => undefined)
          }}
        >
          <span aria-hidden="true">{THEME_GLYPH[theme]}</span>
        </button>
        <span class="health" role="status">
          <span
            class={connected ? 'health-dot is-up' : 'health-dot is-down'}
            aria-hidden="true"
          />
          <span class="health-text">{connected ? 'Healthy' : 'Reconnecting'}</span>
        </span>
        <div
          class="topbar-menu"
          onKeyDown={(event) => {
            if (event.key === 'Escape') setMobileMenuOpen(false)
          }}
        >
          <button
            class="btn icon-btn"
            aria-label="Open account menu"
            aria-haspopup="menu"
            aria-expanded={mobileMenuOpen}
            onClick={() => setMobileMenuOpen((open) => !open)}
          >
            ⋮
          </button>
          {mobileMenuOpen && (
            <>
              <div class="menu-overlay" aria-hidden="true" onClick={() => setMobileMenuOpen(false)} />
              <div class="topbar-menu-popover" role="menu">
                <button class="btn" role="menuitem" onClick={() => void logout()}>
                  Log out
                </button>
              </div>
            </>
          )}
        </div>
      </header>

      {!connected && state.everConnected && (
        <div class="conn-banner" role="alert">
          Connection lost — reconnecting…
        </div>
      )}

      <div class="body-row">
        <nav class="sidebar" aria-label="Primary">
          <div class="sidebar-nav">
            <NavButton route="queue" current={route} icon="☰" label="Queue" />
            <NavButton route="library" current={route} icon="▤" label="Library" />
            <NavButton route="settings" current={route} icon="⚙" label="Settings" />
            <NavButton route="api" current={route} icon="{ }" label="API" />
          </div>
          <div class="sidebar-footer">
            <button class="btn small sidebar-logout" onClick={() => void logout()}>
              Log out
            </button>
          </div>
        </nav>

        <main class="main">
          {route === 'queue' && (
            <QueuePage
              state={state}
              setState={setState}
              pushToast={pushToast}
              onTheme={setTheme}
            />
          )}
          {route === 'library' && <LibraryPage state={state} pushToast={pushToast} />}
          {route === 'api' && <ExplorerPage />}
          {route === 'settings' && (
            <SettingsPage pushToast={pushToast} onTheme={setTheme} onCompact={setCompact} />
          )}
        </main>
      </div>

      <nav class="bottomnav" aria-label="Primary mobile">
        <NavButton route="queue" current={route} icon="☰" label="Queue" />
        <NavButton route="library" current={route} icon="▤" label="Library" />
        <NavButton route="settings" current={route} icon="⚙" label="Settings" />
        <NavButton route="api" current={route} icon="{ }" label="API" />
      </nav>

      <div class="toasts">
        <div aria-live="polite" class="visually-hidden" id="live-region" />
        {toasts.map((t) => (
          <div key={t.id} class={`toast${t.ok ? ' ok' : ''}`}>
            {t.message}
          </div>
        ))}
      </div>
    </div>
  )
}

function NavButton({
  route,
  current,
  icon,
  label
}: {
  route: Route
  current: Route
  icon: string
  label: string
}) {
  return (
    <button
      class="nav-item"
      aria-current={current === route ? 'page' : undefined}
      onClick={() => {
        location.hash = `#/${route}`
      }}
    >
      <span aria-hidden="true">{icon}</span>
      {label}
    </button>
  )
}

