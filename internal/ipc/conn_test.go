package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dialPair creates a real unix-socket pair with a raw server connection for
// scripted protocol exchanges.
func dialPair(t *testing.T) (client *Conn, server net.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	c, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	server, err = ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return c, server
}

func TestConnDemuxesResponsesAndEvents(t *testing.T) {
	c, srv := dialPair(t)

	go func() {
		br := bufio.NewReader(srv)
		var req Request
		if err := ReadLineJSON(br, &req); err != nil || req.Cmd != "subscribe" {
			return
		}
		write := func(v any) { json.NewEncoder(srv).Encode(v) } //nolint:errcheck
		write(Event{Event: "snapshot", Items: []Item{{ID: "a", URL: "u", State: StateQueued}}})
		write(Event{Event: "update", Item: &Item{ID: "a", State: StateDownloading, Progress: 10}})

		// A pause request arrives next; sandwich its response between events
		// to prove the client routes by shape rather than by luck.
		var req2 Request
		if err := ReadLineJSON(br, &req2); err != nil {
			return
		}
		write(Event{Event: "update", Item: &Item{ID: "a", State: StateDownloading, Progress: 20}})
		write(Response{OK: true})
		write(Event{Event: "update", Item: &Item{ID: "a", State: StateDownloading, Progress: 30}})
	}()

	snap, err := c.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Event != "snapshot" || len(snap.Items) != 1 || snap.Items[0].ID != "a" {
		t.Fatalf("snapshot = %+v", snap)
	}

	type recv struct {
		ev  Event
		err error
	}
	events := make(chan recv, 8)
	go func() {
		for {
			ev, err := c.NextEvent()
			events <- recv{ev, err}
			if err != nil {
				return
			}
		}
	}()

	resp, err := c.Call(Request{Cmd: "pause", ID: "a"})
	if err != nil {
		t.Fatalf("Call must receive its own response despite interleaved events: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}

	got := 0
	deadline := time.After(2 * time.Second)
	for got < 3 {
		select {
		case r := <-events:
			if r.err != nil {
				t.Fatalf("event stream error: %v", r.err)
			}
			if r.ev.Item == nil || r.ev.Item.Progress != float64((got+1)*10) {
				t.Fatalf("event %d out of order: %+v", got, r.ev)
			}
			got++
		case <-deadline:
			t.Fatalf("only %d/3 events arrived", got)
		}
	}
}

func TestCallTimesOutWhenServerSilent(t *testing.T) {
	old := callTimeout
	callTimeout = 250 * time.Millisecond
	defer func() { callTimeout = old }()

	c, _ := dialPair(t)
	start := time.Now()
	if _, err := c.Call(Request{Cmd: "list"}); err == nil {
		t.Fatal("expected timeout error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestTimedOutConnectionCannotMisrouteLateResponse(t *testing.T) {
	old := callTimeout
	callTimeout = 80 * time.Millisecond
	defer func() { callTimeout = old }()

	c, srv := dialPair(t)
	go func() {
		br := bufio.NewReader(srv)
		var req Request
		if err := ReadLineJSON(br, &req); err != nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
		_ = json.NewEncoder(srv).Encode(Response{OK: true, ID: "late-first-response"})
	}()
	if _, err := c.Call(Request{Cmd: "first"}); err == nil {
		t.Fatal("first call must time out")
	}
	if resp, err := c.Call(Request{Cmd: "second"}); err == nil {
		t.Fatalf("timed-out connection was reused and returned %+v", resp)
	}
}

func TestReadLineJSONOversizedLineKeepsFraming(t *testing.T) {
	input := `{"junk":"` + string(make([]byte, maxLine+50)) + `"}` + "\n" + `{"ok":true}` + "\n"
	br := bufio.NewReader(strings.NewReader(input))
	var resp Response
	if err := ReadLineJSON(br, &resp); !errors.Is(err, errTooLarge) {
		t.Fatalf("first read err = %v, want too-large", err)
	}
	// The stream must remain usable after an oversized line.
	if err := ReadLineJSON(br, &resp); err != nil {
		t.Fatalf("second read err = %v", err)
	}
	if !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestSubscribeSnapshotIsStrictlyDecoded(t *testing.T) {
	c, srv := dialPair(t)
	go func() {
		br := bufio.NewReader(srv)
		var req map[string]any
		_ = json.NewDecoder(br).Decode(&req)
		json.NewEncoder(srv).Encode(map[string]any{"event": "snapshot", "bogus_field": 1}) //nolint:errcheck
	}()
	if _, err := c.Subscribe(); err == nil {
		t.Fatal("Subscribe must reject snapshots with unknown fields")
	}
}

func TestDefaultSocketPathFallsBackToCacheDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/caches")
	if got := DefaultSocketPath(); got != filepath.Join("/caches", "yt-dlp-manager", SocketName) {
		t.Errorf("path = %q, want cache dir fallback", got)
	}
}

func TestLegacySocketPathUsesLegacyName(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/caches")
	if got := LegacySocketPath(); got != filepath.Join("/caches", "yt-dlp-tui", "yt-dlp-tui.sock") {
		t.Errorf("legacy path = %q, want pre-rename fallback", got)
	}
}

func TestPumpSurvivesOversizedGarbage(t *testing.T) {
	c, srv := dialPair(t)
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		var req Request
		if err := ReadLineJSON(br, &req); err != nil {
			return
		}
		junk := make([]byte, maxLine+1000)
		for i := range junk {
			junk[i] = 'x'
		}
		srv.Write(append(junk, '\n'))                   //nolint:errcheck
		json.NewEncoder(srv).Encode(Response{OK: true}) //nolint:errcheck
	}()
	resp, err := c.Call(Request{Cmd: "ping"})
	if err != nil {
		t.Fatalf("conn must survive an oversized line: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}
}
