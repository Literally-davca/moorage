package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

const maxLogDownloadBytes = 50 << 20 // 50 MiB cap per FEATURE-DESIGN 1.4.3

var safeLogNameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// accept upgrades to a WebSocket with the Origin rules of acceptWS, which block
// cross-site WebSocket hijacking (an issue Dozzle had), and registers it as a live
// connection, so a revoked sign-in closes it (4401). Call the returned func when done.
func (s *Server) accept(w http.ResponseWriter, r *http.Request, kind string) (*websocket.Conn, func(), bool) {
	c, err := s.acceptWS(w, r, websocket.CompressionContextTakeover)
	if err != nil {
		return nil, nil, false
	}
	c.SetReadLimit(4096)
	untrack := s.trackWS(r, kind, false, func() { _ = c.Close(closeSessionEnded, sessionEndedReason) })
	return c, untrack, true
}

// wsLogs streams log lines of a stack or across the host, batched every 150 ms.
func (s *Server) wsLogs(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	stack, ctr := strings.TrimSpace(q.Get("stack")), strings.TrimSpace(q.Get("container"))
	since, until := strings.TrimSpace(q.Get("since")), strings.TrimSpace(q.Get("until"))
	tail, _ := strconv.Atoi(q.Get("tail"))
	if tail <= 0 || tail > 5000 {
		if since != "" || until != "" {
			tail = 0
		} else {
			tail = 300
		}
	}
	follow := until == ""
	if f := q.Get("follow"); f == "0" || f == "false" {
		follow = false
	} else if f == "1" || f == "true" {
		follow = true
	}
	c, untrack, ok := s.accept(w, r, "logs")
	if !ok {
		return
	}
	defer untrack()
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // client messages are not expected; this tracks disconnects

	var (
		mu  sync.Mutex
		buf []engine.LogLine
	)
	emit := func(l engine.LogLine) {
		mu.Lock()
		if len(buf) < 20_000 { // drop when the client can't keep up
			buf = append(buf, l)
		}
		mu.Unlock()
	}

	done := make(chan error, 1)
	go func() {
		if lq, ok := h.(host.LogQuerier); ok && (stack == "" || since != "" || until != "" || !follow) {
			done <- lq.QueryLogs(ctx, engine.LogOptions{
				Stack:     stack,
				Container: ctr,
				Tail:      tail,
				Since:     since,
				Until:     until,
				Follow:    follow,
			}, emit)
			return
		}
		done <- h.StreamLogs(ctx, stack, ctr, tail, emit)
	}()

	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	flush := func() error {
		mu.Lock()
		out := buf
		buf = nil
		mu.Unlock()
		if len(out) == 0 {
			return nil
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return wsjson.Write(wctx, c, map[string]any{"lines": out})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-done:
			flush() //nolint:errcheck
			msg := "Log stream ended."
			if !follow && err == nil {
				msg = "Loaded historical range."
			} else if err != nil && ctx.Err() == nil {
				msg = "No matching containers to read."
			}
			wsjson.Write(ctx, c, map[string]any{"end": msg}) //nolint:errcheck
			c.Close(websocket.StatusNormalClosure, "")
			return
		case <-tick.C:
			if err := flush(); err != nil {
				return
			}
		}
	}
}

// logsHistory fetches a bounded slice of historical log lines (e.g. Load older lines or From–To).
func (s *Server) logsHistory(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	stack := strings.TrimSpace(q.Get("stack"))
	ctr := strings.TrimSpace(q.Get("container"))
	since := strings.TrimSpace(q.Get("since"))
	until := strings.TrimSpace(q.Get("until"))
	tail, _ := strconv.Atoi(q.Get("tail"))
	if tail < 0 || tail > 5000 {
		tail = 300
	}
	if tail == 0 && since == "" {
		tail = 300
	}

	lq, ok := h.(host.LogQuerier)
	if !ok {
		if stack != "" {
			lines, err := h.TailLogs(r.Context(), stack, min(max(tail, 1), 200))
			if err != nil {
				s.engErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "truncated": false})
			return
		}
		writeErr(w, http.StatusNotImplemented, "Update the agent on this host to query historical logs.")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	const maxHistoryLines = 5000
	var (
		mu        sync.Mutex
		lines     = make([]engine.LogLine, 0, 256)
		truncated bool
	)
	err := lq.QueryLogs(ctx, engine.LogOptions{
		Stack:     stack,
		Container: ctr,
		Tail:      tail,
		Since:     since,
		Until:     until,
		Follow:    false,
	}, func(l engine.LogLine) {
		mu.Lock()
		if len(lines) >= maxHistoryLines {
			truncated = true
			cancel()
		} else {
			lines = append(lines, l)
		}
		mu.Unlock()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		if err.Error() == "no containers" {
			writeJSON(w, http.StatusOK, map[string]any{"lines": []engine.LogLine{}, "truncated": false})
			return
		}
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "truncated": truncated})
}

// logsDownload streams plain-text .log output capped at 50 MiB and records an R1 audit entry.
func (s *Server) logsDownload(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	hostID := r.PathValue("host")
	q := r.URL.Query()
	stack := strings.TrimSpace(q.Get("stack"))
	ctr := strings.TrimSpace(q.Get("container"))
	since := strings.TrimSpace(q.Get("since"))
	until := strings.TrimSpace(q.Get("until"))
	tail, _ := strconv.Atoi(q.Get("tail"))
	if tail < 0 {
		tail = 0
	}

	lq, ok := h.(host.LogQuerier)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "Update the agent on this host to download logs.")
		return
	}

	label := stack
	if label == "" {
		label = hostID
	}
	if ctr != "" && !strings.Contains(ctr, ",") {
		label += "-" + ctr
	}
	label = strings.Trim(safeLogNameRe.ReplaceAllString(label, "-"), "-")
	if label == "" {
		label = "logs"
	}
	fname := fmt.Sprintf("moorage-%s-%s.log", label, time.Now().UTC().Format("20060102-150405"))

	target := ctr
	if target == "" {
		target = "all"
	}
	s.audit(r.Context(), store.AuditEntry{
		At:      time.Now(),
		Actor:   currentUser(r).Username,
		IP:      clientIP(r),
		Action:  auditLogsDownload,
		HostID:  hostID,
		Stack:   stack,
		Target:  target,
		Outcome: store.AuditOK,
	})

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fname))
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var (
		mu      sync.Mutex
		written int64
		capped  bool
	)
	_ = lq.QueryLogs(ctx, engine.LogOptions{
		Stack:     stack,
		Container: ctr,
		Tail:      tail,
		Since:     since,
		Until:     until,
		Follow:    false,
	}, func(l engine.LogLine) {
		mu.Lock()
		defer mu.Unlock()
		if capped {
			return
		}
		src := l.Container
		if l.Project != "" {
			svc := l.Service
			if svc == "" {
				svc = l.Container
			}
			src = l.Project + "/" + svc
		}
		var sb strings.Builder
		if l.Time != "" {
			sb.WriteString(l.Time)
			sb.WriteByte(' ')
		}
		if src != "" {
			sb.WriteByte('[')
			sb.WriteString(src)
			sb.WriteString("] ")
		}
		if l.Stream == "stderr" {
			sb.WriteString("(stderr) ")
		}
		sb.WriteString(l.Message)
		sb.WriteByte('\n')
		chunk := []byte(sb.String())
		if written+int64(len(chunk)) > maxLogDownloadBytes {
			rem := maxLogDownloadBytes - written
			if rem > 0 {
				_, _ = w.Write(chunk[:rem])
			}
			_, _ = w.Write([]byte("\n--- truncated at 50 MiB limit ---\n"))
			capped = true
			cancel()
			return
		}
		n, err := w.Write(chunk)
		written += int64(n)
		if err != nil {
			cancel()
			return
		}
		if flusher != nil && written%(64<<10) < int64(len(chunk)) {
			flusher.Flush()
		}
	})
	if flusher != nil {
		flusher.Flush()
	}
}

// wsOp streams the output of a compose operation until it finishes.
func (s *Server) wsOp(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	c, untrack, ok := s.accept(w, r, "op")
	if !ok {
		return
	}
	defer untrack()
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())
	err := h.FollowOp(ctx, r.PathValue("id"), func(ev host.OpEvent) error {
		return wsjson.Write(ctx, c, ev)
	})
	if err != nil && ctx.Err() == nil {
		msg := "Lost the connection to this operation."
		if errors.Is(err, host.ErrOpNotFound) {
			msg = "Operation not found."
		}
		wsjson.Write(ctx, c, host.OpEvent{Done: true, Error: msg}) //nolint:errcheck
	}
	c.Close(websocket.StatusNormalClosure, "")
}
