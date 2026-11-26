package host

import (
	"context"
	"net/http"
)

// SelfApplyInfo is implemented by hosts that report the helper's log of the last
// Apply & restart and let an admin dismiss its rollback banner (M18H). An older agent
// answers 404 (IsUnsupported).
type SelfApplyInfo interface {
	AckSelfResult(ctx context.Context) error
	SelfApplyLog(ctx context.Context) (string, error)
}

func (l Local) AckSelfResult(context.Context) error { return l.E.AckSelfResult() }

func (l Local) SelfApplyLog(context.Context) (string, error) { return l.E.SelfApplyLog() }

func (rm *Remote) AckSelfResult(ctx context.Context) error {
	return rm.call(ctx, http.MethodPost, "/self-apply/ack", nil, nil)
}

func (rm *Remote) SelfApplyLog(ctx context.Context) (string, error) {
	var out struct{ Log string }
	err := rm.call(ctx, http.MethodGet, "/self-apply/log", nil, &out)
	return out.Log, err
}

func selfApplyRoutes(mux *http.ServeMux, h Host, write func(http.HandlerFunc) http.HandlerFunc) {
	si, ok := h.(SelfApplyInfo)
	if !ok {
		return
	}
	mux.HandleFunc("POST /self-apply/ack", write(func(w http.ResponseWriter, r *http.Request) {
		if err := si.AckSelfResult(r.Context()); err != nil {
			agentErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /self-apply/log", func(w http.ResponseWriter, r *http.Request) {
		l, err := si.SelfApplyLog(r.Context())
		reply(w)(struct{ Log string }{l}, err)
	})
}
