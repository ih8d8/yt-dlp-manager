package ipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
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
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	Title      string   `json:"title,omitempty"`
	ThumbURL   string   `json:"thumb_url,omitempty"`
	State      State    `json:"state"`
	Forced     bool     `json:"forced,omitempty"`
	UserPaused bool     `json:"user_paused,omitempty"`
	Files      []string `json:"files,omitempty"`
	Progress   float64  `json:"progress"`
	Got        int64    `json:"got,omitempty"`
	Total      int64    `json:"total,omitempty"`
	Speed      int64    `json:"speed,omitempty"`
	ETA        int64    `json:"eta,omitempty"`
	Options    Options  `json:"options"`
	// OptionsInvalid marks a row whose saved per-download options could not be
	// read back. It is persisted, because the row must stay un-runnable across
	// restarts: re-running it would download something materially different
	// from what was asked for, wearing the same row.
	OptionsInvalid bool       `json:"options_invalid,omitempty"`
	Error          string     `json:"error,omitempty"`
	AddedAt        time.Time  `json:"added_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	DoneAt         *time.Time `json:"done_at,omitempty"`
}

// MarshalNoHTMLEscape encodes v as JSON without escaping "<", ">" and "&".
//
// Go escapes those by default so output can be embedded in HTML. Nothing here
// ever is: this JSON goes to a state file and to API responses read by a JSON
// parser. The escaping is not merely wasteful — it expands each such byte to
// six, so a filename or URL containing them costs six times its length, which
// is the difference between a path fitting its storage budget and being
// dropped as too long.
func MarshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a newline that Marshal does not.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalJSON omits Options when nothing was overridden. encoding/json's
// omitempty does not cover structs, and most items carry no overrides at all,
// so without this every API response and every row of the persisted snapshot
// would grow an empty "options":{} that means nothing.
func (i Item) MarshalJSON() ([]byte, error) {
	// A distinct type with no methods: marshalling Item directly here would
	// recurse into this function.
	type itemJSON Item
	if i.Options.Empty() {
		return MarshalNoHTMLEscape(struct {
			itemJSON
			Options *Options `json:"options,omitempty"`
		}{itemJSON(i), nil})
	}
	return MarshalNoHTMLEscape(itemJSON(i))
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

// Options carries the per-download overrides chosen when an item was added.
// It is deliberately a closed set of structured choices rather than free-form
// yt-dlp arguments: every value here ends up on a command line, so the only
// thing that can appear there is something this file already understands.
// Anything not set falls through to the user's own yt-dlp configuration,
// which stays authoritative for every choice nobody overrode.
type Options struct {
	// FormatID and AudioFormatID name concrete formats from a format probe.
	// Together they become "-f <video>+<audio>"; alone, "-f <id>".
	FormatID      string `json:"format_id,omitempty"`
	AudioFormatID string `json:"audio_format_id,omitempty"`
	// Preset is a resolution ceiling expressed without probing, so the picker
	// stays usable when a format probe fails or times out.
	Preset string `json:"preset,omitempty"`
	// MergeContainer maps to --merge-output-format.
	MergeContainer string `json:"merge_container,omitempty"`
	// AudioOnly maps to --extract-audio; AudioFormat to --audio-format.
	AudioOnly   bool   `json:"audio_only,omitempty"`
	AudioFormat string `json:"audio_format,omitempty"`
	// Subtitles is a tri-state ("", "on", "off") rather than a bool: an unset
	// value must mean "whatever the config file says", which a bool cannot
	// express once it has been through a JSON round trip.
	Subtitles string   `json:"subtitles,omitempty"`
	SubLangs  []string `json:"sub_langs,omitempty"`
	// ExtraArgs is free-form yt-dlp command line text for this download only,
	// held as typed rather than pre-split so it round-trips to the UI exactly
	// as it was written. It is parsed and validated by ExtraArgs() at every
	// entry point; see extraargs.go for what it refuses and why.
	ExtraArgs string `json:"extra_args,omitempty"`
}

// Presets are the resolution ceilings the picker offers.
var optionPresets = map[string]bool{
	"best": true, "2160p": true, "1440p": true, "1080p": true,
	"720p": true, "480p": true, "360p": true, "audio": true,
}

var optionContainers = map[string]bool{"mp4": true, "mkv": true, "webm": true}

// optionAudioFormats mirrors the list the managed yt-dlp config block accepts.
// It is duplicated rather than shared because ipc must not depend on config:
// the two lists describe the same yt-dlp capability from opposite directions.
var optionAudioFormats = map[string]bool{
	"aac": true, "alac": true, "flac": true, "m4a": true,
	"mp3": true, "opus": true, "vorbis": true, "wav": true,
}

var formatIDRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)
var subLangOptRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}$`)

// ValidFormatID reports whether s is a format id this package would accept in
// Options. It is exported so a format probe can refuse to offer an id that
// would be rejected on the way back in.
func ValidFormatID(s string) bool { return formatIDRe.MatchString(s) }

// MaxSubLangs bounds one item's --sub-langs list.
const MaxSubLangs = 20

// Empty reports whether no override was chosen at all, in which case the item
// behaves exactly as it did before per-download options existed.
func (o Options) Empty() bool {
	return o.FormatID == "" && o.AudioFormatID == "" && o.Preset == "" &&
		o.MergeContainer == "" && !o.AudioOnly && o.AudioFormat == "" &&
		o.Subtitles == "" && len(o.SubLangs) == 0 && o.ExtraArgs == ""
}

// Normalize trims and lowercases the fields with fixed vocabularies. It is
// applied before Validate so that "MP4" and " 1080p " are accepted spellings
// rather than rejections, without widening what the command line can contain.
func (o Options) Normalize() Options {
	o.FormatID = strings.TrimSpace(o.FormatID)
	o.AudioFormatID = strings.TrimSpace(o.AudioFormatID)
	o.Preset = strings.ToLower(strings.TrimSpace(o.Preset))
	o.MergeContainer = strings.ToLower(strings.TrimSpace(o.MergeContainer))
	o.AudioFormat = strings.ToLower(strings.TrimSpace(o.AudioFormat))
	o.Subtitles = strings.ToLower(strings.TrimSpace(o.Subtitles))
	o.ExtraArgs = strings.TrimSpace(o.ExtraArgs)
	langs := make([]string, 0, len(o.SubLangs))
	seen := map[string]bool{}
	for _, l := range o.SubLangs {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		langs = append(langs, l)
	}
	if len(langs) == 0 {
		langs = nil
	}
	o.SubLangs = langs
	return o
}

// Validate reports why the options cannot be turned into yt-dlp arguments.
func (o Options) Validate() error {
	if o.FormatID != "" && !formatIDRe.MatchString(o.FormatID) {
		return errors.New("format_id must be 1-64 characters of letters, digits, '.', '_' or '-'")
	}
	if o.AudioFormatID != "" && !formatIDRe.MatchString(o.AudioFormatID) {
		return errors.New("audio_format_id must be 1-64 characters of letters, digits, '.', '_' or '-'")
	}
	if o.Preset != "" && !optionPresets[o.Preset] {
		return errors.New("preset must be one of best, 2160p, 1440p, 1080p, 720p, 480p, 360p, audio")
	}
	if o.Preset != "" && (o.FormatID != "" || o.AudioFormatID != "") {
		return errors.New("preset and explicit format ids cannot both be set")
	}
	if o.MergeContainer != "" && !optionContainers[o.MergeContainer] {
		return errors.New("merge_container must be mp4, mkv or webm")
	}
	if o.AudioFormat != "" && !optionAudioFormats[o.AudioFormat] {
		return errors.New("audio_format is not one of yt-dlp's supported audio formats")
	}
	if o.AudioFormat != "" && !o.AudioOnly {
		return errors.New("audio_format only applies when audio_only is set")
	}
	if o.AudioOnly && o.FormatID != "" {
		return errors.New("audio_only cannot be combined with a video format id; use audio_format_id")
	}
	if o.AudioOnly && o.MergeContainer != "" {
		return errors.New("merge_container does not apply to an audio-only download")
	}
	switch o.Subtitles {
	case "", "on", "off":
	default:
		return errors.New(`subtitles must be "on" or "off"`)
	}
	if len(o.SubLangs) > MaxSubLangs {
		return fmt.Errorf("at most %d subtitle languages may be requested", MaxSubLangs)
	}
	if len(o.SubLangs) > 0 && o.Subtitles != "on" {
		return errors.New(`sub_langs only applies when subtitles is "on"`)
	}
	for _, l := range o.SubLangs {
		if !subLangOptRe.MatchString(l) {
			return fmt.Errorf("subtitle language %q is not a valid language tag", l)
		}
	}
	if _, err := ExtraArgs(o.ExtraArgs); err != nil {
		return err
	}
	return nil
}

// Equal reports whether two option sets describe the same download. Adding a
// URL that is already tracked is refused as a duplicate, so this is what makes
// "the same video again, but at 720p" a distinct request rather than a clash.
func (o Options) Equal(other Options) bool {
	if o.FormatID != other.FormatID || o.AudioFormatID != other.AudioFormatID ||
		o.Preset != other.Preset || o.MergeContainer != other.MergeContainer ||
		o.AudioOnly != other.AudioOnly || o.AudioFormat != other.AudioFormat ||
		o.Subtitles != other.Subtitles || o.ExtraArgs != other.ExtraArgs ||
		len(o.SubLangs) != len(other.SubLangs) {
		return false
	}
	for i := range o.SubLangs {
		if o.SubLangs[i] != other.SubLangs[i] {
			return false
		}
	}
	return true
}

// Key is a canonical string form, used as part of the identity of a download
// request. Field order in the struct is what makes it stable; Normalize is
// what makes two spellings of the same choice produce the same key.
func (o Options) Key() string {
	b, err := json.Marshal(o)
	if err != nil {
		// Options holds only strings, bools and a string slice, so this is
		// unreachable; degrade to "no options" rather than panicking in the
		// middle of a queue mutation.
		return ""
	}
	return string(b)
}

// Describe renders the overrides as short human-readable text for queue rows
// and the details drawer.
func (o Options) Describe() string {
	var parts []string
	switch {
	case o.AudioOnly:
		s := "audio only"
		if o.AudioFormat != "" {
			s += " (" + o.AudioFormat + ")"
		}
		parts = append(parts, s)
	case o.Preset != "":
		parts = append(parts, o.Preset)
	}
	if o.FormatID != "" || o.AudioFormatID != "" {
		ids := o.FormatID
		if ids != "" && o.AudioFormatID != "" {
			ids += "+" + o.AudioFormatID
		} else if ids == "" {
			ids = o.AudioFormatID
		}
		parts = append(parts, "format "+ids)
	}
	if o.MergeContainer != "" {
		parts = append(parts, o.MergeContainer)
	}
	switch o.Subtitles {
	case "on":
		s := "subtitles"
		if len(o.SubLangs) > 0 {
			s += " (" + strings.Join(o.SubLangs, ", ") + ")"
		}
		parts = append(parts, s)
	case "off":
		parts = append(parts, "no subtitles")
	}
	if o.ExtraArgs != "" {
		parts = append(parts, o.ExtraArgs)
	}
	return strings.Join(parts, " · ")
}
