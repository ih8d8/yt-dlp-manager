package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestDefaultsValidateAndResolveWithoutFile(t *testing.T) {
	dir := t.TempDir()
	r, err := Resolve(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.File.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("default listen = %q", r.File.Server.Listen)
	}
	if r.Sources["server.listen"] != SourceDefault {
		t.Errorf("listen source = %v", r.Sources["server.listen"])
	}
	if r.File.Downloads.MaxConcurrent < 1 {
		t.Errorf("max concurrent = %d", r.File.Downloads.MaxConcurrent)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(filepath.Join(dir, "nested", "config.json"))
	f := Defaults()
	f.Server.Listen = "0.0.0.0:9000"
	f.UI.Theme = "light"
	if err := st.Save(&f); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config perms = %v, want 0600", fi.Mode().Perm())
	}

	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.File.Server.Listen != "0.0.0.0:9000" || loaded.File.UI.Theme != "light" {
		t.Fatalf("loaded = %+v", loaded.File)
	}
	// Save serializes every field, so Load reports all leaves present.
	for _, k := range []string{"server.listen", "ui.theme"} {
		if !loaded.Present[k] {
			t.Errorf("presence[%q] = false, want true", k)
		}
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"version":1,"server":{"listen":"127.0.0.1:8080","evil":true}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st := NewStore(path)
	if _, err := st.Load(); err == nil {
		t.Fatal("unknown field must be rejected")
	}
	// Corrupt input is quarantined, not destroyed.
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Errorf("corrupt config not quarantined (matches=%v)", matches)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("original corrupt file must have been moved away")
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"bad version": `{"version":2}`,
		"bad listen":  `{"version":1,"server":{"listen":"nope"}}`,
		"bad max":     `{"version":1,"downloads":{"max_concurrent":500}}`,
		"bad theme":   `{"version":1,"ui":{"theme":"neon"}}`,
		"trailing":    "{\"version\":1} {}",
	}
	for name, body := range cases {
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, []byte(body), 0o600) //nolint:errcheck
		if _, err := NewStore(path).Load(); err == nil {
			t.Errorf("%s: expected error for %s", name, body)
		}
	}
}

func TestValidateRejectsProgrammaticNonsense(t *testing.T) {
	f := Defaults()
	f.Downloads.MaxConcurrent = 0
	if err := Validate(&f); err == nil {
		t.Error("zero concurrency must be rejected")
	}
	f = Defaults()
	f.Server.Listen = "[::1" // unbalanced bracket
	if err := Validate(&f); err == nil {
		t.Error("invalid listen must be rejected")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	f := Defaults()
	f.Downloads.MaxConcurrent = 3
	if err := NewStore(path).Save(&f); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YTDLP_MANAGER_MAX_CONCURRENT", "7")
	t.Setenv("YTDLP_MANAGER_LISTEN", "127.0.0.1:9999")

	r, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.File.Downloads.MaxConcurrent != 7 {
		t.Errorf("env override lost: %+v", r.File.Downloads)
	}
	if r.Sources["downloads.max_concurrent"] != SourceEnv {
		t.Errorf("source = %v, want env", r.Sources["downloads.max_concurrent"])
	}
	if r.File.Server.Listen != "127.0.0.1:9999" {
		t.Errorf("listen override lost: %q", r.File.Server.Listen)
	}
}

func TestCorruptConfigQuarantinedNotErased(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte("{\"version\":1,"), 0o600) //nolint:errcheck
	_, err := NewStore(path).Load()
	if err == nil {
		t.Fatal("expected quarantine error")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("corrupt file must be moved aside, not left in place")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Errorf("expected one quarantined copy, got %v", matches)
	}
}

func TestOversizedConfigRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	big := make([]byte, MaxFileBytes+10)
	for i := range big {
		big[i] = ' '
	}
	os.WriteFile(path, big, 0o600) //nolint:errcheck
	if _, err := NewStore(path).Load(); err == nil {
		t.Fatal("oversized config must be rejected")
	}
}

// --- managed yt-dlp block ---

const outsideA = "# my personal yt-dlp tweaks\n--cookies-from-browser firefox\n\n"

func TestManagedBlockInsertPreservesOutsideBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(outsideA), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &YtDlpSettings{
		DownloadsDir:   "/downloads/videos",
		Format:         "bestvideo*+bestaudio/best",
		OutputTemplate: `%(title)s [%(id)s].%(ext)s`,
		RateLimit:      "4M",
	}
	if err := WriteYtDlpManagedBlock(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	txt := string(got)
	if !strings.HasPrefix(txt, outsideA) {
		t.Errorf("outside bytes not preserved:\n%q", txt)
	}
	if strings.Count(txt, managedBegin) != 1 || strings.Count(txt, managedEnd) != 1 {
		t.Error("markers must appear exactly once")
	}

	status, parsed, err := ReadYtDlpConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Problem != "" {
		t.Fatalf("status problem: %s", status.Problem)
	}
	if !parsed.HasBlock {
		t.Fatal("block missing after write")
	}
	if parsed.Format != s.Format || parsed.OutputTemplate != s.OutputTemplate ||
		parsed.RateLimit != s.RateLimit || parsed.DownloadsDir != s.DownloadsDir {
		t.Fatalf("round trip mismatch: %+v vs %+v", parsed, s)
	}
}

func TestManagedBlockReplaceKeepsOutsideBytesIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	before := outsideA + "--retries 10\n# trailing comment\n"
	os.WriteFile(path, []byte(before), 0o600) //nolint:errcheck

	s := &YtDlpSettings{Format: "b"}
	if err := WriteYtDlpManagedBlock(path, s); err != nil {
		t.Fatal(err)
	}
	s2 := &YtDlpSettings{Format: "bv*+ba/b", ExtractAudio: true, AudioFormat: "mp3"}
	if err := WriteYtDlpManagedBlock(path, s2); err != nil {
		t.Fatal(err)
	}

	after, _ := os.ReadFile(path)
	txt := string(after)
	if !strings.HasPrefix(txt, outsideA) {
		t.Errorf("outside content changed:\n%q", txt)
	}
	middle := strings.SplitN(txt, managedBegin, 2)[0]
	if !strings.Contains(middle, "--retries 10\n# trailing comment\n") {
		t.Errorf("user options lost:\n%q", middle)
	}
	if strings.Count(txt, "--format") != 1 {
		t.Errorf("old block remnants remain:\n%q", txt)
	}
	if !strings.Contains(txt, "-x") || !strings.Contains(txt, "--audio-format mp3") {
		t.Errorf("new settings missing:\n%q", txt)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal("one bounded backup expected after real change")
	}
	if !strings.Contains(string(bak), "--format b\n") {
		t.Errorf("backup should hold previous content, got:\n%q", bak)
	}
}

func TestLegacyManagedMarkersRemainReadableAndMigrateOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	legacy := outsideA + legacyManagedBegin + "\n--format b\n" + legacyManagedEnd + "\n--retries 5\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	status, parsed, err := ReadYtDlpConfig(path)
	if err != nil || status.Problem != "" || parsed == nil || parsed.Format != "b" {
		t.Fatalf("legacy read = status=%+v parsed=%+v err=%v", status, parsed, err)
	}
	if err := WriteYtDlpManagedBlock(path, &YtDlpSettings{Format: "best"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, legacyManagedBegin) || !strings.Contains(text, managedBegin) {
		t.Fatalf("legacy markers were not migrated:\n%s", text)
	}
	if !strings.HasPrefix(text, outsideA) || !strings.HasSuffix(text, "--retries 5\n") {
		t.Fatalf("migration changed user content:\n%s", text)
	}
}

func TestManagedBlockRemoveKeepsRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	before := outsideA + managedBegin + "\n--format b\n" + managedEnd + "\n--retries 5\n"
	os.WriteFile(path, []byte(before), 0o600) //nolint:errcheck

	if err := RemoveYtDlpManagedBlock(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != outsideA+"--retries 5\n" {
		t.Errorf("removal changed outside bytes:\n%q", after)
	}
}

func TestManagedBlockRefusesDuplicateMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	dup := managedBegin + "\n--format b\n" + managedEnd + "\n" + managedBegin + "\n--format c\n" + managedEnd + "\n"
	os.WriteFile(path, []byte(dup), 0o600) //nolint:errcheck

	if err := WriteYtDlpManagedBlock(path, &YtDlpSettings{Format: "x"}); err == nil {
		t.Fatal("duplicated markers must be refused")
	}
	status, _, _ := ReadYtDlpConfig(path)
	if status.Problem == "" {
		t.Error("read status must report the problem")
	}
}

func TestManagedBlockRefusesSymlinkAndControlChars(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	os.WriteFile(target, []byte(""), 0o600) //nolint:errcheck
	link := filepath.Join(dir, "link")
	os.Symlink(target, link) //nolint:errcheck
	if err := WriteYtDlpManagedBlock(link, &YtDlpSettings{Format: "x"}); err == nil {
		t.Error("symlink config must be refused")
	}

	ctrl := filepath.Join(dir, "ctrl")
	os.WriteFile(ctrl, []byte("--format a\x00b"), 0o600) //nolint:errcheck
	if err := WriteYtDlpManagedBlock(ctrl, &YtDlpSettings{Format: "x"}); err == nil {
		t.Error("NUL byte config must be refused")
	}
}

func TestManagedBlockUnknownOptionRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	hostile := outsideA + managedBegin + "\n--exec rm -rf /\n" + managedEnd + "\n"
	os.WriteFile(path, []byte(hostile), 0o600) //nolint:errcheck

	status, parsed, err := ReadYtDlpConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != nil {
		t.Errorf("hostile block must not parse into settings: %+v", parsed)
	}
	if status.Problem == "" || !strings.Contains(status.Problem, "not managed") {
		t.Errorf("problem = %q, want unknown-option refusal", status.Problem)
	}
	// Writing over it is still allowed only via full replacement; the write
	// path replaces the whole block so the hostile line disappears.
	if err := WriteYtDlpManagedBlock(path, &YtDlpSettings{Format: "safe"}); err != nil {
		t.Fatalf("legitimate rewrite should succeed: %v", err)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "--exec") {
		t.Error("dangerous option survived a structured rewrite")
	}
}

// trickyQuoteValues pairs a value with the EXACT line ytDlpQuote must render.
// The expectations are golden bytes, not a round trip through splitShellWord:
// round-tripping through our own reader only proves the two local functions
// agree with each other, which they did while yt-dlp was receiving something
// different. shlex is the authority here, so these are pinned literally and
// cross-checked against Python in TestQuoteMatchesPythonShlex.
var trickyQuoteValues = []struct{ value, rendered string }{
	{`%(title)s [%(id)s].%(ext)s`, `'%(title)s [%(id)s].%(ext)s'`},
	{`it's a test`, `'it'\''s a test'`},
	{`quote "inside" here`, `'quote "inside" here'`},
	{`back\slash`, `'back\slash'`},
	{`dollar $HOME and backtick ` + "`", `'dollar $HOME and backtick ` + "`" + `'`},
	{"tabs\tnot allowed raw but quoted ok", "'tabs\tnot allowed raw but quoted ok'"},
	{`has#hash`, `'has#hash'`},
	{`bv*+ba/b`, `'bv*+ba/b'`},
	{`plain_value-1.2/x`, `plain_value-1.2/x`},
	{``, `''`},
}

func TestQuoteRendersExactBytes(t *testing.T) {
	for _, tc := range trickyQuoteValues {
		if got := ytDlpQuote(tc.value); got != tc.rendered {
			t.Errorf("ytDlpQuote(%q) = %q, want %q", tc.value, got, tc.rendered)
		}
	}
}

func TestQuoteRoundTripTricky(t *testing.T) {
	for _, tc := range trickyQuoteValues {
		toks, err := splitShellWord(tc.rendered)
		if err != nil {
			t.Fatalf("tokenize %q -> %q: %v", tc.value, tc.rendered, err)
		}
		if len(toks) != 1 || toks[0] != tc.value {
			t.Errorf("round trip %q -> %q -> %v", tc.value, tc.rendered, toks)
		}
	}
}

// TestQuoteMatchesPythonShlex is the differential that keeps the two
// tokenizers honest. yt-dlp reads its config with shlex.split(comments=True),
// so a value is only correctly quoted if PYTHON recovers it, not if we do.
// Skipped when python3 is unavailable; it is a guard, not a dependency.
func TestQuoteMatchesPythonShlex(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping shlex differential")
	}
	var lines []string
	for _, tc := range trickyQuoteValues {
		lines = append(lines, tc.rendered)
	}
	// One value per line so a mis-quote cannot silently merge two tokens.
	script := `import sys, shlex, json
out = [shlex.split(line, comments=True) for line in sys.stdin.read().split("\n")]
json.dump(out, sys.stdout)`
	cmd := exec.Command(python, "-c", script)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run python shlex: %v", err)
	}
	var got [][]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode shlex output %q: %v", raw, err)
	}
	if len(got) != len(trickyQuoteValues) {
		t.Fatalf("got %d shlex results, want %d", len(got), len(trickyQuoteValues))
	}
	for i, tc := range trickyQuoteValues {
		want := []string{tc.value}
		if tc.value == "" {
			want = []string{""}
		}
		if !reflect.DeepEqual(got[i], want) {
			t.Errorf("python shlex.split(%q) = %q, want %q (value %q)",
				tc.rendered, got[i], want, tc.value)
		}
	}
}

func TestValidateYtDlpSettingsBounds(t *testing.T) {
	ok := func(s *YtDlpSettings) bool { return ValidateYtDlpSettings(s) == nil }
	if ok(&YtDlpSettings{DownloadsDir: "/etc"}) {
		t.Error("/etc must be rejected")
	}
	if ok(&YtDlpSettings{DownloadsDir: "/downloads/../etc"}) {
		t.Error("traversal must be rejected")
	}
	if !ok(&YtDlpSettings{DownloadsDir: "/downloads/sub/dir"}) {
		t.Error("/downloads/sub/dir should pass")
	}
	if ok(&YtDlpSettings{Format: "a\nb"}) {
		t.Error("newline in format must be rejected")
	}
	if ok(&YtDlpSettings{AudioFormat: "mp3"}) {
		t.Error("audio_format without extract_audio must fail")
	}
	if !ok(&YtDlpSettings{ExtractAudio: true, AudioFormat: "mp3"}) {
		t.Error("extract audio + mp3 should pass")
	}
	if ok(&YtDlpSettings{RateLimit: "ten M"}) {
		t.Error("garbage rate limit must be rejected")
	}
	if !ok(&YtDlpSettings{RateLimit: "4.5M"}) {
		t.Error("4.5M rate limit should pass")
	}
	if ok(&YtDlpSettings{SubLanguages: []string{"en;;drop"}}) {
		t.Error("weird language codes must be rejected")
	}
}

// TestOutputTemplateStaysUnderDownloadRoot pins the confinement that keeps an
// allowlisted settings write from becoming an arbitrary file write. --paths
// names the directory and --output names the file, but yt-dlp lets an absolute
// or upward -o override -P entirely, and runs the template through expand_path
// so "~" and "$VAR" are expansions rather than literal characters.
func TestOutputTemplateStaysUnderDownloadRoot(t *testing.T) {
	rejected := []string{
		"/etc/cron.d/pwn",                  // absolute overrides --paths
		"/config/yt-dlp/config",            // the escalation target
		"../../../../etc/cron.d/pwn",       // upward traversal
		"a/../../b.%(ext)s",                // traversal after a segment
		"~/escaped.%(ext)s",                // expand_path expands "~"
		"$HOME/escaped.%(ext)s",            // expand_path expands "$VAR"
		"sub/$EVIL/x.%(ext)s",              // expansion anywhere, not just leading
		"%(filepath)s",                     // field that is itself a path
		"thumbnail:/etc/evil.%(ext)s",      // TYPE: prefix must not exempt it
		"thumbnail:../../etc/evil.%(ext)s", // same via traversal
	}
	for _, tmpl := range rejected {
		s := &YtDlpSettings{OutputTemplate: tmpl}
		if err := ValidateYtDlpSettings(s); err == nil {
			t.Errorf("output template %q must be rejected", tmpl)
		}
	}

	accepted := []string{
		"%(title)s [%(id)s].%(ext)s",
		"sub/dir/%(title)s.%(ext)s",
		"%(uploader)s/%(upload_date)s - %(title)s.%(ext)s",
		"thumbnail:covers/%(title)s.%(ext)s",
		"",
	}
	for _, tmpl := range accepted {
		s := &YtDlpSettings{OutputTemplate: tmpl}
		if err := ValidateYtDlpSettings(s); err != nil {
			t.Errorf("output template %q must be accepted, got %v", tmpl, err)
		}
	}
}

func TestDownloadsDirRejectsExpansions(t *testing.T) {
	// "/downloads/$VAR" passes a naive prefix check but expand_path resolves it
	// somewhere else entirely.
	for _, dir := range []string{"/downloads/$HOME", "~/downloads", "/downloads/$EVIL/x"} {
		if err := ValidateYtDlpSettings(&YtDlpSettings{DownloadsDir: dir}); err == nil {
			t.Errorf("download dir %q must be rejected", dir)
		}
	}
}

// A control character in the download directory used to pass validation and
// produce a block that this package could no longer read back: ytDlpQuote
// wraps the value in single quotes, so a newline stayed inside the quoted
// string, but parseManagedLines splits the block on newlines and saw an
// unterminated quote. The write "succeeded" and the settings form then went
// blank with "managed block unreadable".
func TestDownloadsDirRejectsControlCharacters(t *testing.T) {
	for _, dir := range []string{"/downloads/a\nb", "/downloads/a\rb", "/downloads/a\x01b"} {
		if err := ValidateYtDlpSettings(&YtDlpSettings{DownloadsDir: dir}); err == nil {
			t.Errorf("download dir %q must be rejected", dir)
		}
	}

	// The whole point is that a written block stays readable, so assert the
	// round trip rather than only the validator.
	path := filepath.Join(t.TempDir(), "config")
	if err := WriteYtDlpManagedBlock(path, &YtDlpSettings{
		DownloadsDir: "/downloads/a\nb", HasBlock: true,
	}); err == nil {
		t.Fatal("write with a newline in the download directory must fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("rejected settings must not create %s", path)
	}
}

func TestConfigJSONShapeMatchesSchema(t *testing.T) {
	data, err := json.Marshal(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	json.Unmarshal(data, &probe) //nolint:errcheck
	for _, k := range []string{"version", "server", "downloads", "ui", "advanced"} {
		if _, ok := probe[k]; !ok {
			t.Errorf("serialized defaults missing key %q", k)
		}
	}
}

func TestCompactDensitySurvivesRestart(t *testing.T) {
	// Regression: ui.compact was persisted but never merged back by Resolve,
	// silently resetting on every restart.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	st := NewStore(path)
	f := Defaults()
	f.UI.Compact = true
	if err := st.Save(&f); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.File.UI.Compact {
		t.Error("ui.compact lost across resolve")
	}
	if r.Sources["ui.compact"] != SourceFile {
		t.Errorf("ui.compact source = %v, want file", r.Sources["ui.compact"])
	}
}

func TestManagedBlockBackupNeverFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(outsideA), 0o600); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	bak := path + ".bak"
	if err := os.Symlink(victim, bak); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s := &YtDlpSettings{Format: "b"}
	if err := WriteYtDlpManagedBlock(path, s); err != nil {
		t.Fatal(err)
	}

	vb, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(vb) != "do not touch" {
		t.Fatalf("backup followed symlink and overwrote victim with %q", vb)
	}
	fi, err := os.Lstat(bak)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("backup path is still a symlink; it should have been replaced atomically")
	}
	got, _ := os.ReadFile(bak)
	if string(got) != outsideA {
		t.Errorf(".bak content = %q, want previous config %q", got, outsideA)
	}
}

func TestManagedBlockConcurrentWritesStayConsistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(outsideA), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := &YtDlpSettings{Format: fmt.Sprintf("b%d", i)}
			if err := WriteYtDlpManagedBlock(path, s); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	txt := string(data)
	if !strings.HasPrefix(txt, outsideA) {
		t.Error("outside bytes lost under concurrency")
	}
	if strings.Count(txt, managedBegin) != 1 || strings.Count(txt, managedEnd) != 1 {
		t.Errorf("markers duplicated under concurrency:\n%q", txt)
	}
	status, _, err := ReadYtDlpConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if status.Problem != "" {
		t.Errorf("post-concurrency status problem: %s", status.Problem)
	}
}

// TestSavedConfigLoadsBackWithEveryField is the regression test for a field
// that could be written but not read: the loader enumerates known keys by
// hand, so a new field added to the struct alone makes the next start
// quarantine the whole configuration file and lose every setting in it.
func TestSavedConfigLoadsBackWithEveryField(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "config.json"))

	saved := Defaults()
	saved.Downloads.MaxConcurrent = 7
	saved.Downloads.ExtraArgs = "--limit-rate 2M --retries 20"
	saved.UI.Theme = "light"
	saved.UI.Compact = true
	saved.Server.Listen = "127.0.0.1:9999"
	saved.Server.AllowUnauthenticated = true
	saved.Server.SecureCookie = true
	saved.Advanced.AllowYtDlpConfigEdit = true
	if err := store.Save(&saved); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("a config this build just wrote was rejected on load: %v", err)
	}
	if loaded.File != saved {
		t.Errorf("round trip lost data:\n saved  %+v\n loaded %+v", saved, loaded.File)
	}
	if !loaded.Present["downloads.extra_args"] {
		t.Error("extra_args loaded but not recorded as present")
	}
}

// TestResolveAppliesSavedExtraArgs is the regression test for global arguments
// that saved correctly and then vanished on the next start: Resolve, not
// Store.Load, is what startup actually uses, and it copied each field by hand.
func TestResolveAppliesSavedExtraArgs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	saved := Defaults()
	saved.Downloads.ExtraArgs = "--limit-rate 2M"
	saved.Downloads.MaxConcurrent = 9
	if err := NewStore(path).Save(&saved); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.File.Downloads.ExtraArgs != "--limit-rate 2M" {
		t.Errorf("extra_args = %q, want the saved value", resolved.File.Downloads.ExtraArgs)
	}
	if resolved.Sources["downloads.extra_args"] != SourceFile {
		t.Errorf("source = %q, want file", resolved.Sources["downloads.extra_args"])
	}
}
