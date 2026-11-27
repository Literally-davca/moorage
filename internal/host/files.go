package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"moorage/internal/engine"
)

// StackFiler is implemented by hosts with the Files tab (M19M). An older agent answers
// 404 (IsUnsupported).
type StackFiler interface {
	ListStackFiles(ctx context.Context, stack string) ([]engine.StackFile, error)
	ReadStackFile(ctx context.Context, stack, path string) (string, error)
	WriteStackFile(ctx context.Context, stack, path, content, author string) error
	StackFileVersions(ctx context.Context, stack, path string) ([]string, error)
	StackFileVersion(ctx context.Context, stack, path, id string) (string, error)
}

func (l Local) ListStackFiles(_ context.Context, stack string) ([]engine.StackFile, error) {
	return l.E.ListStackFiles(stack)
}
func (l Local) ReadStackFile(_ context.Context, stack, p string) (string, error) {
	return l.E.ReadStackFile(stack, p)
}
func (l Local) WriteStackFile(_ context.Context, stack, p, content, author string) error {
	if l.ReadOnly {
		return ErrHostReadOnly
	}
	return l.E.WriteStackFile(stack, p, content, author)
}
func (l Local) StackFileVersions(_ context.Context, stack, p string) ([]string, error) {
	return l.E.StackFileVersions(stack, p)
}
func (l Local) StackFileVersion(_ context.Context, stack, p, id string) (string, error) {
	return l.E.StackFileVersion(stack, p, id)
}

func filePath(stack string) string { return "/stacks/" + url.PathEscape(stack) + "/files" }

func (rm *Remote) ListStackFiles(ctx context.Context, stack string) ([]engine.StackFile, error) {
	var out []engine.StackFile
	err := rm.call(ctx, http.MethodGet, filePath(stack), nil, &out)
	return out, err
}
func (rm *Remote) ReadStackFile(ctx context.Context, stack, p string) (string, error) {
	var out struct{ Content string }
	err := rm.call(ctx, http.MethodGet, filePath(stack)+"/content?path="+url.QueryEscape(p), nil, &out)
	return out.Content, err
}
func (rm *Remote) WriteStackFile(ctx context.Context, stack, p, content, author string) error {
	return rm.call(ctx, http.MethodPut, filePath(stack)+"/content", map[string]string{"path": p, "content": content, "author": author}, nil)
}
func (rm *Remote) StackFileVersions(ctx context.Context, stack, p string) ([]string, error) {
	var out []string
	err := rm.call(ctx, http.MethodGet, filePath(stack)+"/versions?path="+url.QueryEscape(p), nil, &out)
	return out, err
}
func (rm *Remote) StackFileVersion(ctx context.Context, stack, p, id string) (string, error) {
	var out struct{ Content string }
	err := rm.call(ctx, http.MethodGet, filePath(stack)+"/versions/"+url.PathEscape(id)+"?path="+url.QueryEscape(p), nil, &out)
	return out.Content, err
}

func fileRoutes(mux *http.ServeMux, h Host, write func(http.HandlerFunc) http.HandlerFunc) {
	sf, ok := h.(StackFiler)
	if !ok {
		return
	}
	mux.HandleFunc("GET /stacks/{name}/files", func(w http.ResponseWriter, r *http.Request) {
		l, err := sf.ListStackFiles(r.Context(), r.PathValue("name"))
		reply(w)(l, err)
	})
	mux.HandleFunc("GET /stacks/{name}/files/content", func(w http.ResponseWriter, r *http.Request) {
		c, err := sf.ReadStackFile(r.Context(), r.PathValue("name"), r.URL.Query().Get("path"))
		reply(w)(struct{ Content string }{c}, err)
	})
	mux.HandleFunc("PUT /stacks/{name}/files/content", write(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Path, Content, Author string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*engine.MaxStackFile)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := sf.WriteStackFile(r.Context(), r.PathValue("name"), req.Path, req.Content, req.Author); err != nil {
			agentErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /stacks/{name}/files/versions", func(w http.ResponseWriter, r *http.Request) {
		v, err := sf.StackFileVersions(r.Context(), r.PathValue("name"), r.URL.Query().Get("path"))
		reply(w)(v, err)
	})
	mux.HandleFunc("GET /stacks/{name}/files/versions/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, err := sf.StackFileVersion(r.Context(), r.PathValue("name"), r.URL.Query().Get("path"), r.PathValue("id"))
		reply(w)(struct{ Content string }{c}, err)
	})
}
