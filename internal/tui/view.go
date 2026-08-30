package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"yt-dlp-manager/internal/human"
	"yt-dlp-manager/internal/ipc"
)

func progressBar(p float64, width int, s ipc.State, cursor bool) string {
	if width < 5 {
		width = 5
	}
	filled := clamp(int(p/100*float64(width-1)), 0, width-1)
	fg := barFg(s)
	track := lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	if cursor {
		fg = fg.Background(cursorHighlight)
		track = track.Background(cursorHighlight)
	}
	return fg.Render(strings.Repeat("█", filled)) + track.Render(strings.Repeat("░", width-1-filled))
}

type chip struct {
	key  string
	desc string
}

var footerChipOrder = []chipDef{
	{"a", "add"},
	{"space", "select"},
	{"d", "remove"},
	{"s", "pause"},
	{"r", "resume"},
	{"n", "now"},
	{"c", "clr fin"},
	{"C", "CLEAR ALL"},
	{"i", "info"},
	{"?", "keys"},
	{"q", "quit"},
}

type chipDef struct{ k, d string }

func renderChips(cs []chipDef) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, keyChipStyle.Render(c.k)+" "+descChipStyle.Render(c.d))
	}
	return strings.Join(parts, " ")
}

var chipDropOrder = []string{"c", "C", "i", "n", "r", "s", "d", "space", "?"}

func fitChips(chips []chipDef, width int) string {
	cur := chips
	line := renderChips(cur)
	for lipgloss.Width(line) > width {
		removed := false
		for _, k := range chipDropOrder {
			for i, c := range cur {
				if c.k == k && len(cur) > 2 {
					next := make([]chipDef, 0, len(cur)-1)
					next = append(next, cur[:i]...)
					next = append(next, cur[i+1:]...)
					cur = next
					removed = true
					break
				}
			}
			if removed {
				break
			}
		}
		if !removed {
			break
		}
		line = renderChips(cur)
	}
	return line
}

func (m model) footerSegs(width int) (string, string) {
	chipsLine := fitChips(footerChipOrder, width)
	var right string
	switch {
	case m.status != "":
		if m.statusOK {
			right = okNoteStyle.Render(m.status)
		} else {
			right = errStyle.Render(m.status)
		}
	case m.mode == modeAdd:
		right = dimStyle.Render("enter start · esc cancel")
	case len(m.chosen) > 0:
		right = selMarkStyle.Render(fmt.Sprintf("%d selected", len(m.chosen)))
	}
	if right == "" {
		chipsLine = fitChips(footerChipOrder, width-4)
	} else if gap := width - lipgloss.Width(chipsLine) - lipgloss.Width(right); gap < 6 {
		chipsLine = fitChips(footerChipOrder, maxInt(width/2, 20))
	}
	return chipsLine, right
}

func edge(cornerL, cornerR, left, right string, w int) string {
	lw := lipgloss.Width(left)
	rw := lipgloss.Width(right)
	fill := w - lw - rw - 8
	for fill < 1 && rw > 0 {
		right = truncateVisible(right, rw-1)
		rw = lipgloss.Width(right)
		fill = w - lw - rw - 8
	}
	for fill < 1 && lw > 0 {
		left = truncateVisible(left, lw-1)
		lw = lipgloss.Width(left)
		fill = w - lw - rw - 8
	}
	if right != "" {
		return frameStyle.Render(cornerL+"─ ") + left + " " +
			frameStyle.Render(strings.Repeat("─", max(fill, 1))) + " " + right +
			frameStyle.Render(" ─"+cornerR)
	}
	return frameStyle.Render(cornerL+"─ ") + left + " " +
		frameStyle.Render(strings.Repeat("─", max(w-lw-7, 1))) +
		frameStyle.Render("──"+cornerR)
}

func truncateVisible(s string, max int) string {
	if lipgloss.Width(s) <= max {
		return s
	}
	runes := []rune(s)
	out := ""
	for _, r := range runes {
		next := out + string(r)
		if lipgloss.Width(next) > max {
			break
		}
		out = next
	}
	return out
}

func (m model) headerSegs() (string, string) {
	running, queued, done, failed := 0, 0, 0, 0
	var totalSpeed int64
	for _, it := range m.items {
		switch it.State {
		case ipc.StateDownloading:
			running++
			if it.Speed > 0 {
				totalSpeed += it.Speed
			}
		case ipc.StateQueued:
			queued++
		case ipc.StateCompleted:
			done++
		case ipc.StateFailed:
			failed++
		}
	}
	left := titleStyle.Render("yt-dlp-manager")
	right := ""
	if running > 0 && totalSpeed > 0 {
		right += totalSpeedStyle.Render("⇣ "+human.Bytes(totalSpeed)+"/s") + " "
	}
	right += countActive.Render(fmt.Sprintf("%d active", running)) + " " +
		countQueued.Render(fmt.Sprintf("%d queued", queued)) + " " +
		countDone.Render(fmt.Sprintf("%d done", done))
	if failed > 0 {
		right += " " + countFailed.Render(fmt.Sprintf("%d failed", failed))
	}
	if !m.online {
		right += " " + errStyle.Render("offline")
	}
	return left, right
}

func (m model) View() string {
	if m.width == 0 || m.height < 5 {
		return ""
	}
	if m.width < 76 {
		msg := dimStyle.Render("terminal too narrow — widen to at least 76 columns")
		return lipgloss.PlaceHorizontal(maxInt(m.width, 1), lipgloss.Center, msg)
	}
	w := m.width - 1
	innerW := w - 4
	frameH := m.height - 1
	contentH := frameH - 2

	var lines []string
	switch {
	case m.mode == modeAdd:
		lines = m.addCard(innerW, contentH)
	case m.showHelp:
		lines = m.helpCard(innerW, contentH)
	default:
		info := m.showInfo && m.selID != ""
		rowsH := contentH
		if info {
			rowsH -= 2
		}
		rowLines := m.rowsLines(innerW, w-2, rowsH)
		for len(rowLines) < rowsH {
			rowLines = append(rowLines, "")
		}
		if info {
			rowLines = rowLines[:rowsH]
			rowLines = append(rowLines, m.infoLines(innerW)...)
		}
		lines = rowLines
	}

	var b strings.Builder
	hLeft, hRight := m.headerSegs()
	hRight = truncateVisible(hRight, maxInt(w/2, 20))
	b.WriteString(edge("╭", "╮", hLeft, hRight, w) + "\n")
	side := frameStyle
	for _, ln := range lines {
		b.WriteString(side.Render("│") + padTo(ln, w-2) + side.Render("│") + "\n")
	}
	fLeft, fRight := m.footerSegs(w - 12)
	fRight = truncateVisible(fRight, maxInt((w-12)/2, 16))
	return b.String() + edge("╰", "╯", fLeft, fRight, w)
}

type rowMetrics struct {
	barW     int
	maxTitle int
}

const (
	colID    = 8
	colState = 13
	colPct   = 7
	colSpeed = 11
	gapPct   = "  "
	gapMeta  = "  "
	gapName  = "   "
	colEta   = 9
)

func rowMetricsFor(innerW int) rowMetrics {
	baseFixed := 1 + 1 + colID + 1 + colState + 1 + 1 + colPct +
		len(gapPct) + colSpeed + len(gapMeta) + colEta + len(gapName)
	rest := innerW - baseFixed
	if rest < 26 {
		rest = 26
	}
	barW := clamp(rest*65/100, 10, 64)
	maxTitle := rest - barW
	if maxTitle < 10 {
		barW -= 10 - maxTitle
		maxTitle = 10
		if barW < 6 {
			barW = 6
		}
	}
	return rowMetrics{barW: barW, maxTitle: maxTitle}
}

func headerBand(innerW int, mt rowMetrics) []string {
	hdrCol := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Bold(true)
	labels := " " +
		hdrCol.Render(pad("ID", colID)) + " " +
		hdrCol.Render(pad("STATE", colState)) + " " +
		hdrCol.Render(pad("PROGRESS", mt.barW)) + " " +
		hdrCol.Render(pad("%", colPct)) + gapPct +
		hdrCol.Render(pad("SPEED", colSpeed)) + gapMeta +
		hdrCol.Render(pad("ETA", colEta)) + gapName +
		hdrCol.Render("NAME")
	rule := dimStyle.Render(strings.Repeat("┈", maxInt(20, innerW)))
	return []string{" " + padTo(labels, innerW), " " + rule}
}

func (m model) rowsLines(innerW, fullW, h int) []string {
	list := m.viewList()
	if len(list) == 0 {
		msg := dimStyle.Render("queue empty — press ") + keyChipStyle.Render(" a ") +
			dimStyle.Render(" to add a URL")
		return []string{"", lipgloss.PlaceHorizontal(maxInt(innerW, 1), lipgloss.Center, msg)}
	}
	mt := rowMetricsFor(innerW)
	lines := []string{""}
	lines = append(lines, headerBand(innerW, mt)...)
	avail := h - len(lines)
	perItem := 2
	maxShown := avail / perItem
	if maxShown < 1 {
		maxShown = 1
	}

	start := 0
	if len(list) > maxShown {
		for i, it := range list {
			if it.ID == m.selID {
				start = i - maxShown/2
				break
			}
		}
		start = clamp(start, 0, len(list)-maxShown)
	}
	shown := list[start:]
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}

	for idx, it := range shown {
		isCursor := it.ID == m.selID
		cell := func(s string, normal *lipgloss.Style) string {
			if isCursor {
				return cursorRow.Render(s)
			}
			if normal == nil {
				return s
			}
			return normal.Render(s)
		}
		marker := cell("○", &dimStyle)
		if m.chosen[it.ID] {
			marker = selMarkStyle.Render("◉")
		}
		title := it.Title
		if title == "" {
			title = it.URL
		}
		speed, eta := "-", "-"
		if it.State == ipc.StateDownloading {
			speed = human.Speed(it.Speed)
			eta = human.Eta(it.ETA)
		}
		dim := dimStyle
		row := marker + " " + cell(pad(it.ID, colID), &dim) + " " +
			statePill(it.State) + " " +
			progressBar(it.Progress, mt.barW, it.State, isCursor) + " " +
			cell(pad(fmt.Sprintf("%.1f%%", it.Progress), colPct), nil) + gapPct +
			cell(pad(speed, colSpeed), nil) + gapMeta +
			cell(pad(eta, colEta), nil) + gapName +
			cell(truncateVisible(title, mt.maxTitle), nil)
		if isCursor {
			row += cursorRow.Render(strings.Repeat(" ", maxInt(0, fullW-lipgloss.Width(row))))
		} else {
			row = padTo(row, fullW)
		}
		lines = append(lines, row)
		if idx < len(shown)-1 {
			lines = append(lines, "")
		}
	}
	return lines
}

func (m model) infoLines(innerW int) []string {
	it, ok := m.items[m.selID]
	if !ok {
		return nil
	}
	lines := []string{dimStyle.Render(strings.Repeat("┄", maxInt(20, innerW)))}
	if it.Error != "" {
		lines = append(lines, " "+errStyle.Render("error: "+firstLine(it.Error)))
	} else {
		lines = append(lines, " "+dimStyle.Render(truncateVisible(it.URL, innerW)))
	}
	return lines
}

func (m model) helpCard(innerW, innerH int) []string {
	box := helpBoxStyle.Render(renderHelpRows(helpRows))
	return centeredCard(box, innerW, innerH)
}

func (m model) addCard(innerW, innerH int) []string {
	box := dialogBox.Render(
		titleStyle.Render("add download") + "\n\n" +
			"  " + m.input.View() + "\n\n" +
			descChipStyle.Render("enter start · esc cancel"))
	return centeredCard(box, innerW, innerH)
}

func centeredCard(box string, innerW, innerH int) []string {
	boxLines := strings.Split(box, "\n")
	if len(boxLines) > innerH {
		boxLines = boxLines[:maxInt(innerH, 0)]
	}
	padTop := (innerH - len(boxLines)) / 2
	if padTop < 0 {
		padTop = 0
	}
	var lines []string
	for i := 0; i < padTop; i++ {
		lines = append(lines, "")
	}
	for _, bl := range boxLines {
		lines = append(lines, lipgloss.PlaceHorizontal(maxInt(innerW, 1), lipgloss.Center, bl))
	}
	for len(lines) < innerH {
		lines = append(lines, "")
	}
	return lines[:innerH]
}

func renderHelpRows(rows [][2]string) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("keybindings") + "\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %-14s %s\n", keyChipStyle.Render(r[0]), descChipStyle.Render(r[1])))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

var helpRows = [][2]string{
	{"a", "open add-url dialog"},
	{"↑ / ↓  j/k", "move cursor"},
	{"space", "toggle multi-select on row"},
	{"esc", "clear selection / close overlays"},
	{"s", "pause selected item(s)"},
	{"r", "resume paused / retry failed item(s)"},
	{"n", "force start now (jumps the queue)"},
	{"d", "remove selected from list (keeps finished files)"},
	{"c", "clear finished / failed / deleted rows"},
	{"C", "stop and clear EVERYTHING"},
	{"i", "toggle info pane"},
	{"?", "toggle this help"},
	{"q", "quit — downloads pause, state kept"},
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func padTo(s string, w int) string {
	d := w - lipgloss.Width(s)
	if d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
