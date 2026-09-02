package manager

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yt-dlp-manager/internal/ipc"
)

func TestOptionArgs(t *testing.T) {
	cases := []struct {
		name string
		opts ipc.Options
		want []string
	}{
		{
			name: "no options means no arguments at all",
			opts: ipc.Options{},
			want: nil,
		},
		{
			name: "explicit streams get no fallback that could substitute other content",
			opts: ipc.Options{FormatID: "137", AudioFormatID: "140", MergeContainer: "mp4"},
			want: []string{
				"--format", "137+140",
				"--merge-output-format", "mp4",
			},
		},
		{
			name: "a preset is a ceiling, not an exact match",
			opts: ipc.Options{Preset: "1080p"},
			want: []string{"--format",
				"bv*[height<=1080]+ba/b[height<=1080]/wv*[height<=1080]+ba/w[height<=1080]"},
		},
		{
			name: "audio only asks for extraction, never a merge",
			opts: ipc.Options{AudioOnly: true, AudioFormat: "mp3"},
			want: []string{"--format", "ba/b", "--extract-audio", "--audio-format", "mp3"},
		},
		{
			name: "subtitles off must say so, not stay silent",
			opts: ipc.Options{Subtitles: "off"},
			want: []string{"--no-write-subs", "--no-write-auto-subs"},
		},
		{
			name: "requested subtitle languages are joined for --sub-langs",
			opts: ipc.Options{Subtitles: "on", SubLangs: []string{"en", "de"}},
			want: []string{"--write-subs", "--sub-langs", "en,de"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := OptionArgs(tc.opts)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("args = %q\nwant   %q", got, tc.want)
			}
		})
	}
}

// TestOptionArgsOnlyEmitsRealYtDlpFlags runs the flags this package can
// produce past the installed yt-dlp's own help text.
//
// It exists because a live run failed with "no such option:
// --no-extract-audio": -x has no negation, and nothing in the unit tests
// could tell, since they only compared strings against other strings.
func TestOptionArgsOnlyEmitsRealYtDlpFlags(t *testing.T) {
	help, err := exec.Command("yt-dlp", "--help").Output()
	if err != nil {
		t.Skipf("yt-dlp not available: %v", err)
	}
	text := string(help)
	// Every option any option set can produce.
	all := []ipc.Options{
		{FormatID: "137", AudioFormatID: "140", MergeContainer: "mp4"},
		{Preset: "best"},
		{AudioOnly: true, AudioFormat: "mp3"},
		{Subtitles: "on", SubLangs: []string{"en"}},
		{Subtitles: "off"},
	}
	for _, o := range all {
		for _, arg := range OptionArgs(o) {
			if !strings.HasPrefix(arg, "--") {
				continue // a value, not a flag
			}
			if !strings.Contains(text, arg) {
				t.Errorf("OptionArgs emits %q, which this yt-dlp does not accept", arg)
			}
		}
	}
}

// TestOptionsValidateRejectsInjection is the security case for the whole
// feature: format ids reach a command line, so nothing outside the id
// alphabet may pass validation, whatever it would mean to yt-dlp.
func TestOptionsValidateRejectsInjection(t *testing.T) {
	bad := []ipc.Options{
		{FormatID: "137 --exec"},
		{FormatID: "137;id"},
		{FormatID: "bv*+ba"},
		{AudioFormatID: "140/../../etc/passwd"},
		{FormatID: "137\nrm"},
		{Preset: "1080p", FormatID: "137"},
		{Preset: "4k"},
		{MergeContainer: "exe"},
		{AudioOnly: true, AudioFormat: "sh"},
		{AudioFormat: "mp3"},
		{Subtitles: "maybe"},
		{Subtitles: "on", SubLangs: []string{"en;rm -rf /"}},
		{SubLangs: []string{"en"}},
	}
	for _, o := range bad {
		if err := o.Normalize().Validate(); err == nil {
			t.Errorf("Validate accepted %+v", o)
		}
	}
	good := []ipc.Options{
		{},
		{Preset: "best"},
		{FormatID: "137", AudioFormatID: "140", MergeContainer: "mkv"},
		{FormatID: "hls-1080", MergeContainer: "mp4"},
		{AudioOnly: true, AudioFormat: "opus"},
		{Subtitles: "on", SubLangs: []string{"en", "pt-BR"}},
		{Subtitles: "off"},
	}
	for _, o := range good {
		if err := o.Normalize().Validate(); err != nil {
			t.Errorf("Validate rejected %+v: %v", o, err)
		}
	}
}

func TestOptionsNormalizeAndEqual(t *testing.T) {
	a := ipc.Options{Preset: " 1080P ", MergeContainer: "MP4"}.Normalize()
	b := ipc.Options{Preset: "1080p", MergeContainer: "mp4"}.Normalize()
	if !a.Equal(b) {
		t.Errorf("%+v and %+v should be the same request", a, b)
	}
	if a.Key() != b.Key() {
		t.Errorf("keys differ: %q vs %q", a.Key(), b.Key())
	}
	if a.Equal(ipc.Options{Preset: "720p"}) {
		t.Error("different qualities compared equal")
	}
	// Duplicate languages collapse, so two spellings of one request do not
	// become two downloads.
	dupes := ipc.Options{Subtitles: "on", SubLangs: []string{"en", "en", " "}}.Normalize()
	if len(dupes.SubLangs) != 1 {
		t.Errorf("sub langs = %q, want deduplicated", dupes.SubLangs)
	}
}

func TestParseFormatOutput(t *testing.T) {
	out := `@m|{"title":"A video","duration":212.0,"extractor_key":"Youtube"}
@F|[{"format_id":"sb0","ext":"mhtml","vcodec":"none","acodec":"none","protocol":"mhtml","format_note":"storyboard"},` +
		`{"format_id":"140","ext":"m4a","resolution":"audio only","vcodec":"none","acodec":"mp4a.40.2","filesize":10271496,"tbr":129.4},` +
		`{"format_id":"137","ext":"mp4","resolution":"1920x1080","width":1920,"height":1080,"fps":30,"vcodec":"avc1","acodec":"none","filesize_approx":133000000},` +
		`{"format_id":"bad id","ext":"mp4","vcodec":"avc1","acodec":"none"}]
`
	probe, err := parseFormatOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Title != "A video" || probe.DurationSeconds != 212 {
		t.Errorf("meta = %q / %v", probe.Title, probe.DurationSeconds)
	}
	if len(probe.Formats) != 2 {
		t.Fatalf("formats = %+v, want the storyboard and the unusable id dropped", probe.Formats)
	}
	// Best first: the 1080p video, then the audio-only stream.
	if probe.Formats[0].ID != "137" || !probe.Formats[0].HasVideo || probe.Formats[0].HasAudio {
		t.Errorf("first format = %+v", probe.Formats[0])
	}
	if !probe.Formats[0].Approximate || probe.Formats[0].Filesize != 133000000 {
		t.Errorf("approximate size not flagged: %+v", probe.Formats[0])
	}
	if probe.Formats[1].ID != "140" || probe.Formats[1].HasVideo {
		t.Errorf("second format = %+v", probe.Formats[1])
	}
	if probe.Formats[1].Approximate {
		t.Error("a known filesize was reported as an estimate")
	}
}

// TestParseFormatOutputSanitizesRemoteText: format notes come from a remote
// page and are rendered in the picker, so escape sequences must not survive.
func TestParseFormatOutputSanitizesRemoteText(t *testing.T) {
	out := "@F|" + `[{"format_id":"137","ext":"mp4","vcodec":"avc1","acodec":"none",` +
		`"format_note":"1080p\u001b[31mred"}]` + "\n"
	probe, err := parseFormatOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(probe.Formats[0].Note, "\x1b") {
		t.Errorf("note kept an escape sequence: %q", probe.Formats[0].Note)
	}
}

func TestParseFormatOutputRejectsEmptyProbe(t *testing.T) {
	if _, err := parseFormatOutput("@m|{\"title\":\"x\"}\n"); err == nil {
		t.Error("a probe with no format table was accepted")
	}
}

// TestJobArgsPlaceExtraArgsLast: the operator's own text has to be the last
// thing yt-dlp sees, or a global rate limit could not be overridden per
// download and neither could anything the picker set.
func TestJobArgsPlaceExtraArgsLast(t *testing.T) {
	mgr, _ := newTestManager(t, 1, newFakeRunner())
	if err := mgr.SetExtraArgs("--limit-rate 1M"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetExtraArgs("--exec id"); err == nil {
		t.Fatal("the manager accepted a global --exec")
	}
	// The refused value must not have replaced the good one.
	mgr.mu.Lock()
	got := strings.Join(mgr.extraArgs, " ")
	mgr.mu.Unlock()
	if got != "--limit-rate 1M" {
		t.Errorf("global extra args = %q, want the previous value kept", got)
	}
}

// capturingRunner records the composed job so the argument order the manager
// hands to yt-dlp can be asserted directly.
type capturingRunner struct {
	mu   sync.Mutex
	jobs []Job
}

func (c *capturingRunner) Probe(ctx context.Context, job Job) ([]Entry, error) {
	url := job.URL
	return []Entry{{URL: url}}, nil
}

func (c *capturingRunner) Run(ctx context.Context, job Job, onLine func(string)) (string, error) {
	c.mu.Lock()
	c.jobs = append(c.jobs, job)
	c.mu.Unlock()
	return "", nil
}

func TestGlobalExtraArgsComeBeforePerDownloadOnes(t *testing.T) {
	runner := &capturingRunner{}
	mgr, _ := newTestManager(t, 2, runner)
	if err := mgr.SetExtraArgs("--limit-rate 1M"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddWithOptions("https://example.test/v",
		ipc.Options{Preset: "720p", ExtraArgs: "--limit-rate 5M --retries 3"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		return len(runner.jobs) == 1
	}, "the download to run")

	runner.mu.Lock()
	job := runner.jobs[0]
	runner.mu.Unlock()

	// The order is the whole point: global defaults, then this download's
	// picker choice, then its own arguments. yt-dlp honours the last of two
	// conflicting options, so this is what makes a per-download choice beat a
	// blanket setting rather than lose to it.
	got := strings.Join(jobArgs(job), " ")
	want := "--limit-rate 1M " +
		"--format bv*[height<=720]+ba/b[height<=720]/wv*[height<=720]+ba/w[height<=720] " +
		"--limit-rate 5M --retries 3"
	if got != want {
		t.Errorf("command line = %q\nwant           %q", got, want)
	}
}

// TestPickerChoiceBeatsGlobalExtraArgs is the case the ordering exists for: a
// blanket "--format" typed once in Settings must not silently override the
// quality picked for one download.
func TestPickerChoiceBeatsGlobalExtraArgs(t *testing.T) {
	job := Job{
		Options:    ipc.Options{FormatID: "137", AudioFormatID: "140"},
		GlobalArgs: []string{"--format", "worst"},
	}
	args := jobArgs(job)
	lastFormat := -1
	for i, a := range args {
		if a == "--format" {
			lastFormat = i
		}
	}
	if lastFormat < 0 || args[lastFormat+1] != "137+140" {
		t.Errorf("args = %q, want the picker's format to be the last one", args)
	}
}

// TestPerDownloadArgsBeatEverything: the field is documented as the last word,
// including over the picker's own structured choices.
func TestPerDownloadArgsBeatEverything(t *testing.T) {
	job := Job{
		Options:    ipc.Options{Preset: "1080p"},
		GlobalArgs: []string{"--format", "worst"},
		ExtraArgs:  []string{"--format", "137"},
	}
	args := jobArgs(job)
	if args[len(args)-2] != "--format" || args[len(args)-1] != "137" {
		t.Errorf("args = %q, want the download's own --format last", args)
	}
}

// TestPresetsNeverExceedTheirCeiling: every branch of a preset's format
// expression has to carry the height filter. An unfiltered fallback would hand
// back a 4K stream for a "720p" request whenever nothing smaller exists.
func TestPresetsNeverExceedTheirCeiling(t *testing.T) {
	for preset, height := range presetHeights {
		expr := FormatExpr(ipc.Options{Preset: preset})
		for _, branch := range strings.Split(expr, "/") {
			if !strings.Contains(branch, "height<="+height) {
				t.Errorf("preset %s: branch %q has no height ceiling (expression %q)",
					preset, branch, expr)
			}
		}
	}
}

// TestFormatTruncationKeepsAudioChoices: video formats sort first, so cutting
// the sorted list at MaxFormats would leave a picker with no audio at all for
// an extractor exposing hundreds of video variants — and a merge always needs
// one.
func TestFormatTruncationKeepsAudioChoices(t *testing.T) {
	raws := make([]rawFormat, 0, MaxFormats+50)
	for i := range MaxFormats + 20 {
		h := float64(1080)
		raws = append(raws, rawFormat{
			ID: "v" + strconv.Itoa(i), Ext: "mp4", VCodec: "avc1", ACodec: "none", Height: &h,
		})
	}
	for i := range 10 {
		raws = append(raws, rawFormat{
			ID: "a" + strconv.Itoa(i), Ext: "m4a", VCodec: "none", ACodec: "mp4a",
		})
	}
	out, truncated := convertFormats(raws)
	if !truncated {
		t.Error("an oversized format table was not reported as truncated")
	}
	if len(out) > MaxFormats {
		t.Errorf("returned %d formats, over the %d cap", len(out), MaxFormats)
	}
	audio := 0
	for _, f := range out {
		if f.HasAudio && !f.HasVideo {
			audio++
		}
	}
	if audio != 10 {
		t.Errorf("kept %d audio formats, want all 10 — truncation ate the audio", audio)
	}
}

// TestChangingGlobalArgsInvalidatesTheFormatCache: a cached format table was
// produced with the previous proxy, headers and extractor arguments. Serving
// it after those change shows the picker results from a request context that
// no longer exists.
func TestChangingGlobalArgsInvalidatesTheFormatCache(t *testing.T) {
	runner := &countingLister{
		probe: &FormatProbe{Title: "v", Formats: []Format{{ID: "1", HasVideo: true}}},
	}
	mgr, _ := newTestManager(t, 1, runner)
	url := "https://example.test/cached"

	if _, err := mgr.Formats(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Formats(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("probe ran %d times, want the second answered from cache", got)
	}

	if err := mgr.SetExtraArgs("--proxy http://127.0.0.1:8118"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Formats(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls.Load(); got != 2 {
		t.Errorf("probe ran %d times, want a re-probe after the arguments changed", got)
	}
}

type countingLister struct {
	fakeRunner
	calls atomic.Int64
	probe *FormatProbe
}

func (c *countingLister) Formats(ctx context.Context, job Job) (*FormatProbe, error) {
	c.calls.Add(1)
	return c.probe, nil
}

// TestInFlightProbeCannotCacheAfterArgumentsChange: clearing the cache is not
// enough on its own. A probe that started before the change completes after
// it, and inserting its answer would serve a stale request context for the
// life of the cache entry.
func TestInFlightProbeCannotCacheAfterArgumentsChange(t *testing.T) {
	release := make(chan struct{})
	runner := &blockingLister{
		release: release,
		probe:   &FormatProbe{Title: "stale", Formats: []Format{{ID: "1", HasVideo: true}}},
	}
	mgr, _ := newTestManager(t, 1, runner)
	url := "https://example.test/inflight"

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Formats(context.Background(), url)
		done <- err
	}()
	waitFor(t, 2*time.Second, func() bool { return runner.started.Load() == 1 }, "the probe to start")

	// Arguments change while it is in flight.
	if err := mgr.SetExtraArgs("--proxy http://127.0.0.1:8118"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// The next request must re-probe rather than be served the stale answer.
	if _, err := mgr.Formats(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if got := runner.started.Load(); got != 2 {
		t.Errorf("probe ran %d times, want the stale result to have been discarded", got)
	}
}

type blockingLister struct {
	fakeRunner
	started atomic.Int64
	release chan struct{}
	probe   *FormatProbe
}

func (b *blockingLister) Formats(ctx context.Context, job Job) (*FormatProbe, error) {
	if b.started.Add(1) == 1 {
		<-b.release
	}
	return b.probe, nil
}
