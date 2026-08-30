package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Loaded is a config file together with which leaf keys it explicitly set.
type Loaded struct {
	File    File
	Present map[string]bool
}

// Store persists one versioned manager config file atomically.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	if path == "" {
		path = DefaultPath()
	}
	return &Store{path: path}
}

func (s *Store) Path() string { return s.path }

func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

var ErrNoConfig = errors.New("no config file")

// Load strictly parses the config file. Unknown fields are rejected so a
// typo can never silently disable a safety-relevant default. A corrupt or
// invalid file is quarantined (never silently replaced) and reported.
func (s *Store) Load() (*Loaded, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := readLimited(s.path, MaxFileBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoConfig
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := strictUnmarshal(data, &raw); err != nil {
		return nil, s.quarantine(fmt.Errorf("parse config: %w", err))
	}
	if n := intFromRaw(raw["version"]); n != CurrentVersion {
		return nil, s.quarantine(fmt.Errorf("unsupported or missing config version"))
	}

	var f File
	present := map[string]bool{}

	if sec, ok := raw["server"]; ok {
		var m map[string]json.RawMessage
		if err := strictKeys(sec, &m); err != nil {
			return nil, s.quarantine(err)
		}
		for k, v := range m {
			switch k {
			case "listen":
				if err := json.Unmarshal(v, &f.Server.Listen); err != nil {
					return nil, s.quarantine(err)
				}
				present["server.listen"] = true
			case "allow_unauthenticated":
				if err := json.Unmarshal(v, &f.Server.AllowUnauthenticated); err != nil {
					return nil, s.quarantine(err)
				}
				present["server.allow_unauthenticated"] = true
			case "secure_cookie":
				if err := json.Unmarshal(v, &f.Server.SecureCookie); err != nil {
					return nil, s.quarantine(err)
				}
				present["server.secure_cookie"] = true
			default:
				return nil, s.quarantine(fmt.Errorf("unknown server field %q", k))
			}
		}
	}
	if sec, ok := raw["downloads"]; ok {
		var m map[string]json.RawMessage
		if err := strictKeys(sec, &m); err != nil {
			return nil, s.quarantine(err)
		}
		for k, v := range m {
			switch k {
			case "max_concurrent":
				if err := json.Unmarshal(v, &f.Downloads.MaxConcurrent); err != nil {
					return nil, s.quarantine(err)
				}
				present["downloads.max_concurrent"] = true
			default:
				return nil, s.quarantine(fmt.Errorf("unknown downloads field %q", k))
			}
		}
	}
	if sec, ok := raw["ui"]; ok {
		var m map[string]json.RawMessage
		if err := strictKeys(sec, &m); err != nil {
			return nil, s.quarantine(err)
		}
		for k, v := range m {
			switch k {
			case "theme":
				if err := json.Unmarshal(v, &f.UI.Theme); err != nil {
					return nil, s.quarantine(err)
				}
				present["ui.theme"] = true
			case "compact":
				if err := json.Unmarshal(v, &f.UI.Compact); err != nil {
					return nil, s.quarantine(err)
				}
				present["ui.compact"] = true
			default:
				return nil, s.quarantine(fmt.Errorf("unknown ui field %q", k))
			}
		}
	}
	if sec, ok := raw["advanced"]; ok {
		var m map[string]json.RawMessage
		if err := strictKeys(sec, &m); err != nil {
			return nil, s.quarantine(err)
		}
		for k, v := range m {
			switch k {
			case "allow_yt_dlp_config_edit":
				if err := json.Unmarshal(v, &f.Advanced.AllowYtDlpConfigEdit); err != nil {
					return nil, s.quarantine(err)
				}
				present["advanced.allow_yt_dlp_config_edit"] = true
			default:
				return nil, s.quarantine(fmt.Errorf("unknown advanced field %q", k))
			}
		}
	}
	for k := range raw {
		switch k {
		case "version", "saved_at", "server", "downloads", "ui", "advanced":
		default:
			return nil, s.quarantine(fmt.Errorf("unknown top-level field %q", k))
		}
	}
	f.Version = CurrentVersion

	if err := Validate(&f); err != nil {
		return nil, s.quarantine(err)
	}
	return &Loaded{File: f, Present: present}, nil
}

// Save validates then atomically writes: temp file in the same directory,
// fsync, chmod 0600, rename.
func (s *Store) Save(f *File) error {
	f.Version = CurrentVersion
	if err := Validate(f); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
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
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	return syncParentDir(s.path)
}

// quarantine moves an unusable config aside so the operator's data is never
// destroyed, mirroring state-store behavior.
func (s *Store) quarantine(cause error) error {
	target := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UnixNano())
	if err := os.Rename(s.path, target); err == nil {
		return fmt.Errorf("%w (moved to %s)", cause, target)
	}
	return cause
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

// syncParentDir makes an atomic rename durable, not merely visible. The temp
// file itself is fsynced before the rename; this second sync persists the
// directory entry that points at it.
func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func strictUnmarshal(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values in config")
	}
	return nil
}

func strictKeys(section json.RawMessage, m *map[string]json.RawMessage) error {
	var extra any
	d := json.NewDecoder(bytes.NewReader(section))
	if err := d.Decode(m); err != nil {
		return fmt.Errorf("section must be an object: %w", err)
	}
	if d.More() {
		return errors.New("trailing data in section")
	}
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing data in section")
	}
	return nil
}

func intFromRaw(raw json.RawMessage) int {
	if raw == nil {
		return -1
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return -1
	}
	return n
}
