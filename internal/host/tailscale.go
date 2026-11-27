package host

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"moorage/internal/tsserve"
)

// TailscaleServer is implemented by hosts that can publish services with Tailscale Serve
// (M19J). An older agent answers 404 (IsUnsupported); a node with
// MOORAGE_TAILSCALE_SERVE off answers with tsserve.ErrDisabled as the status reason.
type TailscaleServer interface {
	TailscaleStatus(ctx context.Context) (tsserve.Status, error)
	TailscaleServe(ctx context.Context, port int, target string) (tsserve.Entry, error)
	TailscaleStop(ctx context.Context, port int) error
}

func (l Local) TailscaleStatus(ctx context.Context) (tsserve.Status, error) {
	c, err := l.E.Tailscale()
	if err != nil {
		return tsserve.Status{Entries: []tsserve.Entry{}, Reason: err.Error()}, nil
	}
	return c.Status(ctx), nil
}

func (l Local) TailscaleServe(ctx context.Context, port int, target string) (tsserve.Entry, error) {
	if l.ReadOnly {
		return tsserve.Entry{}, ErrHostReadOnly
	}
	c, err := l.E.Tailscale()
	if err != nil {
		return tsserve.Entry{}, err
	}
	return c.Serve(ctx, port, target)
}

func (l Local) TailscaleStop(ctx context.Context, port int) error {
	if l.ReadOnly {
		return ErrHostReadOnly
	}
	c, err := l.E.Tailscale()
	if err != nil {
		return err
	}
	return c.Stop(ctx, port)
}

func (rm *Remote) TailscaleStatus(ctx context.Context) (tsserve.Status, error) {
	var st tsserve.Status
	err := rm.call(ctx, http.MethodGet, "/tailscale/serve", nil, &st)
	return st, err
}

func (rm *Remote) TailscaleServe(ctx context.Context, port int, target string) (tsserve.Entry, error) {
	var e tsserve.Entry
	err := rm.call(ctx, http.MethodPost, "/tailscale/serve", map[string]any{"port": port, "target": target}, &e)
	return e, err
}

func (rm *Remote) TailscaleStop(ctx context.Context, port int) error {
	return rm.call(ctx, http.MethodDelete, "/tailscale/serve/"+strconv.Itoa(port), nil, nil)
}

func tailscaleRoutes(mux *http.ServeMux, h Host, write func(http.HandlerFunc) http.HandlerFunc) {
	ts, ok := h.(TailscaleServer)
	if !ok {
		return
	}
	mux.HandleFunc("GET /tailscale/serve", func(w http.ResponseWriter, r *http.Request) {
		st, err := ts.TailscaleStatus(r.Context())
		reply(w)(st, err)
	})
	mux.HandleFunc("POST /tailscale/serve", write(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Port   int    `json:"port"`
			Target string `json:"target"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		e, err := ts.TailscaleServe(r.Context(), req.Port, req.Target)
		reply(w)(e, err)
	}))
	mux.HandleFunc("DELETE /tailscale/serve/{port}", write(func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.PathValue("port"))
		if err := ts.TailscaleStop(r.Context(), p); err != nil {
			agentErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
