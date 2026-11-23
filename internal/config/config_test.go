package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetricsToken(t *testing.T) {
	if v, err := metricsToken(""); err != nil || v != "" {
		t.Errorf("empty: %q %v (the endpoint must stay off)", v, err)
	}
	if _, err := metricsToken("short-token"); err == nil || !strings.Contains(err.Error(), "16") {
		t.Errorf("short token accepted: %v", err)
	}
	if v, err := metricsToken("0123456789abcdef"); err != nil || v != "0123456789abcdef" {
		t.Errorf("16 chars: %q %v", v, err)
	}
	for _, bad := range []string{"0123456789 abcdef", "0123456789abcdef\n", "\t0123456789abcdef"} {
		if _, err := metricsToken(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFromEnvMetricsToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOORAGE_DATA_DIR", dir+"/data")
	t.Setenv("MOORAGE_STACKS_DIR", dir+"/stacks")
	t.Setenv("MOORAGE_METRICS_TOKEN", "too-short")
	if _, err := FromEnv(); err == nil {
		t.Fatal("short MOORAGE_METRICS_TOKEN accepted")
	}
	t.Setenv("MOORAGE_METRICS_TOKEN", "a-long-enough-metrics-token")
	c, err := FromEnv()
	if err != nil || c.MetricsToken != "a-long-enough-metrics-token" {
		t.Fatalf("%v %q", err, c.MetricsToken)
	}
}

// MOORAGE_PROBES defaults to on, so agents help multi-location checks without extra setup.
func TestFromEnvProbesDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOORAGE_DATA_DIR", dir+"/data")
	t.Setenv("MOORAGE_STACKS_DIR", dir+"/stacks")
	t.Setenv("MOORAGE_PROBES", "")
	if c, err := FromEnv(); err != nil || !c.Probes {
		t.Fatalf("default: %v %v", c.Probes, err)
	}
	t.Setenv("MOORAGE_PROBES", "false")
	if c, err := FromEnv(); err != nil || c.Probes {
		t.Fatalf("false: %v %v", c.Probes, err)
	}
	t.Setenv("MOORAGE_PROBES", "maybe")
	if _, err := FromEnv(); err == nil {
		t.Fatal("invalid MOORAGE_PROBES accepted")
	}
}

// MOORAGE_TEMPLATES_DIR defaults to <data dir>/templates and is not created.
func TestFromEnvTemplatesDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOORAGE_DATA_DIR", dir+"/data")
	t.Setenv("MOORAGE_STACKS_DIR", dir+"/stacks")
	t.Setenv("MOORAGE_TEMPLATES_DIR", "")
	c, err := FromEnv()
	if err != nil || c.TemplatesDir != filepath.Join(dir, "data", "templates") {
		t.Fatalf("default: %q %v", c.TemplatesDir, err)
	}
	if _, err := os.Stat(c.TemplatesDir); !os.IsNotExist(err) {
		t.Fatalf("the templates folder must not be created: %v", err)
	}
	t.Setenv("MOORAGE_TEMPLATES_DIR", dir+"/mine")
	if c, err := FromEnv(); err != nil || c.TemplatesDir != filepath.Join(dir, "mine") {
		t.Fatalf("set: %q %v", c.TemplatesDir, err)
	}
}
