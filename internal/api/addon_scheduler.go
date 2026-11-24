package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
)

// CronField represents one parsed 5-field cron component.
type cronSchedule struct {
	min   [60]bool
	hour  [24]bool
	dom   [32]bool
	month [13]bool
	dow   [7]bool
}

func parseCronField(expr string, minV, maxV int, dst []bool) error {
	for _, part := range strings.Split(strings.TrimSpace(expr), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return errors.New("empty cron field part")
		}
		step := 1
		if rng, stepStr, hasStep := strings.Cut(part, "/"); hasStep {
			n, err := strconv.Atoi(strings.TrimSpace(stepStr))
			if err != nil || n <= 0 {
				return fmt.Errorf("invalid cron step %q", stepStr)
			}
			step = n
			part = rng
		}
		start, end := minV, maxV
		if part == "*" {
			// full range
		} else if a, b, isRange := strings.Cut(part, "-"); isRange {
			var err error
			start, err = strconv.Atoi(strings.TrimSpace(a))
			if err != nil {
				return fmt.Errorf("invalid cron number %q", a)
			}
			end, err = strconv.Atoi(strings.TrimSpace(b))
			if err != nil {
				return fmt.Errorf("invalid cron number %q", b)
			}
			if start < minV || end > maxV || start > end {
				return fmt.Errorf("cron range %d-%d out of bounds [%d..%d]", start, end, minV, maxV)
			}
		} else {
			n, err := strconv.Atoi(part)
			if err != nil {
				return fmt.Errorf("invalid cron value %q", part)
			}
			if maxV == 6 && n == 7 {
				n = 0 // Sunday can be 0 or 7
			}
			if n < minV || n > maxV {
				return fmt.Errorf("cron value %d out of bounds [%d..%d]", n, minV, maxV)
			}
			start, end = n, n
		}
		for i := start; i <= end; i += step {
			dst[i] = true
		}
	}
	return nil
}

func parseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, errors.New("cron expression must have 5 fields: minute hour day month weekday")
	}
	var c cronSchedule
	if err := parseCronField(fields[0], 0, 59, c.min[:]); err != nil {
		return nil, err
	}
	if err := parseCronField(fields[1], 0, 23, c.hour[:]); err != nil {
		return nil, err
	}
	if err := parseCronField(fields[2], 1, 31, c.dom[:]); err != nil {
		return nil, err
	}
	if err := parseCronField(fields[3], 1, 12, c.month[:]); err != nil {
		return nil, err
	}
	if err := parseCronField(fields[4], 0, 6, c.dow[:]); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *cronSchedule) matches(t time.Time) bool {
	return c.min[t.Minute()] &&
		c.hour[t.Hour()] &&
		c.dom[t.Day()] &&
		c.month[int(t.Month())] &&
		c.dow[int(t.Weekday())]
}

type taskScheduler struct {
	srv     *Server
	mu      sync.Mutex
	running map[int64]bool
}

func (s *Server) StartScheduler(ctx context.Context) {
	ts := &taskScheduler{srv: s, running: map[int64]bool{}}
	go ts.loop(ctx)
}

func (ts *taskScheduler) loop(ctx context.Context) {
	lastMinute := ""
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			key := now.Format("2006-01-02T15:04")
			if key == lastMinute {
				continue
			}
			lastMinute = key
			ts.tick(ctx, now)
		}
	}
}

func (ts *taskScheduler) tick(ctx context.Context, now time.Time) {
	if ts.srv == nil || ts.srv.store == nil {
		return
	}
	on, err := ts.srv.store.AddonEnabled(ctx, addonStackScheduler)
	if err != nil || !on {
		return
	}
	tasks, err := ts.srv.store.ScheduledTasks(ctx)
	if err != nil {
		return
	}
	for _, task := range tasks {
		if !task.Enabled {
			continue
		}
		cs, err := parseCron(task.Cron)
		if err != nil || !cs.matches(now) {
			continue
		}
		go ts.runTask(ctx, task, "scheduler")
	}
}

func (ts *taskScheduler) runTask(parentCtx context.Context, task store.ScheduledTask, actor string) {
	ts.mu.Lock()
	if ts.running[task.ID] {
		ts.mu.Unlock()
		return
	}
	ts.running[task.ID] = true
	ts.mu.Unlock()
	defer func() {
		ts.mu.Lock()
		delete(ts.running, task.ID)
		ts.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(parentCtx, 15*time.Minute)
	defer cancel()

	s := ts.srv
	h, meta, err := s.reg.Get(ctx, task.HostID)
	var out string
	if err == nil && meta.ReadOnly {
		err = host.ErrHostReadOnly
	}
	if err == nil {
		if s.mon != nil && (task.Action == "restart" || task.Action == "stop" || task.Action == "pull-up") {
			s.mon.StackEvent(task.HostID, task.Stack, "deploy", actor, "Scheduled "+task.Action+": "+task.Name)
		}
		switch task.Action {
		case "restart", "start", "stop":
			err = runAndWait(ctx, h, task.Stack, task.Action, actor)
		case "pull-up":
			err = runAndWait(ctx, h, task.Stack, "update", actor)
		case "exec":
			ce, ok := h.(host.CommandExecutor)
			if !ok {
				err = errors.New("host agent does not support exec")
			} else {
				var code int
				out, code, err = ce.ExecCommand(ctx, task.Stack, task.Service, task.Command)
				if err == nil && code != 0 {
					err = fmt.Errorf("exit code %d", code)
				}
			}
		default:
			err = fmt.Errorf("unknown action %q", task.Action)
		}
	}

	status := "ok"
	outcome := store.AuditOK
	if err != nil {
		status = "failed: " + err.Error()
		outcome = store.AuditFailed
		if out == "" {
			out = err.Error()
		}
	}
	_ = s.store.RecordScheduledTaskRun(ctx, task.ID, status, out)
	if s.mon != nil {
		s.mon.StackEvent(task.HostID, task.Stack, "scheduler", actor, fmt.Sprintf("%s (%s): %s", task.Name, task.Action, status))
	}
	_ = s.store.Audit(ctx, store.AuditEntry{
		At:      time.Now(),
		Actor:   actor,
		Action:  "scheduler.run",
		HostID:  task.HostID,
		Stack:   task.Stack,
		Target:  task.Name,
		Outcome: outcome,
	})
	if err != nil {
		s.notifyEvent(ctx, notify.EventDeployFailed, notify.Message{
			Status: "down",
			Title:  fmt.Sprintf("Scheduled task failed: %s", task.Name),
			Text:   fmt.Sprintf("Task %s (%s on %s/%s) failed: %v", task.Name, task.Action, task.HostID, task.Stack, err),
			URL:    s.stackLink(task.HostID, task.Stack),
		})
	}
}

// ---- HTTP handlers ---------------------------------------------------------------------

func (s *Server) listSchedulerTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ScheduledTasks(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	if tasks == nil {
		tasks = []store.ScheduledTask{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) saveSchedulerTask(w http.ResponseWriter, r *http.Request) {
	var req store.ScheduledTask
	if !readJSON(w, r, &req) {
		return
	}
	if r.Method == http.MethodPut {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "Invalid task id.")
			return
		}
		req.ID = id
	}
	req.Name = strings.TrimSpace(req.Name)
	req.HostID = strings.TrimSpace(req.HostID)
	req.Stack = strings.TrimSpace(req.Stack)
	req.Cron = strings.TrimSpace(req.Cron)
	req.Action = strings.TrimSpace(req.Action)
	req.Service = strings.TrimSpace(req.Service)
	req.Command = strings.TrimSpace(req.Command)

	if req.Name == "" || req.HostID == "" || req.Stack == "" {
		writeErr(w, http.StatusBadRequest, "Name, host and stack are required.")
		return
	}
	if _, err := parseCron(req.Cron); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch req.Action {
	case "restart", "start", "stop", "pull-up":
	case "exec":
		if req.Command == "" {
			writeErr(w, http.StatusBadRequest, "Command is required for exec tasks.")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "Action must be restart, start, stop, pull-up or exec.")
		return
	}

	saved, err := s.store.SaveScheduledTask(r.Context(), req)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Task not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, saved.Name)
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) deleteSchedulerTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid task id.")
		return
	}
	if err := s.store.DeleteScheduledTask(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Task not found.")
		return
	} else if err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runSchedulerTaskNow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "Invalid task id.")
		return
	}
	task, err := s.store.ScheduledTaskByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Task not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	ts := &taskScheduler{srv: s, running: map[int64]bool{}}
	go ts.runTask(context.Background(), task, currentUser(r).Username)
	setAuditTarget(r, task.Name)
	w.WriteHeader(http.StatusAccepted)
}
