package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

const terminalWSReadLimit = 64 << 10

type terminalSessionDTO struct {
	store.TerminalAuditRow
	HostName string `json:"hostName,omitempty"`
}

func (s *Server) listTerminalSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID := strings.TrimSpace(r.URL.Query().Get("host"))
	stack := strings.TrimSpace(r.URL.Query().Get("stack"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.store.TerminalSessions(ctx, hostID, stack, limit)
	if err != nil {
		s.engErr(w, err)
		return
	}
	hostNames := map[string]string{}
	if s.reg != nil {
		if hs, err := s.reg.List(ctx); err == nil {
			for _, h := range hs {
				hostNames[h.ID] = h.Name
			}
		}
	}
	out := make([]terminalSessionDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, terminalSessionDTO{
			TerminalAuditRow: row,
			HostName:         hostNames[row.HostID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listStackTerminals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hostID, stack := r.PathValue("host"), r.PathValue("name")
	rows, err := s.store.TerminalSessions(ctx, hostID, stack, 100)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if rows == nil {
		rows = []store.TerminalAuditRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) wsTerminal(w http.ResponseWriter, r *http.Request) {
	// Upgrade first: a browser can't read the HTTP status of a refused WebSocket, so every
	// refusal after this point is an error frame the terminal shows. Accept refuses a
	// cross-origin Origin with 403 itself (cross-site WebSocket hijacking).
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(terminalWSReadLimit)
	refuse := func(msg string) {
		b, _ := json.Marshal(host.TermControl{Type: "error", Message: msg})
		wctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_ = ws.Write(wctx, websocket.MessageText, b)
		_ = ws.Close(websocket.StatusPolicyViolation, "refused")
	}

	// write=false: the host itself refuses read-only and terminal_disabled (the agent, not the hub,
	// enforces the switch).
	h, _, msg := s.resolveHost(r, false)
	if h == nil {
		refuse(msg)
		return
	}
	hostID := r.PathValue("host")
	stack := r.PathValue("name")
	q := r.URL.Query()
	containerRef := strings.TrimSpace(q.Get("container"))
	shell := strings.TrimSpace(q.Get("shell"))
	if shell == "" {
		shell = "sh"
	}
	if _, ok := engine.AllowedShells[shell]; !ok {
		refuse(sentence(engine.ErrInvalidShell))
		return
	}
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	cols, rows = engine.ClampTermSize(cols, rows)

	select {
	case s.termSem <- struct{}{}:
		defer func() { <-s.termSem }()
	default:
		refuse("Too many open terminals. Close one and try again.")
		return
	}
	s.termWG.Add(1)
	defer s.termWG.Done()

	ts, ctr, err := h.Terminal(r.Context(), stack, containerRef, shell, cols, rows)
	if err != nil {
		if host.IsUnsupported(err) {
			refuse("Update the agent to use the terminal.")
			return
		}
		_, msg := s.errStatus(err)
		refuse(msg)
		return
	}

	user := currentUser(r).Username
	ip := clientIP(r)
	cookieVal := ""
	if c, err := r.Cookie(cookieName); err == nil {
		cookieVal = c.Value
	}
	ctrName := ctr.Name
	if ctrName == "" {
		ctrName = containerRef
	}
	svcName := ctr.Service
	if svcName == "" {
		svcName = ctrName
	}

	startedAt := time.Now()
	// The global audit log gets one row per opened shell; the session details go to
	// terminal_sessions below.
	s.audit(r.Context(), store.AuditEntry{
		At: startedAt, Actor: user, IP: ip, Action: auditTermOpen,
		HostID: hostID, Stack: stack, Target: ctrName + " · " + shell,
	})
	// The row is written at the start so a hub crash still leaves a trace.
	auditID, err := s.store.StartTerminalSession(r.Context(), store.TerminalAuditRow{
		User:          user,
		IP:            ip,
		HostID:        hostID,
		Stack:         stack,
		ContainerName: ctrName,
		Service:       svcName,
		Shell:         shell,
		StartedAt:     startedAt,
	})
	if err != nil {
		s.log.Warn("audit terminal start", "err", err)
	}
	// Kind "terminal" is not in monitor.DeployKinds: it never silences checks or adds a deploy anchor.
	if s.mon != nil {
		s.mon.StackEvent(hostID, stack, "terminal", user, svcName+" · "+shell)
	}
	s.log.Info("terminal session started",
		"user", user, "ip", ip, "host", hostID, "stack", stack,
		"container", ctrName, "service", svcName, "shell", shell,
	)

	s.serveTerminalSession(r.Context(), ws, ts, auditID, cookieVal, user, ip, hostID, stack, ctrName, shell, startedAt)
}

func (s *Server) wsContainerTerminal(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(terminalWSReadLimit)
	refuse := func(msg string) {
		b, _ := json.Marshal(host.TermControl{Type: "error", Message: msg})
		wctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_ = ws.Write(wctx, websocket.MessageText, b)
		_ = ws.Close(websocket.StatusPolicyViolation, "refused")
	}

	h, _, msg := s.resolveHost(r, false)
	if h == nil {
		refuse(msg)
		return
	}
	ct, ok := h.(host.ContainerTerminalHost)
	if !ok {
		refuse("Update the agent on this host to open a container terminal.")
		return
	}
	hostID := r.PathValue("host")
	containerRef := strings.TrimSpace(r.PathValue("id"))
	q := r.URL.Query()
	shell := strings.TrimSpace(q.Get("shell"))
	if shell == "" {
		shell = "sh"
	}
	if _, ok := engine.AllowedShells[shell]; !ok {
		refuse(sentence(engine.ErrInvalidShell))
		return
	}
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	cols, rows = engine.ClampTermSize(cols, rows)

	select {
	case s.termSem <- struct{}{}:
		defer func() { <-s.termSem }()
	default:
		refuse("Too many open terminals. Close one and try again.")
		return
	}
	s.termWG.Add(1)
	defer s.termWG.Done()

	ts, ctr, err := ct.ContainerTerminal(r.Context(), containerRef, shell, cols, rows)
	if err != nil {
		if host.IsUnsupported(err) {
			refuse("Update the agent on this host to open a container terminal.")
			return
		}
		_, msg := s.errStatus(err)
		refuse(msg)
		return
	}

	user := currentUser(r).Username
	ip := clientIP(r)
	cookieVal := ""
	if c, err := r.Cookie(cookieName); err == nil {
		cookieVal = c.Value
	}
	ctrName := ctr.Name
	if ctrName == "" {
		ctrName = containerRef
	}
	svcName := ctr.Service
	if svcName == "" {
		svcName = ctrName
	}
	stack := ctr.Project
	if stack == "" {
		stack = "@standalone"
	}

	startedAt := time.Now()
	s.audit(r.Context(), store.AuditEntry{
		At: startedAt, Actor: user, IP: ip, Action: auditTermOpen,
		HostID: hostID, Stack: stack, Target: ctrName + " · " + shell,
	})
	auditID, err := s.store.StartTerminalSession(r.Context(), store.TerminalAuditRow{
		User:          user,
		IP:            ip,
		HostID:        hostID,
		Stack:         stack,
		ContainerName: ctrName,
		Service:       svcName,
		Shell:         shell,
		StartedAt:     startedAt,
	})
	if err != nil {
		s.log.Warn("audit container terminal start", "err", err)
	}
	if s.mon != nil && ctr.Project != "" {
		s.mon.StackEvent(hostID, ctr.Project, "terminal", user, svcName+" · "+shell)
	}
	s.log.Info("container terminal session started",
		"user", user, "ip", ip, "host", hostID, "stack", stack,
		"container", ctrName, "service", svcName, "shell", shell,
	)

	s.serveTerminalSession(r.Context(), ws, ts, auditID, cookieVal, user, ip, hostID, stack, ctrName, shell, startedAt)
}

func (s *Server) wsHostTerminal(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(terminalWSReadLimit)
	refuse := func(msg string) {
		b, _ := json.Marshal(host.TermControl{Type: "error", Message: msg})
		wctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_ = ws.Write(wctx, websocket.MessageText, b)
		_ = ws.Close(websocket.StatusPolicyViolation, "refused")
	}

	h, _, msg := s.resolveHost(r, false)
	if h == nil {
		refuse(msg)
		return
	}
	ht, ok := h.(host.HostTerminalProvider)
	if !ok {
		refuse("Update the agent on this host to use the host terminal.")
		return
	}
	hostID := r.PathValue("host")
	q := r.URL.Query()
	shell := strings.TrimSpace(q.Get("shell"))
	if shell == "" {
		shell = "bash"
	}
	if _, ok := engine.AllowedShells[shell]; !ok {
		refuse(sentence(engine.ErrInvalidShell))
		return
	}
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	cols, rows = engine.ClampTermSize(cols, rows)

	select {
	case s.termSem <- struct{}{}:
		defer func() { <-s.termSem }()
	default:
		refuse("Too many open terminals. Close one and try again.")
		return
	}
	s.termWG.Add(1)
	defer s.termWG.Done()

	ts, err := ht.HostTerminal(r.Context(), shell, cols, rows)
	if err != nil {
		if host.IsUnsupported(err) {
			refuse("Update the agent on this host to use the host terminal.")
			return
		}
		_, msg := s.errStatus(err)
		refuse(msg)
		return
	}

	user := currentUser(r).Username
	ip := clientIP(r)
	cookieVal := ""
	if c, err := r.Cookie(cookieName); err == nil {
		cookieVal = c.Value
	}
	hostLabel := s.hostNames(r.Context())[hostID]
	if hostLabel == "" {
		hostLabel = hostID
	}

	startedAt := time.Now()
	s.audit(r.Context(), store.AuditEntry{
		At: startedAt, Actor: user, IP: ip, Action: auditHostTermOpen,
		HostID: hostID, Stack: "@host", Target: hostLabel + " · " + shell,
	})
	auditID, err := s.store.StartTerminalSession(r.Context(), store.TerminalAuditRow{
		User:          user,
		IP:            ip,
		HostID:        hostID,
		Stack:         "@host",
		ContainerName: hostLabel,
		Service:       "host-os",
		Shell:         shell,
		StartedAt:     startedAt,
	})
	if err != nil {
		s.log.Warn("audit host terminal start", "err", err)
	}
	s.log.Info("host terminal session started",
		"user", user, "ip", ip, "host", hostID, "shell", shell,
	)

	s.serveTerminalSession(r.Context(), ws, ts, auditID, cookieVal, user, ip, hostID, "@host", hostLabel, shell, startedAt)
}

func (s *Server) serveTerminalSession(
	reqCtx context.Context,
	ws *websocket.Conn,
	ts engine.TerminalSession,
	auditID int64,
	cookieVal, user, ip, hostID, stack, ctrName, shell string,
	startedAt time.Time,
) {
	baseCtx := reqCtx
	hubCtx := s.termCfg.HubCtx
	ctx, cancel := context.WithCancel(baseCtx)

	var (
		mu       sync.Mutex
		reason   string
		exitCode *int
	)
	setOutcome := func(r string, code *int) {
		mu.Lock()
		if reason == "" {
			reason = r
		}
		if code != nil && exitCode == nil {
			c := *code
			exitCode = &c
		}
		mu.Unlock()
	}

	var writeMu sync.Mutex
	writeFrame := func(typ websocket.MessageType, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		return ws.Write(wctx, typ, data)
	}
	sendCtrl := func(ctrl host.TermControl) {
		b, _ := json.Marshal(ctrl)
		_ = writeFrame(websocket.MessageText, b)
	}

	var cleanOnce sync.Once
	cleanup := func(r string, code *int) {
		setOutcome(r, code)
		cleanOnce.Do(func() {
			cancel()
			_ = ts.Close()
			_ = ws.CloseNow()
		})
	}

	idleTimeout := s.termCfg.IdleTimeout
	hardLimit := s.termCfg.HardLimit
	revokeInt := s.termCfg.RevokeCheckInt

	inputActivity := make(chan struct{}, 1)
	var wg sync.WaitGroup

	// Goroutine 1: timers (idle timeout, hard limit, session revocation, hub shutdown).
	wg.Add(1)
	go func() {
		defer wg.Done()
		idleTimer := time.NewTimer(idleTimeout)
		defer idleTimer.Stop()
		hardTimer := time.NewTimer(hardLimit)
		defer hardTimer.Stop()
		revokeTick := time.NewTicker(revokeInt)
		defer revokeTick.Stop()

		var hubDone <-chan struct{}
		if hubCtx != nil {
			hubDone = hubCtx.Done()
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-hubDone:
				sendCtrl(host.TermControl{Type: "error", Message: "Hub is stopping."})
				cleanup(store.TermReasonHubStopped, nil)
				return
			case <-inputActivity:
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(idleTimeout)
			case <-idleTimer.C:
				sendCtrl(host.TermControl{Type: "error", Message: "Terminal closed after 15 minutes of inactivity."})
				cleanup(store.TermReasonIdle, nil)
				return
			case <-hardTimer.C:
				sendCtrl(host.TermControl{Type: "error", Message: "Terminal reached the 8-hour session limit."})
				cleanup(store.TermReasonLimit, nil)
				return
			case <-revokeTick.C:
				cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
				u, err := s.auth.SessionUser(cctx, cookieVal)
				ccancel()
				if err != nil || u.Role != "admin" {
					sendCtrl(host.TermControl{Type: "error", Message: "Your session ended or is no longer an admin."})
					cleanup(store.TermReasonRevoked, nil)
					return
				}
			}
		}
	}()

	// Goroutine 2: read from browser WebSocket -> forward to container TTY.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				cleanup(store.TermReasonClosed, nil)
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if len(data) > 0 {
					select {
					case inputActivity <- struct{}{}:
					default:
					}
					if _, werr := ts.Write(data); werr != nil {
						cleanup(store.TermReasonError, nil)
						return
					}
				}
			case websocket.MessageText:
				var ctrl host.TermControl
				if json.Unmarshal(data, &ctrl) == nil && ctrl.Type == "resize" {
					_ = ts.Resize(ctx, ctrl.Cols, ctrl.Rows)
				}
			}
		}
	}()

	// Main loop: read from container TTY -> forward to browser WebSocket (bounded 16 KiB buffer, backpressure).
	buf := make([]byte, 16<<10)
	for {
		n, rerr := ts.Read(buf)
		if n > 0 {
			if werr := writeFrame(websocket.MessageBinary, buf[:n]); werr != nil {
				cleanup(store.TermReasonClosed, nil)
				break
			}
		}
		if rerr != nil {
			switch {
			case ctx.Err() != nil:
				// Ended by another path (browser, timer, revocation); it set the reason.
			case !errors.Is(rerr, io.EOF):
				// The stream broke (agent or tunnel gone), the shell did not exit.
				sendCtrl(host.TermControl{Type: "error", Message: "Lost the connection to the host."})
				cleanup(store.TermReasonError, nil)
			default:
				wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
				code, werr := ts.Wait(wctx)
				wcancel()
				if werr != nil {
					sendCtrl(host.TermControl{Type: "error", Message: werr.Error()})
				}
				sendCtrl(host.TermControl{Type: "exit", Code: code})
				cleanup(store.TermReasonExit, &code)
			}
			break
		}
	}

	cleanup(store.TermReasonClosed, nil)
	wg.Wait()

	mu.Lock()
	finalReason := reason
	if finalReason == "" {
		finalReason = store.TermReasonClosed
	}
	finalCode := exitCode
	mu.Unlock()

	endedAt := time.Now()
	if auditID > 0 {
		_ = s.store.EndTerminalSession(context.Background(), auditID, endedAt, finalReason, finalCode)
	}
	s.log.Info("terminal session ended",
		"user", user, "ip", ip, "host", hostID, "stack", stack,
		"container", ctrName, "shell", shell,
		"duration", endedAt.Sub(startedAt).Round(time.Millisecond).String(),
		"reason", finalReason,
	)
}
