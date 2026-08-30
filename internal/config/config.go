// Package config owns the manager application configuration: a versioned,
// bounded, atomically persisted JSON file plus environment/flag resolution.
// It is deliberately separate from yt-dlp's own configuration, which always
// stays authoritative for download choices.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const CurrentVersion = 1

// MaxFileBytes bounds any config file read or write.
const MaxFileBytes = 1 << 20

type ServerSection struct {
	Listen               string `json:"listen"`
	AllowUnauthenticated bool   `json:"allow_unauthenticated"`
	SecureCookie         bool   `json:"secure_cookie"`
}

type DownloadsSection struct {
	MaxConcurrent int `json:"max_concurrent"`
}

type UISection struct {
	Theme   string `json:"theme"` // dark | light | system
	Compact bool   `json:"compact"`
}

type AdvancedSection struct {
	AllowYtDlpConfigEdit bool `json:"allow_yt_dlp_config_edit"`
}

type File struct {
	Version   int              `json:"version"`
	Server    ServerSection    `json:"server"`
	Downloads DownloadsSection `json:"downloads"`
	UI        UISection        `json:"ui"`
	Advanced  AdvancedSection  `json:"advanced"`
}

func Defaults() File {
	return File{
		Version: CurrentVersion,
		Server: ServerSection{
			Listen:               "127.0.0.1:8080",
			AllowUnauthenticated: false,
			SecureCookie:         false,
		},
		Downloads: DownloadsSection{MaxConcurrent: 20},
		UI:        UISection{Theme: "dark", Compact: false},
		Advanced:  AdvancedSection{AllowYtDlpConfigEdit: false},
	}
}

// DefaultPath returns the manager config location. In the container,
// XDG_CONFIG_HOME=/config makes this /config/yt-dlp-manager/config.json.
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "yt-dlp-manager-config.json"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "yt-dlp-manager", "config.json")
}

// Validate checks semantic bounds. Structural strictness (unknown fields) is
// enforced by the decoder in store.go; both paths funnel through here so a
// programmatically built config cannot bypass the same rules.
func Validate(f *File) error {
	if f.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d", f.Version)
	}
	if _, _, err := net.SplitHostPort(f.Server.Listen); err != nil {
		return fmt.Errorf("server.listen %q is not host:port", f.Server.Listen)
	}
	if p, err := strconv.Atoi(portOf(f.Server.Listen)); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("server.listen %q has an invalid port", f.Server.Listen)
	}
	if f.Downloads.MaxConcurrent < MinConcurrent || f.Downloads.MaxConcurrent > MaxConcurrent {
		return fmt.Errorf("downloads.max_concurrent must be between %d and %d, got %d",
			MinConcurrent, MaxConcurrent, f.Downloads.MaxConcurrent)
	}
	switch f.UI.Theme {
	case "dark", "light", "system":
	default:
		return fmt.Errorf("ui.theme must be dark, light, or system, got %q", f.UI.Theme)
	}
	return nil
}

func portOf(listen string) string {
	_, port, _ := net.SplitHostPort(listen)
	return port
}

// Sources records where each effective value came from so the authenticated
// Settings/About view can display it honestly.
type Source string

// Bounds on downloads.max_concurrent. Every entry point — config file,
// settings API, and the --max flags — is checked against these, so a typo
// like "--max 100000" cannot spawn an unbounded number of yt-dlp processes.
const (
	MinConcurrent = 1
	MaxConcurrent = 100
)

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
)

type Resolved struct {
	File File
	// Sources maps dotted keys ("server.listen", "downloads.max_concurrent",
	// "server.allow_unauthenticated", "server.secure_cookie") to their origin.
	Sources map[string]Source
}

// Resolve merges defaults <- file <- environment into an effective view.
// Flags are applied afterwards by the command layer, which overwrites values
// and marks them SourceFlag.
func Resolve(path string) (*Resolved, error) {
	f := Defaults()
	sources := map[string]Source{
		"server.listen":                SourceDefault,
		"server.allow_unauthenticated": SourceDefault,
		"server.secure_cookie":         SourceDefault,
		"downloads.max_concurrent":     SourceDefault,
		"ui.theme":                     SourceDefault,
		"ui.compact":                   SourceDefault,
	}

	st := NewStore(path)
	loaded, err := st.Load()
	switch {
	case errors.Is(err, ErrNoConfig):
		// keep defaults
	case err != nil:
		return nil, err
	default:
		fv, present := loaded.File, loaded.Present
		if present["server.listen"] {
			f.Server.Listen = fv.Server.Listen
			sources["server.listen"] = SourceFile
		}
		if present["server.allow_unauthenticated"] {
			f.Server.AllowUnauthenticated = fv.Server.AllowUnauthenticated
			sources["server.allow_unauthenticated"] = SourceFile
		}
		if present["server.secure_cookie"] {
			f.Server.SecureCookie = fv.Server.SecureCookie
			sources["server.secure_cookie"] = SourceFile
		}
		if present["downloads.max_concurrent"] {
			f.Downloads.MaxConcurrent = fv.Downloads.MaxConcurrent
			sources["downloads.max_concurrent"] = SourceFile
		}
		if present["ui.theme"] {
			f.UI.Theme = fv.UI.Theme
			sources["ui.theme"] = SourceFile
		}
		if present["ui.compact"] {
			f.UI.Compact = fv.UI.Compact
			sources["ui.compact"] = SourceFile
		}
		if present["advanced.allow_yt_dlp_config_edit"] {
			f.Advanced.AllowYtDlpConfigEdit = fv.Advanced.AllowYtDlpConfigEdit
		}
	}

	applyEnv := func(key string, set func(v string)) {
		if v := os.Getenv(key); v != "" {
			set(v)
		}
	}
	boolEnv := func(v string) bool {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	}
	applyEnv("YTDLP_MANAGER_LISTEN", func(v string) {
		f.Server.Listen = strings.TrimSpace(v)
		sources["server.listen"] = SourceEnv
	})
	applyEnv("YTDLP_MANAGER_MAX_CONCURRENT", func(v string) {
		if n, err := strconv.Atoi(v); err == nil && n >= MinConcurrent && n <= MaxConcurrent {
			f.Downloads.MaxConcurrent = n
			sources["downloads.max_concurrent"] = SourceEnv
		}
	})
	applyEnv("YTDLP_MANAGER_ALLOW_UNAUTHENTICATED", func(v string) {
		f.Server.AllowUnauthenticated = boolEnv(v)
		sources["server.allow_unauthenticated"] = SourceEnv
	})
	applyEnv("YTDLP_MANAGER_SECURE_COOKIE", func(v string) {
		f.Server.SecureCookie = boolEnv(v)
		sources["server.secure_cookie"] = SourceEnv
	})
	applyEnv("YTDLP_MANAGER_ALLOW_YTDLP_CONFIG_EDIT", func(v string) {
		f.Advanced.AllowYtDlpConfigEdit = boolEnv(v)
	})

	if err := Validate(&f); err != nil {
		return nil, fmt.Errorf("effective configuration invalid: %w", err)
	}
	return &Resolved{File: f, Sources: sources}, nil
}
