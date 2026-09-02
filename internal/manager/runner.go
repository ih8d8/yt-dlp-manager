package manager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"yt-dlp-manager/internal/ipc"
)

type Entry struct {
	URL       string
	Title     string
	Thumbnail string
}

// Job is one download request: the URL plus whatever per-download overrides
// were chosen when it was added. It is a struct rather than a widening
// parameter list so later options do not churn every implementation again.
type Job struct {
	URL     string
	Options ipc.Options
	// GlobalArgs and ExtraArgs are already-parsed, already-validated free-form
	// arguments: the ones from Settings that apply to every download, and this
	// download's own. They are kept apart because they sit on opposite sides
	// of the picker in the precedence order — see jobArgs.
	GlobalArgs []string
	ExtraArgs  []string
}

// jobArgs renders the option portion of one download's command line, in
// precedence order — later wins, because that is how yt-dlp resolves an option
// given twice:
//
//  1. the user's yt-dlp config file (not here; yt-dlp reads it itself)
//  2. global extra arguments from Settings — defaults for every download
//  3. the picker's choices for this download — deliberately after the global
//     defaults, since asking for 720p on one item has to beat a blanket
//     "--format best" typed once in Settings
//  4. this download's own extra arguments — the last word on everything
//
// probeArgs renders the arguments a metadata or format probe may inherit: the
// network- and identity-shaping subset of both the global and per-download
// extra arguments, and nothing from the structured picker options, which only
// describe what to download.
func probeArgs(job Job) []string {
	args := append([]string(nil), job.GlobalArgs...)
	args = append(args, job.ExtraArgs...)
	return ipc.ProbeSafeArgs(args)
}

func jobArgs(job Job) []string {
	args := append([]string(nil), job.GlobalArgs...)
	args = append(args, OptionArgs(job.Options)...)
	return append(args, job.ExtraArgs...)
}

type Runner interface {
	Probe(ctx context.Context, job Job) ([]Entry, error)
	Run(ctx context.Context, job Job, onLine func(string)) (tail string, err error)
}

// FormatLister is an optional capability: a runner that can enumerate the
// formats a URL offers. It is deliberately not part of Runner so the many
// fake runners in tests are not forced to implement a probe they never use;
// callers type-assert and report the feature as unavailable otherwise.
type FormatLister interface {
	Formats(ctx context.Context, job Job) (*FormatProbe, error)
}

const (
	graceKillDelay = 3 * time.Second
	maxLineLen     = 1 << 20
	stderrTailSize = 30
	maxProbeStdout = 64 << 20
	maxProbeStderr = 1 << 20
	// maxFormatStdout bounds a format probe separately from a playlist probe:
	// one video's format table is orders of magnitude smaller than a
	// playlist's entry list, and this output is parsed into a response body.
	maxFormatStdout = 8 << 20
	// MaxFormats bounds how many formats one probe reports. A picker is
	// unusable past a few dozen entries and the cap keeps an extractor with a
	// pathological format table from filling a response.
	MaxFormats = 400
	// FormatProbeTimeout bounds a format probe END TO END, including the wait
	// for a free slot. It is deliberately shorter than the HTTP server's write
	// timeout: a probe allowed to outlive that would have its documented
	// timeout response discarded, and the caller would see a dropped
	// connection instead of an explanation.
	FormatProbeTimeout = 20 * time.Second
	// MaxPlaylistEntries bounds how many entries one playlist probe can turn
	// into. The 64 MiB stdout cap alone is not a bound on memory or on the
	// queue: at ~200 bytes per flat-playlist line it still permits hundreds of
	// thousands of entries, each of which would become a live item, an SSE
	// event and a row in the persisted snapshot. Parsing stops here instead.
	MaxPlaylistEntries = 5000
)

// cappedBuffer absorbs an unbounded writer (e.g. a runaway yt-dlp) while
// retaining at most max bytes. Overflowing output is discarded but the pipe
// keeps being drained, so the child never blocks on a full pipe.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.truncated = true
			p = p[:room]
		}
		b.buf.Write(p)
	} else if len(p) > 0 {
		b.truncated = true
	}
	return n, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

type YtdlpRunner struct{}

func (YtdlpRunner) Probe(ctx context.Context, job Job) ([]Entry, error) {
	url := job.URL
	args := []string{
		"--quiet", "--no-warnings", "--color", "no_color",
		"--flat-playlist",
		// One JSON object per entry. The previous "@e|%(url)s|%(title)s|..."
		// form was ambiguous in two ways an untrusted page controls: a title
		// containing "|" shifted every later field, and a title containing a
		// newline forged an entire extra entry — with an attacker-chosen URL.
		"--print", "@e|" + entryFieldsTemplate,
	}
	// A probe talks to the same site as the download, so it inherits how to
	// reach it — but only that. Options that select or limit what gets
	// downloaded change a probe's exit status instead of helping it
	// (--max-downloads makes yt-dlp exit 101 after printing), so they are
	// filtered out rather than forwarded wholesale.
	args = append(args, probeArgs(job)...)
	args = append(args, "--", url)
	cmd := exec.Command("yt-dlp", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr cappedBuffer
	stdout.max = maxProbeStdout
	stderr.max = maxProbeStderr
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			t := time.AfterFunc(graceKillDelay, func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			})
			defer t.Stop()
			<-done
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		return nil, errors.New(stderrTail(stderr.String()))
	}
	if stdout.truncated {
		return nil, errors.New("yt-dlp playlist probe output exceeded 64 MiB")
	}

	return parseProbeOutput(stdout.String(), url), nil
}

// entryFieldsTemplate asks yt-dlp for one JSON object per playlist entry. The
// "j" conversion escapes every byte that could otherwise break the line
// protocol, which matters because all three values originate on a remote page.
const entryFieldsTemplate = `%(.{url,title,thumbnail})j`

type probeEntry struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Thumbnail string `json:"thumbnail"`
}

// naValue reports whether a field is one of yt-dlp's "missing" placeholders.
func naValue(s string) bool {
	switch s {
	case "", "NA", "None", "null":
		return true
	}
	return false
}

func parseProbeOutput(out, original string) []Entry {
	var entries []Entry
	var fallbackTitle, fallbackThumb string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "@e|") {
			continue
		}
		var pe probeEntry
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "@e|")), &pe); err != nil {
			// Not our framing (or a yt-dlp too old for the "j" conversion).
			// Dropping the line is the safe direction: a half-parsed entry
			// would carry an attacker-influenced URL into the queue.
			continue
		}
		e := Entry{URL: strings.TrimSpace(pe.URL)}
		if thumb := strings.TrimSpace(pe.Thumbnail); !naValue(thumb) {
			e.Thumbnail = thumb
		}
		if t := strings.TrimSpace(pe.Title); !naValue(t) {
			e.Title = t
			if fallbackTitle == "" {
				fallbackTitle = t
			}
		} else {
			e.Title = fallbackTitle
		}
		if e.Thumbnail != "" && fallbackThumb == "" {
			fallbackThumb = e.Thumbnail
		}
		if naValue(e.URL) {
			continue
		}
		entries = append(entries, e)
		if len(entries) >= MaxPlaylistEntries {
			break
		}
	}
	if len(entries) == 0 {
		return []Entry{{URL: original, Title: fallbackTitle, Thumbnail: fallbackThumb}}
	}
	return entries
}

func (YtdlpRunner) Run(ctx context.Context, job Job, onLine func(string)) (string, error) {
	args := []string{
		"--quiet", "--no-warnings", "--color", "no_color",
		"--progress", "--newline", "--progress-delta", "0.1",
		// total_bytes is present for plain HTTP/generic downloads while
		// total_bytes_estimate covers streaming formats; the comma syntax
		// makes yt-dlp fall back to the second when the first is NA.
		//
		// status and format_id are what make the byte counters add up. One
		// item is routinely several downloads (a video stream, then an audio
		// stream, then subtitles), and each one restarts downloaded_bytes at
		// zero with its own total — so the last stream's numbers used to be
		// the only ones left on the item: "7.2 MB / 7.2 MB" for a 134 MB
		// file. The extra fields let the reader tell one stream's end from
		// the next stream's start and accumulate instead of overwrite.
		// format_id is JSON-encoded and placed last: it comes from a remote
		// extractor, so it must not be able to shift the fields before it.
		"--progress-template", "download:@p|%(progress.downloaded_bytes)s|%(progress.total_bytes,progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s|%(progress._percent_str)s|%(progress.status)s|%(info.format_id)j",
		// JSON-encoded ("j"), not raw. %(title)s is remote-controlled text on
		// the SAME stdout stream that carries the "@f|"/"@g|" file records, so
		// a title containing a newline used to emit a second protocol line of
		// the page's choosing — naming any path on disk, which cleanup would
		// then act on.
		"--print", "before_dl:@t|%(title)j",
		"--print", "before_dl:@f|%(filename)j",
		// The combined size of every stream this download will fetch, known
		// before the first byte. Without it the percentage can only be
		// measured against the stream in flight, so finishing the video and
		// starting the audio made the bar fall backwards. filesize is exact
		// where an extractor provides it; filesize_approx is the sum of the
		// requested formats' estimates and is what YouTube actually reports.
		"--print", "before_dl:@n|%(filesize,filesize_approx)s",
		"--print", "after_move:@g|%(filepath)j",
	}
	// Everything here goes on the command line, which yt-dlp gives priority
	// over its own configuration file — so what was chosen for this item beats
	// the defaults, and anything nobody chose still comes from the config.
	args = append(args, jobArgs(job)...)
	args = append(args, "--", job.URL)
	cmd := exec.Command("yt-dlp", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	procDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			t := time.AfterFunc(graceKillDelay, func() {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			})
			defer t.Stop()
			<-procDone
		case <-procDone:
		}
	}()

	var (
		wg        sync.WaitGroup
		errTailMu sync.Mutex
		ring      []string
	)
	wg.Add(2)
	go scanLines(stdout, onLine, &wg)
	go collectStderr(stderr, &ring, &errTailMu, &wg)

	waitErr := cmd.Wait()
	close(procDone)
	wg.Wait()

	if ctx.Err() != nil {
		errTailMu.Lock()
		tail := strings.Join(ring, "\n")
		errTailMu.Unlock()
		return tail, ctx.Err()
	}
	errTailMu.Lock()
	tail := strings.Join(ring, "\n")
	errTailMu.Unlock()

	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok && ee.Exited() {
			return tail, errors.New("yt-dlp exited with code " + strconv.Itoa(ee.ExitCode()))
		}
		return tail, waitErr
	}
	return tail, nil
}

func scanLines(r io.Reader, onLine func(string), wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, oversized, err := readBoundedProcessLine(br, maxLineLen)
		if !oversized && line != nil {
			onLine(string(line))
		}
		if err != nil {
			return
		}
	}
}

func collectStderr(r io.Reader, ring *[]string, mu *sync.Mutex, wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		lineBytes, oversized, err := readBoundedProcessLine(br, maxLineLen)
		if !oversized && lineBytes != nil {
			line := strings.TrimSpace(string(lineBytes))
			if line != "" {
				mu.Lock()
				*ring = append(*ring, line)
				if len(*ring) > stderrTailSize {
					*ring = (*ring)[len(*ring)-stderrTailSize:]
				}
				mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// readBoundedProcessLine uses ReadSlice rather than ReadBytes. ReadBytes
// allocates the entire line before returning, making the old post-read size
// check ineffective against a child that emitted a line of arbitrary size.
// Oversized lines are drained without retaining their remainder.
func readBoundedProcessLine(br *bufio.Reader, limit int) ([]byte, bool, error) {
	var line []byte
	oversized := false
	for {
		fragment, err := br.ReadSlice('\n')
		if !oversized {
			if len(line)+len(fragment) > limit+2 { // room for optional CRLF
				line = nil
				oversized = true
			} else {
				line = append(line, fragment...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, oversized, err
		}
		if errors.Is(err, io.EOF) && len(fragment) == 0 && len(line) == 0 && !oversized {
			return nil, false, io.EOF
		}
		if !oversized {
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(line) > limit {
				line = nil
				oversized = true
			}
		}
		return line, oversized, err
	}
}

func stderrTail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > stderrTailSize {
		lines = lines[len(lines)-stderrTailSize:]
	}
	return strings.Join(lines, "\n")
}

func ParseNum(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NA" || s == "None" {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return n
	}
	f, ferr := strconv.ParseFloat(s, 64)
	if ferr != nil || math.IsNaN(f) {
		return -1
	}
	if f >= math.MaxInt64 {
		return math.MaxInt64
	}
	if f <= math.MinInt64 {
		return math.MinInt64
	}
	return int64(f)
}

func isSafeBase(p string) bool {
	if p == "" {
		return false
	}
	c := filepath.Clean(p)
	if c == "." || c == ".." || c == string(filepath.Separator) {
		return false
	}
	if strings.HasPrefix(c, "-") {
		return false
	}
	for _, seg := range strings.Split(c, string(filepath.Separator)) {
		if seg == ".." {
			return false
		}
	}
	return true
}

// cleanupPartials removes yt-dlp's own scratch for a recorded output and
// deliberately leaves the finished file itself untouched: removing a row must
// never destroy media, even if the row is removed during post-processing.
func cleanupPartials(files []string) error {
	var errs []error
	for _, base := range files {
		if !isSafeBase(base) {
			continue
		}
		for _, path := range []string{base + ".part", base + ".ytdl"} {
			if err := removeIfExists(path); err != nil {
				errs = append(errs, err)
			}
		}
		if err := removeScratchSiblings(base); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// scratchSuffix reports whether a filename is unambiguously yt-dlp working
// state rather than media worth keeping.
func scratchSuffix(name string) bool {
	return strings.HasSuffix(name, ".part") ||
		strings.HasSuffix(name, ".ytdl") ||
		strings.Contains(name, ".part-")
}

// formatIntermediate reports whether name is yt-dlp's per-format working file
// for the output whose filename (without its extension) is stem — that is
// "<stem>.f<digits>.<ext>" with a scratch suffix.
//
// This matters because yt-dlp reports only the FINAL merged name through
// --print (%(filepath)s is still NA at before_dl), while a video+audio
// download actually writes "<stem>.f137.mp4.part" and friends. Matching only
// "<base>.part" therefore missed every leftover from a cancelled merge, which
// is how partial files accumulated in the download directory.
func formatIntermediate(name, stem string) bool {
	rest, ok := strings.CutPrefix(name, stem+".f")
	if !ok || !scratchSuffix(name) {
		return false
	}
	// Require at least one digit, and only digits, up to the next separator —
	// so a user's own "<stem>.final.part" is never mistaken for one of ours.
	digits := rest
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		digits = rest[:i]
	}
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// removeScratchSiblings deletes the working files yt-dlp writes next to an
// output: "<base>.part-*" fragments (HLS/DASH) and "<stem>.f<id>.*" per-format
// intermediates. filepath.Glob is deliberately avoided: titles routinely
// contain glob metacharacters ("video [1080p].mp4"), which would both miss the
// item's own files (or error with ErrBadPattern) and match unrelated siblings,
// deleting other downloads' data. Literal prefix matching is exact and safe.
func removeScratchSiblings(base string) error {
	dir := filepath.Dir(base)
	name := filepath.Base(base)
	fragPrefix := name + ".part-"
	stem := strings.TrimSuffix(name, filepath.Ext(name))

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read scratch directory %q: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if !strings.HasPrefix(n, fragPrefix) && !formatIntermediate(n, stem) {
			continue
		}
		if err := removeIfExists(filepath.Join(dir, n)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove %q: %w", path, err)
}
