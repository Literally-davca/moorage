package host

import (
	"context"
	"net/http"
	"net/url"

	"moorage/internal/engine"
)

// ContainerManager is implemented by hosts that list every container (compose or not)
// and run actions on containers and compose projects outside the stacks folder
// (MOORAGE_MANAGE_EXTERNAL). Listing is read-only and always allowed; actions are refused
// by the host itself unless its env allows them.
type ContainerManager interface {
	Containers(ctx context.Context) ([]engine.ContainerInfo, error)
	ContainerAction(ctx context.Context, id, action, author string) (engine.OpInfo, error)
	ExternalStackAction(ctx context.Context, project, action, author string) (engine.OpInfo, error)
}

// RegistryReceiver receives private Docker registry credentials over the mTLS tunnel.
type RegistryReceiver interface {
	SetRegistries(ctx context.Context, list []engine.RegistryLogin) error
}

func (l Local) SetRegistries(_ context.Context, list []engine.RegistryLogin) error {
	return l.E.SetManagedRegistries(list)
}

func (rm *Remote) SetRegistries(ctx context.Context, list []engine.RegistryLogin) error {
	return rm.call(ctx, http.MethodPut, "/registries", list, nil)
}

// ---- local --------------------------------------------------------------------------

func (l Local) Containers(ctx context.Context) ([]engine.ContainerInfo, error) {
	return l.E.AllContainers(ctx)
}

func (l Local) ContainerAction(ctx context.Context, id, action, _ string) (engine.OpInfo, error) {
	if l.ReadOnly {
		return engine.OpInfo{}, ErrHostReadOnly
	}
	op, err := l.E.ContainerAction(ctx, id, action)
	if err != nil {
		return engine.OpInfo{}, err
	}
	return op.Info(), nil
}

func (l Local) ExternalStackAction(ctx context.Context, project, action, _ string) (engine.OpInfo, error) {
	if l.ReadOnly {
		return engine.OpInfo{}, ErrHostReadOnly
	}
	op, err := l.E.ExternalStackAction(ctx, project, action)
	if err != nil {
		return engine.OpInfo{}, err
	}
	return op.Info(), nil
}

// ---- remote -------------------------------------------------------------------------

func (rm *Remote) Containers(ctx context.Context) ([]engine.ContainerInfo, error) {
	var out []engine.ContainerInfo
	err := rm.call(ctx, http.MethodGet, "/containers", nil, &out)
	return out, err
}

func (rm *Remote) ContainerAction(ctx context.Context, id, action, author string) (engine.OpInfo, error) {
	var op engine.OpInfo
	err := rm.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/actions/"+url.PathEscape(action)+"?author="+url.QueryEscape(author), nil, &op)
	return op, err
}

func (rm *Remote) ExternalStackAction(ctx context.Context, project, action, author string) (engine.OpInfo, error) {
	var op engine.OpInfo
	err := rm.call(ctx, http.MethodPost, "/external/"+url.PathEscape(project)+"/actions/"+url.PathEscape(action)+"?author="+url.QueryEscape(author), nil, &op)
	return op, err
}

// ---- agent routes ---------------------------------------------------------------------

func externalRoutes(mux *http.ServeMux, h Host, write func(http.HandlerFunc) http.HandlerFunc) {
	portRoutes(mux, h)
	cm, ok := h.(ContainerManager)
	if !ok {
		return
	}
	mux.HandleFunc("GET /containers", func(w http.ResponseWriter, r *http.Request) {
		reply(w)(cm.Containers(r.Context()))
	})
	mux.HandleFunc("POST /containers/{id}/actions/{action}", write(func(w http.ResponseWriter, r *http.Request) {
		reply(w)(cm.ContainerAction(r.Context(), r.PathValue("id"), r.PathValue("action"), r.URL.Query().Get("author")))
	}))
	mux.HandleFunc("POST /external/{name}/actions/{action}", write(func(w http.ResponseWriter, r *http.Request) {
		reply(w)(cm.ExternalStackAction(r.Context(), r.PathValue("name"), r.PathValue("action"), r.URL.Query().Get("author")))
	}))
	if rr, ok := h.(RegistryReceiver); ok {
		mux.HandleFunc("PUT /registries", func(w http.ResponseWriter, r *http.Request) {
			var list []engine.RegistryLogin
			if !agentRead(w, r, &list) {
				return
			}
			if err := rr.SetRegistries(r.Context(), list); err != nil {
				agentErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
}
