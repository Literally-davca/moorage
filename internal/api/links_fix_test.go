package api

import (
	"strings"
	"testing"

	"moorage/internal/engine"
)

func TestRewritePortLine(t *testing.T) {
	compose := "services:\r\n  beszel:\r\n    ports:\r\n      - \"127.0.0.1:8090:8090\" # web\r\n      - 127.0.0.1:9000:9000/tcp\r\n"
	b := engine.PortBinding{IP: "127.0.0.1", Public: 8090, Private: 8090}
	out, err := rewritePortLine(compose, b, "100.87.100.2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "      - \"100.87.100.2:8090:8090\" # web\r\n") {
		t.Fatalf("vpn: %q", out)
	}
	out, err = rewritePortLine(compose, engine.PortBinding{IP: "127.0.0.1", Public: 9000, Private: 9000}, "")
	if err != nil || !strings.Contains(out, "      - 9000:9000/tcp\r\n") {
		t.Fatalf("all: %q %v", out, err)
	}
	if _, err := rewritePortLine("services:\n  a:\n    ports:\n      - ${PORT}:80\n", engine.PortBinding{IP: "127.0.0.1", Public: 80, Private: 80}, ""); err == nil || !strings.Contains(err.Error(), "variable") {
		t.Fatalf("variable: %v", err)
	}
}
