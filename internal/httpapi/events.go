package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// SSE contract:
//   - the first event is always a full snapshot (subscribing under the
//     manager's lock guarantees ordering),
//   - updates are named "download" / "removed" / "stats",
//   - correctness relies on fresh snapshots on reconnect, never replay,
//   - per-client queues are bounded; an overflowing client is disconnected
//     rather than allowed to accumulate unbounded memory,
//   - a heartbeat comment every 15 seconds detects dead peers.

const (
	sseHeartbeat  = 15 * time.Second
	sseClientBuf  = 256
	sseWriteSlack = 30 * time.Second
)

var sseEventSeq atomic.Uint64

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "streaming unsupported")
		return
	}
	rc := http.NewResponseController(w)
	// Long-lived stream: disable the request-scoped read deadline (nothing
	// else will ever be read) and manage write deadlines per write below.
	// Both require the ResponseWriter chain to support Unwrap.
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		// Fall back: without deadline control the server's ReadTimeout would
		// kill this stream; fail fast so the client retries immediately
		// rather than experiencing periodic silent drops.
		writeError(w, r, http.StatusInternalServerError, codeInternal, "streaming unsupported")
		return
	}
	if err := rc.SetWriteDeadline(time.Now().Add(sseWriteSlack)); err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // disable reverse-proxy buffering when present
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": stream open\n\n")
	flusher.Flush()

	events, unsubscribe := s.deps.Manager.Subscribe()
	defer unsubscribe()

	ctx := r.Context()
	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	sendEvent := func(name string, payload any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		id := sseEventSeq.Add(1)
		if _, werr := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, name, data); werr != nil {
			return false
		}
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteSlack))
		flusher.Flush()
		return true
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			var sent bool
			switch ev.Event {
			case "snapshot":
				sent = sendEvent("snapshot", map[string]any{
					"downloads": toDownloads(ev.Items),
				})
			case "update":
				if ev.Item != nil {
					dl := toDownload(*ev.Item)
					sent = sendEvent("download", map[string]any{"download": dl})
				} else {
					sent = true
				}
			case "removed":
				// The id is manager-minted here, so eviction is safe and this
				// is the earliest point the cached image is known to be dead.
				s.EvictThumbnail(ev.ID)
				sent = sendEvent("removed", map[string]any{"id": ev.ID})
			default:
				sent = true
			}
			if !sent {
				return
			}

		case <-ticker.C:
			// Heartbeat comment keeps proxies from idling out and doubles as
			// a periodic stats push.
			if !sendEvent("stats", s.deps.Manager.StatsSnapshot()) {
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()

		case <-ctx.Done():
			return
		}
	}
}
