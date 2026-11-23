package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// accept upgrades to a WebSocket. coder/websocket rejects a cross-origin Origin
// by default, which blocks cross-site WebSocket hijacking (an issue Dozzle had).
func accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, bool) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return nil, false
	}
	c.SetReadLimit(4096)
	return c, true
}

// wsLogs streams log lines of a stack, batched every 150 ms.
func (s *Server) wsLogs(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	stack, ctr := q.Get("stack"), q.Get("container")
	tail, _ := strconv.Atoi(q.Get("tail"))
	if tail <= 0 || tail > 5000 {
		tail = 300
	}
	c, ok := accept(w, r)
	if !ok {
		return
	}
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
	go func() { done <- h.StreamLogs(ctx, stack, ctr, tail, emit) }()

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
			if err != nil && ctx.Err() == nil {
				msg = "No running containers to follow."
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

// wsOp streams the output of a compose operation until it finishes.
func (s *Server) wsOp(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	c, ok := accept(w, r)
	if !ok {
		return
	}
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
