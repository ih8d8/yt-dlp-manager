import { h } from 'preact'
import { useEffect, useState } from 'preact/hooks'
import { api, setCsrfToken } from '../api/client'
import type {
  SettingsView,
  SystemInfo,
  YtDlpSettings,
  YtDlpSettingsResponse
} from '../api/types'

interface PageProps {
  pushToast: (message: string, ok?: boolean) => void
  onTheme: (t: 'dark' | 'light' | 'system') => void
  onCompact: (compact: boolean) => void
}

type Tab = 'general' | 'downloads' | 'security' | 'advanced' | 'about'

const SETTINGS_SECTIONS: [Tab, string, string][] = [
  ['general', '⚙', 'General'],
  ['downloads', '⭳', 'Downloads'],
  ['security', '🛡', 'Security'],
  ['advanced', '⚖', 'Advanced'],
  ['about', 'ℹ', 'About']
]

export function SettingsPage({ pushToast, onTheme, onCompact }: PageProps): h.JSX.Element {
  const [tab, setTab] = useState<Tab>('general')
  const [view, setView] = useState<SettingsView | null>(null)
  const [system, setSystem] = useState<SystemInfo | null>(null)

  useEffect(() => {
    api
      .settings()
      .then((v) => {
        setView(v)
        const t = v.ui.theme as 'dark' | 'light' | 'system'
        onTheme(t)
        onCompact(v.ui.compact)
      })
      .catch((e) => pushToast(e instanceof Error ? e.message : 'could not load settings'))
    api
      .system()
      .then(setSystem)
      .catch(() => undefined)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (!view) return <div class="empty-state">Loading settings…</div>

  return (
    <div>
      <h1 class="page-title">Settings</h1>
      <p class="page-sub">Configure the manager and yt-dlp defaults</p>

      <div class="settings-shell">
        <nav class="settings-nav" aria-label="Settings sections">
          {SETTINGS_SECTIONS.map(([id, icon, label]) => (
            <button
              key={id}
              id={`settings-tab-${id}`}
              aria-current={tab === id ? 'page' : undefined}
              class={`settings-nav-item${tab === id ? ' active' : ''}`}
              onClick={() => setTab(id)}
            >
              <span aria-hidden="true">{icon}</span>
              {label}
            </button>
          ))}
        </nav>

        <section
          class="settings-content"
          aria-labelledby={`settings-tab-${tab}`}
        >
          {tab === 'general' && <GeneralTab view={view} onSaved={(v) => { setView(v); const t = v.ui.theme as 'dark'|'light'|'system'; onTheme(t); onCompact(v.ui.compact) }} pushToast={pushToast} />}
          {tab === 'downloads' && <DownloadsTab view={view} onSaved={setView} pushToast={pushToast} />}
          {tab === 'security' && <SecurityTab view={view} pushToast={pushToast} />}
          {tab === 'advanced' && <AdvancedTab view={view} system={system} />}
          {tab === 'about' && <AboutTab system={system} />}
        </section>
      </div>
    </div>
  )
}

function GeneralTab({
  view,
  onSaved,
  pushToast
}: {
  view: SettingsView
  onSaved: (v: SettingsView) => void
  pushToast: (m: string, ok?: boolean) => void
}) {
  const [theme, setTheme] = useState(view.ui.theme)
  const [compact, setCompact] = useState(view.ui.compact)
  const dirty = theme !== view.ui.theme || compact !== view.ui.compact

  const save = async () => {
    try {
      const v = await api.saveSettings({
        ui: { theme, compact }
      })
      onSaved(v)
      pushToast('settings saved', true)
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'save failed')
    }
  }

  return (
    <section class="card">
      <h3>Appearance</h3>
      <div class="field-row">
        <span id="lbl-theme">Theme</span>
        <select aria-labelledby="lbl-theme" class="input input-auto" value={theme} onChange={(e) => setTheme((e.target as HTMLSelectElement).value)}>
          <option value="dark">Dark</option>
          <option value="light">Light</option>
          <option value="system">System</option>
        </select>
      </div>
      <div class="field-row">
        <span id="lbl-compact">Compact density</span>
        <input aria-labelledby="lbl-compact" type="checkbox" checked={compact} onChange={(e) => setCompact((e.target as HTMLInputElement).checked)} />
      </div>
      <div class="dialog-actions">
        <button class="btn" disabled={!dirty} onClick={() => { setTheme(view.ui.theme); setCompact(view.ui.compact) }}>
          Reset
        </button>
        <button class="btn primary" disabled={!dirty} onClick={() => void save()}>
          Save
        </button>
      </div>
    </section>
  )
}

function DownloadsTab({
  view,
  onSaved,
  pushToast
}: {
  view: SettingsView
  onSaved: (v: SettingsView) => void
  pushToast: (m: string, ok?: boolean) => void
}) {
  const [maxC, setMaxC] = useState(String(view.downloads.max_concurrent))
  const [extraArgs, setExtraArgs] = useState(view.downloads.extra_args ?? '')
  const [extraArgsError, setExtraArgsError] = useState<string | null>(null)
  const [savingArgs, setSavingArgs] = useState(false)
  const dirtyArgs = extraArgs !== (view.downloads.extra_args ?? '')
  const [ytdlp, setYtdlp] = useState<YtDlpSettingsResponse | null>(null)
  const [form, setForm] = useState<YtDlpSettings>({})
  const dirtyMax = Number(maxC) !== view.downloads.max_concurrent

  useEffect(() => {
    api
      .ytDlpSettings()
      .then((r) => {
        setYtdlp(r)
        setForm(r.settings ?? {})
      })
      .catch((e) => pushToast(e instanceof Error ? e.message : 'could not load yt-dlp settings'))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const saveConcurrency = async () => {
    try {
      const v = await api.saveSettings({ downloads: { max_concurrent: Number(maxC) } })
      onSaved(v)
      pushToast('concurrency updated; active downloads are unaffected', true)
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'save failed')
    }
  }

  const saveExtraArgs = async () => {
    setSavingArgs(true)
    setExtraArgsError(null)
    try {
      const v = await api.saveSettings({ downloads: { extra_args: extraArgs.trim() } })
      onSaved(v)
      setExtraArgs(v.downloads.extra_args ?? '')
      pushToast('extra arguments saved; new downloads use them', true)
    } catch (e) {
      // The server names the option it refused, so it is shown at the field
      // rather than as a toast that disappears before it can be acted on.
      setExtraArgsError(e instanceof Error ? e.message : 'save failed')
    } finally {
      setSavingArgs(false)
    }
  }

  const setF = (patch: Partial<YtDlpSettings>) => setForm((f) => ({ ...f, ...patch }))

  const saveYtDlp = async () => {
    try {
      const r = await api.saveYtDlpManaged(form)
      setYtdlp(r)
      setForm(r.settings ?? {})
      pushToast('managed yt-dlp settings saved; new downloads pick them up', true)
    } catch (e) {
      pushToast(e instanceof Error ? e.message : 'save failed')
    }
  }

  return (
    <>
      <section class="card">
        <h3>Runtime concurrency</h3>
        <div class="field-row">
          <label for="max-conc">Maximum concurrent downloads</label>
          <input
            id="max-conc"
            class="input input-num"
            type="number"
            min={1}
            max={100}
            value={maxC}
            onInput={(e) => setMaxC((e.target as HTMLInputElement).value)}
          />
        </div>
        <p class="hint">
          Lowering the cap never stops active downloads — it only delays new
          starts. Source: {view.downloads.source}.
        </p>
        <div class="dialog-actions">
          <button class="btn primary" disabled={!dirtyMax} onClick={() => void saveConcurrency()}>
            Save
          </button>
        </div>
      </section>

      <section class="card">
        <h3>yt-dlp managed settings</h3>
        <p class="hint">
          These options are written into a clearly marked block in{' '}
          <span class="kv">{ytdlp?.status.path ?? 'your yt-dlp config'}</span>.
          Everything outside that block stays yours and untouched. Changes
          apply to future downloads only.
        </p>
        {!ytdlp?.edit_enabled && (
          <p class="hint text-warning">
            Editing is disabled. Enable <code>allow_yt_dlp_config_edit</code>{' '}
            in the manager configuration to use these controls.
          </p>
        )}
        {ytdlp?.warning && (
          <p class="hint text-danger" role="alert">
            {ytdlp.warning}
          </p>
        )}
        {ytdlp && (
          <div>
            <Field label="Download directory (under /downloads)" enabled={ytdlp.edit_enabled}>
              <input class="input" disabled={!ytdlp.edit_enabled} value={form.downloads_dir ?? ''} placeholder="/downloads" onInput={(e) => setF({ downloads_dir: (e.target as HTMLInputElement).value })} />
            </Field>
            <Field label="Format expression" enabled={ytdlp.edit_enabled}>
              <input class="input" disabled={!ytdlp.edit_enabled} value={form.format ?? ''} placeholder="bestvideo*+bestaudio/best" onInput={(e) => setF({ format: (e.target as HTMLInputElement).value })} />
            </Field>
            <Field label="Output template" enabled={ytdlp.edit_enabled}>
              <input class="input" disabled={!ytdlp.edit_enabled} value={form.output_template ?? ''} placeholder='%(title)s [%(id)s].%(ext)s' onInput={(e) => setF({ output_template: (e.target as HTMLInputElement).value })} />
            </Field>
            <div class="field-row">
              <span id="lbl-xa">Extract audio</span>
              <input aria-labelledby="lbl-xa" type="checkbox" disabled={!ytdlp.edit_enabled} checked={!!form.extract_audio} onChange={(e) => setF({ extract_audio: (e.target as HTMLInputElement).checked })} />
            </div>
            <Field label="Audio format" enabled={ytdlp.edit_enabled && !!form.extract_audio}>
              <select
                class="input input-auto"
                disabled={!ytdlp.edit_enabled || !form.extract_audio}
                value={form.audio_format ?? ''}
                onChange={(e) => setF({ audio_format: (e.target as HTMLSelectElement).value })}
              >
                <option value="">—</option>
                {['aac', 'alac', 'flac', 'm4a', 'mp3', 'opus', 'vorbis', 'wav'].map((f) => (
                  <option key={f} value={f}>{f}</option>
                ))}
              </select>
            </Field>
            <Field label="Subtitle languages (comma separated)" enabled={ytdlp.edit_enabled}>
              <input
                class="input"
                disabled={!ytdlp.edit_enabled}
                value={(form.sub_languages ?? []).join(',')}
                placeholder="en,en-US"
                onInput={(e) =>
                  setF({
                    sub_languages: (e.target as HTMLInputElement).value
                      .split(',')
                      .map((s) => s.trim())
                      .filter(Boolean)
                  })
                }
              />
            </Field>
            <div class="field-row">
              <span id="lbl-ws">Write subtitle files</span>
              <input aria-labelledby="lbl-ws" type="checkbox" disabled={!ytdlp.edit_enabled} checked={!!form.write_subs} onChange={(e) => setF({ write_subs: (e.target as HTMLInputElement).checked })} />
            </div>
            <Field label="Rate limit (e.g. 500K, 4M)" enabled={ytdlp.edit_enabled}>
              <input class="input" disabled={!ytdlp.edit_enabled} value={form.rate_limit ?? ''} onInput={(e) => setF({ rate_limit: (e.target as HTMLInputElement).value })} />
            </Field>
            <div class="dialog-actions">
              <button class="btn primary" disabled={!ytdlp.edit_enabled} onClick={() => void saveYtDlp()}>
                Save managed block
              </button>
            </div>
          </div>
        )}

        <div class="subsection">
          <h4>Extra arguments</h4>
          <p class="hint">
            Added to the command line of <strong>every</strong> download, after
            your yt-dlp config, so these win over it. They are defaults: a
            single download's own options and extra arguments come later still
            and override these. Written as you would type them on a command
            line — quotes are honoured, nothing else is: no shell runs, so{' '}
            <code class="kv">$(…)</code> and <code class="kv">*</code> stay
            literal text. Empty by default. Saved separately from the block
            above, in the manager's own configuration, so this field stays
            editable either way.
          </p>
          <textarea
            id="extra-args"
            class="input input-code"
            rows={2}
            spellcheck={false}
            placeholder="--limit-rate 2M --retries 20"
            value={extraArgs}
            onInput={(e) => {
              setExtraArgs((e.target as HTMLTextAreaElement).value)
              setExtraArgsError(null)
            }}
          />
          {extraArgsError && (
            <p class="field-error" role="alert">
              {extraArgsError}
            </p>
          )}
          <p class="hint">
            Only a fixed list of ordinary download options is accepted — rate
            limits, retries, proxies, headers, format sorting, subtitles,
            SponsorBlock and similar — spelled in full, since yt-dlp also
            accepts abbreviations. Anything else is refused by name, including
            options that run programs, load code, choose paths, or carry a
            password. Put those in your yt-dlp config file, which the manager
            never overrides. Avoid secrets here: this text is stored with the
            download and returned by the API.
          </p>
          <div class="dialog-actions">
            <button
              class="btn primary"
              disabled={!dirtyArgs || savingArgs}
              onClick={() => void saveExtraArgs()}
            >
              {savingArgs ? 'Saving…' : 'Save extra arguments'}
            </button>
          </div>
        </div>
      </section>
    </>
  )
}

function Field({ label, enabled, children }: { label: string; enabled: boolean; children: h.JSX.Element }) {
  return (
    <div class="field-row">
      <span class="spacer">{label}</span>
      <span class="field-value">{children}</span>
      {!enabled && <span class="visually-hidden">(read-only)</span>}
      {/* read-only rendering handled by disabling inputs via parent */}
    </div>
  )
}

function SecurityTab({
  view,
  pushToast
}: {
  view: SettingsView
  pushToast: (m: string, ok?: boolean) => void
}) {
  return (
    <>
      <section class="card">
        <h3>Security</h3>
        <div class="field-row">
          <span>Authentication</span>
          <span>{view.security.authentication_enabled ? 'enabled' : 'disabled (unauthenticated mode)'}</span>
        </div>
        <div class="field-row">
          <span>Cookies marked Secure</span>
          <span>{view.security.secure_cookie ? 'yes' : 'no (enable behind an HTTPS reverse proxy)'}</span>
        </div>
        <div class="field-row">
          <span>Listen address</span>
          <span class="kv">{view.security.listen}</span>
        </div>
        <p class="hint">
          Sessions expire after 24 hours of inactivity. Logging out invalidates
          the current session cookie.
        </p>
      </section>
      {view.security.authentication_enabled && <ChangePasswordCard pushToast={pushToast} />}
    </>
  )
}

function ChangePasswordCard({ pushToast }: { pushToast: (m: string, ok?: boolean) => void }) {
  const [password, setPassword] = useState('')
  const [confirmation, setConfirmation] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const tooShort = password.length > 0 && password.length < 8
  const mismatch = confirmation.length > 0 && password !== confirmation
  const canSubmit = !busy && password.length >= 8 && password === confirmation

  const submit = async (e: Event) => {
    e.preventDefault()
    if (!canSubmit) return
    setBusy(true)
    setError(null)
    try {
      const session = await api.changePassword(password, confirmation)
      // Changing the password is a security boundary: the server drops every
      // existing session and issues a fresh pair, so this tab has to adopt the
      // new CSRF token or its next mutation would be rejected.
      setCsrfToken(session.csrf_token ?? null)
      setPassword('')
      setConfirmation('')
      pushToast('Password changed. Other signed-in sessions were logged out.', true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'could not change password')
    } finally {
      setBusy(false)
    }
  }

  return (
    <section class="card card-spaced">
      <h3>Administrator password</h3>
      <p class="hint">
        Changing it signs out every other browser immediately. This one stays
        signed in.
      </p>
      <form onSubmit={submit}>
        <input type="text" name="username" autocomplete="username" value="admin" hidden />
        <div class="field-row">
          <label for="sec-new-password">New password</label>
          <input
            id="sec-new-password"
            class="input field-value"
            type="password"
            autocomplete="new-password"
            minlength={8}
            maxlength={1024}
            value={password}
            onInput={(e) => {
              setPassword((e.target as HTMLInputElement).value)
              setError(null)
            }}
            required
          />
        </div>
        <div class="field-row">
          <label for="sec-confirm-password">Confirm new password</label>
          <input
            id="sec-confirm-password"
            class="input field-value"
            type="password"
            autocomplete="new-password"
            minlength={8}
            maxlength={1024}
            value={confirmation}
            onInput={(e) => {
              setConfirmation((e.target as HTMLInputElement).value)
              setError(null)
            }}
            required
          />
        </div>
        {tooShort && <p class="field-error" role="alert">Use at least 8 characters.</p>}
        {mismatch && <p class="field-error" role="alert">Passwords do not match.</p>}
        {error && <p class="field-error" role="alert">{error}</p>}
        <div class="dialog-actions">
          <button class="btn primary" type="submit" disabled={!canSubmit}>
            {busy ? 'Saving…' : 'Change password'}
          </button>
        </div>
      </form>
    </section>
  )
}

function AdvancedTab({ view, system }: { view: SettingsView; system: SystemInfo | null }) {
  return (
    <section class="card">
      <h3>Paths &amp; internals (read-only)</h3>
      <PathRow label="State file" value={view.advanced.state_path} />
      <PathRow label="Manager config" value={view.advanced.config_path} />
      <PathRow label="yt-dlp config" value={view.advanced.yt_dlp_config_path} />
      <div class="field-row">
        <span>Config editing feature</span>
        <span>{view.advanced.allow_yt_dlp_config_edit ? 'enabled' : 'disabled'}</span>
      </div>
      <p class="hint">
        The standard yt-dlp configuration decides every download option, except
        where a single download overrides it through the queue's “Add with
        options” picker.
      </p>
      <p class="hint">Uptime: {system?.runtime.uptime_human ?? '…'}</p>
    </section>
  )
}

function PathRow({ label, value }: { label: string; value: string }) {
  return (
    <div class="field-row">
      <span>{label}</span>
      <span class="kv">{value || '—'}</span>
    </div>
  )
}

function AboutTab({ system }: { system: SystemInfo | null }) {
  // withName prefixes the detected binary. Only the JavaScript runtime needs
  // it: its label cannot name one interpreter (yt-dlp drives whichever of
  // deno/node/qjs/bun is installed), and node reports a bare "v24.18.1" that
  // identifies nothing. For yt-dlp and ffmpeg the label already says which
  // tool it is, so prefixing would just stutter.
  const tool = (
    label: string,
    t?: { present: boolean; name?: string; version?: string } | null,
    withName = false
  ) => (
    <div class="field-row">
      <span>{label}</span>
      <span>
        {!t?.present
          ? 'not found'
          : withName && t.name
            ? `${t.name} — ${t.version ?? 'present'}`
            : t.version ?? 'present'}
      </span>
    </div>
  )

  const commit = system?.version.commit
  const buildDate = system?.version.build_date
  const buildDetails = system
    ? [
        commit && commit !== 'unknown' ? commit : '',
        buildDate && buildDate !== 'unknown' ? `built ${buildDate}` : ''
      ].filter(Boolean)
    : []
  const buildLabel = system
    ? `${system.version.app_version}${buildDetails.length ? ` (${buildDetails.join(', ')})` : ''}`
    : '…'

  return (
    <section class="card">
      <h3>About</h3>
      <div class="field-row">
        <span>yt-dlp-manager</span>
        <span>{buildLabel}</span>
      </div>
      <div class="field-row">
        <span>API version</span>
        <span>v1</span>
      </div>
      {tool('Go runtime', { present: true, version: system?.version.go_version })}
      {tool('yt-dlp', system && system.tools.yt_dlp)}
      {tool('ffmpeg', system && system.tools.ffmpeg)}
      {tool('JavaScript runtime', system && system.tools.js_runtime, true)}
    </section>
  )
}
