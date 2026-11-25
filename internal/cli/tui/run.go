package tui

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"moorage/internal/cli/client"
)

// Run launches the interactive lazydocker-style TUI until the user quits or ctx is cancelled.
func Run(ctx context.Context, c *client.Client, version, initialHost string) error {
	scr, err := EnterScreen()
	if err != nil {
		return err
	}
	defer scr.Close()

	hostID, err := c.ResolveHost(ctx, initialHost)
	if err != nil {
		hostID = "local"
	}

	m := NewModel(c.TransportDescription(), version, hostID)
	refreshData(ctx, c, m)

	keys := make(chan KeyEvent, 32)
	resize := make(chan struct{}, 4)
	done := make(chan struct{})
	defer close(done)

	startKeyReader(keys, done)
	stopResize := watchResize(resize)
	defer stopResize()

	draw := func() {
		w, h := scr.Size()
		scr.Draw(Render(m, w, h))
	}
	draw()

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-resize:
			draw()
		case <-tick.C:
			if !m.Busy && m.Confirm == nil {
				refreshData(ctx, c, m)
				draw()
			}
		case k := <-keys:
			cmd, dirty := m.HandleKey(k)
			if cmd.Quit {
				return nil
			}
			if cmd.Refresh {
				m.Busy = true
				draw()
				refreshData(ctx, c, m)
				m.Busy = false
				dirty = true
			} else if cmd.LoadStack {
				loadSelectedDetail(ctx, c, m)
				dirty = true
			}
			if cmd.RunOp != nil {
				m.Busy = true
				draw()
				execOp(ctx, c, m, cmd.RunOp)
				refreshData(ctx, c, m)
				m.Busy = false
				dirty = true
			}
			if cmd.ToggleCheckID > 0 {
				toggleCheck(ctx, c, m, cmd.ToggleCheckID)
				refreshChecks(ctx, c, m)
				dirty = true
			}
			if dirty {
				draw()
			}
		}
	}
}

func refreshData(ctx context.Context, c *client.Client, m *Model) {
	if hosts, err := c.Hosts(ctx); err == nil {
		m.Hosts = hosts
		if m.HostID == "" && len(hosts) > 0 {
			m.HostID = hosts[0].ID
		}
	}
	c.LoadCoolifyNames(ctx)
	var stResp struct {
		Stacks []client.StackSummary `json:"stacks"`
	}
	if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(m.HostID)+"/stacks", &stResp); err != nil {
		m.StatusMsg = err.Error()
		m.StatusIsErr = true
	} else {
		for i := range stResp.Stacks {
			stResp.Stacks[i].DisplayName = c.StackDisplayName(m.HostID, stResp.Stacks[i].Name, stResp.Stacks[i].DisplayName)
		}
		m.Stacks = stResp.Stacks
	}
	refreshChecks(ctx, c, m)
	loadSelectedDetail(ctx, c, m)
}

func refreshChecks(ctx context.Context, c *client.Client, m *Model) {
	var checks []client.Check
	if err := c.Get(ctx, "/api/checks", &checks); err == nil {
		m.Checks = checks
	}
}

func loadSelectedDetail(ctx context.Context, c *client.Client, m *Model) {
	sel := m.SelectedStack()
	if sel == nil {
		m.Detail = nil
		m.Stats = map[string]client.ContainerStat{}
		m.Logs = nil
		m.Versions = nil
		return
	}
	base := "/api/hosts/" + url.PathEscape(m.HostID) + "/stacks/" + url.PathEscape(sel.Name)
	var det client.Stack
	if err := c.Get(ctx, base, &det); err == nil {
		det.DisplayName = c.StackDisplayName(m.HostID, det.Name, det.DisplayName)
		m.Detail = &det
	}
	var stats []client.ContainerStat
	if err := c.Get(ctx, base+"/stats", &stats); err == nil {
		mp := make(map[string]client.ContainerStat, len(stats))
		for _, s := range stats {
			mp[s.Name] = s
		}
		m.Stats = mp
	}
	var logsResp struct {
		Lines []client.LogLine `json:"lines"`
	}
	if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(m.HostID)+"/logs/history?stack="+url.QueryEscape(sel.Name)+"&tail=100", &logsResp); err == nil {
		m.Logs = logsResp.Lines
	}
	var vers []client.Version
	if err := c.Get(ctx, base+"/history", &vers); err == nil {
		m.Versions = vers
	}
}

func execOp(ctx context.Context, c *client.Client, m *Model, act *ConfirmAction) {
	hEsc := url.PathEscape(m.HostID)
	sEsc := url.PathEscape(act.Stack)
	if act.Action == "rollback" {
		var resp map[string]any
		if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/restore", hEsc, sEsc), map[string]any{
			"hash":   act.Hash,
			"deploy": true,
		}, &resp); err != nil {
			m.StatusMsg = err.Error()
			m.StatusIsErr = true
			return
		}
		m.StatusMsg = fmt.Sprintf("Rolled back & deployed %s.", act.Stack)
		m.StatusIsErr = false
		return
	}
	var op client.Op
	if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/actions/%s", hEsc, sEsc, url.PathEscape(act.Action)), nil, &op); err != nil {
		m.StatusMsg = err.Error()
		m.StatusIsErr = true
		return
	}
	m.StatusMsg = fmt.Sprintf("Started %s on %s.", act.Action, act.Stack)
	m.StatusIsErr = false
}

func toggleCheck(ctx context.Context, c *client.Client, m *Model, id int64) {
	for _, ch := range m.Checks {
		if ch.ID != id {
			continue
		}
		next := !ch.Enabled
		payload := map[string]any{
			"name":     ch.Name,
			"kind":     ch.Kind,
			"target":   ch.Target,
			"hostId":   ch.HostID,
			"stack":    ch.Stack,
			"interval": ch.Interval,
			"enabled":  next,
		}
		if err := c.Put(ctx, fmt.Sprintf("/api/checks/%d", id), payload, nil); err != nil {
			m.StatusMsg = err.Error()
			m.StatusIsErr = true
		} else {
			m.StatusMsg = fmt.Sprintf("Check %q enabled=%v.", ch.Name, next)
			m.StatusIsErr = false
		}
		return
	}
}
