package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"moorage/internal/diagnose"
	"moorage/internal/engine"
)

// AgentInfo is what an agent reports about itself.
type AgentInfo struct {
	engine.HostInfo
	ReadOnly     bool   `json:"readOnly"`
	Terminal     bool   `json:"terminal,omitempty"`
	Version      string `json:"version"`
	Watchdog     bool   `json:"watchdog,omitempty"`
	WatchdogAddr string `json:"watchdogAddr,omitempty"`
	// Probes reports whether the agent runs checks for the hub (MOORAGE_PROBES). Older
	// agents don't send it, which reads as false.
	Probes bool `json:"probes,omitempty"`
}

// TermControl is a JSON control frame on a terminal WebSocket.
type TermControl struct {
	Type    string `json:"type"` // resize | exit | error
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

const termReadLimit = 64 << 10

// LogEvent is one NDJSON record of a log stream between agent and hub.
type LogEvent struct {
	Lines []engine.LogLine `json:"lines,omitempty"`
	End   bool             `json:"end,omitempty"`
	Error string           `json:"error,omitempty"`
}

type saveReq struct {
	Compose string `json:"compose"`
	Env     string `json:"env"`
	Author  string `json:"author"`
	Create  bool   `json:"create"`
	// Note is the first version message of a created stack (from a template). Older
	// agents ignore the field; the engine cleans it to one short line.
	Note string `json:"note,omitempty"`
}

type restoreReq struct {
	Hash   string `json:"hash"`
	Author string `json:"author"`
}

type rollbackImagesReq struct {
	Targets map[string]engine.RollbackTarget `json:"targets"`
	Author  string                           `json:"author"`
}

type changeImageReq struct {
	Service string `json:"service"`
	OldRef  string `json:"oldRef"`
	NewRef  string `json:"newRef"`
	Author  string `json:"author"`
}

// AgentHandler exposes h to the hub over the tunnel. It is the only thing an
// agent offers: a fixed list of stack operations, never the raw Docker API
// (a built-in socket proxy). readOnly refuses every change on the agent side,
// so even a compromised hub can't modify a read-only host.
func AgentHandler(h Host, readOnly bool, version string, terminal ...bool) http.Handler {
	return NewAgentHandler(h, AgentOptions{ReadOnly: readOnly, Version: version, Terminal: len(terminal) > 0 && terminal[0]})
}

// AgentOptions configure NewAgentHandler.
type AgentOptions struct {
	ReadOnly bool
	Version  string
	Terminal bool
	// Prober runs checks for the hub (POST /probe); nil means MOORAGE_PROBES=false.
	Prober Prober
	// SelfUpdate handles Moorage binary/container self-update endpoints.
	SelfUpdate AgentSelfUpdater
}

// NewAgentHandler is AgentHandler with all options, including remote probes.
func NewAgentHandler(h Host, o AgentOptions) http.Handler {
	readOnly, version := o.ReadOnly, o.Version
	allowTerm := o.Terminal && !readOnly
	mux := http.NewServeMux()
	write := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if readOnly {
				agentErr(w, ErrHostReadOnly)
				return
			}
			fn(w, r)
		}
	}
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		if o.SelfUpdate != nil {
			o.SelfUpdate.ConfirmHealthy(version)
		}
		agentJSON(w, AgentInfo{HostInfo: h.Info(r.Context()), ReadOnly: readOnly, Terminal: allowTerm, Version: version, Probes: o.Prober != nil})
	})
	if o.SelfUpdate != nil {
		// Self-update is governed by MOORAGE_SELF_UPDATE and Ed25519 signature checks
		// rather than MOORAGE_READ_ONLY (an update changes Moorage itself, never user stacks).
		mux.HandleFunc("GET /system/update/status", func(w http.ResponseWriter, r *http.Request) {
			reply(w)(o.SelfUpdate.Status(r.Context()))
		})
		mux.HandleFunc("POST /system/update/apply-binary", func(w http.ResponseWriter, r *http.Request) {
			handleAgentApplyBinary(w, r, o.SelfUpdate)
		})
		mux.HandleFunc("POST /system/update/apply-container", func(w http.ResponseWriter, r *http.Request) {
			var m SelfUpdateManifest
			if !agentRead(w, r, &m) {
				return
			}
			if err := o.SelfUpdate.ApplyContainer(r.Context(), m); err != nil {
				agentErr(w, err)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})
		mux.HandleFunc("POST /system/update/rollback", func(w http.ResponseWriter, r *http.Request) {
			if err := o.SelfUpdate.Rollback(r.Context()); err != nil {
				agentErr(w, err)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})
	}
	// Not wrapped in write(): a probe only reads the network, it changes nothing here.
	mux.HandleFunc("POST /probe", probeHandler(o.Prober))
	externalRoutes(mux, h, write)
	mux.HandleFunc("GET /stacks", func(w http.ResponseWriter, r *http.Request) {
		list, err := h.ListStacks(r.Context())
		resp := map[string]any{"stacks": list}
		if err != nil {
			resp["error"] = err.Error()
		}
		agentJSON(w, resp)
	})
	if df, ok := h.(DepFactsSource); ok {
		// Read-only: compose wiring only, never env values (see engine.DepFacts).
		mux.HandleFunc("GET /deps/facts", func(w http.ResponseWriter, r *http.Request) {
			reply(w)(df.DepFacts(r.Context()))
		})
	}
	if he, ok := h.(HistoryExporter); ok {
		mux.HandleFunc("GET /history/archive", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-tar")
			// Errors after the first byte can't change the status: the hub notices a
			// broken tar instead.
			if err := he.ExportHistory(r.Context(), w); err != nil {
				agentErr(w, err)
			}
		})
	}
	if ds, ok := h.(DeletedStacksSource); ok {
		mux.HandleFunc("GET /deleted-stacks", func(w http.ResponseWriter, r *http.Request) {
			reply(w)(ds.DeletedStacks(r.Context()))
		})
	}
	snapshotRoutes(mux, h, write)
	selfApplyRoutes(mux, h, write)
	if sd, ok := h.(StackDiagnoser); ok {
		mux.HandleFunc("GET /stacks/{name}/diagnose", func(w http.ResponseWriter, r *http.Request) {
			reply(w)(sd.DiagnoseStack(r.Context(), r.PathValue("name")))
		})
	}
	if dp, ok := h.(DiskPruner); ok {
		mux.HandleFunc("GET /prune/preview", func(w http.ResponseWriter, r *http.Request) {
			reply(w)(dp.PrunePreview(r.Context()))
		})
		mux.HandleFunc("POST /prune", write(func(w http.ResponseWriter, r *http.Request) {
			var req engine.PruneRequest
			if !agentRead(w, r, &req) {
				return
			}
			reply(w)(dp.PruneDisk(r.Context(), req))
		}))
	}
	mux.HandleFunc("GET /stacks/{name}", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(h.GetStack(r.Context(), r.PathValue("name")))
	})
	mux.HandleFunc("GET /stacks/{name}/stats", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(h.Stats(r.Context(), r.PathValue("name")))
	})
	mux.HandleFunc("POST /stacks/{name}/validate", func(w http.ResponseWriter, r *http.Request) {
		var req saveReq
		if !agentRead(w, r, &req) {
			return
		}
		replyErr(w, h.Validate(r.Context(), r.PathValue("name"), req.Compose, req.Env))
	})
	mux.HandleFunc("PUT /stacks/{name}", write(func(w http.ResponseWriter, r *http.Request) {
		var req saveReq
		if !agentRead(w, r, &req) {
			return
		}
		if nc, ok := h.(NotedCreator); ok && req.Create && req.Note != "" {
			replyErr(w, nc.CreateNoted(r.Context(), r.PathValue("name"), req.Compose, req.Env, req.Author, req.Note))
			return
		}
		replyErr(w, h.Save(r.Context(), r.PathValue("name"), req.Compose, req.Env, req.Author, req.Create))
	}))
	if sa, ok := h.(SelfApplier); ok {
		mux.HandleFunc("POST /stacks/{name}/self-apply", write(func(w http.ResponseWriter, r *http.Request) {
			var req saveReq
			if !agentRead(w, r, &req) {
				return
			}
			replyErr(w, sa.SelfApply(r.Context(), r.PathValue("name"), req.Compose, req.Env, req.Author))
		}))
	}
	mux.HandleFunc("DELETE /stacks/{name}", write(func(w http.ResponseWriter, r *http.Request) {
		replyErr(w, h.Delete(r.Context(), r.PathValue("name"), r.URL.Query().Get("author")))
	}))
	if br, ok := h.(BindMountRelocator); ok {
		mux.HandleFunc("POST /stacks/{name}/relocate-binds", write(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				AddonID  string `json:"addonId"`
				Rollback bool   `json:"rollback"`
			}
			if !agentRead(w, r, &req) {
				return
			}
			replyErr(w, br.RelocateBindMounts(r.Context(), r.PathValue("name"), req.AddonID, req.Rollback))
		}))
	}
	if ce, ok := h.(CommandExecutor); ok {
		mux.HandleFunc("POST /stacks/{name}/exec", write(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Service string `json:"service"`
				Command string `json:"command"`
			}
			if !agentRead(w, r, &req) {
				return
			}
			out, code, err := ce.ExecCommand(r.Context(), r.PathValue("name"), req.Service, req.Command)
			resp := map[string]any{"output": out, "exitCode": code}
			if err != nil {
				resp["error"] = err.Error()
			}
			agentJSON(w, resp)
		}))
	}
	mux.HandleFunc("POST /stacks/{name}/actions/{action}", write(func(w http.ResponseWriter, r *http.Request) {
		reply(w)(h.RunAction(r.Context(), r.PathValue("name"), r.PathValue("action"), r.URL.Query().Get("author")))
	}))
	mux.HandleFunc("GET /stacks/{name}/history", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(h.History(r.Context(), r.PathValue("name")))
	})
	mux.HandleFunc("GET /stacks/{name}/history/{hash}", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(h.Version(r.Context(), r.PathValue("name"), r.PathValue("hash")))
	})
	mux.HandleFunc("POST /stacks/{name}/restore", write(func(w http.ResponseWriter, r *http.Request) {
		var req restoreReq
		if !agentRead(w, r, &req) {
			return
		}
		reply(w)(h.Restore(r.Context(), r.PathValue("name"), req.Hash, req.Author))
	}))
	mux.HandleFunc("GET /stacks/{name}/images/check", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(h.CheckImages(r.Context(), r.PathValue("name")))
	})
	mux.HandleFunc("GET /stacks/{name}/images", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(h.StackImages(r.Context(), r.PathValue("name")))
	})
	mux.HandleFunc("POST /stacks/{name}/images/rollback", write(func(w http.ResponseWriter, r *http.Request) {
		var req rollbackImagesReq
		if !agentRead(w, r, &req) {
			return
		}
		reply(w)(h.RollbackImages(r.Context(), r.PathValue("name"), req.Targets, req.Author))
	}))
	mux.HandleFunc("POST /stacks/{name}/images/change", write(func(w http.ResponseWriter, r *http.Request) {
		var req changeImageReq
		if !agentRead(w, r, &req) {
			return
		}
		reply(w)(h.ChangeImage(r.Context(), r.PathValue("name"), req.Service, req.OldRef, req.NewRef, req.Author))
	}))
	mux.HandleFunc("GET /ops/{id}", func(w http.ResponseWriter, r *http.Request) {
		s := newNDJSON(w)
		err := h.FollowOp(r.Context(), r.PathValue("id"), func(ev OpEvent) error { return s.send(ev) })
		if errors.Is(err, ErrOpNotFound) && !s.started {
			agentErr(w, err)
		}
	})
	mux.HandleFunc("GET /logs/tail", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		reply(w)(h.TailLogs(r.Context(), r.URL.Query().Get("stack"), min(max(n, 1), 200)))
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tail, _ := strconv.Atoi(q.Get("tail"))
		follow := true
		if f := q.Get("follow"); f == "0" || f == "false" {
			follow = false
		}
		since := q.Get("since")
		until := q.Get("until")
		streamNDJSONLogs(w, r, func(emit func(engine.LogLine)) error {
			if lq, ok := h.(LogQuerier); ok && (q.Get("stack") == "" || since != "" || until != "" || !follow) {
				return lq.QueryLogs(r.Context(), engine.LogOptions{
					Stack:     q.Get("stack"),
					Container: q.Get("container"),
					Tail:      tail,
					Since:     since,
					Until:     until,
					Follow:    follow,
				}, emit)
			}
			return h.StreamLogs(r.Context(), q.Get("stack"), q.Get("container"), tail, emit)
		})
	})
	mux.HandleFunc("GET /logs/watch", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := engine.LogFilter{
			Pattern:       q.Get("pattern"),
			Regex:         q.Get("regex") == "1" || q.Get("regex") == "true",
			CaseSensitive: q.Get("cs") == "1" || q.Get("cs") == "true",
		}
		if _, err := f.Compile(); err != nil {
			agentErr(w, engine.ValidationError{Msg: err.Error()})
			return
		}
		streamNDJSONLogs(w, r, func(emit func(engine.LogLine)) error {
			return h.WatchLogs(r.Context(), q.Get("stack"), q.Get("container"), f, emit)
		})
	})
	mux.HandleFunc("GET /stacks/{name}/terminal", func(w http.ResponseWriter, r *http.Request) {
		if !allowTerm {
			agentErr(w, ErrTerminalDisabled)
			return
		}
		q := r.URL.Query()
		cols, _ := strconv.Atoi(q.Get("cols"))
		rows, _ := strconv.Atoi(q.Get("rows"))
		ts, ctr, err := h.Terminal(r.Context(), r.PathValue("name"), q.Get("container"), q.Get("shell"), cols, rows)
		if err != nil {
			agentErr(w, err)
			return
		}
		defer ts.Close()
		w.Header().Set("X-Moorage-Container-Id", ctr.ID)
		w.Header().Set("X-Moorage-Container-Name", ctr.Name)
		w.Header().Set("X-Moorage-Container-Service", ctr.Service)
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(termReadLimit)
		serveAgentTerminalWS(r.Context(), ws, ts)
	})
	if ct, ok := h.(ContainerTerminalHost); ok {
		mux.HandleFunc("GET /containers/{id}/terminal", func(w http.ResponseWriter, r *http.Request) {
			if !allowTerm {
				agentErr(w, ErrTerminalDisabled)
				return
			}
			q := r.URL.Query()
			cols, _ := strconv.Atoi(q.Get("cols"))
			rows, _ := strconv.Atoi(q.Get("rows"))
			ts, ctr, err := ct.ContainerTerminal(r.Context(), r.PathValue("id"), q.Get("shell"), cols, rows)
			if err != nil {
				agentErr(w, err)
				return
			}
			defer ts.Close()
			w.Header().Set("X-Moorage-Container-Id", ctr.ID)
			w.Header().Set("X-Moorage-Container-Name", ctr.Name)
			w.Header().Set("X-Moorage-Container-Service", ctr.Service)
			w.Header().Set("X-Moorage-Container-Project", ctr.Project)
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			ws.SetReadLimit(termReadLimit)
			serveAgentTerminalWS(r.Context(), ws, ts)
		})
	}
	if ht, ok := h.(HostTerminalProvider); ok {
		mux.HandleFunc("GET /host-terminal", func(w http.ResponseWriter, r *http.Request) {
			if readOnly {
				agentErr(w, ErrHostReadOnly)
				return
			}
			q := r.URL.Query()
			cols, _ := strconv.Atoi(q.Get("cols"))
			rows, _ := strconv.Atoi(q.Get("rows"))
			ts, err := ht.HostTerminal(r.Context(), q.Get("shell"), cols, rows)
			if err != nil {
				agentErr(w, err)
				return
			}
			defer ts.Close()
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			ws.SetReadLimit(termReadLimit)
			serveAgentTerminalWS(r.Context(), ws, ts)
		})
	}
	return mux
}

func serveAgentTerminalWS(reqCtx context.Context, ws *websocket.Conn, ts engine.TerminalSession) {
	ctx, cancel := context.WithCancel(reqCtx)
	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			cancel()
			_ = ts.Close()
			_ = ws.CloseNow()
		})
	}
	defer cleanup()

	var writeMu sync.Mutex
	writeFrame := func(typ websocket.MessageType, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return ws.Write(wctx, typ, data)
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				cancel()
				_ = ts.Close()
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := ts.Write(data); err != nil {
					cancel()
					return
				}
			case websocket.MessageText:
				var ctrl TermControl
				if json.Unmarshal(data, &ctrl) == nil && ctrl.Type == "resize" {
					_ = ts.Resize(ctx, ctrl.Cols, ctrl.Rows)
				}
			}
		}
	}()

	buf := make([]byte, 16<<10)
	for {
		n, rerr := ts.Read(buf)
		if n > 0 {
			if werr := writeFrame(websocket.MessageBinary, buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			if ctx.Err() == nil {
				wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
				code, werr := ts.Wait(wctx)
				wcancel()
				if werr != nil {
					b, _ := json.Marshal(TermControl{Type: "error", Message: werr.Error()})
					_ = writeFrame(websocket.MessageText, b)
				}
				b, _ := json.Marshal(TermControl{Type: "exit", Code: code})
				_ = writeFrame(websocket.MessageText, b)
			}
			break
		}
	}
	cleanup()
	<-readDone
}

func streamNDJSONLogs(w http.ResponseWriter, r *http.Request, run func(emit func(engine.LogLine)) error) {
	s := newNDJSON(w)
	var mu sync.Mutex
	var buf []engine.LogLine
	done := make(chan error, 1)
	go func() {
		done <- run(func(l engine.LogLine) {
			mu.Lock()
			if len(buf) < 20_000 {
				buf = append(buf, l)
			}
			mu.Unlock()
		})
	}()
	flush := func() error {
		mu.Lock()
		out := buf
		buf = nil
		mu.Unlock()
		if len(out) == 0 {
			return s.keepalive()
		}
		return s.send(LogEvent{Lines: out})
	}
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case err := <-done:
			flush() //nolint:errcheck
			ev := LogEvent{End: true}
			if err != nil {
				ev.Error = err.Error()
			}
			s.send(ev) //nolint:errcheck
			return
		case <-tick.C:
			if err := flush(); err != nil {
				return
			}
		}
	}
}

// ndjson streams one JSON value per line and flushes after each.
type ndjson struct {
	w       http.ResponseWriter
	f       http.Flusher
	started bool
	idle    int
}

func newNDJSON(w http.ResponseWriter) *ndjson {
	f, _ := w.(http.Flusher)
	return &ndjson{w: w, f: f}
}

func (s *ndjson) send(v any) error {
	if !s.started {
		s.w.Header().Set("Content-Type", "application/x-ndjson")
		s.started = true
	}
	s.idle = 0
	if err := json.NewEncoder(s.w).Encode(v); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

// keepalive writes an empty line every ~15 s so a dead hub is noticed.
func (s *ndjson) keepalive() error {
	s.idle++
	if s.idle < 100 {
		return nil
	}
	s.idle = 0
	if _, err := s.w.Write([]byte("\n")); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

func reply(w http.ResponseWriter) func(any, error) {
	return func(v any, err error) {
		if err != nil {
			agentErr(w, err)
			return
		}
		agentJSON(w, v)
	}
}

func replyErr(w http.ResponseWriter, err error) {
	if err != nil {
		agentErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func agentRead(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2*engine.MaxFileSize+4096)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, `{"code":"internal","error":"bad request"}`, http.StatusBadRequest)
		return false
	}
	return true
}

func agentJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func agentErr(w http.ResponseWriter, err error) {
	code, status := ErrorCode(err)
	msg := err.Error()
	var ve engine.ValidationError
	if errors.As(err, &ve) {
		msg = ve.Msg
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg}) //nolint:errcheck
}

// ---- remote host (hub side) ------------------------------------------------

// Remote is a Host reached through an agent tunnel.
type Remote struct {
	c *http.Client
}

// NewRemote builds a client whose connections are opened by dial (a tunnel stream).
func NewRemote(dial func(ctx context.Context) (net.Conn, error)) *Remote {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
	}
	return &Remote{c: &http.Client{Transport: tr}}
}

const agentBase = "http://agent"

var errRouteNotFound = errors.New("agent endpoint not found")

func (rm *Remote) req(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, agentBase+path, rd)
	if err != nil {
		return nil, err
	}
	resp, err := rm.c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct{ Code, Error string }
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e) //nolint:errcheck
		if resp.StatusCode == http.StatusNotFound && e.Code == "" {
			return nil, errRouteNotFound
		}
		return nil, FromCode(e.Code, e.Error)
	}
	return resp, nil
}

func (rm *Remote) call(ctx context.Context, method, path string, body, out any) error {
	return rm.callTimeout(ctx, 60*time.Second, method, path, body, out)
}

func (rm *Remote) callTimeout(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := rm.req(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// AgentInfo asks the agent about itself.
func (rm *Remote) AgentInfo(ctx context.Context) (AgentInfo, error) {
	var a AgentInfo
	err := rm.call(ctx, http.MethodGet, "/info", nil, &a)
	return a, err
}

func (rm *Remote) Info(ctx context.Context) engine.HostInfo {
	a, err := rm.AgentInfo(ctx)
	if err != nil {
		return engine.HostInfo{Error: err.Error()}
	}
	return a.HostInfo
}

func (rm *Remote) ListStacks(ctx context.Context) ([]engine.StackSummary, error) {
	var resp struct {
		Stacks []engine.StackSummary `json:"stacks"`
		Error  string                `json:"error"`
	}
	if err := rm.call(ctx, http.MethodGet, "/stacks", nil, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return resp.Stacks, remoteError(resp.Error)
	}
	return resp.Stacks, nil
}

func p(name string) string { return "/stacks/" + url.PathEscape(name) }

func (rm *Remote) GetStack(ctx context.Context, name string) (engine.Stack, error) {
	var s engine.Stack
	err := rm.call(ctx, http.MethodGet, p(name), nil, &s)
	return s, err
}

func (rm *Remote) Stats(ctx context.Context, name string) ([]engine.Stat, error) {
	var s []engine.Stat
	err := rm.call(ctx, http.MethodGet, p(name)+"/stats", nil, &s)
	return s, err
}

func (rm *Remote) Validate(ctx context.Context, name, compose, env string) error {
	return rm.call(ctx, http.MethodPost, p(name)+"/validate", saveReq{Compose: compose, Env: env}, nil)
}

func (rm *Remote) Save(ctx context.Context, name, compose, env, author string, create bool) error {
	return rm.call(ctx, http.MethodPut, p(name), saveReq{Compose: compose, Env: env, Author: author, Create: create}, nil)
}

// CreateNoted creates a stack on the agent with a first version note. An older agent
// ignores the note and creates the stack with the plain message.
func (rm *Remote) CreateNoted(ctx context.Context, name, compose, env, author, note string) error {
	return rm.call(ctx, http.MethodPut, p(name), saveReq{Compose: compose, Env: env, Author: author, Create: true, Note: note}, nil)
}

// SelfApply asks the agent to apply changes to the stack it runs from.
func (rm *Remote) SelfApply(ctx context.Context, name, compose, env, author string) error {
	return rm.call(ctx, http.MethodPost, p(name)+"/self-apply", saveReq{Compose: compose, Env: env, Author: author}, nil)
}

func (rm *Remote) ExportHistory(ctx context.Context, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	resp, err := rm.req(ctx, http.MethodGet, "/history/archive", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (rm *Remote) DeletedStacks(ctx context.Context) ([]engine.DeletedStack, error) {
	var out []engine.DeletedStack
	err := rm.call(ctx, http.MethodGet, "/deleted-stacks", nil, &out)
	return out, err
}

func (rm *Remote) DiagnoseStack(ctx context.Context, stack string) ([]diagnose.Facts, error) {
	var out []diagnose.Facts
	err := rm.call(ctx, http.MethodGet, p(stack)+"/diagnose", nil, &out)
	return out, err
}

func (rm *Remote) PrunePreview(ctx context.Context) (engine.PrunePreview, error) {
	var out engine.PrunePreview
	err := rm.call(ctx, http.MethodGet, "/prune/preview", nil, &out)
	return out, err
}

func (rm *Remote) PruneDisk(ctx context.Context, req engine.PruneRequest) (engine.PruneResult, error) {
	var out engine.PruneResult
	err := rm.call(ctx, http.MethodPost, "/prune", req, &out)
	return out, err
}

func (rm *Remote) Delete(ctx context.Context, name, author string) error {
	return rm.call(ctx, http.MethodDelete, p(name)+"?author="+url.QueryEscape(author), nil, nil)
}

func (rm *Remote) RelocateBindMounts(ctx context.Context, fromStack, addonID string, rollback bool) error {
	return rm.call(ctx, http.MethodPost, p(fromStack)+"/relocate-binds", map[string]any{
		"addonId":  addonID,
		"rollback": rollback,
	}, nil)
}

func (rm *Remote) ExecCommand(ctx context.Context, stack, service, command string) (string, int, error) {
	var resp struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
		Error    string `json:"error"`
	}
	err := rm.callTimeout(ctx, 16*time.Minute, http.MethodPost, p(stack)+"/exec", map[string]string{
		"service": service,
		"command": command,
	}, &resp)
	if err != nil {
		return "", 0, err
	}
	if resp.Error != "" {
		return resp.Output, resp.ExitCode, remoteError(resp.Error)
	}
	return resp.Output, resp.ExitCode, nil
}

func (rm *Remote) RunAction(ctx context.Context, name, action, author string) (engine.OpInfo, error) {
	var op engine.OpInfo
	err := rm.call(ctx, http.MethodPost, p(name)+"/actions/"+url.PathEscape(action)+"?author="+url.QueryEscape(author), nil, &op)
	return op, err
}

func (rm *Remote) History(ctx context.Context, name string) ([]engine.Version, error) {
	var v []engine.Version
	err := rm.call(ctx, http.MethodGet, p(name)+"/history", nil, &v)
	return v, err
}

func (rm *Remote) Version(ctx context.Context, name, hash string) (engine.VersionDetail, error) {
	var v engine.VersionDetail
	err := rm.call(ctx, http.MethodGet, p(name)+"/history/"+url.PathEscape(hash), nil, &v)
	return v, err
}

func (rm *Remote) Restore(ctx context.Context, name, hash, author string) (*engine.Version, error) {
	var v *engine.Version
	err := rm.call(ctx, http.MethodPost, p(name)+"/restore", restoreReq{Hash: hash, Author: author}, &v)
	return v, err
}

func (rm *Remote) CheckImages(ctx context.Context, stack string) ([]engine.ServiceImage, error) {
	var out []engine.ServiceImage
	// A stack of a dozen services against a slow registry needs more than the usual minute
	// (each lookup may take 20 s, four run at a time).
	err := rm.callTimeout(ctx, 5*time.Minute, http.MethodGet, p(stack)+"/images/check", nil, &out)
	return out, err
}

func (rm *Remote) StackImages(ctx context.Context, stack string) (map[string]engine.RunningImage, error) {
	var out map[string]engine.RunningImage
	err := rm.call(ctx, http.MethodGet, p(stack)+"/images", nil, &out)
	return out, err
}

func (rm *Remote) RollbackImages(ctx context.Context, stack string, targets map[string]engine.RollbackTarget, author string) (engine.OpInfo, error) {
	var op engine.OpInfo
	err := rm.call(ctx, http.MethodPost, p(stack)+"/images/rollback", rollbackImagesReq{Targets: targets, Author: author}, &op)
	return op, err
}

func (rm *Remote) ChangeImage(ctx context.Context, stack, service, oldRef, newRef, author string) (*engine.Version, error) {
	var v *engine.Version
	err := rm.call(ctx, http.MethodPost, p(stack)+"/images/change", changeImageReq{
		Service: service, OldRef: oldRef, NewRef: newRef, Author: author,
	}, &v)
	return v, err
}

func (rm *Remote) Terminal(ctx context.Context, stack, container, shell string, cols, rows int) (engine.TerminalSession, engine.Container, error) {
	q := url.Values{
		"container": {container},
		"shell":     {shell},
		"cols":      {strconv.Itoa(cols)},
		"rows":      {strconv.Itoa(rows)},
	}
	u := "ws://agent" + p(stack) + "/terminal?" + q.Encode()
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: rm.c})
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			var e struct{ Code, Error string }
			json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e) //nolint:errcheck
			if resp.StatusCode == http.StatusNotFound && e.Code == "" {
				return nil, engine.Container{}, errRouteNotFound
			}
			if e.Code != "" || e.Error != "" {
				return nil, engine.Container{}, FromCode(e.Code, e.Error)
			}
		}
		if ctx.Err() != nil {
			return nil, engine.Container{}, ctx.Err()
		}
		return nil, engine.Container{}, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	ws.SetReadLimit(termReadLimit)
	var ctr engine.Container
	if resp != nil {
		ctr.ID = resp.Header.Get("X-Moorage-Container-Id")
		ctr.Name = resp.Header.Get("X-Moorage-Container-Name")
		ctr.Service = resp.Header.Get("X-Moorage-Container-Service")
	}
	if ctr.Name == "" {
		ctr.Name = container
	}
	rctx, rcancel := context.WithCancel(context.Background())
	return &remoteTerminalSession{ws: ws, ctx: rctx, cancel: rcancel}, ctr, nil
}

func (rm *Remote) dialTerminalWS(ctx context.Context, u string) (engine.TerminalSession, *http.Response, error) {
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: rm.c})
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			var e struct{ Code, Error string }
			json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e) //nolint:errcheck
			if resp.StatusCode == http.StatusNotFound && e.Code == "" {
				return nil, nil, errRouteNotFound
			}
			if e.Code != "" || e.Error != "" {
				return nil, nil, FromCode(e.Code, e.Error)
			}
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrOffline, err)
	}
	ws.SetReadLimit(termReadLimit)
	rctx, rcancel := context.WithCancel(context.Background())
	return &remoteTerminalSession{ws: ws, ctx: rctx, cancel: rcancel}, resp, nil
}

func (rm *Remote) ContainerTerminal(ctx context.Context, containerRef, shell string, cols, rows int) (engine.TerminalSession, engine.ContainerInfo, error) {
	q := url.Values{
		"shell": {shell},
		"cols":  {strconv.Itoa(cols)},
		"rows":  {strconv.Itoa(rows)},
	}
	u := "ws://agent/containers/" + url.PathEscape(containerRef) + "/terminal?" + q.Encode()
	ts, resp, err := rm.dialTerminalWS(ctx, u)
	if err != nil {
		return nil, engine.ContainerInfo{}, err
	}
	var ctr engine.ContainerInfo
	if resp != nil {
		ctr.ID = resp.Header.Get("X-Moorage-Container-Id")
		ctr.Name = resp.Header.Get("X-Moorage-Container-Name")
		ctr.Service = resp.Header.Get("X-Moorage-Container-Service")
		ctr.Project = resp.Header.Get("X-Moorage-Container-Project")
	}
	if ctr.Name == "" {
		ctr.Name = containerRef
	}
	ctr.Standalone = ctr.Project == ""
	return ts, ctr, nil
}

func (rm *Remote) HostTerminal(ctx context.Context, shell string, cols, rows int) (engine.TerminalSession, error) {
	q := url.Values{
		"shell": {shell},
		"cols":  {strconv.Itoa(cols)},
		"rows":  {strconv.Itoa(rows)},
	}
	u := "ws://agent/host-terminal?" + q.Encode()
	ts, _, err := rm.dialTerminalWS(ctx, u)
	return ts, err
}

type remoteTerminalSession struct {
	ws        *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	mu        sync.Mutex
	outBuf    []byte
	exited    bool
	exitCode  int
	exitErr   error
	closeOnce sync.Once
}

func (r *remoteTerminalSession) Read(p []byte) (int, error) {
	r.mu.Lock()
	if len(r.outBuf) > 0 {
		n := copy(p, r.outBuf)
		r.outBuf = r.outBuf[n:]
		r.mu.Unlock()
		return n, nil
	}
	if r.exited {
		r.mu.Unlock()
		return 0, io.EOF
	}
	r.mu.Unlock()

	for {
		typ, data, err := r.ws.Read(r.ctx)
		if err != nil {
			// Only an "exit" frame means the shell ended; a broken stream is an error,
			// so the hub doesn't record a lost agent as "exit 0".
			return 0, fmt.Errorf("%w: %v", ErrOffline, err)
		}
		if typ == websocket.MessageBinary {
			if len(data) == 0 {
				continue
			}
			n := copy(p, data)
			if n < len(data) {
				r.mu.Lock()
				r.outBuf = append(r.outBuf, data[n:]...)
				r.mu.Unlock()
			}
			return n, nil
		}
		if typ == websocket.MessageText {
			var ctrl TermControl
			if json.Unmarshal(data, &ctrl) != nil {
				continue
			}
			switch ctrl.Type {
			case "error":
				r.mu.Lock()
				r.exitErr = errors.New(ctrl.Message)
				r.mu.Unlock()
			case "exit":
				r.mu.Lock()
				r.exited = true
				r.exitCode = ctrl.Code
				r.mu.Unlock()
				return 0, io.EOF
			}
		}
	}
}

func (r *remoteTerminalSession) Write(p []byte) (int, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	wctx, wcancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer wcancel()
	if err := r.ws.Write(wctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *remoteTerminalSession) Resize(ctx context.Context, cols, rows int) error {
	cols, rows = engine.ClampTermSize(cols, rows)
	b, _ := json.Marshal(TermControl{Type: "resize", Cols: cols, Rows: rows})
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	return r.ws.Write(wctx, websocket.MessageText, b)
}

func (r *remoteTerminalSession) Wait(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitCode, r.exitErr
}

func (r *remoteTerminalSession) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		_ = r.ws.CloseNow()
	})
	return nil
}

// stream reads NDJSON records from path and passes each to fn.
func (rm *Remote) stream(ctx context.Context, path string, fn func([]byte) (stop bool, err error)) error {
	resp, err := rm.req(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		stop, err := fn(line)
		if err != nil || stop {
			return err
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%w: %v", ErrOffline, err)
	}
	return ctx.Err()
}

func (rm *Remote) FollowOp(ctx context.Context, id string, emit func(OpEvent) error) error {
	return rm.stream(ctx, "/ops/"+url.PathEscape(id), func(b []byte) (bool, error) {
		var ev OpEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			return true, err
		}
		return ev.Done, emit(ev)
	})
}

func (rm *Remote) StreamLogs(ctx context.Context, stack, container string, tail int, emit func(engine.LogLine)) error {
	q := url.Values{"stack": {stack}, "container": {container}, "tail": {strconv.Itoa(tail)}}
	return rm.readLogs(ctx, "/logs?"+q.Encode(), emit)
}

func (rm *Remote) QueryLogs(ctx context.Context, opts engine.LogOptions, emit func(engine.LogLine)) error {
	q := url.Values{
		"stack":     {opts.Stack},
		"container": {opts.Container},
		"tail":      {strconv.Itoa(opts.Tail)},
	}
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	if opts.Until != "" {
		q.Set("until", opts.Until)
	}
	if !opts.Follow {
		q.Set("follow", "0")
	}
	return rm.readLogs(ctx, "/logs?"+q.Encode(), emit)
}

func (rm *Remote) WatchLogs(ctx context.Context, stack, container string, f engine.LogFilter, emit func(engine.LogLine)) error {
	match, err := f.Compile()
	if err != nil {
		return err
	}
	filterEmit := func(l engine.LogLine) {
		if match(l.Message) {
			emit(l)
		}
	}
	q := url.Values{"stack": {stack}, "container": {container}, "pattern": {f.Pattern}}
	if f.Regex {
		q.Set("regex", "1")
	}
	if f.CaseSensitive {
		q.Set("cs", "1")
	}
	err = rm.readLogs(ctx, "/logs/watch?"+q.Encode(), filterEmit)
	if errors.Is(err, errRouteNotFound) {
		// Older agent without /logs/watch: follow the latest line via /logs and filter on the hub.
		start := time.Now().UTC().Format(time.RFC3339Nano)
		return rm.StreamLogs(ctx, stack, container, 1, func(l engine.LogLine) {
			if (l.Time == "" || l.Time >= start) && match(l.Message) {
				emit(l)
			}
		})
	}
	return err
}

func (rm *Remote) readLogs(ctx context.Context, path string, emit func(engine.LogLine)) error {
	var end error
	err := rm.stream(ctx, path, func(b []byte) (bool, error) {
		var ev LogEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			return true, err
		}
		for _, l := range ev.Lines {
			emit(l)
		}
		if ev.End && ev.Error != "" {
			end = remoteError(ev.Error)
		}
		return ev.End, nil
	})
	if err != nil {
		return err
	}
	return end
}

func (rm *Remote) TailLogs(ctx context.Context, stack string, n int) ([]engine.LogLine, error) {
	var lines []engine.LogLine
	q := url.Values{"stack": {stack}, "n": {strconv.Itoa(n)}}
	err := rm.call(ctx, http.MethodGet, "/logs/tail?"+q.Encode(), nil, &lines)
	return lines, err
}

// DepFacts asks the agent for the compose wiring of its stacks. Older agents answer
// 404, which IsUnsupported recognises.
func (rm *Remote) DepFacts(ctx context.Context) (engine.DepFacts, error) {
	var f engine.DepFacts
	err := rm.call(ctx, http.MethodGet, "/deps/facts", nil, &f)
	return f, err
}

// WatchdogCSR asks a participating agent for a peer certificate signing request.
func (rm *Remote) WatchdogCSR(ctx context.Context) ([]byte, error) {
	var resp struct {
		CSR []byte `json:"csr"`
	}
	err := rm.call(ctx, http.MethodPost, "/watchdog/csr", nil, &resp)
	return resp.CSR, err
}

// PushWatchdog sends the watchdog configuration (or enabled=false) to the agent.
func (rm *Remote) PushWatchdog(ctx context.Context, cfg any) error {
	return rm.call(ctx, http.MethodPut, "/watchdog", cfg, nil)
}

// TestWatchdog asks the agent to send a test notification directly to the channel.
func (rm *Remote) TestWatchdog(ctx context.Context, req any) error {
	return rm.call(ctx, http.MethodPost, "/watchdog/test", req, nil)
}

// IsUnsupported reports whether err was a 404 from an older agent without the endpoint.
func IsUnsupported(err error) bool {
	return errors.Is(err, errRouteNotFound)
}
