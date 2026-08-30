import { useState } from 'preact/hooks'
import { api, setCsrfToken } from '../api/client'
import { IconDownload } from '../components/icons'
import type { SessionInfo } from '../api/types'

// Mirrors minPasswordRunes in internal/httpapi/auth.go. The server is still the
// authority — this only lets the form say what the rule is before you hit it.
const MIN_PASSWORD = 8

/** Code points, matching the server's utf8.RuneCountInString rather than JS's
 *  UTF-16 length, so an emoji counts as one character in both places. */
const runeLength = (s: string): number => [...s].length

export function LoginPage({ onSuccess }: { onSuccess: (session: SessionInfo) => void }) {
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  // Preserve the intended route for after login — but never a cross-origin
  // or malformed target.
  const returnTo = /^#\/[a-z]*$/.test(location.hash) ? location.hash : '#/queue'

  const submit = async (e: Event) => {
    e.preventDefault()
    if (!password || busy) return
    setBusy(true)
    setError(null)
    try {
      const s = await api.login(password)
      setCsrfToken(s.csrf_token ?? null)
      onSuccess(s)
      if (location.hash !== returnTo) location.hash = returnTo
    } catch (err) {
      setError(err instanceof Error ? err.message : 'login failed')
    } finally {
      setPassword('')
      setBusy(false)
    }
  }

  return (
    <div class="login-wrap">
      <form class="login-card" onSubmit={submit} aria-labelledby="login-title">
        <h1 id="login-title">yt-dlp-manager</h1>
        <p class="hint">Sign in with the administrator password.</p>
        <input
          type="text"
          name="username"
          autocomplete="username"
          value="admin"
          hidden
        />
        <label for="password" class="auth-label">
          Password
        </label>
        <input
          id="password"
          class="input"
          type="password"
          autocomplete="current-password"
          value={password}
          onInput={(e) => setPassword((e.target as HTMLInputElement).value)}
          required
          autoFocus
        />
        {error && (
          <p class="field-error" role="alert">
            {error}
          </p>
        )}
        <button class="btn primary btn-block" type="submit" disabled={busy || !password}>
          <IconDownload size={16} /> Log in
        </button>
      </form>
    </div>
  )
}

export function SetupPasswordPage({
  onSuccess,
  tokenRequired = false
}: {
  onSuccess: (session: SessionInfo) => void
  /** The server could not tell that this browser is on the machine running it
   *  — a container's port proxy or another host — so claiming the account
   *  needs the setup token it printed to its log. */
  tokenRequired?: boolean
}) {
  const [password, setPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [setupToken, setSetupToken] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  // The submit button stays disabled until the rules are met, which means the
  // browser's own minlength bubble never fires — without these the form is a
  // dead button with nothing explaining why. Both only speak once the user has
  // typed, so an untouched form is not scolded on arrival.
  const length = runeLength(password)
  const tooShort = length > 0 && length < MIN_PASSWORD
  const mismatch = confirmation.length > 0 && confirmation !== password

  const submit = async (e: Event) => {
    e.preventDefault()
    if (busy) return
    if (password !== confirmation) {
      setError('Passwords do not match')
      return
    }
    setBusy(true)
    setError(null)
    const token = setupToken.trim() || undefined
    try {
      // An origin the server cannot recognise as local is only handed a setup
      // CSRF token once it presents the setup token, so exchange it first.
      if (token) {
        const bootstrap = await api.session(token)
        setCsrfToken(bootstrap.csrf_token ?? null)
      }
      const session = await api.changePassword(password, confirmation, token)
      setCsrfToken(session.csrf_token ?? null)
      onSuccess(session)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'could not set password')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div class="login-wrap">
      <form class="login-card" onSubmit={submit} aria-labelledby="setup-password-title">
        <h1 id="setup-password-title">Set administrator password</h1>
        <p class="hint">Create a password before using yt-dlp-manager.</p>
        {tokenRequired && (
          <p class="hint">
            This browser is not on the machine running the server, so setup also needs the
            one-time token from the server log (<code>docker compose logs yt-dlp-manager</code>,
            or the terminal it was started in). It changes on every restart.
          </p>
        )}
        <input
          type="text"
          name="username"
          autocomplete="username"
          value="admin"
          hidden
        />
        {tokenRequired && (
          <>
            <label for="setup-token" class="auth-label">Setup token</label>
            <input
              id="setup-token"
              class="input"
              type="text"
              autocomplete="off"
              spellcheck={false}
              value={setupToken}
              onInput={(e) => setSetupToken((e.target as HTMLInputElement).value)}
              required
            />
          </>
        )}
        <label for="new-password" class="auth-label">New password</label>
        <input
          id="new-password"
          class="input"
          type="password"
          autocomplete="new-password"
          minlength={MIN_PASSWORD}
          maxlength={1024}
          value={password}
          onInput={(e) => setPassword((e.target as HTMLInputElement).value)}
          aria-describedby="new-password-hint"
          aria-invalid={tooShort}
          required
          autoFocus
        />
        <p
          id="new-password-hint"
          class={tooShort ? 'field-error' : 'hint'}
          role={tooShort ? 'alert' : undefined}
        >
          {tooShort
            ? `Too short — ${MIN_PASSWORD - length} more character${MIN_PASSWORD - length === 1 ? '' : 's'} needed.`
            : `At least ${MIN_PASSWORD} characters.`}
        </p>
        <label for="confirm-password" class="auth-label">Confirm new password</label>
        <input
          id="confirm-password"
          class="input"
          type="password"
          autocomplete="new-password"
          minlength={MIN_PASSWORD}
          maxlength={1024}
          value={confirmation}
          onInput={(e) => setConfirmation((e.target as HTMLInputElement).value)}
          aria-describedby={mismatch ? 'confirm-password-error' : undefined}
          aria-invalid={mismatch}
          required
        />
        {mismatch && (
          <p id="confirm-password-error" class="field-error" role="alert">
            Passwords do not match.
          </p>
        )}
        {error && <p class="field-error" role="alert">{error}</p>}
        <button
          class="btn primary btn-block"
          type="submit"
          disabled={
            busy ||
            length < MIN_PASSWORD ||
            confirmation !== password ||
            (tokenRequired && setupToken.trim() === '')
          }
        >
          {busy ? 'Saving…' : 'Set password'}
        </button>
      </form>
    </div>
  )
}
