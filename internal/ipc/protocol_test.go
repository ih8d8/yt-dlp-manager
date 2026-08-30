package ipc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRequestResponseRoundTrip(t *testing.T) {
	req := Request{Cmd: "add", URL: "https://example.com"}
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := ReadLineJSON(bufio.NewReader(bytes.NewReader(line)), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req, got) {
		t.Errorf("round trip = %+v, want %+v", got, req)
	}
}

func TestReadLineJSONRejectsUnknownFields(t *testing.T) {
	line := []byte(`{"cmd":"add","evil":1}` + "\n")
	var req Request
	if err := ReadLineJSON(bufio.NewReader(bytes.NewReader(line)), &req); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestReadLineJSONRejectsTrailingValue(t *testing.T) {
	line := []byte(`{"cmd":"list"}{"cmd":"delete","id":"victim"}` + "\n")
	var req Request
	if err := ReadLineJSON(bufio.NewReader(bytes.NewReader(line)), &req); err == nil {
		t.Fatal("expected error for multiple JSON values on one message line")
	}
}

func TestReadLineJSONRejectsNonObject(t *testing.T) {
	for _, line := range []string{"null\n", "[]\n", `"string"` + "\n"} {
		var req Request
		if err := ReadLineJSON(bufio.NewReader(strings.NewReader(line)), &req); err == nil {
			t.Errorf("accepted non-object message %q", line)
		}
	}
}

func TestReadLineJSONRejectsOversized(t *testing.T) {
	big := bytes.Repeat([]byte("a"), maxLine+10)
	line := append(append([]byte{'"'}, big...), []byte("\"\n")...)
	var out map[string]any
	if err := ReadLineJSON(bufio.NewReader(bytes.NewReader(line)), &out); err == nil {
		t.Fatal("expected error for oversized line")
	}
}

func TestDefaultSocketPathUsesRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	if got := DefaultSocketPath(); got != "/run/user/1234/"+SocketName {
		t.Errorf("path = %q", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := DefaultSocketPath(); got == "" {
		t.Error("fallback path must not be empty")
	}
}

func TestEventIsProgress(t *testing.T) {
	st := StateDownloading
	ev := Event{Event: "update", Item: &Item{State: st}}
	if !ev.IsProgress() {
		t.Error("downloading update should be progress")
	}
	fin := Event{Event: "update", Item: &Item{State: StateCompleted}}
	if fin.IsProgress() {
		t.Error("completed update is not progress")
	}
}

func TestValidURLRejectsTerminalSpoofingControls(t *testing.T) {
	for _, u := range []string{"https://x/\u009b31m", "https://x/\u202eevil"} {
		if ValidURL(u) {
			t.Errorf("ValidURL(%q) accepted a terminal-spoofing control", u)
		}
	}
}

func TestItemJSONShape(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	it := Item{ID: "x", URL: "u", State: StateDownloading, AddedAt: now}
	data, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"x","url":"u","state":"downloading","progress":0,"added_at":"` + now.Format(time.RFC3339Nano) + `"}`
	if string(data) != want {
		t.Errorf("json = %s\nwant  %s", data, want)
	}
}
