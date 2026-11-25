package config

import (
	"path/filepath"
	"testing"
)

func TestFromEnvWebUIAndSocket(t *testing.T) {
	dir := t.TempDir()
	baseEnv := func() {
		t.Setenv("MOORAGE_DATA_DIR", filepath.Join(dir, "data"))
		t.Setenv("MOORAGE_STACKS_DIR", filepath.Join(dir, "stacks"))
	}

	// 1. Neither MOORAGE_LISTEN nor MOORAGE_WEB_UI set -> headless CLI-only mode
	baseEnv()
	t.Setenv("MOORAGE_LISTEN", "")
	t.Setenv("MOORAGE_WEB_UI", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.WebUI || c.Listen != "" {
		t.Fatalf("expected headless CLI-only when unset, got WebUI=%v Listen=%q", c.WebUI, c.Listen)
	}
	if c.SocketPath != filepath.Join(dir, "data", "moorage.sock") {
		t.Fatalf("expected default socket path, got %q", c.SocketPath)
	}

	// 2. MOORAGE_LISTEN=:5080 set -> Web UI auto-enabled
	baseEnv()
	t.Setenv("MOORAGE_LISTEN", ":5080")
	t.Setenv("MOORAGE_WEB_UI", "")
	c, err = FromEnv()
	if err != nil || !c.WebUI || c.Listen != ":5080" {
		t.Fatalf("expected WebUI on :5080, got WebUI=%v Listen=%q err=%v", c.WebUI, c.Listen, err)
	}

	// 3. MOORAGE_WEB_UI=false wins over MOORAGE_LISTEN
	baseEnv()
	t.Setenv("MOORAGE_LISTEN", ":5080")
	t.Setenv("MOORAGE_WEB_UI", "false")
	c, err = FromEnv()
	if err != nil || c.WebUI || c.Listen != "" {
		t.Fatalf("expected MOORAGE_WEB_UI=false to disable HTTP listener, got WebUI=%v Listen=%q err=%v", c.WebUI, c.Listen, err)
	}

	// 4. MOORAGE_WEB_UI=true without MOORAGE_LISTEN defaults to :5080
	baseEnv()
	t.Setenv("MOORAGE_LISTEN", "")
	t.Setenv("MOORAGE_WEB_UI", "true")
	c, err = FromEnv()
	if err != nil || !c.WebUI || c.Listen != ":5080" {
		t.Fatalf("expected MOORAGE_WEB_UI=true to default :5080, got WebUI=%v Listen=%q err=%v", c.WebUI, c.Listen, err)
	}

	// 5. MOORAGE_SOCKET=off disables Unix socket
	baseEnv()
	t.Setenv("MOORAGE_SOCKET", "off")
	c, err = FromEnv()
	if err != nil || c.SocketPath != "" {
		t.Fatalf("expected empty SocketPath, got %q err=%v", c.SocketPath, err)
	}
}
