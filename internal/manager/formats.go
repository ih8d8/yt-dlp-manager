package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"yt-dlp-manager/internal/ipc"
)

// Format is one selectable stream of a video, as reported by yt-dlp. Every
// string in it originates on a remote page and has been sanitized and length
// bounded before it gets here.
type Format struct {
	ID         string  `json:"format_id"`
	Ext        string  `json:"ext,omitempty"`
	Resolution string  `json:"resolution,omitempty"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	VCodec     string  `json:"vcodec,omitempty"`
	ACodec     string  `json:"acodec,omitempty"`
	// Filesize is bytes; Approximate says whether it came from
	// filesize_approx rather than a known filesize. Streaming formats report
	// only the estimate, and presenting an estimate as fact is what made the
	// old size display untrustworthy in the first place.
	Filesize    int64   `json:"filesize,omitempty"`
	Approximate bool    `json:"filesize_approximate,omitempty"`
	TBR         float64 `json:"tbr,omitempty"`
	Note        string  `json:"format_note,omitempty"`
	Protocol    string  `json:"protocol,omitempty"`
	Language    string  `json:"language,omitempty"`
	HasVideo    bool    `json:"has_video"`
	HasAudio    bool    `json:"has_audio"`
}

// FormatProbe is what one URL offers: enough metadata to title the picker,
// plus the formats themselves.
type FormatProbe struct {
	Title           string   `json:"title,omitempty"`
	DurationSeconds float64  `json:"duration_seconds,omitempty"`
	Extractor       string   `json:"extractor,omitempty"`
	Formats         []Format `json:"formats"`
	// Truncated reports that the extractor offered more formats than
	// MaxFormats, so the list is the best ones rather than all of them.
	Truncated bool `json:"truncated,omitempty"`
}

// ErrFormatsUnsupported is returned when the configured runner cannot probe
// formats (every fake runner in the tests, for instance).
var ErrFormatsUnsupported = errors.New("this runner cannot list formats")

const (
	// maxConcurrentFormatProbes bounds how many extractor round trips one
	// impatient picker can start at once. Unlike a download, a format probe is
	// triggered directly by a UI interaction, so without a ceiling a handful
	// of dialog opens would each spawn a yt-dlp process.
	maxConcurrentFormatProbes = 3
	// formatCacheTTL is short on purpose: format tables carry signed URLs and
	// go stale, and the only thing being optimised is reopening the picker for
	// the same link a moment later.
	formatCacheTTL = 2 * time.Minute
	// maxFormatCacheEntries bounds the cache; entries are evicted oldest-first.
	maxFormatCacheEntries = 64
)

type formatCacheEntry struct {
	probe *FormatProbe
	at    time.Time
}

// Formats lists the formats a URL offers, subject to a concurrency ceiling and
// a short cache. It reports ErrFormatsUnsupported when the configured runner
// has no format probe at all.
func (m *Manager) Formats(ctx context.Context, url string) (*FormatProbe, error) {
	url = strings.TrimSpace(url)
	if !ValidURL(url) {
		return nil, errors.New("invalid url")
	}
	lister, ok := m.runner.(FormatLister)
	if !ok {
		return nil, ErrFormatsUnsupported
	}
	if m.ctx.Err() != nil {
		return nil, ErrShuttingDown
	}
	// The cache is keyed by URL AND by the arguments the probe will use, so a
	// result produced under a different proxy, header set or extractor
	// configuration can never be served for a different one. Keying on the URL
	// alone and clearing on change is not enough: a probe already in flight
	// finishes afterwards and would insert its stale answer.
	m.mu.Lock()
	globalArgs := append([]string(nil), m.extraArgs...)
	m.mu.Unlock()
	probeSafe := ipc.ProbeSafeArgs(globalArgs)
	key := formatCacheKey(url, probeSafe)

	if p := m.cachedFormats(key); p != nil {
		return p, nil
	}

	// The deadline covers queueing too. Started after the semaphore, it would
	// bound only the yt-dlp run, leaving the wait behind other probes
	// unbounded — which is exactly when a caller is most likely to give up.
	probeCtx, cancel := context.WithTimeout(ctx, FormatProbeTimeout)
	defer cancel()

	select {
	case m.fmtSem <- struct{}{}:
		defer func() { <-m.fmtSem }()
	case <-probeCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("timed out waiting for a free format probe slot")
	}
	// Another caller may have filled the cache while this one waited.
	if p := m.cachedFormats(key); p != nil {
		return p, nil
	}

	probe, err := lister.Formats(probeCtx, Job{URL: url, GlobalArgs: globalArgs})
	if err != nil {
		if probeCtx.Err() != nil && ctx.Err() == nil {
			return nil, errors.New("timed out reading the available formats")
		}
		// yt-dlp's stderr is remote-influenced text on its way to a response
		// body, so it is sanitized and bounded here rather than at each
		// caller — the same treatment a failed download's error tail gets.
		return nil, errors.New(truncate(sanitize(strings.TrimSpace(err.Error())), 300))
	}
	m.storeFormats(key, probe)
	return probe, nil
}

// formatCacheKey binds a cached table to the exact arguments that produced it.
func formatCacheKey(url string, probeArgs []string) string {
	return url + "\x00" + strings.Join(probeArgs, "\x00")
}

func (m *Manager) cachedFormats(key string) *FormatProbe {
	m.fmtMu.Lock()
	defer m.fmtMu.Unlock()
	e, ok := m.fmtCache[key]
	if !ok || time.Since(e.at) > formatCacheTTL {
		return nil
	}
	return e.probe
}

func (m *Manager) storeFormats(key string, probe *FormatProbe) {
	m.fmtMu.Lock()
	defer m.fmtMu.Unlock()
	if m.fmtCache == nil {
		m.fmtCache = make(map[string]formatCacheEntry)
	}
	for len(m.fmtCache) >= maxFormatCacheEntries {
		var oldestURL string
		var oldest time.Time
		for k, v := range m.fmtCache {
			if oldestURL == "" || v.at.Before(oldest) {
				oldestURL, oldest = k, v.at
			}
		}
		delete(m.fmtCache, oldestURL)
	}
	m.fmtCache[key] = formatCacheEntry{probe: probe, at: time.Now()}
}

const (
	maxFormatFieldLen = 120
	maxProbeTitleLen  = 300
)

// formatMetaTemplate and formatListTemplate are both "j" conversions, so each
// arrives as exactly one JSON value on one line no matter what a remote page
// puts in a title or a format note.
const (
	formatMetaTemplate = `@m|%(.{title,duration,extractor_key})j`
	formatListTemplate = `@F|%(formats.:.{format_id,ext,resolution,width,height,fps,vcodec,acodec,filesize,filesize_approx,tbr,format_note,protocol,language})j`
)

// Formats enumerates the formats a single URL offers.
//
// It is bounded the same way the playlist probe is — process group kill on
// context cancellation, capped stdout, capped stderr — and additionally
// refuses to walk a playlist: --no-playlist plus --playlist-items 1 means a
// link to a 5000-video playlist costs one extractor round trip, not 5000.
func (YtdlpRunner) Formats(ctx context.Context, job Job) (*FormatProbe, error) {
	url := job.URL
	args := []string{
		"--quiet", "--no-warnings", "--color", "no_color",
		"--skip-download", "--no-playlist", "--playlist-items", "1",
		"--print", formatMetaTemplate,
		"--print", formatListTemplate,
	}
	// The picker talks to the same site the download will, so it needs the
	// same proxy, headers, impersonation and extractor arguments — and only
	// those. See probeArgs.
	args = append(args, probeArgs(job)...)
	args = append(args, "--", url)
	cmd := exec.Command("yt-dlp", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr cappedBuffer
	stdout.max = maxFormatStdout
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
		return nil, errors.New("yt-dlp format probe produced too much output")
	}
	return parseFormatOutput(stdout.String())
}

type rawFormat struct {
	ID             string   `json:"format_id"`
	Ext            string   `json:"ext"`
	Resolution     string   `json:"resolution"`
	Width          *float64 `json:"width"`
	Height         *float64 `json:"height"`
	FPS            *float64 `json:"fps"`
	VCodec         string   `json:"vcodec"`
	ACodec         string   `json:"acodec"`
	Filesize       *float64 `json:"filesize"`
	FilesizeApprox *float64 `json:"filesize_approx"`
	TBR            *float64 `json:"tbr"`
	Note           string   `json:"format_note"`
	Protocol       string   `json:"protocol"`
	Language       string   `json:"language"`
}

type rawMeta struct {
	Title     string   `json:"title"`
	Duration  *float64 `json:"duration"`
	Extractor string   `json:"extractor_key"`
}

func parseFormatOutput(out string) (*FormatProbe, error) {
	probe := &FormatProbe{Formats: []Format{}}
	seenList := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "@m|") && probe.Title == "":
			var m rawMeta
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "@m|")), &m); err != nil {
				continue
			}
			if t := strings.TrimSpace(m.Title); !naValue(t) {
				probe.Title = truncate(sanitize(t), maxProbeTitleLen)
			}
			if m.Duration != nil {
				probe.DurationSeconds = *m.Duration
			}
			probe.Extractor = truncate(sanitize(strings.TrimSpace(m.Extractor)), maxFormatFieldLen)
		case strings.HasPrefix(line, "@F|") && !seenList:
			var raws []rawFormat
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "@F|")), &raws); err != nil {
				continue
			}
			// Only the first entry's format table is used: --playlist-items 1
			// should mean one, and a second table would silently mix two
			// videos' format ids into one picker.
			seenList = true
			probe.Formats, probe.Truncated = convertFormats(raws)
		}
	}
	if !seenList {
		return nil, errors.New("yt-dlp reported no formats for this URL")
	}
	return probe, nil
}

func convertFormats(raws []rawFormat) ([]Format, bool) {
	out := make([]Format, 0, len(raws))
	for _, r := range raws {
		id := strings.TrimSpace(r.ID)
		// A format whose id would be rejected on the way back in is not
		// offerable, so it is never offered. This keeps the picker's output
		// and the add endpoint's input describing the same closed set.
		if naValue(id) || !ipc.ValidFormatID(id) {
			continue
		}
		f := Format{
			ID:         id,
			Ext:        cleanField(r.Ext),
			Resolution: cleanField(r.Resolution),
			VCodec:     cleanField(r.VCodec),
			ACodec:     cleanField(r.ACodec),
			Note:       cleanField(r.Note),
			Protocol:   cleanField(r.Protocol),
			Language:   cleanField(r.Language),
		}
		f.Width = intField(r.Width)
		f.Height = intField(r.Height)
		f.FPS = floatField(r.FPS)
		f.TBR = floatField(r.TBR)
		switch {
		case r.Filesize != nil && *r.Filesize > 0:
			f.Filesize = int64(*r.Filesize)
		case r.FilesizeApprox != nil && *r.FilesizeApprox > 0:
			f.Filesize = int64(*r.FilesizeApprox)
			f.Approximate = true
		}
		f.HasVideo = f.VCodec != "" && f.VCodec != "none"
		f.HasAudio = f.ACodec != "" && f.ACodec != "none"
		// Storyboards are neither: they are contact sheets yt-dlp exposes as
		// formats, and offering them as a download choice is never useful.
		if !f.HasVideo && !f.HasAudio {
			continue
		}
		if f.Protocol == "mhtml" {
			continue
		}
		out = append(out, f)
	}
	sortFormats(out)
	// Truncate within each category, not across the sorted whole. Video
	// formats sort first, so a plain cut at MaxFormats would hand back a
	// picker with no audio at all for an extractor that exposes hundreds of
	// video variants — the one thing a merge always needs.
	var video, audio []Format
	for _, f := range out {
		if f.HasVideo {
			video = append(video, f)
		} else {
			audio = append(audio, f)
		}
	}
	truncated := false
	half := MaxFormats / 2
	if len(video) > MaxFormats-min(len(audio), half) {
		video = video[:MaxFormats-min(len(audio), half)]
		truncated = true
	}
	if len(audio) > MaxFormats-len(video) {
		audio = audio[:MaxFormats-len(video)]
		truncated = true
	}
	return append(video, audio...), truncated
}

// sortFormats puts the most useful choice first: video formats by descending
// resolution then bitrate, then audio-only formats by descending bitrate.
// yt-dlp's own order is worst-first, which is the wrong end for a picker.
func sortFormats(fs []Format) {
	rank := func(f Format) int {
		if f.HasVideo {
			return 0
		}
		return 1
	}
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		if a.FPS != b.FPS {
			return a.FPS > b.FPS
		}
		if a.TBR != b.TBR {
			return a.TBR > b.TBR
		}
		return a.ID < b.ID
	})
}

func cleanField(s string) string {
	s = strings.TrimSpace(s)
	if naValue(s) {
		return ""
	}
	return truncate(sanitize(s), maxFormatFieldLen)
}

func intField(v *float64) int {
	if v == nil || *v <= 0 || *v > 1e6 {
		return 0
	}
	return int(*v)
}

func floatField(v *float64) float64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}
