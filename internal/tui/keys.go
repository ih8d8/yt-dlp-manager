package tui

type keyDef struct {
	keys []string
	help string
	desc string
}

type keymap struct {
	Add      keyDef
	Up       keyDef
	Down     keyDef
	Select   keyDef
	ClearSel keyDef
	Pause    keyDef
	Resume   keyDef
	StartNow keyDef
	Remove   keyDef
	ClearFin keyDef
	ClearAll keyDef
	Info     keyDef
	Help     keyDef
	Quit     keyDef
}

func newKeys() keymap {
	return keymap{
		Add:      keyDef{[]string{"a"}, "a", "add"},
		Up:       keyDef{[]string{"up", "k"}, "↑/↓", "up"},
		Down:     keyDef{[]string{"down", "j"}, "↓/j", "down"},
		Select:   keyDef{[]string{" "}, "space", "select"},
		ClearSel: keyDef{[]string{"esc"}, "esc", "unselect"},
		Pause:    keyDef{[]string{"s"}, "s", "pause"},
		Resume:   keyDef{[]string{"r"}, "r", "resume"},
		StartNow: keyDef{[]string{"n"}, "n", "start now"},
		Remove:   keyDef{[]string{"d"}, "d", "remove"},
		ClearFin: keyDef{[]string{"c"}, "c", "clear finished"},
		ClearAll: keyDef{[]string{"C"}, "C", "clear all"},
		Info:     keyDef{[]string{"i"}, "i", "info"},
		Help:     keyDef{[]string{"?"}, "?", "keys"},
		Quit:     keyDef{[]string{"q", "ctrl+c"}, "q", "quit"},
	}
}
