package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"time"
)

// ---- access log (M18I) ------------------------------------------------------------
//
// Every API request is logged at Debug; answers 5xx at Error with the method, the
// route pattern (never the query or the body), host, stack, status, duration and the
// error that caused it. Requests the client cancelled (SSE, WebSockets closing) are not
// errors. Handlers report the cause through engErr / authErr instead of logging alone,
// so one failure is one line with its context.

type accessWriter struct {
	http.ResponseWriter
	code int
	err  error
}

func (w *accessWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps streaming responses (SSE) working through the wrapper.
func (w *accessWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack keeps WebSocket upgrades (terminals, live updates) working through the wrapper.
func (w *accessWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		w.code = http.StatusSwitchingProtocols
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

// reportErr hands the cause of a 5xx answer to the access log. It returns false when w
// isn't wrapped (tests, other listeners): the caller logs it itself.
func reportErr(w http.ResponseWriter, err error) bool {
	for {
		if aw, ok := w.(*accessWriter); ok {
			aw.err = err
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		aw := &accessWriter{ResponseWriter: w}
		next.ServeHTTP(aw, r)
		code := aw.code
		if code == 0 {
			code = http.StatusOK
		}
		route := r.Pattern
		if route == "" {
			route = r.Method + " " + r.URL.Path
		}
		args := []any{"route", route, "status", code, "ms", time.Since(start).Milliseconds()}
		if h := r.PathValue("host"); h != "" {
			args = append(args, "host", h)
		}
		if st := r.PathValue("name"); st != "" {
			args = append(args, "stack", st)
		}
		if code >= 500 && r.Context().Err() == nil {
			if aw.err != nil {
				args = append(args, "err", aw.err)
			}
			s.log.Error("request failed", args...)
			return
		}
		s.log.Debug("request", args...)
	})
}
