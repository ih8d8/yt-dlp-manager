package tui

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"yt-dlp-manager/internal/config"
	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
	"yt-dlp-manager/internal/service"
)

// Run executes the terminal UI. It attaches to an already-running owner over
// the Unix socket when one exists (trying the legacy pre-rename path as a
// client-only fallback); otherwise it owns a manager plus control socket for
// the lifetime of the UI.
func Run(args []string) int {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	max := fs.Int("max", manager.DefaultMax(), "maximum concurrent downloads")
	socketOverride := fs.String("socket", "", "unix socket path")
	statePath := fs.String("state", "", "state file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "yt-dlp-manager tui: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *max < config.MinConcurrent || *max > config.MaxConcurrent {
		fmt.Fprintf(os.Stderr, "yt-dlp-manager tui: --max must be between %d and %d, got %d\n",
			config.MinConcurrent, config.MaxConcurrent, *max)
		return 2
	}

	path := *socketOverride
	if path == "" {
		path = ipc.DefaultSocketPath()
	}

	if attached, runErr := attach(path); attached {
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "yt-dlp-manager:", runErr)
			return 1
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := service.Start(ctx, service.Options{
		Max:        *max,
		StatePath:  *statePath,
		SocketPath: path,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yt-dlp-manager:", err)
		return 1
	}

	be := newLocalBackend(svc.Manager())
	md := newModel(be)
	switch {
	case svc.Interrupted > 0 && svc.InboxDrained > 0:
		md.status = fmt.Sprintf("%d unfinished download(s) restored paused, added %d from inbox — press r to resume",
			svc.Interrupted, svc.InboxDrained)
		md.statusOK = true
	case svc.Interrupted > 0:
		md.status = fmt.Sprintf("%d unfinished download(s) restored paused — press r to resume", svc.Interrupted)
		md.statusOK = true
	case svc.InboxDrained > 0:
		md.status = fmt.Sprintf("added %d url(s) from inbox", svc.InboxDrained)
		md.statusOK = true
	}

	pg := tea.NewProgram(md, tea.WithAltScreen())
	go func() {
		<-ctx.Done()
		pg.Quit()
	}()

	_, runErr := pg.Run()

	stop()
	svc.Close()

	if runErr != nil {
		fmt.Fprintln(os.Stderr, "yt-dlp-manager:", runErr)
		return 1
	}
	return 0
}

// attach tries to connect and render against an existing owner. The first
// result is true only when a full UI session ran against that owner; the
// second carries any error from that session so the caller can exit non-zero
// instead of reporting success after a crashed UI.
func attach(path string) (bool, error) {
	candidates := []string{path}
	if legacy := ipc.LegacySocketPath(); legacy != path {
		// A live pre-rename daemon is still a valid manager; explicit client
		// launches may use it rather than starting a competing instance.
		candidates = append(candidates, legacy)
	}
	for _, p := range candidates {
		c, err := ipc.Dial(p)
		if err != nil {
			continue
		}
		snap, serr := c.Subscribe()
		if serr != nil {
			c.Close()
			continue
		}
		be := newSocketBackend(c, snap)
		pg := tea.NewProgram(newModel(be), tea.WithAltScreen())
		_, runErr := pg.Run()
		be.Close()
		return true, runErr
	}
	return false, nil
}
