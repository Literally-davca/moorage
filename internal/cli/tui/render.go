package tui

import (
	"fmt"
	"strings"
)

// Render draws the full Moorage TUI frame for the given terminal dimensions.
func Render(m *Model, width, height int) string {
	if width < 40 {
		width = 40
	}
	if height < 12 {
		height = 12
	}

	var out strings.Builder
	// 1. Header bar
	out.WriteString(renderHeader(m, width))
	out.WriteString("\x1b[K\r\n")

	bodyH := height - 2
	if bodyH < 6 {
		bodyH = 6
	}

	var bodyLines []string
	if m.HelpOpen {
		bodyLines = renderHelpOverlay(width, bodyH)
	} else if m.HostModal {
		bodyLines = renderHostOverlay(m, width, bodyH)
	} else if m.Confirm != nil {
		bodyLines = renderConfirmOverlay(m, width, bodyH)
	} else if width >= 90 {
		leftW := width / 3
		if leftW < 28 {
			leftW = 28
		}
		if leftW > 42 {
			leftW = 42
		}
		rightW := width - leftW - 1
		left := renderStacksBox(m, leftW, bodyH)
		right := renderDetailBox(m, rightW, bodyH)
		bodyLines = make([]string, bodyH)
		for i := 0; i < bodyH; i++ {
			bodyLines[i] = left[i] + " " + right[i]
		}
	} else {
		if m.Pane == PaneLeft {
			bodyLines = renderStacksBox(m, width, bodyH)
		} else {
			bodyLines = renderDetailBox(m, width, bodyH)
		}
	}

	for _, line := range bodyLines {
		out.WriteString(line)
		out.WriteString("\x1b[K\r\n")
	}

	// Footer keybar
	out.WriteString(renderFooter(m, width))
	out.WriteString("\x1b[K")
	return out.String()
}

func renderHeader(m *Model, width int) string {
	hostName := m.HostID
	for _, h := range m.Hosts {
		if h.ID == m.HostID {
			hostName = h.Name
			break
		}
	}
	left := fmt.Sprintf(" ⚓ Moorage CLI  ·  Host: %s (%s)  ·  %d stacks", hostName, m.HostID, len(m.Stacks))
	if m.Busy {
		left += "  [working…]"
	}
	right := m.Transport + " "
	gap := width - len([]rune(left)) - len([]rune(right))
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	return bgBar + fgFoam + ansiBold + padRight(line, width) + ansiReset
}

func renderFooter(m *Model, width int) string {
	var msg string
	color := fgMist
	if m.StatusMsg != "" {
		msg = " " + m.StatusMsg
		if m.StatusIsErr {
			color = fgRed
		} else {
			color = fgGreen
		}
	} else if m.FilterMode {
		msg = fmt.Sprintf(" Filter: %s_  (Enter apply · Esc clear)", m.FilterText)
		color = fgBuoy
	} else {
		msg = " j/k move · Tab pane · 1-5 tabs · / filter · H host · u up · d down · r restart · s stop · U update · ? help · q quit"
	}
	return bgBar + color + padRight(msg, width) + ansiReset
}

func boxFrame(title string, active bool, inner []string, width, height int) []string {
	borderColor := fgHaze
	if active {
		borderColor = fgBuoy
	}
	innerW := width - 2
	if innerW < 4 {
		innerW = 4
	}
	t := " " + title + " "
	fillLen := innerW - len([]rune(t)) - 1
	if fillLen < 0 {
		t = padRight(t, innerW)
		fillLen = 0
	}
	lines := make([]string, 0, height)
	lines = append(lines, borderColor+"┌─"+ansiReset+fgFoam+ansiBold+t+ansiReset+borderColor+strings.Repeat("─", fillLen)+"┐"+ansiReset)
	contentH := height - 2
	for i := 0; i < contentH; i++ {
		var row string
		if i < len(inner) {
			row = inner[i]
		}
		lines = append(lines, borderColor+"│"+ansiReset+padRight(row, innerW)+borderColor+"│"+ansiReset)
	}
	lines = append(lines, borderColor+"└"+strings.Repeat("─", innerW)+"┘"+ansiReset)
	return lines
}

func renderStacksBox(m *Model, width, height int) []string {
	list := m.FilteredStacks()
	title := "Stacks"
	if m.FilterText != "" && !m.FilterMode {
		title = fmt.Sprintf("Stacks (/%s)", m.FilterText)
	}
	innerW := width - 2
	maxRows := height - 2
	start := 0
	if m.Selected >= maxRows && maxRows > 0 {
		start = m.Selected - maxRows + 1
	}
	rows := make([]string, 0, maxRows)
	for i := start; i < len(list) && len(rows) < maxRows; i++ {
		st := list[i]
		selected := i == m.Selected
		glyph := statusColor(st.Status) + statusGlyph(st.Status) + ansiReset
		name := m.StackLabel(st)
		cnt := fmt.Sprintf("%d/%d", st.Running, st.Total)
		lineText := fmt.Sprintf("%s %-*s %s", glyph, max(8, innerW-len(cnt)-5), padRight(name, max(8, innerW-len(cnt)-5)), fgMist+cnt+ansiReset)
		if selected {
			rows = append(rows, bgSel+fgFoam+ansiBold+"▸ "+padRight(name, max(8, innerW-len(cnt)-5))+" "+cnt+ansiReset)
		} else {
			rows = append(rows, "  "+lineText)
		}
	}
	if len(rows) == 0 {
		rows = append(rows, fgHaze+"  No stacks found."+ansiReset)
	}
	return boxFrame(title, m.Pane == PaneLeft, rows, width, height)
}

func renderDetailBox(m *Model, width, height int) []string {
	st := m.SelectedStack()
	if st == nil {
		return boxFrame("Details", m.Pane == PaneRight, []string{fgHaze + "Select a stack to inspect." + ansiReset}, width, height)
	}
	tabs := []struct {
		id    Tab
		label string
	}{
		{TabContainers, "1:Containers"},
		{TabLogs, "2:Logs"},
		{TabCompose, "3:Compose"},
		{TabHistory, "4:History"},
		{TabChecks, "5:Checks"},
	}
	var tabBar strings.Builder
	for _, t := range tabs {
		if t.id == m.Tab {
			tabBar.WriteString(fgBuoy + ansiBold + "[" + t.label + "]" + ansiReset + " ")
		} else {
			tabBar.WriteString(fgHaze + " " + t.label + " " + ansiReset + " ")
		}
	}

	var body []string
	body = append(body, tabBar.String())
	if m.Detail != nil {
		meta := fmt.Sprintf("Status: %s%s%s · %d/%d running", statusColor(m.Detail.Status), m.Detail.Status, ansiReset, m.Detail.Running, m.Detail.Total)
		if m.Detail.CoolifyProject != "" {
			meta += fmt.Sprintf(" · Coolify: %s", m.Detail.CoolifyProject)
		}
		if !m.Detail.Managed {
			meta += " · [read-only]"
		}
		body = append(body, fgMist+meta+ansiReset)
	}
	body = append(body, fgHaze+strings.Repeat("─", max(4, width-4))+ansiReset)

	var lines []string
	switch m.Tab {
	case TabContainers:
		lines = renderContainersTab(m)
	case TabLogs:
		lines = renderLogsTab(m)
	case TabCompose:
		lines = renderComposeTab(m)
	case TabHistory:
		lines = renderHistoryTab(m)
	case TabChecks:
		lines = renderChecksTab(m)
	}

	avail := height - 2 - len(body)
	if avail < 1 {
		avail = 1
	}
	start := m.RightScroll
	if start > len(lines)-avail && len(lines) > avail {
		start = len(lines) - avail
	}
	if start < 0 {
		start = 0
	}
	for i := start; i < len(lines) && len(body) < height-2; i++ {
		body = append(body, lines[i])
	}
	return boxFrame(m.StackLabel(*st), m.Pane == PaneRight, body, width, height)
}

func renderContainersTab(m *Model) []string {
	if m.Detail == nil {
		return []string{fgHaze + "Loading containers…" + ansiReset}
	}
	if len(m.Detail.Containers) == 0 {
		return []string{fgHaze + "No containers running for this stack." + ansiReset}
	}
	out := make([]string, 0, len(m.Detail.Containers)+2)
	out = append(out, fgFoam+ansiBold+fmt.Sprintf("%-18s %-12s %-10s %-10s %s", "SERVICE", "STATE", "CPU", "MEM", "IMAGE")+ansiReset)
	for _, c := range m.Detail.Containers {
		svc := c.Service
		if svc == "" {
			svc = c.Name
		}
		cpuStr, memStr := "-", "-"
		if st, ok := m.Stats[c.Name]; ok {
			cpuStr = fmt.Sprintf("%.1f%%", st.CPUPercent)
			memStr = fmt.Sprintf("%.0f MiB", float64(st.MemUsage)/(1024*1024))
		}
		stateTxt := c.State
		if c.Health != "" {
			stateTxt += "(" + c.Health + ")"
		}
		out = append(out, fmt.Sprintf("%s%-18s %-12s%s %-10s %-10s %s",
			statusColor(c.State), padRight(svc, 18), padRight(stateTxt, 12), ansiReset, cpuStr, memStr, fgMist+c.Image+ansiReset))
	}
	if len(m.Detail.URLs) > 0 {
		out = append(out, "", fgBuoy+"URLs: "+strings.Join(m.Detail.URLs, ", ")+ansiReset)
	}
	return out
}

func renderLogsTab(m *Model) []string {
	if len(m.Logs) == 0 {
		return []string{fgHaze + "No recent log lines." + ansiReset}
	}
	out := make([]string, 0, len(m.Logs))
	for _, l := range m.Logs {
		prefix := ""
		if l.Service != "" {
			prefix = fgBuoy + "[" + l.Service + "] " + ansiReset
		}
		out = append(out, prefix+l.Msg)
	}
	return out
}

func renderComposeTab(m *Model) []string {
	if m.Detail == nil {
		return []string{fgHaze + "Loading compose.yaml…" + ansiReset}
	}
	if strings.TrimSpace(m.Detail.Compose) == "" {
		return []string{fgHaze + "No compose.yaml available." + ansiReset}
	}
	return strings.Split(strings.ReplaceAll(m.Detail.Compose, "\r\n", "\n"), "\n")
}

func renderHistoryTab(m *Model) []string {
	if len(m.Versions) == 0 {
		return []string{fgHaze + "No recorded versions yet." + ansiReset}
	}
	out := []string{fgMist + "Press Enter to rollback & deploy the highlighted revision:" + ansiReset}
	for i, v := range m.Versions {
		short := v.Short
		if short == "" && len(v.Hash) >= 7 {
			short = v.Hash[:7]
		}
		line := fmt.Sprintf("%-8s %-19s %-12s %s", short, v.At.Local().Format("2006-01-02 15:04"), padRight(v.Author, 12), v.Message)
		if i == m.HistSel {
			out = append(out, bgSel+fgFoam+ansiBold+"▸ "+line+ansiReset)
		} else {
			out = append(out, "  "+line)
		}
	}
	return out
}

func renderChecksTab(m *Model) []string {
	if len(m.Checks) == 0 {
		return []string{fgHaze + "No monitoring checks configured." + ansiReset}
	}
	out := []string{fgMist + "Press Space to pause/resume the highlighted check:" + ansiReset}
	for i, c := range m.Checks {
		st := c.State.Status
		if !c.Enabled {
			st = "paused"
		}
		line := fmt.Sprintf("%s%-10s%s %-24s %s", statusColor(st), padRight(st, 10), ansiReset, padRight(c.Name, 24), fgMist+c.Target+ansiReset)
		if i == m.CheckSel {
			out = append(out, bgSel+fgFoam+ansiBold+"▸ "+line+ansiReset)
		} else {
			out = append(out, "  "+line)
		}
	}
	return out
}

func renderHelpOverlay(width, height int) []string {
	lines := []string{
		fgFoam + ansiBold + "Navigation" + ansiReset,
		"  j / ↓ , k / ↑    Move selection or scroll detail pane",
		"  Tab              Switch focus between Stacks and Detail pane",
		"  1..5             Switch tab (Containers / Logs / Compose / History / Checks)",
		"  PgUp / PgDn      Scroll detail pane by 10 lines",
		"  /                Filter stacks (by name or Coolify display name)",
		"  H                Switch active host",
		"",
		fgFoam + ansiBold + "Actions" + ansiReset,
		"  u / d / r / s    Deploy (up) / Down / Restart / Stop stack (with confirmation)",
		"  p / U            Pull images / Update images & recreate",
		"  Enter (History)  Rollback stack to selected revision",
		"  Space (Checks)   Pause or resume selected monitoring check",
		"  R / Ctrl+R       Refresh now",
		"  q / Esc          Close overlay or quit Moorage CLI",
	}
	return boxFrame("Help (? or Esc to close)", true, lines, width, height)
}

func renderHostOverlay(m *Model, width, height int) []string {
	lines := make([]string, 0, len(m.Hosts)+1)
	for i, h := range m.Hosts {
		state := "online"
		if !h.Connected {
			state = "offline"
		}
		row := fmt.Sprintf("%-20s (%s) · %s", h.Name, h.ID, state)
		if i == m.HostSel {
			lines = append(lines, bgSel+fgFoam+ansiBold+"▸ "+row+ansiReset)
		} else {
			lines = append(lines, "  "+row)
		}
	}
	return boxFrame("Select Host (Enter confirm · Esc cancel)", true, lines, width, height)
}

func renderConfirmOverlay(m *Model, width, height int) []string {
	c := m.Confirm
	lines := []string{
		"",
		fgFoam + ansiBold + "  " + c.Title + ansiReset,
		"  " + fgMist + c.Body + ansiReset,
		"",
		fgBuoy + "  Press [y / Enter] to confirm, or [n / Esc] to cancel." + ansiReset,
	}
	return boxFrame("Confirm Action", true, lines, width, height)
}
