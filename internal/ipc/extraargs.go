package ipc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Extra arguments are the one place where free-form yt-dlp options are
// accepted, so this file is a trust boundary: the web UI can be exposed, and a
// stolen session must not become code execution or filesystem access on the
// host.
//
// It is an ALLOWLIST, deliberately. A refusal list cannot work here, because
// yt-dlp's option parser accepts unambiguous abbreviations: "--qui" reaches
// --quiet and "--outpu" reaches --output, so any list of forbidden spellings is
// bypassed by typing a shorter one. yt-dlp also offers --alias, which defines
// new options that expand to arbitrary other options, and several innocuous
// looking options that run programs or touch paths (--netrc-cmd runs a command,
// --exec runs a command, --ffmpeg-location picks the binary to run,
// --cache-dir with --rm-cache-dir deletes a directory tree, --cookies and
// --download-archive read and write files of the caller's choosing).
//
// Only exact spellings of the options below are accepted. Anything else — a new
// option, an abbreviation, a short flag, a bare word — is refused, so the set
// of things yt-dlp can be told to do from the API is finite and reviewable.
//
// Dedicated credential options (--username, --password, --netrc-cmd,
// --video-password, --ap-*) are deliberately absent for a second reason: extra
// arguments are persisted in the state file and returned by the API, so a
// secret typed here would outlive the download in plain text. That is a
// mitigation, not a guarantee — --add-header is allowed and can carry an
// Authorization or Cookie value — so the UI says plainly that this field is
// stored and shown, and secrets do not belong in it.
//
// The escape hatch for anything not listed is the user's own yt-dlp config
// file, which the manager never overrides and which requires access to the
// host rather than to a browser session.

const (
	// MaxExtraArgsLen bounds the raw text. It is deliberately small: every
	// queued item may carry its own copy into the persisted snapshot, which
	// has its own hard ceiling.
	MaxExtraArgsLen = 1024
	// MaxExtraArgTokens bounds how many tokens it may parse into.
	MaxExtraArgTokens = 48
	// MaxExtraArgTokenLen bounds one token.
	MaxExtraArgTokenLen = 512
)

// allowedExtraArgs maps each permitted option to how many values it consumes.
//
// The arity matters for correctness, not just for parsing: a bare word is only
// accepted when it is the value of an option that takes one, which is what
// stops extra arguments from smuggling in a second URL for yt-dlp to download
// outside the queue's knowledge. TestAllowlistArityMatchesYtDlp checks every
// entry here against the installed yt-dlp so the table cannot silently drift.
var allowedExtraArgs = map[string]int{
	// Rate limiting, retries and connection behaviour.
	"--limit-rate":                     1,
	"--throttled-rate":                 1,
	"--retries":                        1,
	"--file-access-retries":            1,
	"--fragment-retries":               1,
	"--retry-sleep":                    1,
	"--skip-unavailable-fragments":     0,
	"--abort-on-unavailable-fragments": 0,
	"--keep-fragments":                 0,
	"--no-keep-fragments":              0,
	"--buffer-size":                    1,
	"--resize-buffer":                  0,
	"--no-resize-buffer":               0,
	"--http-chunk-size":                1,
	"--concurrent-fragments":           1,
	"--sleep-requests":                 1,
	"--sleep-interval":                 1,
	"--max-sleep-interval":             1,
	"--sleep-subtitles":                1,
	"--socket-timeout":                 1,
	"--source-address":                 1,
	"--force-ipv4":                     0,
	"--force-ipv6":                     0,
	"--proxy":                          1,
	"--geo-verification-proxy":         1,
	"--xff":                            1,
	// Selection, filtering and playlist shaping are deliberately NOT here.
	//
	// Two reasons, both found by running them. First, the manager owns
	// playlist expansion: it probes with --flat-playlist and turns a playlist
	// into one queue entry per video, so --playlist-items or --no-playlist on
	// the download command arrives long after the queue was built and shapes
	// nothing. Second, options that skip a download exit 0 without producing a
	// file (--match-filters with a non-matching filter prints nothing and
	// exits 0), and --ignore-errors can report success over a failed
	// post-process — both of which would mark an entry completed with no
	// media behind it.
	// Format preference.
	"--format-sort":            1,
	"--format-sort-force":      0,
	"--no-format-sort-force":   0,
	"--video-multistreams":     0,
	"--no-video-multistreams":  0,
	"--audio-multistreams":     0,
	"--no-audio-multistreams":  0,
	"--prefer-free-formats":    0,
	"--no-prefer-free-formats": 0,
	"--check-formats":          0,
	"--no-check-formats":       0,
	"--check-all-formats":      0,
	"--merge-output-format":    1,
	"--remux-video":            1,
	"--recode-video":           1,
	"--audio-quality":          1,
	// Subtitles.
	"--write-subs":         0,
	"--no-write-subs":      0,
	"--write-auto-subs":    0,
	"--no-write-auto-subs": 0,
	"--sub-langs":          1,
	"--sub-format":         1,
	"--convert-subs":       1,
	"--embed-subs":         0,
	"--no-embed-subs":      0,
	// Thumbnails, metadata and sidecar files, written beside the download.
	"--embed-thumbnail":    0,
	"--no-embed-thumbnail": 0,
	"--embed-metadata":     0,
	"--no-embed-metadata":  0,
	"--embed-chapters":     0,
	"--no-embed-chapters":  0,
	"--write-thumbnail":    0,
	"--no-write-thumbnail": 0,
	"--write-description":  0,
	"--write-info-json":    0,
	// HTTP request shaping.
	"--user-agent":            1,
	"--referer":               1,
	"--add-header":            1,
	"--impersonate":           1,
	"--no-check-certificates": 0,
	// SponsorBlock.
	"--sponsorblock-mark":          1,
	"--sponsorblock-remove":        1,
	"--sponsorblock-chapter-title": 1,
	"--no-sponsorblock":            0,
	// Geo restrictions.
	"--geo-bypass":          0,
	"--no-geo-bypass":       0,
	"--geo-bypass-country":  1,
	"--geo-bypass-ip-block": 1,
	// Extractor behaviour.
	"--extractor-args":             1,
	"--extractor-retries":          1,
	"--ignore-no-formats-error":    0,
	"--no-ignore-no-formats-error": 0,
	"--hls-use-mpegts":             0,
	"--no-hls-use-mpegts":          0,
	"--hls-split-discontinuity":    0,
	"--no-hls-split-discontinuity": 0,
	// File handling inside the configured download directory.
	"--no-overwrites":    0,
	"--force-overwrites": 0,
	"--continue":         0,
	"--no-continue":      0,
	"--part":             0,
	// --no-part is deliberately absent: it makes yt-dlp write straight to the
	// final filename, so cancelling or removing a download would leave a
	// truncated file under the name of finished media. Cleanup removes only
	// scratch names precisely so real media is never deleted, and that
	// promise cannot hold when the two are the same file.
	"--mtime":                 0,
	"--no-mtime":              0,
	"--windows-filenames":     0,
	"--no-windows-filenames":  0,
	"--restrict-filenames":    0,
	"--no-restrict-filenames": 0,
	"--trim-filenames":        1,
	"--keep-video":            0,
	"--no-keep-video":         0,
}

// probeSafeExtraArgs is the subset of allowed options that may also be passed
// to a metadata or format probe.
//
// A probe is not a download: options that select, filter or limit what gets
// downloaded change its output, and several change its exit status, which a
// probe reads as failure. Forwarding them wholesale meant a valid setting
// could stop every download from starting.
//
// What remains is what a probe genuinely needs: how to reach the site and how
// to identify itself.
var probeSafeExtraArgs = map[string]bool{
	// Reaching the site.
	"--proxy": true, "--geo-verification-proxy": true, "--source-address": true,
	"--force-ipv4": true, "--force-ipv6": true, "--socket-timeout": true,
	"--xff": true, "--no-check-certificates": true,
	"--retries": true, "--extractor-retries": true, "--sleep-requests": true,
	// Identifying itself to the site.
	"--user-agent": true, "--referer": true, "--add-header": true,
	"--impersonate": true,
	// Geo handling, which decides whether the site answers at all.
	"--geo-bypass": true, "--no-geo-bypass": true,
	"--geo-bypass-country": true, "--geo-bypass-ip-block": true,
	// Extractor tuning, which decides what metadata comes back.
	"--extractor-args": true,
}

// ProbeSafeArgs filters already-validated arguments down to the ones a probe
// may use, dropping each rejected option together with the value it consumes.
//
// "Already-validated" is the contract, not an assumption it makes: an option
// left without its value would otherwise slice past the end of the slice, and
// this runs on the manager's own goroutines where a panic takes the process
// down rather than one request. A truncated tail is simply dropped — passing a
// valueless option on to yt-dlp would make it read the next thing on the
// command line as the value.
func ProbeSafeArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, _, hasInline := strings.Cut(args[i], "=")
		values := 0
		if !hasInline {
			values = allowedExtraArgs[name]
		}
		if i+values >= len(args) {
			break
		}
		if probeSafeExtraArgs[name] {
			out = append(out, args[i:i+1+values]...)
		}
		i += values
	}
	return out
}

// AllowedExtraArgOptions lists the accepted options, sorted, for documentation
// and for the settings UI to show.
func AllowedExtraArgOptions() []string {
	out := make([]string, 0, len(allowedExtraArgs))
	for name := range allowedExtraArgs {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// ParseExtraArgs splits a command-line string into tokens.
//
// It implements quoting only — single quotes, double quotes and backslash
// escaping — and deliberately performs no expansion of any kind: no globbing,
// no variables, no command substitution, no tilde. The result is passed to
// exec without a shell, so "$(id)" is four literal characters and stays that
// way.
func ParseExtraArgs(raw string) ([]string, error) {
	if len(raw) > MaxExtraArgsLen {
		return nil, fmt.Errorf("extra arguments must not exceed %d characters", MaxExtraArgsLen)
	}
	for _, r := range raw {
		if isUnsafeControl(r) {
			return nil, errors.New("extra arguments must not contain control characters")
		}
	}
	var (
		args    []string
		current strings.Builder
		quote   rune // 0, '\'' or '"'
		started bool // a token exists even if it is empty ("" is a valid arg)
	)
	flush := func() {
		if started {
			args = append(args, current.String())
			current.Reset()
			started = false
		}
	}
	runes := []rune(raw)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
				continue
			}
			current.WriteRune(c)
		case quote == '"':
			// Inside double quotes a backslash escapes only the characters a
			// POSIX shell would let it escape; anything else stays literal.
			if c == '\\' && i+1 < len(runes) {
				next := runes[i+1]
				if next == '"' || next == '\\' {
					current.WriteRune(next)
					i++
					continue
				}
			}
			if c == '"' {
				quote = 0
				continue
			}
			current.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == '\\' && i+1 < len(runes):
			current.WriteRune(runes[i+1])
			started = true
			i++
		case c == ' ' || c == '\t':
			flush()
		default:
			current.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, errors.New("extra arguments have an unclosed quote")
	}
	flush()
	if len(args) > MaxExtraArgTokens {
		return nil, fmt.Errorf("extra arguments must not exceed %d separate arguments", MaxExtraArgTokens)
	}
	for _, a := range args {
		if len(a) > MaxExtraArgTokenLen {
			return nil, fmt.Errorf("a single extra argument must not exceed %d characters", MaxExtraArgTokenLen)
		}
	}
	return args, nil
}

// ValidateExtraArgs walks the tokens as yt-dlp's parser would, accepting only
// allowlisted options and the values they consume.
func ValidateExtraArgs(args []string) error {
	pendingValues := 0
	for _, a := range args {
		if pendingValues > 0 {
			// The value of an allowlisted option. It is never inspected as an
			// option itself, which is why an option's arity has to be right.
			pendingValues--
			continue
		}
		if !strings.HasPrefix(a, "--") {
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf(
					"%q is a short flag; extra arguments accept only long options such as \"--limit-rate\"", a)
			}
			// A bare word in option position is a URL to yt-dlp: it would add
			// a download the queue knows nothing about, with none of the URL
			// validation the API applies.
			return fmt.Errorf(
				"%q is not an option; extra arguments cannot contain URLs or stray values", a)
		}
		name, inlineValue, hasInline := strings.Cut(a, "=")
		arity, allowed := allowedExtraArgs[name]
		if !allowed {
			return fmt.Errorf(
				"%s is not an accepted extra argument. Only a fixed list of download options is "+
					"allowed here, spelled in full; put anything else in your yt-dlp config file", name)
		}
		if hasInline {
			if arity == 0 {
				return fmt.Errorf("%s does not take a value", name)
			}
			if inlineValue == "" {
				return fmt.Errorf("%s needs a value", name)
			}
			continue
		}
		pendingValues = arity
	}
	if pendingValues > 0 {
		return fmt.Errorf("%s needs a value", args[len(args)-1])
	}
	return nil
}

// ExtraArgs parses and validates raw text in one step. It is what every entry
// point should call: nothing reaches a command line without both.
func ExtraArgs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	args, err := ParseExtraArgs(raw)
	if err != nil {
		return nil, err
	}
	if err := ValidateExtraArgs(args); err != nil {
		return nil, err
	}
	return args, nil
}
