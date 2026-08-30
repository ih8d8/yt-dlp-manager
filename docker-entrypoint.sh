#!/bin/sh
# Container entrypoint: best-effort repair of the app-owned volume paths,
# then drop to the non-root runtime user.
#
# Scope discipline (deliberate):
#   * Only the documented volume roots (/downloads /config /state), runtime
#     tmpdirs, and manager-owned config/state subdirectories are touched. We
#     never recursively chown the media tree.
#   * All repairs are best-effort: failures warn and continue, and the Go
#     server keeps its own authoritative preflight checks, so a genuinely
#     unusable mount still fails with an actionable message instead of a
#     half-fixed state.
set -eu

APP_UID=${PUID:-1000}
APP_GID=${PGID:-1000}

case "$APP_UID:$APP_GID" in
  *[!0-9:]*|:*|*:|*:*:*)
    echo "entrypoint: PUID and PGID must be positive numeric IDs" >&2
    exit 1
    ;;
esac
if [ "$APP_UID" -eq 0 ] || [ "$APP_GID" -eq 0 ]; then
  echo "entrypoint: refusing to run the application as root (PUID/PGID must be non-zero)" >&2
  exit 1
fi

# CMD supplies only the subcommand ("server"), so route everything through
# the manager binary unless an explicit executable path was given.
BIN=/usr/local/bin/yt-dlp-manager

run_as_app() {
  case "${1:-}" in
    /*|./*)
      exec su-exec "$APP_UID:$APP_GID" "$@"
      ;;
    *)
      exec su-exec "$APP_UID:$APP_GID" "$BIN" "$@"
      ;;
  esac
}

if [ "$(id -u)" != "0" ]; then
  # Started with an explicit user (e.g. compose `user:`): nothing to do.
  case "${1:-}" in
    /*|./*)
      exec "$@"
      ;;
    *)
      exec "$BIN" "$@"
      ;;
  esac
fi

# Order matters, and so does who does the work.
#
# The image chowns /config, /state and /downloads to its own build-time user,
# so a freshly initialised named volume is NOT root-owned. That means root
# cannot create subdirectories inside them without CAP_DAC_OVERRIDE — a
# capability worth not needing at all. So: root only ever chowns the volume
# roots (CAP_CHOWN is enough for that), and the subdirectories underneath are
# created by the application user itself, which owns them by then.
for d in /downloads /config /state /tmp/runtime /tmp/home /tmp/cache; do
  mkdir -p "$d" 2>/dev/null || true
done

# XDG_RUNTIME_DIR must be 0700: the control socket is chmod 0600 inside it, and
# the socket-path reasoning in internal/ipc assumes a private parent. The
# Dockerfile chmods it at build time, but /tmp is a fresh tmpfs (mode 1777) at
# runtime, so that mode does not survive to here.
chmod 700 /tmp/runtime 2>/dev/null || true

chown "$APP_UID:$APP_GID" \
  /downloads /config /state /tmp/runtime /tmp/home /tmp/cache \
  2>/dev/null || echo "entrypoint: warning: could not adjust volume ownership" >&2

# Now that the roots belong to the app user, let it create its own subtree.
# The single quotes are deliberate: "$d" has to be expanded by the inner shell
# running as the app user, not by this one.
# shellcheck disable=SC2016
su-exec "$APP_UID:$APP_GID" sh -c '
  for d in /config/yt-dlp /config/yt-dlp-manager /state/yt-dlp-manager; do
    mkdir -p "$d" 2>/dev/null || true
  done
' || true

# Repair anything a previous release left owned by a different uid. Best-effort:
# a pre-existing tree the app user cannot chown is reported by the server's own
# preflight with a far more actionable message than anything available here.
chown -R "$APP_UID:$APP_GID" \
  /config/yt-dlp /config/yt-dlp-manager /state/yt-dlp-manager \
  2>/dev/null || true

run_as_app "$@"
