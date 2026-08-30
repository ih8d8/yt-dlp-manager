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
  /**
   * Set when an action succeeded but did not do everything its name implies —
   * chiefly a purge whose files another entry also records and which were
   * therefore kept. Surfacing it stops "Delete files" reading as a plain
   * success while the files are still on disk.
   */
  note?: string
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
  downloads: { max_concurrent: number; source: string }
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
