import { h } from 'preact'
import { useEffect, useMemo, useState } from 'preact/hooks'
import { Dialog } from './dialog'
import { api } from '../api/client'
import { formatBytes } from '../format'
import type {
  DownloadOptions,
  Format,
  FormatPreset,
  FormatsResponse,
  YtDlpSettings
} from '../api/types'

const PRESETS: { value: FormatPreset; label: string }[] = [
  { value: 'best', label: 'Best available' },
  { value: '2160p', label: '2160p' },
  { value: '1440p', label: '1440p' },
  { value: '1080p', label: '1080p' },
  { value: '720p', label: '720p' },
  { value: '480p', label: '480p' },
  { value: '360p', label: '360p' },
  { value: 'audio', label: 'Audio only' }
]

const AUDIO_FORMATS = ['m4a', 'mp3', 'opus', 'flac', 'wav', 'aac', 'vorbis', 'alac']
const CONTAINERS = ['mp4', 'mkv', 'webm'] as const

/** Quality choice: either a preset ceiling or two hand-picked streams. */
type Mode = 'preset' | 'streams'

function formatDuration(seconds: number | undefined): string {
  if (!seconds || !Number.isFinite(seconds) || seconds <= 0) return ''
  const total = Math.round(seconds)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  const mm = String(m).padStart(h > 0 ? 2 : 1, '0')
  return h > 0
    ? `${h}:${mm}:${String(s).padStart(2, '0')}`
    : `${mm}:${String(s).padStart(2, '0')}`
}

/**
 * One line per stream in the pickers. Size is prefixed with "~" when yt-dlp
 * only estimated it, so an approximate figure is never read as a promise.
 */
export function formatLabel(f: Format): string {
  const bits: string[] = []
  if (f.has_video) {
    bits.push(f.resolution || (f.height ? `${f.height}p` : 'video'))
    if (f.fps && f.fps >= 1) bits.push(`${Math.round(f.fps)}fps`)
  } else {
    bits.push(f.format_note || 'audio')
    if (f.tbr) bits.push(`${Math.round(f.tbr)}k`)
  }
  if (f.ext) bits.push(f.ext)
  const codec = f.has_video ? f.vcodec : f.acodec
  if (codec && codec !== 'none') bits.push(codec.split('.')[0])
  if (f.filesize && f.filesize > 0) {
    bits.push(`${f.filesize_approximate ? '~' : ''}${formatBytes(f.filesize)}`)
  }
  if (f.has_video && f.has_audio) bits.push('with audio')
  return `${bits.join(' · ')} [${f.format_id}]`
}

/**
 * The options the dialog will send, or undefined when nothing was overridden.
 * Kept pure and exported so the preview line, the submit path and the tests
 * all agree on exactly what the server is being asked for.
 */
export function buildOptions(state: {
  mode: Mode
  preset: FormatPreset | ''
  videoId: string
  audioId: string
  container: string
  audioFormat: string
  subtitles: '' | 'on' | 'off'
  subLangs: string
  extraArgs: string
}): DownloadOptions | undefined {
  const o: DownloadOptions = {}
  if (state.mode === 'streams') {
    if (state.videoId) o.format_id = state.videoId
    if (state.audioId) o.audio_format_id = state.audioId
  } else if (state.preset) {
    o.preset = state.preset
  }
  const audioOnly = state.mode === 'preset' && state.preset === 'audio'
  if (audioOnly) {
    // An audio-only request is expressed as extraction plus, at most, an
    // audio format; a video format id and a merge container are meaningless
    // for it and the server rejects them outright.
    o.audio_only = true
    delete o.format_id
    if (state.audioFormat) o.audio_format = state.audioFormat
  } else if (state.container) {
    o.merge_container = state.container as DownloadOptions['merge_container']
  }
  if (state.subtitles) {
    o.subtitles = state.subtitles
    if (state.subtitles === 'on') {
      const langs = state.subLangs
        .split(',')
        .map((l) => l.trim())
        .filter(Boolean)
      if (langs.length > 0) o.sub_langs = langs
    }
  }
  const extra = state.extraArgs.trim()
  if (extra) o.extra_args = extra
  return Object.keys(o).length > 0 ? o : undefined
}

/** The yt-dlp arguments the server will add, mirrored for the preview line. */
export function previewArgs(o: DownloadOptions | undefined): string {
  if (!o) return 'no overrides — your Settings decide'
  const args: string[] = []
  const expr = formatExpression(o)
  if (expr) args.push('--format', expr)
  if (o.audio_only) {
    args.push('--extract-audio')
    if (o.audio_format) args.push('--audio-format', o.audio_format)
  } else if (o.merge_container) {
    args.push('--merge-output-format', o.merge_container)
  }
  if (o.subtitles === 'on') {
    args.push('--write-subs')
    if (o.sub_langs?.length) args.push('--sub-langs', o.sub_langs.join(','))
  } else if (o.subtitles === 'off') {
    args.push('--no-write-subs', '--no-write-auto-subs')
  }
  // Last, exactly as the server places them: whatever is typed here is the
  // final word on the command line. Shown verbatim rather than re-tokenized —
  // the preview must not imply a parse the server did not agree to.
  if (o.extra_args) args.push(o.extra_args)
  return args.join(' ')
}

const PRESET_HEIGHTS: Record<string, string> = {
  '2160p': '2160',
  '1440p': '1440',
  '1080p': '1080',
  '720p': '720',
  '480p': '480',
  '360p': '360'
}

// Mirrors FormatExpr in internal/manager/options.go. Explicit format ids get no
// fallback (a fallback would silently substitute other content), and every
// preset branch keeps its height ceiling.
function formatExpression(o: DownloadOptions): string {
  if (o.audio_only) return o.audio_format_id || 'ba/b'
  if (o.format_id && o.audio_format_id) return `${o.format_id}+${o.audio_format_id}`
  if (o.format_id) return o.format_id
  if (o.audio_format_id) return o.audio_format_id
  if (o.preset === 'best') return 'bv*+ba/b'
  if (o.preset === 'audio') return 'ba/b'
  if (o.preset) {
    const h = PRESET_HEIGHTS[o.preset]
    return h ? `bv*[height<=${h}]+ba/b[height<=${h}]/wv*[height<=${h}]+ba/w[height<=${h}]` : ''
  }
  return ''
}

interface Props {
  url: string
  onCancel: () => void
  onAdd: (options: DownloadOptions | undefined, startNow: boolean) => Promise<void>
}

/**
 * The "Add with options" dialog.
 *
 * It probes the URL for real formats on open, but never depends on that
 * probe: the presets are usable immediately and stay usable when the probe
 * fails, so a slow or unsupported extractor degrades to the same choices the
 * Settings page offers rather than blocking the add entirely.
 */
export function AddOptionsDialog({ url, onCancel, onAdd }: Props): h.JSX.Element {
  const [probe, setProbe] = useState<FormatsResponse | null>(null)
  const [probeError, setProbeError] = useState<string | null>(null)
  const [defaults, setDefaults] = useState<YtDlpSettings | null>(null)
  const [loading, setLoading] = useState(true)
  const [submitting, setSubmitting] = useState(false)
  const [submitError, setSubmitError] = useState<string | null>(null)

  const [mode, setMode] = useState<Mode>('preset')
  const [preset, setPreset] = useState<FormatPreset | ''>('best')
  const [videoId, setVideoId] = useState('')
  const [audioId, setAudioId] = useState('')
  const [container, setContainer] = useState('')
  const [audioFormat, setAudioFormat] = useState('')
  const [subtitles, setSubtitles] = useState<'' | 'on' | 'off'>('')
  const [subLangs, setSubLangs] = useState('en')
  const [extraArgs, setExtraArgs] = useState('')
  const [startNow, setStartNow] = useState(false)

  useEffect(() => {
    let live = true
    setLoading(true)
    api
      .formats(url)
      .then((resp) => {
        if (!live) return
        setProbe(resp)
        setProbeError(null)
      })
      .catch((e: unknown) => {
        if (!live) return
        setProbeError(e instanceof Error ? e.message : 'could not read formats')
      })
      .finally(() => {
        if (live) setLoading(false)
      })
    return () => {
      live = false
    }
  }, [url])

  // The configured defaults are read so the dialog can say what it cannot do:
  // yt-dlp's --extract-audio has no negation, so a configuration that sets it
  // converts every download to audio and no per-item argument can take that
  // back. A failure here is silently ignored — it only costs the warning.
  useEffect(() => {
    let live = true
    api
      .ytDlpSettings()
      .then((resp) => {
        if (live) setDefaults(resp.settings)
      })
      .catch(() => undefined)
    return () => {
      live = false
    }
  }, [])

  const videoFormats = useMemo(
    () => (probe?.formats ?? []).filter((f) => f.has_video),
    [probe]
  )
  const audioFormats = useMemo(
    () => (probe?.formats ?? []).filter((f) => !f.has_video && f.has_audio),
    [probe]
  )

  // A progressive format already contains audio. Offering a second audio
  // stream on top of it asks yt-dlp to merge audio into a file that has some,
  // which produces a duplicate track or an outright merge failure.
  const chosenVideo = videoFormats.find((f) => f.format_id === videoId)
  const videoHasAudio = Boolean(chosenVideo?.has_audio)
  const effectiveAudioId = videoHasAudio ? '' : audioId

  const state = {
    mode,
    preset,
    videoId,
    audioId: effectiveAudioId,
    container,
    audioFormat,
    subtitles,
    subLangs,
    extraArgs
  }
  const options = buildOptions(state)
  const audioOnly = mode === 'preset' && preset === 'audio'
  // Streams mode with no video picked downloads the audio stream on its own.
  // There is nothing to merge, so a container choice would be meaningless —
  // and it is deliberately NOT turned into --extract-audio: the chosen stream
  // is already the audio, and re-encoding it was not asked for.
  const audioStreamOnly = mode === 'streams' && !videoId && Boolean(audioId)
  const showContainer = !audioOnly && !audioStreamOnly
  // A stream picker with nothing picked would send no format at all, which
  // silently means "the configured default" — not what someone who switched
  // to this mode is asking for.
  const streamsIncomplete = mode === 'streams' && !videoId && !audioId

  const extractionForced = Boolean(defaults?.extract_audio) && !audioOnly

  const submit = async () => {
    setSubmitting(true)
    setSubmitError(null)
    try {
      await onAdd(options, startNow)
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : 'could not add download')
      setSubmitting(false)
    }
  }

  return (
    <Dialog title="Add with options" onClose={onCancel} wide>
      <p class="dialog-sub">
        {probe?.title ? (
          <>
            <strong>{probe.title}</strong>
            {probe.duration_seconds ? ` · ${formatDuration(probe.duration_seconds)}` : ''}
          </>
        ) : (
          <span class="kv">{url}</span>
        )}
      </p>

      {loading && <p class="hint">Reading available formats…</p>}
      {probeError && (
        <p class="hint text-warning" role="status">
          Could not read this URL's formats ({probeError}). The quality presets below
          still work.
        </p>
      )}

      <fieldset class="opt-group">
        <legend>Quality</legend>
        <div class="opt-modes">
          <label>
            <input
              type="radio"
              name="quality-mode"
              checked={mode === 'preset'}
              onChange={() => setMode('preset')}
            />{' '}
            Preset
          </label>
          <label>
            <input
              type="radio"
              name="quality-mode"
              checked={mode === 'streams'}
              disabled={videoFormats.length === 0 && audioFormats.length === 0}
              onChange={() => setMode('streams')}
            />{' '}
            Choose streams
            {videoFormats.length + audioFormats.length > 0
              ? ` (${videoFormats.length + audioFormats.length})`
              : ''}
          </label>
        </div>

        {mode === 'preset' ? (
          <div class="opt-presets">
            {PRESETS.map((p) => (
              <button
                key={p.value}
                type="button"
                class={`btn small${preset === p.value ? ' primary' : ''}`}
                aria-pressed={preset === p.value}
                onClick={() => setPreset(preset === p.value ? '' : p.value)}
              >
                {p.label}
              </button>
            ))}
          </div>
        ) : (
          <div class="opt-rows">
            <label class="opt-row">
              <span>Video</span>
              <select
                class="input"
                value={videoId}
                onChange={(e) => setVideoId((e.target as HTMLSelectElement).value)}
              >
                <option value="">No video (audio only)</option>
                {videoFormats.map((f) => (
                  <option key={f.format_id} value={f.format_id}>
                    {formatLabel(f)}
                  </option>
                ))}
              </select>
            </label>
            <label class="opt-row">
              <span>Audio</span>
              <select
                class="input"
                value={effectiveAudioId}
                disabled={videoHasAudio}
                title={
                  videoHasAudio
                    ? 'This video stream already contains audio'
                    : undefined
                }
                onChange={(e) => setAudioId((e.target as HTMLSelectElement).value)}
              >
                <option value="">
                  {videoHasAudio
                    ? 'Included in the video stream'
                    : videoId
                      ? 'None (video only)'
                      : 'Use the default'}
                </option>
                {audioFormats.map((f) => (
                  <option key={f.format_id} value={f.format_id}>
                    {formatLabel(f)}
                  </option>
                ))}
              </select>
            </label>
          </div>
        )}
      </fieldset>

      <fieldset class="opt-group">
        <legend>Output</legend>
        <div class="opt-rows">
          {audioOnly ? (
            <label class="opt-row">
              <span>Audio format</span>
              <select
                class="input"
                value={audioFormat}
                onChange={(e) => setAudioFormat((e.target as HTMLSelectElement).value)}
              >
                <option value="">Use my settings</option>
                {AUDIO_FORMATS.map((a) => (
                  <option key={a} value={a}>
                    {a}
                  </option>
                ))}
              </select>
            </label>
          ) : (
            showContainer && (
              <label class="opt-row">
                <span>Container</span>
                <select
                  class="input"
                  value={container}
                  onChange={(e) => setContainer((e.target as HTMLSelectElement).value)}
                >
                  <option value="">Use my settings</option>
                  {CONTAINERS.map((c) => (
                    <option key={c} value={c}>
                      {c}
                    </option>
                  ))}
                </select>
              </label>
            )
          )}
          <label class="opt-row">
            <span>Subtitles</span>
            <select
              class="input"
              value={subtitles}
              onChange={(e) =>
                setSubtitles((e.target as HTMLSelectElement).value as '' | 'on' | 'off')
              }
            >
              <option value="">Use my settings</option>
              <option value="on">Download</option>
              <option value="off">Skip</option>
            </select>
          </label>
          {subtitles === 'on' && (
            <label class="opt-row">
              <span>Languages</span>
              <input
                class="input"
                value={subLangs}
                placeholder="en, de"
                onInput={(e) => setSubLangs((e.target as HTMLInputElement).value)}
              />
            </label>
          )}
          <label class="opt-row opt-check">
            <input
              type="checkbox"
              checked={startNow}
              onChange={(e) => setStartNow((e.target as HTMLInputElement).checked)}
            />
            <span>Start immediately, ahead of the queue</span>
          </label>
        </div>
      </fieldset>

      {extractionForced && (
        <p class="hint text-warning" role="status">
          Your Settings extract audio from every download (yt-dlp's
          <code class="kv"> --extract-audio</code>), and that option cannot be
          turned off for a single download. This will still produce an audio
          file. Turn it off under Settings → yt-dlp to keep the video.
        </p>
      )}

      <fieldset class="opt-group">
        <legend>Extra arguments</legend>
        <p class="hint opt-hint">
          For this download only. They go last on the command line, so they
          override the choices above, the global arguments from Settings and
          your yt-dlp config. Only a fixed list of download options is accepted,
          spelled in full; a refusal names the option. Empty means none.
        </p>
        <textarea
          class="input input-code"
          rows={2}
          spellcheck={false}
          placeholder="--limit-rate 2M --retries 20"
          value={extraArgs}
          onInput={(e) => {
            setExtraArgs((e.target as HTMLTextAreaElement).value)
            setSubmitError(null)
          }}
        />
      </fieldset>

      <p class="opt-preview">
        <span class="hint">yt-dlp will be run with:</span>
        <code class="kv">{previewArgs(options)}</code>
      </p>

      {submitError && (
        <p class="field-error" role="alert">
          {submitError}
        </p>
      )}

      <div class="dialog-actions">
        <button class="btn" type="button" onClick={onCancel} disabled={submitting}>
          Cancel
        </button>
        <button
          class="btn primary"
          type="button"
          disabled={submitting || streamsIncomplete}
          title={streamsIncomplete ? 'Pick a video or an audio stream first' : undefined}
          onClick={() => void submit()}
        >
          {submitting ? 'Adding…' : 'Add download'}
        </button>
      </div>
    </Dialog>
  )
}
