package httpapi

import (
	"sort"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// Download is the web-facing DTO. It is deliberately separate from ipc.Item
// so the two wire protocols can evolve independently.
type Download struct {
	ID                  string   `json:"id"`
	URL                 string   `json:"url"`
	Title               string   `json:"title"`
	ThumbnailURL        string   `json:"thumbnail_url,omitempty"`
	State               string   `json:"state"`
	Forced              bool     `json:"forced"`
	PauseOrigin         string   `json:"pause_origin"` // none | user | shutdown
	Progress            float64  `json:"progress"`
	DownloadedBytes     int64    `json:"downloaded_bytes"`
	TotalBytes          int64    `json:"total_bytes"`
	SpeedBytesPerSecond int64    `json:"speed_bytes_per_second"`
	ETASeconds          int64    `json:"eta_seconds"`
	Error               string   `json:"error"`
	Files               []string `json:"files"`
	// Options is present only when this download overrode the configured
	// defaults; OptionsSummary is the same thing rendered for display.
	Options        *ipc.Options `json:"options,omitempty"`
	OptionsSummary string       `json:"options_summary,omitempty"`
	// OptionsInvalid marks a row whose saved options could not be read back.
	// It cannot be retried; only removed and re-added.
	OptionsInvalid bool       `json:"options_invalid,omitempty"`
	AddedAt        time.Time  `json:"added_at"`
	StartedAt      *time.Time `json:"started_at"`
	CompletedAt    *time.Time `json:"completed_at"`
	AllowedActions []string   `json:"allowed_actions"`
}

// allowedActionsFor is a pure helper: which actions make sense from a state.
func allowedActionsFor(s ipc.State) []string {
	switch s {
	case ipc.StateQueued:
		return []string{"pause", "start_now", "remove"}
	case ipc.StateDownloading:
		return []string{"pause", "remove"}
	case ipc.StatePaused:
		return []string{"resume", "start_now", "remove"}
	case ipc.StateCompleted:
		return []string{"remove"}
	case ipc.StateFailed:
		return []string{"resume", "start_now", "remove"}
	case ipc.StateDeleted:
		return []string{"resume", "start_now", "remove"}
	default:
		return nil
	}
}

func pauseOrigin(it ipc.Item) string {
	if it.State != ipc.StatePaused {
		return "none"
	}
	if it.UserPaused {
		return "user"
	}
	return "shutdown"
}

func toDownload(it ipc.Item) Download {
	d := Download{
		ID:                  it.ID,
		URL:                 it.URL,
		Title:               it.Title,
		ThumbnailURL:        it.ThumbURL,
		State:               string(it.State),
		Forced:              it.Forced,
		PauseOrigin:         pauseOrigin(it),
		Progress:            it.Progress,
		DownloadedBytes:     it.Got,
		TotalBytes:          it.Total,
		SpeedBytesPerSecond: it.Speed,
		ETASeconds:          it.ETA,
		Error:               it.Error,
		Files:               it.Files,
		AddedAt:             it.AddedAt,
		StartedAt:           it.StartedAt,
		CompletedAt:         it.DoneAt,
		AllowedActions:      allowedActionsFor(it.State),
	}
	if it.OptionsInvalid {
		// Retrying would run a different download than the one this row
		// records, so the only honest action left is removing it.
		d.AllowedActions = []string{"remove"}
		d.OptionsInvalid = true
	}
	if !it.Options.Empty() {
		opts := it.Options
		d.Options = &opts
		d.OptionsSummary = opts.Describe()
	}
	if d.Files == nil {
		d.Files = []string{}
	}
	if d.AllowedActions == nil {
		d.AllowedActions = []string{}
	}
	return d
}

func toDownloads(items []ipc.Item) []Download {
	out := make([]Download, 0, len(items))
	for _, it := range items {
		out = append(out, toDownload(it))
	}
	return out
}

// sortQueue mirrors the TUI ordering: active first, then queued in scheduler
// order, then paused, then terminal states last. Frontend sorting must not
// imply server-side reorder.
func sortQueue(items []ipc.Item) {
	rank := map[ipc.State]int{
		ipc.StateDownloading: 0,
		ipc.StateQueued:      1,
		ipc.StatePaused:      2,
		ipc.StateFailed:      3,
		ipc.StateCompleted:   4,
		ipc.StateDeleted:     4,
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := rank[items[i].State], rank[items[j].State]
		if ri != rj {
			return ri < rj
		}
		return items[i].AddedAt.Before(items[j].AddedAt)
	})
}
