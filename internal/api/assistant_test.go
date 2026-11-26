package api

import (
	"strings"
	"testing"
)

func TestAssistantMasking(t *testing.T) {
	env := "DB_PASS=s3cretvalue\nPORT=80\n"
	logs := "login ok with s3cretvalue\npassword=hunter22 user=x\nAuthorization: Bearer abc.def-ghi\npostgres://app:pw1234@db/app\nport 80"
	got := maskSecrets(logs, env)
	for _, leak := range []string{"s3cretvalue", "hunter22", "abc.def-ghi", "pw1234"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "port 80") {
		t.Errorf("short values must stay: %s", got)
	}
	if k := envKeyNames(env); len(k) != 2 || k[0] != "DB_PASS" {
		t.Errorf("%v", k)
	}
}

func TestLineDiffAndBase(t *testing.T) {
	d := lineDiff("a\nb\nc", "a\nB\nc")
	if d != "- b\n+ B\n" {
		t.Errorf("%q", d)
	}
	if baseMarker("x", "y") == baseMarker("x", "z") || !strings.HasPrefix(baseMarker("x", ""), "base:") {
		t.Error("baseMarker")
	}
}
