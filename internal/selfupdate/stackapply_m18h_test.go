package selfupdate

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func TestCappedWriter(t *testing.T) {
	var b bytes.Buffer
	w := &cappedWriter{w: &b, max: 5}
	n, err := w.Write([]byte("hello world"))
	if err != nil || n != 11 || b.String() != "hello" {
		t.Fatalf("%d %v %q", n, err, b.String())
	}
}

func TestIgnoredPorts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  moorage:\n    image: m:1\n    network_mode: host\n    ports: [\"8080:5080\"]\n"), 0o644)
	why := ignoredPorts(context.Background(), StackApplyOptions{StackDir: dir, Project: "m", Service: "moorage"})
	if !strings.Contains(why, "network_mode: host ignores ports:") {
		t.Fatalf("%q", why)
	}
	if ignoredPorts(context.Background(), StackApplyOptions{StackDir: dir, Project: "m", Service: "other"}) != "" {
		t.Error("other service")
	}
}

func TestProbeEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	addr := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://"))
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	o := StackApplyOptions{ListenAddr: ":" + port, HTTPClient: srv.Client()}
	insp := container.InspectResponse{HostConfig: &container.HostConfig{}, NetworkSettings: &container.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{"proxy": {IPAddress: addr.Addr()}},
	}}
	checked, fail := probeEndpoints(context.Background(), o, insp, map[string]string{"proxy": "traefik"})
	if fail != "" || len(checked) != 1 {
		t.Fatalf("%v %q", checked, fail)
	}
	_, fail = probeEndpoints(context.Background(), o, insp, map[string]string{"edge": "caddy"})
	if fail != "Moorage left network edge that caddy uses" {
		t.Fatalf("%q", fail)
	}
}
