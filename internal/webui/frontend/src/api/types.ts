export type DownloadState =
  | 'queued'
  | 'downloading'
  | 'paused'
  | 'completed'
  | 'failed'
  | 'deleted'

export type PauseOrigin = 'none' | 'user' | 'shutdown'

export type AllowedAction =
  | 'pause'
  | 'resume'
  | 'start_now'
  | 'remove'
  | 'purge'
  | 'retry'

/**
 * Per-download overrides. Every field is optional: an omitted field means the
 * server's yt-dlp configuration decides, which is what the plain paste-and-add
 * path sends. The server validates this same shape and rejects anything
 * outside it, so the picker can only ever ask for choices it was offered.
 */
export interface DownloadOptions {
  format_id?: string
  audio_format_id?: string
  preset?: FormatPreset
  merge_container?: 'mp4' | 'mkv' | 'webm'
  audio_only?: boolean
  audio_format?: string
  subtitles?: 'on' | 'off'
  sub_langs?: string[]
  /** Free-form yt-dlp arguments for this download only. */
  extra_args?: string
}

export type FormatPreset =
  | 'best'
  | '2160p'
  | '1440p'
  | '1080p'
  | '720p'
  | '480p'
  | '360p'
  | 'audio'

/** One selectable stream, as reported by POST /api/v1/formats. */
export interface Format {
  format_id: string
  ext?: string
  resolution?: string
  width?: number
  height?: number
  fps?: number
  vcodec?: string
  acodec?: string
  filesize?: number
  /** The size is yt-dlp's estimate rather than a known length. */
  filesize_approximate?: boolean
  tbr?: number
  format_note?: string
  protocol?: string
  language?: string
  has_video: boolean
  has_audio: boolean
}

export interface FormatsResponse {
  url: string
  title?: string
  duration_seconds?: number
  extractor?: string
  formats: Format[]
  truncated?: boolean
}

export interface Download {
  id: string
  url: string
  title: string
  thumbnail_url?: string
  state: DownloadState
  forced: boolean
  pause_origin: PauseOrigin
  progress: number
  downloaded_bytes: number
  total_bytes: number
  speed_bytes_per_second: number
  eta_seconds: number
  error: string
  files: string[]
  added_at: string
  started_at: string | null
  completed_at: string | null
  allowed_actions: AllowedAction[]
  /** Present only when this download overrode the configured defaults. */
  options?: DownloadOptions
  options_summary?: string
  /** The row's saved options could not be read back after a restart, so it
   *  cannot be retried — only removed and re-added. */
  options_invalid?: boolean
}

export interface StartNowResult {
  requested: boolean
  applied: boolean
  error?: string
}

export interface AddDownloadResponse {
  download: Download
  start_now: StartNowResult
}

export interface ApiError {
  code: string
  message: string
  request_id?: string
}

export interface BatchResult {
  id: string
  ok: boolean
  error?: ApiError
}

export interface Stats {
  running: number
  queued: number
  paused: number
  completed: number
  failed: number
  deleted: number
  total_speed_bytes_per_second: number
}

export interface SessionInfo {
  authenticated: boolean
  csrf_token?: string
  setup_required: boolean
  /** True when this origin cannot claim the administrator account on its own
   *  and first-run setup has to present the token from the server log. */
  setup_token_required?: boolean
}

export interface SettingsView {
  ui: { theme: string; compact: boolean }
  downloads: { max_concurrent: number; source: string; extra_args: string }
  security: {
    authentication_enabled: boolean
    unauthenticated_mode: boolean
    secure_cookie: boolean
    listen: string
  }
  advanced: {
    allow_yt_dlp_config_edit: boolean
    state_path: string
    config_path: string
    yt_dlp_config_path: string
  }
  sources: Record<string, string>
}

export interface YtDlpSettings {
  downloads_dir?: string
  format?: string
  output_template?: string
  extract_audio?: boolean
  audio_format?: string
  sub_languages?: string[]
  write_subs?: boolean
  rate_limit?: string
  has_block?: boolean
}

export interface YtDlpSettingsResponse {
  status: {
    path: string
    exists: boolean
    symlink: boolean
    size_bytes: number
    has_block: boolean
    problem?: string
  }
  settings: YtDlpSettings | null
  edit_enabled: boolean
  warning?: string
}

export interface SystemInfo {
  version: {
    app_version: string
    commit: string
    build_date: string
    go_version: string
  }
  runtime: { go: string; uptime_human: string }
  tools: {
    yt_dlp: ToolVersion
    ffmpeg: ToolVersion
    js_runtime: ToolVersion
  }
  paths: {
    state_path: string
    config_path: string
    yt_dlp_config_path: string
    listen: string
  }
  uptime_seconds: number
}

export interface ToolVersion {
  present: boolean
  /** Binary that answered the probe; absent when nothing was found. */
  name?: string
  version?: string
  path?: string
  error?: string
}
