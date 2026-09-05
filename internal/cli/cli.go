package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"yt-dlp-manager/internal/human"
	"yt-dlp-manager/internal/inbox"
	"yt-dlp-manager/internal/ipc"
)

const usage = `usage: yt-dlp-manager <command> [args]

  add [URL|-]               add url (default: clipboard)
  list                      show downloads
  pause <id>                pause (keeps .part, resumable)
  resume <id>               re-queue paused download
  start-now <id>            force start ahead of the queue (up to 8 over the limit)
  cancel|remove <id>        remove from list (stops if running, clears partials)
  clear-finished            drop completed, failed and files-deleted rows
  clear-all                 stop everything and empty the list

ids are shown by 'yt-dlp-manager list'; commands need a running manager —
an open TUI, 'yt-dlp-manager daemon', or 'yt-dlp-manager server'.`

const maxURLBytes = 4096

// IsClientCommand reports whether the first argument routes to this package
// rather than a lifecycle mode.
func IsClientCommand(cmd string) bool {
	switch cmd {
	case "add", "list", "ls", "pause", "resume", "cancel", "remove", "rm",
		"start-now", "startnow", "clear-finished", "clear", "clear-all":
		return true
	}
	return false
}

func Run(args []string) int {
	if len(args) == 0 {
		fmt.Println(usage)
		return 2
	}
	conn, connErr := dialManager()

	cmd, rest := args[0], args[1:]
	if cmd == "add" && connErr != nil {
		return handleOfflineAdd(rest)
	}
	if connErr != nil {
		return die("manager not running — open 'yt-dlp-manager tui', 'server', or run 'yt-dlp-manager daemon' first")
	}
	defer conn.Close()

	switch cmd {
	case "add":
		return handleAdd(conn, rest)
	case "list", "ls":
		return handleList(conn)
	case "pause", "resume", "cancel", "remove", "rm", "start-now", "startnow":
		if len(rest) != 1 {
			return die("usage: yt-dlp-manager %s <id>", cmd)
		}
		c := map[string]string{
			"pause": "pause", "resume": "resume", "cancel": "cancel",
			"remove": "remove", "rm": "remove",
			"start-now": "start_now", "startnow": "start_now",
		}[cmd]
		resp, err := conn.Call(ipc.Request{Cmd: c, ID: rest[0]})
		return exitOnErr(resp, err)
	case "clear-finished", "clear":
		resp, err := conn.Call(ipc.Request{Cmd: "clear_finished"})
		return exitOnErr(resp, err)
	case "clear-all":
		resp, err := conn.Call(ipc.Request{Cmd: "clear_all"})
		return exitOnErr(resp, err)
	default:
		fmt.Println(usage)
		return 2
	}
}

// dialManager connects to the current control socket, falling back to the
// legacy pre-rename path when a pre-rename daemon is still serving.
func dialManager() (*ipc.Conn, error) {
	c, err := ipc.Dial(ipc.DefaultSocketPath())
	if err == nil {
		return c, nil
	}
	if legacy := ipc.LegacySocketPath(); legacy != ipc.DefaultSocketPath() {
		if lc, lerr := ipc.Dial(legacy); lerr == nil {
			return lc, nil
		}
	}
	return nil, err
}

func handleOfflineAdd(args []string) int {
	raw, code := resolveURLArg(args)
	if code != 0 {
		return code
	}
	return appendOffline(raw, inbox.DefaultPath())
}

// resolveURLArg extracts the URL from positional args: '-' reads stdin and an
// empty argument list falls back to the clipboard.
func resolveURLArg(args []string) (string, int) {
	var raw string
	switch {
	case len(args) >= 1 && args[0] == "-":
		data, err := readURLInput(os.Stdin)
		if err != nil {
			return "", die("read stdin: %v", err)
		}
		raw = data
	case len(args) >= 1:
		raw = strings.Join(args, " ")
	default:
		cb, err := clipboard()
		if err != nil {
			return "", die("no url given and clipboard unavailable: %v", err)
		}
		raw = cb
	}
	return strings.TrimSpace(raw), 0
}

// appendOffline validates raw and parks it in the persistent inbox file.
func appendOffline(raw, inboxPath string) int {
	if raw == "" {
		return die("empty url")
	}
	if !ipc.ValidURL(raw) {
		return die("invalid url")
	}
	if err := inbox.Append(inboxPath, raw); err != nil {
		return die("inbox: %v", err)
	}
	fmt.Println("queued offline — will start next time you open yt-dlp-manager")
	return 0
}

func handleAdd(conn *ipc.Conn, args []string) int {
	raw, code := resolveURLArg(args)
	if code != 0 {
		return code
	}
	resp, err := conn.Call(ipc.Request{Cmd: "add", URL: raw})
	if code := exitOnErr(resp, err); code != 0 {
		return code
	}
	fmt.Printf("added %s\n", resp.ID)
	return 0
}

func readURLInput(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxURLBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxURLBytes {
		return "", errors.New("url exceeds 4096 bytes")
	}
	return string(data), nil
}

func handleList(conn *ipc.Conn) int {
	resp, err := conn.Call(ipc.Request{Cmd: "list"})
	if code := exitOnErr(resp, err); code != 0 {
		return code
	}
	if len(resp.Items) == 0 {
		fmt.Println("no downloads")
		return 0
	}
	fmt.Printf("%-7s %-12s %6s %10s %8s  %s\n", "ID", "STATE", "%", "SPEED", "ETA", "TITLE")
	for _, it := range resp.Items {
		title := it.Title
		if title == "" {
			title = it.URL
		}
		note := ""
		if it.Error != "" {
			note = "  [" + firstLine(it.Error) + "]"
		}
		speed, eta := "-", "-"
		if it.State == ipc.StateDownloading {
			speed = human.Speed(it.Speed)
			eta = human.Eta(it.ETA)
		}
		fmt.Printf("%-7s %-12s %5.1f %10s %8s  %s%s\n",
			it.ID, it.State, it.Progress, speed, eta,
			human.TruncateTitle(title, 60), note)
	}
	return 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clipboard() (string, error) {
	tools := [][]string{
		{"wl-paste", "--no-newline"},
		{"xclip", "-selection", "clipboard", "-o"},
		{"xsel", "-b", "-o"},
	}
	var lastErr error
	for _, t := range tools {
		out, err := exec.Command(t[0], t[1:]...).Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			if s != "" {
				return s, nil
			}
			lastErr = errors.New("clipboard empty")
			continue
		}
		lastErr = err
	}
	return "", lastErr
}

func exitOnErr(resp ipc.Response, err error) int {
	if err != nil {
		return die("daemon communication failed: %v", err)
	}
	if !resp.OK {
		return die("error: %s", resp.Error)
	}
	return 0
}

func die(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	return 1
}
