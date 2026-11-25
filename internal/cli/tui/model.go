package tui

import (
	"fmt"
	"strings"

	"moorage/internal/cli/client"
)

type Pane string

const (
	PaneLeft  Pane = "left"
	PaneRight Pane = "right"
)

type Tab string

const (
	TabContainers Tab = "containers"
	TabLogs       Tab = "logs"
	TabCompose    Tab = "compose"
	TabHistory    Tab = "history"
	TabChecks     Tab = "checks"
)

var tabOrder = []Tab{TabContainers, TabLogs, TabCompose, TabHistory, TabChecks}

type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyUp
	KeyDown
	KeyPageUp
	KeyPageDown
	KeyEnter
	KeyEsc
	KeyTab
	KeyBackspace
	KeyCtrlC
	KeyCtrlR
)

type KeyEvent struct {
	Kind KeyKind
	Rune rune
}

type ConfirmAction struct {
	Action string // "up", "down", "restart", "stop", "pull", "update", "rollback"
	Stack  string
	Hash   string
	Title  string
	Body   string
}

// ActionCmd tells the event loop what async API work to run after HandleKey.
type ActionCmd struct {
	Quit           bool
	Refresh        bool
	LoadStack      bool
	RunOp          *ConfirmAction
	ToggleCheckID  int64
}

type Model struct {
	Transport  string
	Version    string
	Hosts      []client.HostEntry
	HostID     string
	Stacks     []client.StackSummary
	Selected   int
	FilterMode bool
	FilterText string

	Pane        Pane
	Tab         Tab
	Detail      *client.Stack
	Stats       map[string]client.ContainerStat
	Logs        []client.LogLine
	Versions    []client.Version
	Checks      []client.Check
	RightScroll int
	HistSel     int
	CheckSel    int

	HelpOpen    bool
	HostModal   bool
	HostSel     int
	Confirm     *ConfirmAction
	StatusMsg   string
	StatusIsErr bool
	Busy        bool
}

func NewModel(transport, version, initialHost string) *Model {
	return &Model{
		Transport: transport,
		Version:   version,
		HostID:    initialHost,
		Pane:      PaneLeft,
		Tab:       TabContainers,
		Stats:     map[string]client.ContainerStat{},
	}
}

func (m *Model) FilteredStacks() []client.StackSummary {
	q := strings.ToLower(strings.TrimSpace(m.FilterText))
	if q == "" {
		return m.Stacks
	}
	out := make([]client.StackSummary, 0, len(m.Stacks))
	for _, s := range m.Stacks {
		name := strings.ToLower(s.Name)
		disp := strings.ToLower(s.DisplayName)
		proj := strings.ToLower(s.CoolifyProject)
		if strings.Contains(name, q) || strings.Contains(disp, q) || strings.Contains(proj, q) {
			out = append(out, s)
		}
	}
	return out
}

func (m *Model) SelectedStack() *client.StackSummary {
	list := m.FilteredStacks()
	if len(list) == 0 {
		return nil
	}
	if m.Selected < 0 {
		m.Selected = 0
	}
	if m.Selected >= len(list) {
		m.Selected = len(list) - 1
	}
	s := list[m.Selected]
	return &s
}

func (m *Model) StackLabel(s client.StackSummary) string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}

// HandleKey processes one key event and returns any side-effect command plus whether redraw is needed.
func (m *Model) HandleKey(k KeyEvent) (ActionCmd, bool) {
	if k.Kind == KeyCtrlC {
		return ActionCmd{Quit: true}, false
	}

	// Confirm modal takes precedence
	if m.Confirm != nil {
		switch {
		case k.Kind == KeyEsc || (k.Kind == KeyRune && (k.Rune == 'n' || k.Rune == 'N')):
			m.Confirm = nil
			m.StatusMsg = "Cancelled."
			m.StatusIsErr = false
			return ActionCmd{}, true
		case k.Kind == KeyEnter || (k.Kind == KeyRune && (k.Rune == 'y' || k.Rune == 'Y')):
			act := m.Confirm
			m.Confirm = nil
			return ActionCmd{RunOp: act}, true
		}
		return ActionCmd{}, false
	}

	// Help modal
	if m.HelpOpen {
		if k.Kind == KeyEsc || k.Kind == KeyEnter || (k.Kind == KeyRune && (k.Rune == '?' || k.Rune == 'q')) {
			m.HelpOpen = false
			return ActionCmd{}, true
		}
		return ActionCmd{}, false
	}

	// Host picker modal
	if m.HostModal {
		switch {
		case k.Kind == KeyEsc || (k.Kind == KeyRune && (k.Rune == 'q' || k.Rune == 'H')):
			m.HostModal = false
			return ActionCmd{}, true
		case k.Kind == KeyUp || (k.Kind == KeyRune && k.Rune == 'k'):
			if m.HostSel > 0 {
				m.HostSel--
			}
			return ActionCmd{}, true
		case k.Kind == KeyDown || (k.Kind == KeyRune && k.Rune == 'j'):
			if m.HostSel+1 < len(m.Hosts) {
				m.HostSel++
			}
			return ActionCmd{}, true
		case k.Kind == KeyEnter:
			if m.HostSel >= 0 && m.HostSel < len(m.Hosts) {
				m.HostID = m.Hosts[m.HostSel].ID
				m.Selected = 0
				m.RightScroll = 0
				m.Detail = nil
				m.HostModal = false
				return ActionCmd{Refresh: true}, true
			}
		}
		return ActionCmd{}, false
	}

	// Filter input mode
	if m.FilterMode {
		switch k.Kind {
		case KeyEsc:
			m.FilterMode = false
			m.FilterText = ""
			m.Selected = 0
			return ActionCmd{LoadStack: true}, true
		case KeyEnter:
			m.FilterMode = false
			m.Selected = 0
			return ActionCmd{LoadStack: true}, true
		case KeyBackspace:
			if len(m.FilterText) > 0 {
				runes := []rune(m.FilterText)
				m.FilterText = string(runes[:len(runes)-1])
				m.Selected = 0
			}
			return ActionCmd{}, true
		case KeyRune:
			if k.Rune >= ' ' {
				m.FilterText += string(k.Rune)
				m.Selected = 0
				return ActionCmd{}, true
			}
		}
		return ActionCmd{}, false
	}

	if k.Kind == KeyCtrlR {
		return ActionCmd{Refresh: true}, true
	}

	switch k.Kind {
	case KeyTab:
		if m.Pane == PaneLeft {
			m.Pane = PaneRight
		} else {
			m.Pane = PaneLeft
		}
		return ActionCmd{}, true

	case KeyUp:
		return m.moveCursor(-1)
	case KeyDown:
		return m.moveCursor(1)
	case KeyPageUp:
		m.RightScroll -= 10
		if m.RightScroll < 0 {
			m.RightScroll = 0
		}
		return ActionCmd{}, true
	case KeyPageDown:
		m.RightScroll += 10
		return ActionCmd{}, true
	case KeyEnter:
		if m.Pane == PaneLeft {
			m.Pane = PaneRight
			return ActionCmd{LoadStack: true}, true
		}
		if m.Tab == TabHistory && m.HistSel >= 0 && m.HistSel < len(m.Versions) {
			st := m.SelectedStack()
			if st != nil {
				v := m.Versions[m.HistSel]
				short := v.Short
				if short == "" && len(v.Hash) >= 7 {
					short = v.Hash[:7]
				}
				m.Confirm = &ConfirmAction{
					Action: "rollback",
					Stack:  st.Name,
					Hash:   v.Hash,
					Title:  fmt.Sprintf("Rollback %s to %s?", m.StackLabel(*st), short),
					Body:   "Restores compose.yaml/.env to this version and deploys it.",
				}
				return ActionCmd{}, true
			}
		}
		return ActionCmd{}, false

	case KeyRune:
		switch k.Rune {
		case 'q':
			return ActionCmd{Quit: true}, false
		case '?':
			m.HelpOpen = true
			return ActionCmd{}, true
		case '/':
			m.FilterMode = true
			return ActionCmd{}, true
		case 'H':
			m.HostModal = true
			for i, h := range m.Hosts {
				if h.ID == m.HostID {
					m.HostSel = i
					break
				}
			}
			return ActionCmd{}, true
		case 'j':
			return m.moveCursor(1)
		case 'k':
			return m.moveCursor(-1)
		case '1', '2', '3', '4', '5':
			idx := int(k.Rune - '1')
			m.Tab = tabOrder[idx]
			m.RightScroll = 0
			return ActionCmd{}, true
		case ' ':
			if m.Tab == TabChecks && m.CheckSel >= 0 && m.CheckSel < len(m.Checks) {
				return ActionCmd{ToggleCheckID: m.Checks[m.CheckSel].ID}, true
			}
		case 'u':
			return m.promptStackAction("up", "Deploy / up", "Runs docker compose up -d on the stack.")
		case 'd':
			return m.promptStackAction("down", "Take down", "Stops and removes containers & networks (volumes stay).")
		case 'r':
			return m.promptStackAction("restart", "Restart", "Restarts running containers in this stack.")
		case 's':
			return m.promptStackAction("stop", "Stop", "Stops running containers without removing them.")
		case 'p':
			return m.promptStackAction("pull", "Pull images", "Pulls latest matching image tags from registry.")
		case 'U':
			return m.promptStackAction("update", "Pull & recreate", "Pulls images and recreates updated containers.")
		case 'R':
			return ActionCmd{Refresh: true}, true
		}
	}
	return ActionCmd{}, false
}

func (m *Model) promptStackAction(action, verb, body string) (ActionCmd, bool) {
	st := m.SelectedStack()
	if st == nil {
		return ActionCmd{}, false
	}
	if !st.Managed {
		m.StatusMsg = "Stack is read-only / unmanaged."
		m.StatusIsErr = true
		return ActionCmd{}, true
	}
	m.Confirm = &ConfirmAction{
		Action: action,
		Stack:  st.Name,
		Title:  fmt.Sprintf("%s %s?", verb, m.StackLabel(*st)),
		Body:   body,
	}
	return ActionCmd{}, true
}

func (m *Model) moveCursor(delta int) (ActionCmd, bool) {
	if m.Pane == PaneLeft {
		list := m.FilteredStacks()
		if len(list) == 0 {
			return ActionCmd{}, false
		}
		prev := m.Selected
		m.Selected += delta
		if m.Selected < 0 {
			m.Selected = 0
		}
		if m.Selected >= len(list) {
			m.Selected = len(list) - 1
		}
		if m.Selected != prev {
			m.RightScroll = 0
			m.HistSel = 0
			return ActionCmd{LoadStack: true}, true
		}
		return ActionCmd{}, false
	}

	switch m.Tab {
	case TabHistory:
		m.HistSel += delta
		if m.HistSel < 0 {
			m.HistSel = 0
		}
		if m.HistSel >= len(m.Versions) && len(m.Versions) > 0 {
			m.HistSel = len(m.Versions) - 1
		}
	case TabChecks:
		m.CheckSel += delta
		if m.CheckSel < 0 {
			m.CheckSel = 0
		}
		if m.CheckSel >= len(m.Checks) && len(m.Checks) > 0 {
			m.CheckSel = len(m.Checks) - 1
		}
	default:
		m.RightScroll += delta
		if m.RightScroll < 0 {
			m.RightScroll = 0
		}
	}
	return ActionCmd{}, true
}
