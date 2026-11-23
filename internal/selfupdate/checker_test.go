package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckerChannelsAndErrorsKeepCache(t *testing.T) {
	fxStable := makeSignedFixture(t, "1.1.0", "moorage-linux-amd64", "linux/amd64", []byte("bin-1.1.0"), "", "")
	fxPre := makeSignedFixture(t, "1.2.0-rc.1", "moorage-linux-amd64", "linux/amd64", []byte("bin-1.2.0-rc.1"), "", "")

	var mode atomic.Int32
	var seenUA atomic.Value
	var seenExtraHeaders atomic.Bool

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA.Store(r.UserAgent())
		for k := range r.Header {
			lk := strings.ToLower(k)
			if lk != "user-agent" && lk != "accept" && lk != "accept-encoding" {
				seenExtraHeaders.Store(true)
			}
		}
		if r.URL.RawQuery != "" {
			seenExtraHeaders.Store(true)
		}

		switch mode.Load() {
		case 1:
			http.NotFound(w, r)
			return
		case 2:
			w.Write([]byte("[]"))
			return
		case 3:
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(10*time.Minute).Unix()))
			http.Error(w, "rate limit", http.StatusForbidden)
			return
		case 4:
			time.Sleep(100 * time.Millisecond)
			http.Error(w, "slow", http.StatusGatewayTimeout)
			return
		}

		switch r.URL.Path {
		case "/releases":
			rels := []map[string]any{
				{
					"tag_name":   "v1.2.0-rc.1",
					"prerelease": true,
					"html_url":   srv.URL + "/tag/v1.2.0-rc.1",
					"body":       "RC release notes",
					"assets": []map[string]string{
						{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/assets/1.2.0-rc.1/SHA256SUMS"},
						{"name": "SHA256SUMS.sig", "browser_download_url": srv.URL + "/assets/1.2.0-rc.1/SHA256SUMS.sig"},
						{"name": "images.json", "browser_download_url": srv.URL + "/assets/1.2.0-rc.1/images.json"},
					},
				},
				{
					"tag_name":   "v1.1.0",
					"prerelease": false,
					"html_url":   srv.URL + "/tag/v1.1.0",
					"body":       "Stable release notes",
					"assets": []map[string]string{
						{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/assets/1.1.0/SHA256SUMS"},
						{"name": "SHA256SUMS.sig", "browser_download_url": srv.URL + "/assets/1.1.0/SHA256SUMS.sig"},
						{"name": "images.json", "browser_download_url": srv.URL + "/assets/1.1.0/images.json"},
					},
				},
			}
			json.NewEncoder(w).Encode(rels)
		case "/assets/1.1.0/SHA256SUMS":
			w.Write(fxStable.sums)
		case "/assets/1.1.0/SHA256SUMS.sig":
			w.Write(fxStable.sigB64)
		case "/assets/1.1.0/images.json":
			w.Write(fxStable.imagesJSON)
		case "/assets/1.2.0-rc.1/SHA256SUMS":
			w.Write(fxPre.sums)
		case "/assets/1.2.0-rc.1/SHA256SUMS.sig":
			w.Write(fxPre.sigB64)
		case "/assets/1.2.0-rc.1/images.json":
			w.Write(fxPre.imagesJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Note: fxStable and fxPre use separate random keys unless paired; give Checker a shared key!
	// Use fxStable's keypair for both in this channel test by re-signing fxPre? Wait: instead, test stable first with fxStable's key, then create pre-checker with fxPre's key!
	chkStable := NewChecker(srv.URL+"/releases", "stable", "1.0.0", fxStable.pub)
	chkStable.Client = srv.Client()

	st, err := chkStable.Refresh(context.Background())
	if err != nil {
		t.Fatalf("stable refresh failed: %v", err)
	}
	if st.Release == nil || st.Release.Version != "1.1.0" {
		t.Fatalf("expected stable 1.1.0 (skipping pre-release), got %+v", st.Release)
	}
	if gotUA, _ := seenUA.Load().(string); gotUA != "Moorage/1.0.0" {
		t.Fatalf("unexpected User-Agent %q", gotUA)
	}
	if seenExtraHeaders.Load() {
		t.Fatal("unexpected telemetry headers or query string sent")
	}

	// Simulate 404, empty list, rate limit, timeout: cached 1.1.0 must be preserved!
	for _, code := range []int32{1, 2, 3, 4} {
		mode.Store(code)
		ctx := context.Background()
		var cancel context.CancelFunc
		if code == 4 {
			ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
		}
		stErr, rerr := chkStable.Refresh(ctx)
		if cancel != nil {
			cancel()
		}
		if rerr == nil {
			t.Fatalf("expected refresh failure in mode %d", code)
		}
		if stErr.Release == nil || stErr.Release.Version != "1.1.0" {
			t.Fatalf("cached release lost during error mode %d", code)
		}
		if stErr.Error == "" {
			t.Fatalf("expected error text in CheckState for mode %d", code)
		}
	}

	// Pre channel picks 1.2.0-rc.1
	mode.Store(0)
	chkPre := NewChecker(srv.URL+"/releases", "pre", "1.0.0", fxPre.pub)
	chkPre.Client = srv.Client()
	stPre, err := chkPre.Refresh(context.Background())
	if err != nil || stPre.Release == nil || stPre.Release.Version != "1.2.0-rc.1" {
		t.Fatalf("expected pre-release 1.2.0-rc.1, got rel=%+v err=%v", stPre.Release, err)
	}
}
