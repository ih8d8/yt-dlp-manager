package human

import "testing"

func TestBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{-1, "?"},
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
	}
	for _, c := range cases {
		if got := Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEta(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{-1, "--:--"},
		{0, "00:00"},
		{59, "00:59"},
		{61, "01:01"},
		{3661, "1:01:01"},
	}
	for _, c := range cases {
		if got := Eta(c.in); got != c.want {
			t.Errorf("Eta(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSpeed(t *testing.T) {
	if got := Speed(0); got != "-" {
		t.Errorf("Speed(0) = %q", got)
	}
	if got := Speed(2048); got != "2.0 KiB/s" {
		t.Errorf("Speed(2048) = %q", got)
	}
}

func TestTruncateTitle(t *testing.T) {
	if got := TruncateTitle("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := TruncateTitle("abcdef", 4); got != "abc…" {
		t.Errorf("got %q", got)
	}
	if got := TruncateTitle("a\nb", 10); got != "a b" {
		t.Errorf("got %q", got)
	}
}
