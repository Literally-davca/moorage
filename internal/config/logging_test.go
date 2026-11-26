package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewLogger(t *testing.T) {
	var b bytes.Buffer
	t.Setenv("MOORAGE_LOG_LEVEL", "warn")
	t.Setenv("MOORAGE_LOG_FORMAT", "json")
	l, warns := NewLogger(&b)
	l.Info("hidden")
	l.Warn("shown", "k", 1)
	if len(warns) != 0 || strings.Contains(b.String(), "hidden") || !strings.Contains(b.String(), `"msg":"shown"`) {
		t.Fatalf("%v %s", warns, b.String())
	}
	t.Setenv("MOORAGE_LOG_LEVEL", "loud")
	t.Setenv("MOORAGE_LOG_FORMAT", "xml")
	if _, warns := NewLogger(&b); len(warns) != 2 {
		t.Fatalf("%v", warns)
	}
}
