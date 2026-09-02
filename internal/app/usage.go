package app

const usageText = `yt-dlp-manager — one binary, four ways to run yt-dlp downloads

  yt-dlp-manager                  open the terminal UI (attaches if a manager is running)
  yt-dlp-manager tui [flags]      explicitly open the terminal UI
  yt-dlp-manager server [flags]   headless manager + HTTP web UI + API (container mode)
  yt-dlp-manager daemon [flags]   headless manager, Unix-socket control only
  yt-dlp-manager add [URL|-]      add url ('-' reads stdin; default: clipboard)
  yt-dlp-manager list             show downloads
  yt-dlp-manager pause <id>       pause (resumable, keeps partials)
  yt-dlp-manager resume <id>      re-queue paused download
  yt-dlp-manager start-now <id>   prioritize now (at most 8 above concurrency limit)
  yt-dlp-manager remove <id>      remove from list (stops if running, clears partials)
  yt-dlp-manager clear-finished   drop completed/failed rows
  yt-dlp-manager clear-all        stop everything and empty the list
  yt-dlp-manager healthcheck      container health probe (HTTP /healthz or live socket)
  yt-dlp-manager version          print build information

server flags:
  --listen ADDR              HTTP listen address (default 127.0.0.1:8080)
  --max N                    maximum concurrent downloads
  --state PATH               state file path
  --socket PATH              unix control socket path
  --config PATH              manager config file path
  --allow-unauthenticated    explicitly serve without authentication (risk)
  --secure-cookie            mark session cookies Secure (use behind HTTPS proxy)

Lifecycle: 'tui' with no live owner starts a manager for the duration of the
UI; closing it stops all yt-dlp children cleanly and saves state. 'daemon'
and 'server' keep running until signalled. Exactly one process may own a
state file and socket at a time. With nothing running, 'add' parks URLs into
a persistent inbox that drains on next launch.

Your existing yt-dlp configuration stays authoritative for download choices:
format selection, output directory/template, cookies, retries, and the rest.
Download jobs add only quiet, color-safe, machine-readable output flags —
plus, for a download added through the web UI's options picker, the specific
format/container/subtitle choices made for that one item.

Environment:
  YTDLP_MANAGER_MAX_CONCURRENT  default concurrency when no flag is given
                                (YTDLPTUI_MAX_CONCURRENT still honored as a
                                deprecated fallback)
  YTDLP_MANAGER_LISTEN          server listen address (host:port)
  YTDLP_MANAGER_ALLOW_UNAUTHENTICATED
                                serve without authentication; refused unless
                                the listen address is loopback
  YTDLP_MANAGER_SECURE_COOKIE   mark session cookies Secure (HTTPS proxy)
  YTDLP_MANAGER_TRUSTED_HOSTS   comma-separated host[:port] values accepted in
                                the Host header (a wildcard bind otherwise
                                accepts any syntactically valid one).

First-run administrator setup is limited to requests that arrive from this
machine. Anything else — another host, or a container port proxy — must send
the one-time setup token printed to this log at startup as the X-Setup-Token
header; the web UI's setup screen asks for it when it is needed.
  YTDLP_MANAGER_ALLOW_YTDLP_CONFIG_EDIT
                                let the web UI edit the managed block in your
                                yt-dlp config (off by default; the shipped
                                compose files turn it on)

Flags override environment variables, which override the config file.

Paths (XDG):
  socket: $XDG_RUNTIME_DIR/yt-dlp-manager.sock
          fallback $XDG_CACHE_HOME/yt-dlp-manager/yt-dlp-manager.sock
  state:  $XDG_STATE_HOME/yt-dlp-manager/state.json
  inbox:  $XDG_STATE_HOME/yt-dlp-manager/inbox.jsonl

ids are shown by 'list'; client commands attach to whichever manager is
running — TUI, daemon, or server.`

// Usage returns the complete help text.
func Usage() string { return usageText }
