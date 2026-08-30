package tui

import (
	"errors"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"yt-dlp-manager/internal/ipc"
)

type (
	eventMsg struct {
		ev  ipc.Event
		err error
	}
	actionMsg struct {
		ok   bool
		note string
	}
)

func (m model) Init() tea.Cmd {
	return waitEvent(m.be)
}

func waitEvent(be backend) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-be.Events()
		if !ok {
			return eventMsg{err: errors.New("event stream closed")}
		}
		if ev.Event == "__closed" {
			return eventMsg{err: errors.New("connection lost")}
		}
		return eventMsg{ev: ev}
	}
}

func doAction(be backend, cmd, id, url string) tea.Cmd {
	return func() tea.Msg {
		if err := be.Do(cmd, id, url); err != nil {
			return actionMsg{ok: false, note: err.Error()}
		}
		return actionMsg{ok: true}
	}
}

func batchActions(be backend, cmd string, ids []string) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(ids))
	for _, id := range ids {
		cmds = append(cmds, doAction(be, cmd, id, ""))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func matches(msg tea.KeyMsg, k keyDef) bool {
	for _, s := range k.keys {
		if msg.String() == s {
			return true
		}
	}
	return false
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = clamp(m.width-28, 20, 80)
		return m, nil

	case eventMsg:
		if msg.err != nil {
			m.online = false
			m.status = "manager connection lost — restart the UI"
			return m, nil
		}
		m.apply(msg.ev)
		return m, waitEvent(m.be)

	case actionMsg:
		if !msg.ok && msg.note != "" {
			m.status = msg.note
			m.statusOK = false
		} else {
			m.status = ""
		}
		return m, nil

	case tea.KeyMsg:
		if m.mode == modeAdd {
			switch msg.String() {
			case "enter":
				url := strings.TrimSpace(m.input.Value())
				m.mode = modeNormal
				m.input.Reset()
				if url == "" {
					return m, nil
				}
				return m, doAction(m.be, "add", "", url)
			case "esc":
				m.mode = modeNormal
				m.input.Reset()
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

		switch {
		case matches(msg, m.keys.Quit):
			return m, tea.Quit

		case matches(msg, m.keys.Help):
			m.showHelp = !m.showHelp

		case matches(msg, m.keys.Add):
			m.mode = modeAdd
			m.input.Focus()
			m.input.SetValue("")
			return m, textinput.Blink

		case matches(msg, m.keys.Up):
			m.move(-1)

		case matches(msg, m.keys.Down):
			m.move(1)

		case matches(msg, m.keys.Select):
			if id := m.cursorID(); id != "" {
				m.chosen[id] = !m.chosen[id]
			}

		case matches(msg, m.keys.ClearSel):
			switch {
			case len(m.chosen) > 0:
				m.chosen = make(map[string]bool)
			case m.showHelp:
				m.showHelp = false
			case m.showInfo:
				m.showInfo = false
			}

		case matches(msg, m.keys.Pause):
			return m, batchActions(m.be, "pause", m.targets())

		case matches(msg, m.keys.Resume):
			return m, batchActions(m.be, "resume", m.targets())

		case matches(msg, m.keys.StartNow):
			return m, batchActions(m.be, "start_now", m.targets())

		case matches(msg, m.keys.Remove):
			return m, batchActions(m.be, "remove", m.targets())

		case matches(msg, m.keys.ClearFin):
			return m, doAction(m.be, "clear_finished", "", "")

		case matches(msg, m.keys.ClearAll):
			return m, doAction(m.be, "clear_all", "", "")

		case matches(msg, m.keys.Info):
			m.showInfo = !m.showInfo
		}
	}
	return m, nil
}

func (m *model) apply(ev ipc.Event) {
	switch ev.Event {
	case "snapshot":
		m.items = make(map[string]ipc.Item, len(ev.Items))
		for _, it := range ev.Items {
			m.items[it.ID] = it
		}
		m.chosen = make(map[string]bool)
	case "update":
		if ev.Item != nil {
			m.items[ev.Item.ID] = *ev.Item
		}
	case "removed":
		prev := m.viewList()
		idx := -1
		for i, it := range prev {
			if it.ID == ev.ID {
				idx = i
				break
			}
		}
		delete(m.items, ev.ID)
		delete(m.chosen, ev.ID)
		if m.selID == ev.ID {
			m.selID = ""
			rest := m.viewList()
			if len(rest) > 0 {
				idx = clamp(idx, 0, len(rest)-1)
				m.selID = rest[idx].ID
			}
		}
	}
	m.ensureCursor()
}

func (m *model) ensureCursor() {
	if _, ok := m.items[m.selID]; ok {
		return
	}
	m.selID = ""
	list := m.viewList()
	if len(list) > 0 {
		m.selID = list[0].ID
	}
}

func (m *model) viewList() []ipc.Item {
	list := make([]ipc.Item, 0, len(m.items))
	for _, it := range m.items {
		list = append(list, it)
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rank(list[i].State), rank(list[j].State)
		if ri != rj {
			return ri < rj
		}
		return list[i].AddedAt.Before(list[j].AddedAt)
	})
	return list
}

func (m *model) move(delta int) {
	list := m.viewList()
	if len(list) == 0 {
		return
	}
	idx := -1
	for i, it := range list {
		if it.ID == m.selID {
			idx = i
			break
		}
	}
	if idx == -1 {
		idx = 0
	} else {
		idx += delta
	}
	idx = clamp(idx, 0, len(list)-1)
	m.selID = list[idx].ID
}

func (m *model) cursorID() string { return m.selID }

func (m *model) targets() []string {
	if len(m.chosen) == 0 {
		if m.selID == "" {
			return nil
		}
		return []string{m.selID}
	}
	list := m.viewList()
	ids := make([]string, 0, len(m.chosen))
	for _, it := range list {
		if m.chosen[it.ID] {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

func rank(s ipc.State) int {
	switch s {
	case ipc.StateDownloading:
		return 0
	case ipc.StateQueued:
		return 1
	case ipc.StatePaused:
		return 2
	case ipc.StateFailed:
		return 3
	default:
		return 4
	}
}
