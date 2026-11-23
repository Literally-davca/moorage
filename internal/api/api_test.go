package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

func testServer(t *testing.T) http.Handler {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(db, log)
	if err != nil {
		t.Fatal(err)
	}
	return New(config.Config{}, a, db, nil, nil, nil, log, "test").Handler()
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGuards(t *testing.T) {
	h := testServer(t)
	csrf := map[string]string{csrfHeader: "1"}

	if w := do(h, "POST", "/api/login", `{}`, nil); w.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF header: got %d", w.Code)
	}
	if w := do(h, "GET", "/api/hosts/local/stacks", "", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("stacks without session: got %d", w.Code)
	}
	if w := do(h, "POST", "/api/hosts/local/stacks/x/actions/up", "", csrf); w.Code != http.StatusUnauthorized {
		t.Errorf("action without session: got %d", w.Code)
	}
	if w := do(h, "GET", "/api/hosts/local/stacks", "", map[string]string{"Cookie": cookieName + "=forged"}); w.Code != http.StatusUnauthorized {
		t.Errorf("forged session: got %d", w.Code)
	}
	if w := do(h, "POST", "/api/setup/start", `{"token":"wrong","username":"admin","password":"long-enough-pass"}`, csrf); w.Code != http.StatusUnauthorized {
		t.Errorf("setup with wrong token: got %d", w.Code)
	}
	w := do(h, "GET", "/", "", nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("index headers missing: %d %v", w.Code, w.Header())
	}
	if w := do(h, "GET", "/api/session", "", nil); !strings.Contains(w.Body.String(), `"setupRequired":true`) {
		t.Errorf("fresh instance should require setup: %s", w.Body)
	}
}

func TestValidateLogCheck(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	base := func() checkInput {
		return checkInput{
			Name: "log rule", Kind: monitor.KindLog, HostID: "local", Stack: "web",
			Interval: 30, Timeout: 5, Retries: 1,
			Settings: monitor.Settings{Pattern: "FATAL", Threshold: 2, Window: 300},
		}
	}
	in := base()
	if err := s.validateCheck(ctx, &in); err != nil {
		t.Fatalf("valid log check rejected: %v", err)
	}
	badRe := base()
	badRe.Settings.Regex = true
	badRe.Settings.Pattern = "[unclosed"
	if err := s.validateCheck(ctx, &badRe); err == nil || !strings.Contains(err.Error(), "Invalid regular expression") {
		t.Fatalf("invalid regex accepted: %v", err)
	}
	tooLong := base()
	tooLong.Settings.Pattern = strings.Repeat("x", 257)
	if err := s.validateCheck(ctx, &tooLong); err == nil || !strings.Contains(err.Error(), "256 characters") {
		t.Fatalf("overlong pattern accepted: %v", err)
	}
	noStack := base()
	noStack.Stack = ""
	if err := s.validateCheck(ctx, &noStack); err == nil {
		t.Fatal("missing stack accepted")
	}
}

func TestValidateCheckLocations(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.CreateJoinToken(ctx, "h", "wd1", time.Now().Add(time.Hour))
	if _, err := st.RedeemJoinToken(ctx, "h", "ag1", "wd1"); err != nil {
		t.Fatal(err)
	}
	s := &Server{reg: host.NewRegistry(host.Local{}, "box", false, st)}
	base := func(locs []string, quorum string) checkInput {
		return checkInput{Name: "site", Kind: monitor.KindHTTP, Target: "https://x", Interval: 60, Timeout: 10, Retries: 1,
			Locations: locs, Quorum: quorum}
	}
	// hub only is stored as no locations, so the check runs exactly as before
	in := base([]string{"hub"}, "any")
	if err := s.validateCheck(ctx, &in); err != nil || in.Locations != nil || in.Quorum != "" {
		t.Fatalf("hub only: %v %v %q", err, in.Locations, in.Quorum)
	}
	// two locations default to majority; an offline agent may be picked; duplicates go
	in = base([]string{"hub", "ag1", "ag1", " "}, "")
	if err := s.validateCheck(ctx, &in); err != nil || len(in.Locations) != 2 || in.Quorum != monitor.QuorumMajority {
		t.Fatalf("two locations: %v %v %q", err, in.Locations, in.Quorum)
	}
	for _, bad := range []checkInput{
		base([]string{"hub", "ag1"}, "most"),
		base([]string{"nope"}, ""),
		base([]string{"local"}, ""), // the local Docker host is the hub itself
		base([]string{" "}, ""),
	} {
		if err := s.validateCheck(ctx, &bad); err == nil {
			t.Errorf("accepted %v %q", bad.Locations, bad.Quorum)
		}
	}
	// stack and log checks never run elsewhere
	sc := checkInput{Name: "web", Kind: monitor.KindStack, Target: "web", HostID: "local", Interval: 60, Timeout: 10, Retries: 1,
		Locations: []string{"hub", "ag1"}, Quorum: "any"}
	if err := s.validateCheck(ctx, &sc); err != nil || sc.Locations != nil || sc.Quorum != "" {
		t.Fatalf("stack check: %v %v %q", err, sc.Locations, sc.Quorum)
	}
}
