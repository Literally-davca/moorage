package api

import (
	"context"
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
	// M20F: only secret values are hidden; paths and generated-looking values are judged by value
	env = "DB_PASS=s3cretvalue\nDATA=/opt/stacks\nHOST_IP=192.168.1.10\nJWT=Xk29aLq81mZpR7tYv\nDB_URL=postgres://u:p4ss@db/x\n"
	want := "DB_PASS=********\nDATA=/opt/stacks\nHOST_IP=192.168.1.10\nJWT=********\nDB_URL=********\n"
	if got := maskEnvForModel(env); got != want {
		t.Errorf("maskEnvForModel =\n%s\nwant\n%s", got, want)
	}
	if got := maskEnvValues("volumes: [/opt/stacks:/opt/stacks]\npw: s3cretvalue", env); got != "volumes: [/opt/stacks:/opt/stacks]\npw: ********" {
		t.Errorf("maskEnvValues = %q", got)
	}
}

func TestSelfRemoves(t *testing.T) {
	ctx := context.Background()
	oldP, err1 := loadDraftProject(ctx, "moorage", "services:\n  moorage:\n    image: m\n    volumes: [/opt/stacks:/opt/stacks, /data:/data]\n    environment: [A=1]\n", "")
	newP, err2 := loadDraftProject(ctx, "moorage", "services:\n  moorage:\n    image: m\n    volumes: [/data:/data]\n    environment: [A=1]\n", "")
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	got := selfRemoves(oldP.Services["moorage"], newP.Services["moorage"])
	if len(got) != 1 || got[0] != "Removes the mount /opt/stacks:/opt/stacks" {
		t.Errorf("selfRemoves = %v", got)
	}
}

func TestApplyComposeEdits(t *testing.T) {
	cur := "services:\r\n  app:\r\n    # keep me\r\n    volumes:\r\n      - /a:/a\r\n"
	got, err := applyComposeEdits(cur, []composeEdit{{Find: "      - /a:/a", Replace: "      - /a:/a\n      - /b:/b"}})
	if err != nil || got != "services:\r\n  app:\r\n    # keep me\r\n    volumes:\r\n      - /a:/a\r\n      - /b:/b\r\n" {
		t.Fatalf("edit = %q, %v", got, err)
	}
	for _, bad := range []composeEdit{{Find: "  - /a"}, {Find: "      - /x:/x"}, {Find: ""}} {
		if _, err := applyComposeEdits(cur, []composeEdit{bad}); err == nil {
			t.Errorf("find %q must fail (not whole lines, missing or empty)", bad.Find)
		}
	}
	if got, _ := applyComposeEdits("a\nb\nc\n", []composeEdit{{Find: "b", Replace: ""}}); got != "a\nc\n" {
		t.Errorf("delete line = %q", got)
	}
}

func TestLineDiffAndBase(t *testing.T) {
	d := lineDiff("a\r\nb\r\nc\r\nd\r\ne\r\nf\r\n", "a\nB\nc\nd\ne\nf\n")
	if d != "@@ line 1 @@\n  a\n- b\n+ B\n  c\n  d\n  e\n" { // M20F: CRLF = LF, unchanged lines are context
		t.Errorf("%q", d)
	}
	if baseMarker("x", "y") == baseMarker("x", "z") || !strings.HasPrefix(baseMarker("x", ""), "base:") {
		t.Error("baseMarker")
	}
}
