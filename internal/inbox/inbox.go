package inbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yt-dlp-manager/internal/ipc"
)

type entry struct {
	URL     string    `json:"url"`
	AddedAt time.Time `json:"added_at"`
}

func DefaultPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "yt-dlp-manager-inbox.jsonl"
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "yt-dlp-manager", "inbox.jsonl")
}

func Append(path, url string) error {
	url = strings.TrimSpace(url)
	if !ipc.ValidURL(url) {
		return errors.New("invalid url")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	e := entry{URL: url, AddedAt: time.Now()}
	data, err := json.Marshal(e)
	if err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Drain atomically claims the inbox by renaming it aside before reading, so
// URLs appended concurrently (an offline `add` command racing a launch)
// land in the fresh file and survive for the next drain instead of being
// destroyed with the consumed one.
func Drain(path string) ([]string, error) {
	tmp, err := claim(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	f, err := os.Open(tmp)
	if err != nil {
		_ = restore(tmp, path)
		return nil, err
	}

	var urls []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 16<<10)
	lineNo := 0
	var parseErr error
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e entry
		if err := decodeEntry([]byte(line), &e); err != nil {
			parseErr = fmt.Errorf("inbox line %d: %w", lineNo, err)
			break
		}
		if strings.TrimSpace(e.URL) == "" {
			parseErr = fmt.Errorf("inbox line %d: empty url", lineNo)
			break
		}
		urls = append(urls, e.URL)
	}
	readErr := errors.Join(parseErr, sc.Err(), f.Close())
	if readErr != nil || len(urls) == 0 {
		// Nothing consumed (or unreadable): put the file back rather than
		// destroying entries. Return no URLs on error: returning a valid
		// prefix as well would enqueue it now and again on the next drain.
		return nil, errors.Join(readErr, restore(tmp, path))
	}
	if err := os.Remove(tmp); err != nil {
		return nil, err
	}
	return urls, nil
}

func decodeEntry(line []byte, e *entry) error {
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(e); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// claim uses a unique same-directory name so two launchers can never replace
// each other's in-flight inbox, which a fixed ".draining" name allowed.
func claim(path string) (string, error) {
	placeholder, err := os.CreateTemp(filepath.Dir(path), ".inbox-*.draining")
	if err != nil {
		return "", err
	}
	tmp := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(path, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// restore appends the claimed bytes to a fresh/current inbox instead of
// renaming over it. This preserves URLs appended while the claim was read.
func restore(tmp, path string) error {
	src, err := os.Open(tmp)
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		_ = src.Close()
		return err
	}
	_, copyErr := io.Copy(dst, src)
	err = errors.Join(copyErr, dst.Sync(), dst.Close(), src.Close())
	if err != nil {
		return err
	}
	return os.Remove(tmp)
}
