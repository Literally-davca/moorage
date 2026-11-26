package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"moorage/internal/engine"
	"moorage/internal/stacksnap"
)

// Snapshotter is implemented by hosts that keep stack snapshots (M18F). The files live
// on the host where the stack runs; the hub reads them through this interface and caches
// nothing. An older agent answers 404 (IsUnsupported).
type Snapshotter interface {
	SnapshotPlan(ctx context.Context, stack, service string) (engine.SnapshotPlan, error)
	ListSnapshots(ctx context.Context, stack string) ([]stacksnap.Manifest, error)
	CreateSnapshot(ctx context.Context, stack string, req engine.SnapshotRequest) (engine.OpInfo, error)
	RestoreSnapshot(ctx context.Context, stack, id string, req engine.RestoreRequest) (engine.OpInfo, error)
	DeleteSnapshot(ctx context.Context, stack, id string) error
	PinSnapshot(ctx context.Context, stack, id string, pinned bool) error
	SnapshotConfig(ctx context.Context, stack, id string) (compose, env string, err error)
	// DownloadSnapshot streams the file to w.
	DownloadSnapshot(ctx context.Context, stack, id string, w io.Writer) (stacksnap.Manifest, error)
}

// ---- Local ---------------------------------------------------------------------------

func (l Local) SnapshotPlan(ctx context.Context, stack, service string) (engine.SnapshotPlan, error) {
	return l.E.SnapshotPlan(ctx, stack, service)
}

func (l Local) ListSnapshots(_ context.Context, stack string) ([]stacksnap.Manifest, error) {
	return l.E.ListSnapshots(stack)
}

func (l Local) CreateSnapshot(ctx context.Context, stack string, req engine.SnapshotRequest) (engine.OpInfo, error) {
	if l.ReadOnly {
		return engine.OpInfo{}, ErrHostReadOnly
	}
	op, err := l.E.CreateSnapshot(ctx, stack, req)
	if err != nil {
		return engine.OpInfo{}, err
	}
	return op.Info(), nil
}

func (l Local) RestoreSnapshot(ctx context.Context, stack, id string, req engine.RestoreRequest) (engine.OpInfo, error) {
	if l.ReadOnly {
		return engine.OpInfo{}, ErrHostReadOnly
	}
	op, err := l.E.RestoreSnapshot(ctx, stack, id, req)
	if err != nil {
		return engine.OpInfo{}, err
	}
	return op.Info(), nil
}

func (l Local) DeleteSnapshot(_ context.Context, stack, id string) error {
	if l.ReadOnly {
		return ErrHostReadOnly
	}
	return l.E.DeleteSnapshot(stack, id)
}

func (l Local) PinSnapshot(_ context.Context, stack, id string, pinned bool) error {
	if l.ReadOnly {
		return ErrHostReadOnly
	}
	return l.E.PinSnapshot(stack, id, pinned)
}

func (l Local) SnapshotConfig(_ context.Context, stack, id string) (string, string, error) {
	return l.E.SnapshotConfig(stack, id)
}

func (l Local) DownloadSnapshot(_ context.Context, stack, id string, w io.Writer) (stacksnap.Manifest, error) {
	rc, m, err := l.E.OpenSnapshot(stack, id)
	if err != nil {
		return m, err
	}
	defer rc.Close()
	_, err = io.Copy(w, rc)
	return m, err
}

// ---- Remote (hub side of an agent) ----------------------------------------------------

func snapPath(stack, id string) string { return p(stack) + "/snapshots/" + url.PathEscape(id) }

func (rm *Remote) SnapshotPlan(ctx context.Context, stack, service string) (engine.SnapshotPlan, error) {
	var out engine.SnapshotPlan
	err := rm.call(ctx, http.MethodGet, p(stack)+"/snapshot-plan?service="+url.QueryEscape(service), nil, &out)
	return out, err
}

func (rm *Remote) ListSnapshots(ctx context.Context, stack string) ([]stacksnap.Manifest, error) {
	var out []stacksnap.Manifest
	err := rm.call(ctx, http.MethodGet, p(stack)+"/snapshots", nil, &out)
	return out, err
}

func (rm *Remote) CreateSnapshot(ctx context.Context, stack string, req engine.SnapshotRequest) (engine.OpInfo, error) {
	var op engine.OpInfo
	err := rm.call(ctx, http.MethodPost, p(stack)+"/snapshots", req, &op)
	return op, err
}

func (rm *Remote) RestoreSnapshot(ctx context.Context, stack, id string, req engine.RestoreRequest) (engine.OpInfo, error) {
	var op engine.OpInfo
	err := rm.call(ctx, http.MethodPost, snapPath(stack, id)+"/restore", req, &op)
	return op, err
}

func (rm *Remote) DeleteSnapshot(ctx context.Context, stack, id string) error {
	return rm.call(ctx, http.MethodDelete, snapPath(stack, id), nil, nil)
}

func (rm *Remote) PinSnapshot(ctx context.Context, stack, id string, pinned bool) error {
	return rm.call(ctx, http.MethodPut, snapPath(stack, id)+"/pin", map[string]bool{"pinned": pinned}, nil)
}

func (rm *Remote) SnapshotConfig(ctx context.Context, stack, id string) (string, string, error) {
	var out struct{ Compose, Env string }
	err := rm.call(ctx, http.MethodGet, snapPath(stack, id)+"/config", nil, &out)
	return out.Compose, out.Env, err
}

// snapshotDownloadRate limits a download through the tunnel so it doesn't crowd out
// control requests (the tunnel interleaves streams, M18D; this keeps the uplink usable).
const snapshotDownloadRate = 8 << 20 // bytes per second

func (rm *Remote) DownloadSnapshot(ctx context.Context, stack, id string, w io.Writer) (stacksnap.Manifest, error) {
	var m stacksnap.Manifest
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	resp, err := rm.req(ctx, http.MethodGet, snapPath(stack, id)+"/download", nil)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	if mh := resp.Header.Get("X-Moorage-Manifest"); mh != "" {
		_ = json.Unmarshal([]byte(mh), &m)
	}
	_, err = io.Copy(w, &rateReader{r: resp.Body, rate: snapshotDownloadRate, start: time.Now()})
	return m, err
}

type rateReader struct {
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func (rr *rateReader) Read(b []byte) (int, error) {
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	n, err := rr.r.Read(b)
	rr.n += int64(n)
	want := time.Duration(float64(rr.n) / float64(rr.rate) * float64(time.Second))
	if d := want - time.Since(rr.start); d > 0 {
		time.Sleep(d)
	}
	return n, err
}

// ---- agent routes -------------------------------------------------------------------

func snapshotRoutes(mux *http.ServeMux, h Host, write func(http.HandlerFunc) http.HandlerFunc) {
	sn, ok := h.(Snapshotter)
	if !ok {
		return
	}
	mux.HandleFunc("GET /stacks/{name}/snapshot-plan", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(sn.SnapshotPlan(r.Context(), r.PathValue("name"), r.URL.Query().Get("service")))
	})
	mux.HandleFunc("GET /stacks/{name}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(sn.ListSnapshots(r.Context(), r.PathValue("name")))
	})
	mux.HandleFunc("POST /stacks/{name}/snapshots", write(func(w http.ResponseWriter, r *http.Request) {
		var req engine.SnapshotRequest
		if !agentRead(w, r, &req) {
			return
		}
		reply(w)(sn.CreateSnapshot(r.Context(), r.PathValue("name"), req))
	}))
	mux.HandleFunc("POST /stacks/{name}/snapshots/{id}/restore", write(func(w http.ResponseWriter, r *http.Request) {
		var req engine.RestoreRequest
		if !agentRead(w, r, &req) {
			return
		}
		reply(w)(sn.RestoreSnapshot(r.Context(), r.PathValue("name"), r.PathValue("id"), req))
	}))
	mux.HandleFunc("DELETE /stacks/{name}/snapshots/{id}", write(func(w http.ResponseWriter, r *http.Request) {
		if err := sn.DeleteSnapshot(r.Context(), r.PathValue("name"), r.PathValue("id")); err != nil {
			agentErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("PUT /stacks/{name}/snapshots/{id}/pin", write(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Pinned bool `json:"pinned"`
		}
		if !agentRead(w, r, &req) {
			return
		}
		if err := sn.PinSnapshot(r.Context(), r.PathValue("name"), r.PathValue("id"), req.Pinned); err != nil {
			agentErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /stacks/{name}/snapshots/{id}/config", func(w http.ResponseWriter, r *http.Request) {
		c, e, err := sn.SnapshotConfig(r.Context(), r.PathValue("name"), r.PathValue("id"))
		reply(w)(struct{ Compose, Env string }{c, e}, err)
	})
	mux.HandleFunc("GET /stacks/{name}/snapshots/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		name, id := r.PathValue("name"), r.PathValue("id")
		list, err := sn.ListSnapshots(r.Context(), name)
		if err != nil {
			agentErr(w, err)
			return
		}
		for _, m := range list {
			if m.ID != id {
				continue
			}
			mb, _ := json.Marshal(m)
			w.Header().Set("X-Moorage-Manifest", string(mb))
			w.Header().Set("Content-Type", "application/zstd")
			w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
			_, _ = sn.DownloadSnapshot(r.Context(), name, id, w)
			return
		}
		agentErr(w, engine.ErrNotFound)
	})
}
