package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"yt-dlp-manager/internal/manager"
)

// listingRunner is instantRunner plus the optional format-probe capability,
// so the endpoint can be tested without a real yt-dlp.
type listingRunner struct {
	instantRunner
	probe *manager.FormatProbe
	err   error
}

func (r listingRunner) Formats(ctx context.Context, job manager.Job) (*manager.FormatProbe, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.probe, nil
}

func TestFormatsEndpointReturnsChoices(t *testing.T) {
	dir := t.TempDir()
	runner := listingRunner{
		instantRunner: instantRunner{dir},
		probe: &manager.FormatProbe{
			Title: "A video",
			Formats: []manager.Format{
				{ID: "137", Ext: "mp4", Height: 1080, HasVideo: true},
				{ID: "140", Ext: "m4a", HasAudio: true},
			},
		},
	}
	d := testDeps(t)
	d.Manager = newTestManagerWithRunner(t, runner)
	s := testServer(t, d)

	rec := do(t, s, "POST", "/api/v1/formats",
		map[string]any{"url": "https://example.com/v"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		URL     string           `json:"url"`
		Title   string           `json:"title"`
		Formats []manager.Format `json:"formats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Title != "A video" || len(resp.Formats) != 2 || resp.Formats[0].ID != "137" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestFormatsEndpointRejectsBadURL(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)
	rec := do(t, s, "POST", "/api/v1/formats",
		map[string]any{"url": "file:///etc/passwd"}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestFormatsEndpointReportsUnsupportedRunner: a runner with no format probe
// must produce a clean "not implemented", not a 500.
func TestFormatsEndpointReportsUnsupportedRunner(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)
	rec := do(t, s, "POST", "/api/v1/formats",
		map[string]any{"url": "https://example.com/v"}, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAddDownloadStoresOptions(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{
		"url":     "https://example.com/opts",
		"options": map[string]any{"preset": "1080p", "merge_container": "mp4"},
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp addDownloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Download.Options == nil || resp.Download.Options.Preset != "1080p" {
		t.Fatalf("options not echoed back: %+v", resp.Download.Options)
	}
	if resp.Download.OptionsSummary == "" {
		t.Error("no display summary for an item with overrides")
	}
}

func TestAddDownloadRejectsUnusableOptions(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{
		"url":     "https://example.com/bad",
		"options": map[string]any{"format_id": "137 --exec touch /tmp/x"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != codeInvalidOptions {
		t.Errorf("code = %q, want %q", body.Error.Code, codeInvalidOptions)
	}
}

// TestAddDownloadSameURLIsRefused: identity is the URL, not the URL plus its
// options, because two rows for one URL resolve to one file on disk.
func TestAddDownloadSameURLIsRefused(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)
	url := "https://example.com/hold-dupe"

	first := do(t, s, "POST", "/api/v1/downloads", map[string]any{
		"url": url, "options": map[string]any{"preset": "1080p"},
	}, nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	second := do(t, s, "POST", "/api/v1/downloads", map[string]any{
		"url": url, "options": map[string]any{"preset": "720p"},
	}, nil)
	if second.Code != http.StatusConflict {
		t.Fatalf("same URL at another quality status = %d, want 409", second.Code)
	}
}

func TestSettingsExtraArgsRoundTrip(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	rec := do(t, s, "PUT", "/api/v1/settings",
		map[string]any{"downloads": map[string]any{"extra_args": "  --limit-rate 2M  "}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var view settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Downloads.ExtraArgs != "--limit-rate 2M" {
		t.Errorf("extra_args = %q", view.Downloads.ExtraArgs)
	}
	// And it survives a re-read rather than living only in the response.
	get := do(t, s, "GET", "/api/v1/settings", nil, nil)
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Downloads.ExtraArgs != "--limit-rate 2M" {
		t.Errorf("after GET extra_args = %q", view.Downloads.ExtraArgs)
	}
}

// TestSettingsRefusesDangerousExtraArgs: the settings endpoint is reachable
// with an authenticated session, so it must not be a path to running commands
// on the host.
func TestSettingsRefusesDangerousExtraArgs(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	rec := do(t, s, "PUT", "/api/v1/settings",
		map[string]any{"downloads": map[string]any{"extra_args": "--exec touch /tmp/pwned"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != codeInvalidOptions {
		t.Errorf("code = %q", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "--exec") {
		t.Errorf("message %q does not name the refused option", body.Error.Message)
	}
}

func TestAddDownloadRefusesDangerousExtraArgs(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{
		"url":     "https://example.com/x-args",
		"options": map[string]any{"extra_args": "--downloader /bin/sh"},
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// TestAddDownloadRefusesAnUnstorableURL: a URL that is valid by every other
// rule but would not fit one queue entry is refused at the door, with a
// message rather than a generic failure.
func TestAddDownloadRefusesAnUnstorableURL(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)
	rec := do(t, s, "POST", "/api/v1/downloads", map[string]any{
		"url": "https://example.com/" + strings.Repeat("<", 4000),
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}
