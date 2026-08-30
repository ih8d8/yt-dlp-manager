package tui

import (
	"github.com/charmbracelet/bubbles/textinput"

	"yt-dlp-manager/internal/ipc"
)

type mode int

const (
	modeNormal mode = iota
	modeAdd
)

type model struct {
	be       backend
	items    map[string]ipc.Item
	selID    string
	chosen   map[string]bool
	online   bool
	status   string
	statusOK bool
	showInfo bool
	showHelp bool
	mode     mode
	input    textinput.Model
	keys     keymap
	width    int
	height   int
}

func newModel(be backend) model {
	ti := textinput.New()
	ti.Placeholder = "paste a video URL…"
	ti.CharLimit = 4096
	ti.Width = 60
	return model{
		be:     be,
		items:  make(map[string]ipc.Item),
		chosen: make(map[string]bool),
		keys:   newKeys(),
		input:  ti,
		online: true,
	}
}
