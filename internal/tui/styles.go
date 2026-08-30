// Package tui implements the Bubble Tea terminal UI and its startup paths.
// It is a presentation adapter over either an owned manager (standalone mode)
// or a remote manager reached through the Unix IPC socket.
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"yt-dlp-manager/internal/ipc"
)

var (
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	subTitle        = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	totalSpeedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	countActive     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	countQueued     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250"))
	countDone       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	countFailed     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("204"))
	dimStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	frameStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	cursorHighlight = lipgloss.Color("61")
	cursorRow       = lipgloss.NewStyle().Bold(true).Background(cursorHighlight).Foreground(lipgloss.Color("255"))
	selMarkStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true)
	errStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Bold(true)
	okNoteStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	keyChipStyle    = lipgloss.NewStyle().Background(lipgloss.Color("62")).Foreground(lipgloss.Color("255")).Bold(true).Padding(0, 1)
	descChipStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	dialogBox       = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("213")).
			Padding(1, 4)
	helpBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(1, 3)
)

func statePill(s ipc.State) string {
	var color string
	switch s {
	case ipc.StateDownloading:
		color = "36"
	case ipc.StateCompleted:
		color = "35"
	case ipc.StatePaused:
		color = "130"
	case ipc.StateFailed:
		color = "125"
	default:
		color = "240"
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("255")).
		Background(lipgloss.Color(color)).
		Render(pad(strings.ToUpper(string(s)), colState))
}

func barFg(s ipc.State) lipgloss.Style {
	var c string
	switch s {
	case ipc.StateDownloading:
		c = "39"
	case ipc.StateCompleted:
		c = "42"
	case ipc.StatePaused:
		c = "214"
	case ipc.StateFailed:
		c = "204"
	default:
		c = "242"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c))
}
