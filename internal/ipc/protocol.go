package ipc

import (
	"net/url"
	"strings"
	"time"
)

type State string

const (
	StateQueued      State = "queued"
	StateDownloading State = "downloading"
	StatePaused      State = "paused"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateDeleted     State = "deleted"
)

type Item struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Title      string     `json:"title,omitempty"`
	ThumbURL   string     `json:"thumb_url,omitempty"`
	State      State      `json:"state"`
	Forced     bool       `json:"forced,omitempty"`
	UserPaused bool       `json:"user_paused,omitempty"`
	Files      []string   `json:"files,omitempty"`
	Progress   float64    `json:"progress"`
	Got        int64      `json:"got,omitempty"`
	Total      int64      `json:"total,omitempty"`
	Speed      int64      `json:"speed,omitempty"`
	ETA        int64      `json:"eta,omitempty"`
	Error      string     `json:"error,omitempty"`
	AddedAt    time.Time  `json:"added_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	DoneAt     *time.Time `json:"done_at,omitempty"`
}

type Request struct {
	Cmd string `json:"cmd"`
	ID  string `json:"id,omitempty"`
	URL string `json:"url,omitempty"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	ID    string `json:"id,omitempty"`
	Count *int   `json:"count,omitempty"`
	Items []Item `json:"items,omitempty"`
}

type Event struct {
	Event string `json:"event"`
	ID    string `json:"id,omitempty"`
	Item  *Item  `json:"item,omitempty"`
	Items []Item `json:"items,omitempty"`
}

func (s State) String() string {
	return string(s)
}

func (e Event) IsProgress() bool {
	return e.Event == "update" && e.Item != nil && e.Item.State == StateDownloading
}

// ValidURL reports whether u is safe to pass to yt-dlp as a positional
// argument. The runner always places it after "--", but this is defense in
// depth: no control bytes (which could smuggle newlines into logs or
// terminals) and nothing that looks like a flag.
//
// The scheme check is the contract every caller already advertises — the HTTP
// error text, the OpenAPI description and the README all say http(s). Without
// it any argument yt-dlp's parser accepts became an API input: a bare local
// path, "file://", or "ytsearch10:cats", which silently expands one request
// into ten downloads.
func ValidURL(u string) bool {
	u = strings.TrimSpace(u)
	switch {
	case u == "", len(u) > 4096:
		return false
	case strings.HasPrefix(u, "-"):
		return false
	}
	for _, r := range u {
		if isUnsafeControl(r) {
			return false
		}
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return false
	}
	// Reject "http:///path" and "http://" — a scheme without an authority is
	// not something yt-dlp can fetch, and it is a common typo shape.
	return parsed.Host != ""
}

func isUnsafeControl(r rune) bool {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return true
	}
	// Unicode bidi controls can visually reorder an otherwise harmless URL
	// into misleading terminal text.
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
