package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The logo preview fetches <base>/<role>.png through Moorage (the page's CSP blocks
// external images) and passes only PNGs.
func TestNotifyLogoPreview(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/logos/hub.png":
			_, _ = w.Write(png)
		case "/logos/agent.png":
			_, _ = w.Write([]byte("<html>not a png</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	s := &Server{}
	get := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.notifyLogo(rec, httptest.NewRequest(http.MethodGet, "/api/notifications/logo?"+q, nil))
		return rec
	}
	base := url.QueryEscape(up.URL + "/logos/")
	if rec := get("role=hub&base=" + base); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("hub: %d %s", rec.Code, rec.Body)
	}
	if rec := get("role=agent&base=" + base); rec.Code != http.StatusBadGateway {
		t.Fatalf("non-PNG must be refused, got %d", rec.Code)
	}
	if rec := get("role=watchdog&base=" + base); rec.Code != http.StatusBadGateway {
		t.Fatalf("404 must be reported, got %d", rec.Code)
	}
	if rec := get("role=../x&base=" + base); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role, got %d", rec.Code)
	}
	if rec := get("role=hub&base=" + url.QueryEscape("file:///etc")); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-http base, got %d", rec.Code)
	}
}
