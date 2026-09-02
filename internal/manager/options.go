package manager

import (
	"strings"

	"yt-dlp-manager/internal/ipc"
)

// presetHeights maps a picker preset to the resolution ceiling it means.
// "best" and "audio" are handled separately because they are not ceilings.
var presetHeights = map[string]string{
	"2160p": "2160",
	"1440p": "1440",
	"1080p": "1080",
	"720p":  "720",
	"480p":  "480",
	"360p":  "360",
}

// FormatExpr renders the --format expression for a set of options, or "" when
// the options say nothing about format selection and the user's own config
// should decide.
//
// Every value that reaches this function has been through ipc.Options.Validate,
// which restricts format ids to letters, digits, '.', '_' and '-'. That is what
// makes string concatenation into a format expression safe: none of the
// characters yt-dlp's format parser treats as operators can appear in an id.
func FormatExpr(o ipc.Options) string {
	if o.AudioOnly {
		if o.AudioFormatID != "" {
			return o.AudioFormatID
		}
		return "ba/b"
	}
	switch {
	// Explicitly chosen formats get NO fallback. A fallback here would let a
	// vanished audio stream turn into a silent video-only file, or a vanished
	// pair into "whatever is best" — and the row would still be marked
	// completed, claiming to hold something it does not. Failing is the honest
	// outcome: the picker can be reopened against what the site offers now.
	case o.FormatID != "" && o.AudioFormatID != "":
		return o.FormatID + "+" + o.AudioFormatID
	case o.FormatID != "":
		return o.FormatID
	case o.AudioFormatID != "":
		return o.AudioFormatID
	case o.Preset == "best":
		return "bv*+ba/b"
	case o.Preset == "audio":
		return "ba/b"
	case o.Preset != "":
		h := presetHeights[o.Preset]
		if h == "" {
			return ""
		}
		// Every branch keeps the ceiling. An unfiltered "wv*+ba/w" fallback
		// would quietly hand back a 4K stream when nothing at or below the
		// requested height exists, which is not what picking "720p" asks for;
		// failing to find a format is the honest outcome there.
		return "bv*[height<=" + h + "]+ba/b[height<=" + h + "]" +
			"/wv*[height<=" + h + "]+ba/w[height<=" + h + "]"
	}
	return ""
}

// OptionArgs turns validated per-download options into yt-dlp arguments.
//
// Negative flags are emitted deliberately rather than omitted: the user's
// config file may already carry --extract-audio or --write-subs, and an option
// set that means "a normal video download" has to be able to say so, not just
// stay silent and inherit the opposite.
func OptionArgs(o ipc.Options) []string {
	if o.Empty() {
		return nil
	}
	var args []string
	if expr := FormatExpr(o); expr != "" {
		args = append(args, "--format", expr)
	}
	if o.AudioOnly {
		args = append(args, "--extract-audio")
		if o.AudioFormat != "" {
			args = append(args, "--audio-format", o.AudioFormat)
		}
	} else if o.MergeContainer != "" {
		args = append(args, "--merge-output-format", o.MergeContainer)
	}
	// There is deliberately no "--no-extract-audio" here: yt-dlp's -x has no
	// negation, so a configuration file that sets it wins over any per-item
	// video choice and there is no argument that can take it back. The dialog
	// warns about that combination instead of emitting an option that does
	// not exist — which is exactly what a live run caught this doing.
	switch o.Subtitles {
	case "on":
		args = append(args, "--write-subs")
		if len(o.SubLangs) > 0 {
			args = append(args, "--sub-langs", strings.Join(o.SubLangs, ","))
		}
	case "off":
		args = append(args, "--no-write-subs", "--no-write-auto-subs")
	}
	return args
}
