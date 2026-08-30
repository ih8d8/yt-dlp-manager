package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// This file implements comment-preserving editing of a clearly delimited
// block inside the user's normal yt-dlp configuration file. Everything
// outside the markers is preserved byte-for-byte. Only an allowlist of
// recognized options can appear in or be written to the block; dangerous
// directives (--exec, external downloaders, plugin paths, arbitrary config
// locations) can therefore never be introduced through the structured API.

const (
	managedBegin = "# BEGIN yt-dlp-manager managed settings"
	managedEnd   = "# END yt-dlp-manager managed settings"

	// Existing installations may already contain the pre-rename markers.
	// They remain readable and are migrated to the canonical spelling on the
	// next structured save, without changing anything outside the block.
	legacyManagedBegin = "# BEGIN yt-dlp " + "Manager managed settings"
	legacyManagedEnd   = "# END yt-dlp " + "Manager managed settings"

	MaxYtDlpConfigBytes = 1 << 20
)

type YtDlpSettings struct {
	DownloadsDir   string   `json:"downloads_dir"`   // absolute, rooted at /downloads
	Format         string   `json:"format"`          // --format expression
	OutputTemplate string   `json:"output_template"` // --output template
	ExtractAudio   bool     `json:"extract_audio"`
	AudioFormat    string   `json:"audio_format,omitempty"`
	SubLanguages   []string `json:"sub_languages,omitempty"` // --sub-langs entries
	WriteSubs      bool     `json:"write_subs"`
	RateLimit      string   `json:"rate_limit,omitempty"` // --limit-rate
	HasBlock       bool     `json:"has_block"`
}

var audioFormats = map[string]bool{
	"aac": true, "alac": true, "flac": true, "m4a": true,
	"mp3": true, "opus": true, "vorbis": true, "wav": true,
}

var rateLimitRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[KkMmGgTt]?B?$`)
var subLangRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)

// outtmplTypeRe matches yt-dlp's optional "TYPE:TEMPLATE" output-template
// prefix (e.g. "thumbnail:cover.%(ext)s"). Each type gets its own template, so
// each must be confined independently.
var outtmplTypeRe = regexp.MustCompile(`^([a-z_][a-z0-9_]*):`)

// pathFieldRe matches output-template fields whose value is itself a path
// (rather than a sanitized filename component). Allowing them would let the
// template resolve outside the download root even though every ordinary field
// value has its path separators escaped by yt-dlp.
var pathFieldRe = regexp.MustCompile(`%\(\s*(filepath|__files|_filename|filename|__infojson_filename|__thumbnail_filename)\b`)

// confinePath rejects any value that could resolve outside the directory
// yt-dlp is pointed at. yt-dlp runs both --paths and --output through
// expand_path, so "~" and "$VAR" are expansions, not literals: a template of
// "$HOME/x" or "~/x" writes to the home directory regardless of --paths, and
// an absolute template ignores --paths entirely. Field values are NOT a vector
// here — yt-dlp escapes path separators inside them — so only the literal
// template text needs constraining.
func confinePath(field, v string) error {
	if v == "" {
		return nil
	}
	if strings.ContainsRune(v, '$') {
		return fmt.Errorf("%s must not contain %q: yt-dlp expands environment variables in it", field, "$")
	}
	if strings.HasPrefix(v, "~") {
		return fmt.Errorf("%s must not start with %q: yt-dlp expands it to a home directory", field, "~")
	}
	if strings.HasPrefix(v, "/") {
		return fmt.Errorf("%s must be relative to the download directory, not an absolute path", field)
	}
	for _, seg := range strings.Split(filepath.ToSlash(v), "/") {
		if seg == ".." {
			return fmt.Errorf("%s must not traverse upward with %q", field, "..")
		}
	}
	return nil
}

// YtDlpStatus describes the on-disk yt-dlp config for read-only display.
type YtDlpStatus struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Symlink   bool   `json:"symlink"`
	SizeBytes int64  `json:"size_bytes"`
	HasBlock  bool   `json:"has_block"`
	Problem   string `json:"problem,omitempty"`
}

// ValidateYtDlpSettings enforces the allowlist bounds. It rejects control
// characters everywhere so no option can smuggle extra lines into the
// configuration file.
func ValidateYtDlpSettings(s *YtDlpSettings) error {
	if s.DownloadsDir != "" {
		// Every other free-text option is control-character checked; this one
		// was not. A newline here survives ytDlpQuote (single quotes make it
		// literal, which is exactly right for yt-dlp's whole-file shlex parse)
		// but splits the option across two lines, and parseManagedLines reads
		// the block line by line — so the block this very function just wrote
		// came back as "unreadable: unterminated single quote", blanking the
		// settings form until someone saved over it.
		if err := rejectControls("download directory", s.DownloadsDir); err != nil {
			return err
		}
		// Checked before Clean: Clean does not remove "$" or "~", and yt-dlp
		// expands both, so "/downloads/$VAR" could still resolve elsewhere.
		if strings.ContainsRune(s.DownloadsDir, '$') || strings.HasPrefix(s.DownloadsDir, "~") {
			return errors.New("download directory must not contain \"$\" or start with \"~\": yt-dlp expands both")
		}
		c := filepath.Clean(s.DownloadsDir)
		if c != "/downloads" && !strings.HasPrefix(c, "/downloads/") {
			return errors.New("download directory must stay under /downloads")
		}
		for _, seg := range strings.Split(c, "/") {
			if seg == ".." {
				return errors.New("download directory must not traverse upward")
			}
		}
		s.DownloadsDir = c
	}
	if len(s.Format) > 500 {
		return errors.New("format expression exceeds 500 characters")
	}
	if err := rejectControls("format", s.Format); err != nil {
		return err
	}
	if len(s.OutputTemplate) > 500 {
		return errors.New("output template exceeds 500 characters")
	}
	if err := rejectControls("output template", s.OutputTemplate); err != nil {
		return err
	}
	// The output template names the FILE; --paths names the DIRECTORY. Without
	// this the template escapes the download root entirely (an absolute -o
	// overrides -P), which is how an allowlisted settings write could reach any
	// path the process can write — including yt-dlp's own config file.
	if s.OutputTemplate != "" {
		body := s.OutputTemplate
		if m := outtmplTypeRe.FindStringSubmatch(body); m != nil {
			// "TYPE:TEMPLATE" gives that output type its own template; confine
			// the template, not the type name.
			body = body[len(m[0]):]
		}
		if pathFieldRe.MatchString(body) {
			return errors.New("output template must not use a path-valued field such as %(filepath)s")
		}
		if err := confinePath("output template", body); err != nil {
			return err
		}
	}
	if s.AudioFormat != "" {
		if !s.ExtractAudio {
			return errors.New("audio format requires extract audio")
		}
		if !audioFormats[s.AudioFormat] {
			return errors.New("unsupported audio format")
		}
	}
	if len(s.SubLanguages) > 16 {
		return errors.New("too many subtitle languages")
	}
	for _, l := range s.SubLanguages {
		if len(l) > 40 || !subLangRe.MatchString(l) {
			return fmt.Errorf("invalid subtitle language %q", l)
		}
	}
	if s.RateLimit != "" && !rateLimitRe.MatchString(s.RateLimit) {
		return errors.New("rate limit must look like 500K, 4M, or a byte count")
	}
	if err := rejectControls("rate limit", s.RateLimit); err != nil {
		return err
	}
	return nil
}

func rejectControls(field, v string) error {
	for _, r := range v {
		if r == '\n' || r == '\r' || r == 0x7f || (r < 0x20 && r != '\t') || r == 0 {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}

// renderManagedBlock produces the delimited block for these settings.
func renderManagedBlock(s *YtDlpSettings) string {
	var b strings.Builder
	b.WriteString(managedBegin + "\n")
	add := func(name, val string) {
		if val != "" {
			b.WriteString(name + " " + ytDlpQuote(val) + "\n")
		}
	}
	if s.DownloadsDir != "" {
		add("--paths", s.DownloadsDir)
	}
	add("--format", s.Format)
	add("--output", s.OutputTemplate)
	if s.ExtractAudio {
		b.WriteString("-x\n")
	}
	add("--audio-format", s.AudioFormat)
	if len(s.SubLanguages) > 0 {
		add("--sub-langs", strings.Join(s.SubLanguages, ","))
	}
	if s.WriteSubs {
		b.WriteString("--write-subs\n")
	}
	add("--limit-rate", s.RateLimit)
	b.WriteString(managedEnd + "\n")
	return b.String()
}

// unsafeUnquoted matches any byte outside the set Python's shlex.quote treats
// as safe to emit bare. yt-dlp parses its config with shlex.split(comments=True),
// so this is the authoritative rule, not shell intuition.
var unsafeUnquoted = regexp.MustCompile(`[^\w@%+=:,./-]`)

// ytDlpQuote quotes a value for a yt-dlp configuration file.
//
// yt-dlp reads its config with Python's shlex.split(contents, comments=True),
// so this must match shlex semantics rather than general shell rules. Two
// consequences drive the implementation:
//
//   - Inside DOUBLE quotes, shlex only honors a backslash before '"' or '\'.
//     Backslash-escaping '$' or '`' there (as this function used to) leaves a
//     LITERAL backslash in the value yt-dlp receives, silently corrupting any
//     format or template containing them.
//   - '#' begins a comment, so a value containing '#' must be quoted or
//     yt-dlp truncates it.
//
// Single quotes avoid both problems: every byte between them is literal, and
// only an embedded single quote needs handling. This mirrors shlex.quote.
func ytDlpQuote(v string) string {
	if v == "" {
		return "''"
	}
	if !unsafeUnquoted.MatchString(v) {
		return v
	}
	// Close the quote, emit an escaped apostrophe, reopen.
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// splitShellWord tokenizes one configuration line the way yt-dlp does
// (POSIX-shell-like). It supports single quotes, double quotes with limited
// backslash escapes, and bare backslash escapes.
func splitShellWord(line string) ([]string, error) {
	var (
		tokens []string
		cur    strings.Builder
		inTok  bool
	)
	i := 0
	appendCur := func() {
		if inTok {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i < len(line) {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			appendCur()
			i++
		case c == '#' && !inTok:
			// shlex(comments=True) treats '#' as a comment only where a token
			// would start; mid-token it is an ordinary character ("a#b").
			i = len(line)
		case c == '\'':
			inTok = true
			j := i + 1
			end := strings.IndexByte(line[j:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(line[j : j+end])
			i = j + end + 1
		case c == '"':
			inTok = true
			i++
			closed := false
			for i < len(line) {
				ch := line[i]
				if ch == '"' {
					closed = true
					i++
					break
				}
				if ch == '\\' && i+1 < len(line) {
					// shlex honors a backslash inside double quotes only before
					// '"' or '\'. Before anything else BOTH bytes are literal,
					// so reading "\$" back as "$" would report a value yt-dlp
					// never sees.
					switch line[i+1] {
					case '"', '\\':
						cur.WriteByte(line[i+1])
						i += 2
						continue
					default:
						cur.WriteByte(ch)
						i++
						continue
					}
				}
				cur.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, errors.New("unterminated double quote")
			}
		case c == '\\':
			if i+1 >= len(line) {
				return nil, errors.New("trailing backslash")
			}
			inTok = true
			cur.WriteByte(line[i+1])
			i += 2
		default:
			inTok = true
			cur.WriteByte(c)
			i++
		}
	}
	appendCur()
	return tokens, nil
}

// parseManagedLines decodes the allowlisted options found between markers.
func parseManagedLines(block string) (*YtDlpSettings, error) {
	s := &YtDlpSettings{HasBlock: true}
	for ln, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		toks, err := splitShellWord(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln+1, err)
		}
		if len(toks) == 0 {
			continue
		}
		switch toks[0] {
		case "-x", "--extract-audio":
			s.ExtractAudio = true
		case "--write-subs":
			s.WriteSubs = true
		case "--paths":
			if len(toks) != 2 {
				return nil, fmt.Errorf("line %d: --paths needs exactly one value", ln+1)
			}
			s.DownloadsDir = toks[1]
		case "--format":
			if len(toks) != 2 {
				return nil, fmt.Errorf("line %d: --format needs exactly one value", ln+1)
			}
			s.Format = toks[1]
		case "--output":
			if len(toks) != 2 {
				return nil, fmt.Errorf("line %d: --output needs exactly one quoted value", ln+1)
			}
			s.OutputTemplate = toks[1]
		case "--audio-format":
			if len(toks) != 2 {
				return nil, fmt.Errorf("line %d: --audio-format needs exactly one value", ln+1)
			}
			s.AudioFormat = toks[1]
		case "--sub-langs":
			if len(toks) != 2 {
				return nil, fmt.Errorf("line %d: --sub-langs needs exactly one value", ln+1)
			}
			s.SubLanguages = strings.Split(toks[1], ",")
		case "--limit-rate":
			if len(toks) != 2 {
				return nil, fmt.Errorf("line %d: --limit-rate needs exactly one value", ln+1)
			}
			s.RateLimit = toks[1]
		default:
			return nil, fmt.Errorf("line %d: option %q is not managed by yt-dlp-manager", ln+1, toks[0])
		}
	}
	return s, nil
}

// locateManagedBlock finds the byte range spanned by the marker lines. It
// refuses duplicated, nested, or reversed markers rather than guessing. When
// no block exists it returns (-1,-1,nil).
func locateManagedBlock(content string) (start, end int, err error) {
	newBeginCount := strings.Count(content, managedBegin)
	newEndCount := strings.Count(content, managedEnd)
	legacyBeginCount := strings.Count(content, legacyManagedBegin)
	legacyEndCount := strings.Count(content, legacyManagedEnd)
	beginCount := newBeginCount + legacyBeginCount
	endCount := newEndCount + legacyEndCount
	switch {
	case beginCount > 1 || endCount > 1:
		return 0, 0, errors.New("managed settings block markers are duplicated")
	case beginCount == 0 && endCount == 0:
		return -1, -1, nil
	case beginCount == 0 || endCount == 0:
		return 0, 0, errors.New("managed settings block has only one marker")
	case (newBeginCount == 1) != (newEndCount == 1) ||
		(legacyBeginCount == 1) != (legacyEndCount == 1):
		return 0, 0, errors.New("managed settings block uses mismatched markers")
	}
	beginMarker, endMarker := managedBegin, managedEnd
	if legacyBeginCount == 1 {
		beginMarker, endMarker = legacyManagedBegin, legacyManagedEnd
	}
	bIdx := strings.Index(content, beginMarker)
	eIdx := strings.Index(content, endMarker)
	if eIdx < bIdx {
		return 0, 0, errors.New("managed settings end marker appears before begin marker")
	}
	if strings.Contains(content[bIdx+len(beginMarker):eIdx], beginMarker) {
		return 0, 0, errors.New("managed settings blocks are nested")
	}
	// Expand to full lines so removal never leaves stray separators behind.
	lineStart := func(pos int) int {
		if i := strings.LastIndexByte(content[:pos], '\n'); i >= 0 {
			return i + 1
		}
		return 0
	}
	start = lineStart(bIdx)
	end = eIdx + len(endMarker)
	// Extend past the end marker's newline, if present.
	if nl := strings.IndexByte(content[end:], '\n'); nl >= 0 {
		end += nl + 1
	}
	return start, end, nil
}

// ReadYtDlpConfig inspects the yt-dlp configuration file without modifying
// it. Problems are reported through Status.Problem instead of failing hard so
// the UI can always show the configured path.
func ReadYtDlpConfig(path string) (*YtDlpStatus, *YtDlpSettings, error) {
	st := &YtDlpStatus{Path: path}
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil, nil
		}
		st.Problem = err.Error()
		return st, nil, nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		st.Symlink = true
		st.Problem = "configuration path is a symlink; refusing to manage it"
		return st, nil, nil
	}
	if !fi.Mode().IsRegular() {
		st.Problem = "configuration path is not a regular file"
		return st, nil, nil
	}
	st.Exists = true
	st.SizeBytes = fi.Size()

	data, err := os.ReadFile(path)
	if err != nil {
		st.Problem = "unreadable configuration file"
		return st, nil, nil
	}
	if len(data) > MaxYtDlpConfigBytes {
		st.Problem = "configuration file exceeds 1 MiB"
		return st, nil, nil
	}
	if err := checkPrintable(data); err != nil {
		st.Problem = err.Error()
		return st, nil, nil
	}
	content := string(data)
	start, end, err := locateManagedBlock(content)
	if err != nil {
		st.Problem = err.Error()
		return st, nil, nil
	}
	if start < 0 {
		return st, nil, nil
	}
	st.HasBlock = true
	block := content[start:end]
	beginMarker, endMarker := managedBegin, managedEnd
	if strings.Contains(block, legacyManagedBegin) {
		beginMarker, endMarker = legacyManagedBegin, legacyManagedEnd
	}
	body := strings.TrimPrefix(block, beginMarker)
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimSuffix(body, endMarker)
	body = strings.TrimSuffix(body, "\n")
	settings, perr := parseManagedLines(body)
	if perr != nil {
		st.Problem = "managed block unreadable: " + perr.Error()
		return st, nil, nil
	}
	if err := ValidateYtDlpSettings(settings); err != nil {
		st.Problem = "managed block invalid: " + err.Error()
		return st, nil, nil
	}
	return st, settings, nil
}

func checkPrintable(data []byte) error {
	for _, b := range data {
		if b == 0 || (b < 0x20 && b != '\n' && b != '\t' && b != '\r') || b == 0x7f {
			return errors.New("configuration file contains NUL or control characters")
		}
	}
	return nil
}

// writeMu serializes managed-block writes and removals within this process
// so two concurrent structured edits can never interleave their
// read-modify-write cycles and lose one update.
var writeMu sync.Mutex

// WriteYtDlpManagedBlock splices the rendered block into the yt-dlp config,
// preserving every byte outside the markers. Missing parent directories are
// created (the container ships an empty /config). Writes go through a temp
// file + fsync + chmod 0600 + rename, and one bounded backup is kept when a
// real change occurred. The path is checked for symlinks/non-regular files
// immediately before use; a path swapped between check and read remains a
// same-user race that this process cannot prevent portably, and the temp +
// rename below never follows symlinks when writing.
func WriteYtDlpManagedBlock(path string, s *YtDlpSettings) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := ValidateYtDlpSettings(s); err != nil {
		return err
	}
	newBlock := renderManagedBlock(s)

	dir := filepath.Dir(path)
	fi, err := os.Lstat(path)
	var content string
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		return errors.New("refusing to edit: configuration path is a symlink")
	case err == nil && !fi.Mode().IsRegular():
		return errors.New("refusing to edit: configuration path is not a regular file")
	case err == nil:
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read config: %w", rerr)
		}
		if len(data) > MaxYtDlpConfigBytes {
			return errors.New("refusing to edit: configuration file exceeds 1 MiB")
		}
		if cerr := checkPrintable(data); cerr != nil {
			return fmt.Errorf("refusing to edit: %w", cerr)
		}
		content = string(data)
	case errors.Is(err, os.ErrNotExist):
		content = ""
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return mkErr
		}
	default:
		return err
	}

	updated, err := spliceBlock(content, newBlock)
	if err != nil {
		return err
	}
	if updated == content {
		return nil // nothing changed: no backup, no write
	}

	if fi != nil {
		// One bounded backup, overwritten on each real change. Written
		// through a temp file + rename so a symlink planted at path+".bak"
		// can never redirect the write to another file. A failed backup must
		// not block the edit itself.
		_ = atomicWriteSameDir(path+".bak", []byte(content))
	}

	tmp, err := os.CreateTemp(dir, ".ytdlp-config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(updated); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncParentDir(path)
}

// spliceBlock replaces an existing block or appends the rendered block,
// preserving all outside bytes.
func spliceBlock(content, newBlock string) (string, error) {
	start, end, err := locateManagedBlock(content)
	switch {
	case err != nil:
		return "", err
	case start < 0:
		if content == "" {
			return newBlock, nil
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if !strings.HasSuffix(content, "\n\n") {
			newBlock = "\n" + newBlock
		}
		return content + newBlock, nil
	default:
		before := content[:start]
		after := content[end:]
		// Keep the exact original separators: the block itself carries its
		// own trailing newline.
		out := before + newBlock + after
		return out, nil
	}
}

// RemoveYtDlpManagedBlock deletes the delimited block while keeping the rest
// of the file intact. Removing a missing block is a successful no-op. Unlike
// the write path, no backup is kept: removal is itself the recovery path for
// an unwanted block, and the pre-block content stays in the file's history
// through any external backups.
func RemoveYtDlpManagedBlock(path string) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to edit: configuration path is a symlink")
	}
	if !fi.Mode().IsRegular() {
		return errors.New("refusing to edit: configuration path is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > MaxYtDlpConfigBytes {
		return errors.New("refusing to edit: configuration file exceeds 1 MiB")
	}
	if err := checkPrintable(data); err != nil {
		return err
	}
	start, end, err := locateManagedBlock(string(data))
	if err != nil {
		return err
	}
	if start < 0 {
		return nil
	}
	updated := string(data)[:start] + string(data)[end:]
	if updated == string(data) {
		return nil
	}
	return atomicWriteSameDir(path, []byte(updated))
}

func atomicWriteSameDir(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ytdlp-config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncParentDir(path)
}
