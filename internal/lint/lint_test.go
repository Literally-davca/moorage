package lint

import (
	"context"
	"strings"
	"testing"
)

func codes(is []Issue) map[string]string {
	out := map[string]string{}
	for _, i := range is {
		out[i.Code+"/"+i.Service] = i.Severity
	}
	return out
}

func TestLintRules(t *testing.T) {
	compose := `version: "3.8"
services:
  db:
    image: postgres:16
    ports:
      - "5432:5432"
    privileged: true
  web:
    image: nginx
    command: ["sh", "-c", "app --password=hunter22"]
    ports:
      - "8080:80"
  web2:
    image: nginx:1.27
    restart: always
    ports:
      - "8080:81"
  job:
    image: busybox:1.37
    environment:
      URL: postgres://u:${DB_PASS}@db/x
    restart: unless-stopped
`
	got := codes(Lint(context.Background(), compose, "UNUSED=1\n", Options{Name: "t", Running: []PortUse{{Port: 5432, Proto: "tcp", Owner: "other/pg"}}}))
	want := map[string]string{
		"version/":                Info,
		"no-restart/db":           Info,
		"no-restart/web":          Info,
		"secret-in-command/web":   Error,
		"unused-env/":             Info,
		"missing-var/":            DeployError,
		"db-port-public/db":       Warning,
		"privileged/db":           Warning,
		"latest-tag/web":          Info,
		"duplicate-port/web2":     Error,
		"port-taken/db":           DeployError,
	}
	for k, sev := range want {
		if got[k] != sev {
			t.Errorf("%s: got %q, want %q (all: %v)", k, got[k], sev, got)
		}
	}
	if got["secret-in-command/job"] != "" {
		t.Error("a ${VAR} password isn't a written-in secret")
	}
	if Blocking(Lint(context.Background(), compose, "", Options{}), false) == nil {
		t.Error("errors must block a save")
	}
	if c := codes(Lint(context.Background(), "services:\n  job:\n    command: x\n", "", Options{})); c["no-image/job"] != Error {
		t.Errorf("no image: %v", c)
	}
	ok := "services:\n  a:\n    image: nginx:1.27\n    restart: unless-stopped\n    environment:\n      X: ${X:-1}\n"
	is := Lint(context.Background(), ok, "", Options{})
	if b := Blocking(is, true); b != nil {
		t.Fatalf("clean file blocked: %+v", b)
	}
}

func TestMoveEnv(t *testing.T) {
	compose := `services:
  web:
    image: x
    environment:
      TZ: Europe/Prague # zone
      PORT: 3000
      REF: ${REF}
      ESC: "$$HOME"
      EMPTY:
  api:
    image: y
    environment:
      - TZ=Europe/Prague
      - PORT=4000
      - PASSTHROUGH
`
	r, err := MoveEnv(compose, "# keep me\nOTHER=1\n", false)
	if err != nil || !r.Changed {
		t.Fatalf("move: %v %+v", err, r)
	}
	for _, want := range []string{"TZ: ${TZ} # zone", "PORT: ${WEB_PORT}", "REF: ${REF}", `ESC: "$$HOME"`, `- "TZ=${TZ}"`, `- "PORT=${API_PORT}"`, "- PASSTHROUGH"} {
		if !strings.Contains(r.Compose, want) {
			t.Errorf("compose lacks %q:\n%s", want, r.Compose)
		}
	}
	for _, want := range []string{"# keep me\nOTHER=1\n", "TZ=Europe/Prague\n", "WEB_PORT=3000\n", "API_PORT=4000\n"} {
		if !strings.Contains(r.Env, want) {
			t.Errorf("env lacks %q:\n%s", want, r.Env)
		}
	}
	if strings.Count(r.Env, "TZ=") != 1 {
		t.Errorf("one shared TZ:\n%s", r.Env)
	}
	// conflict: kept in compose unless overwrite
	r, _ = MoveEnv("services:\n  a:\n    image: x\n    environment:\n      K: new\n", "K=old\n", false)
	if len(r.Conflicts) != 1 || r.Changed {
		t.Fatalf("conflict: %+v", r)
	}
	r, _ = MoveEnv("services:\n  a:\n    image: x\n    environment:\n      K: new\n", "K=old\n", true)
	if !strings.Contains(r.Env, "K=new") || strings.Contains(r.Env, "K=old") || !strings.Contains(r.Compose, "K: ${K}") {
		t.Fatalf("overwrite: %+v", r)
	}
}

func TestQuickFixes(t *testing.T) {
	compose := "version: '3'\nservices:\n  # the db\n  db:\n    image: postgres:16\n    ports:\n      - 5432:5432 # pg\n    command: run --password=s3cret\n"
	r, err := Apply(compose, "", Fix{Kind: "remove-version"})
	if err != nil || strings.Contains(r.Compose, "version") || !strings.Contains(r.Compose, "# the db") {
		t.Fatalf("remove-version: %v\n%s", err, r.Compose)
	}
	r, err = Apply(compose, "", Fix{Kind: "add-restart", Service: "db"})
	if err != nil || !strings.Contains(r.Compose, "  db:\n    restart: unless-stopped\n    image: postgres:16") {
		t.Fatalf("add-restart: %v\n%s", err, r.Compose)
	}
	r, err = Apply(compose, "", Fix{Kind: "bind-localhost", Service: "db"})
	if err != nil || !strings.Contains(r.Compose, `- "127.0.0.1:5432:5432" # pg`) {
		t.Fatalf("bind-localhost: %v\n%s", err, r.Compose)
	}
	r, err = Apply(compose, "A=1\n", Fix{Kind: "extract-secret", Service: "db"})
	if err != nil || !strings.Contains(r.Compose, "--password=${DB_SECRET}") || !strings.Contains(r.Env, "DB_SECRET=s3cret") {
		t.Fatalf("extract-secret: %v\n%s\n%s", err, r.Compose, r.Env)
	}
	r, err = Apply(compose, "A=1\n# c\nB=2\n", Fix{Kind: "remove-unused", Keys: []string{"A"}})
	if err != nil || r.Env != "# c\nB=2\n" {
		t.Fatalf("remove-unused: %v %q", err, r.Env)
	}
	r, err = Apply(compose, "", Fix{Kind: "add-mem-limit", Service: "db"})
	if err != nil || !strings.Contains(r.Compose, "  db:\n    mem_limit: 512m\n") {
		t.Fatalf("add-mem-limit: %v\n%s", err, r.Compose)
	}
	r, err = Apply(compose, "", Fix{Kind: "add-healthcheck", Service: "db"})
	if err != nil || !strings.Contains(r.Compose, "pg_isready") {
		t.Fatalf("add-healthcheck postgres: %v\n%s", err, r.Compose)
	}
	httpCompose := "services:\n  api:\n    image: nginx:latest\n    ports:\n      - \"8080:80\"\n"
	r, err = Apply(httpCompose, "", Fix{Kind: "add-healthcheck", Service: "api"})
	if err != nil || !strings.Contains(r.Compose, "http://127.0.0.1:80/") {
		t.Fatalf("add-healthcheck http: %v\n%s", err, r.Compose)
	}
	r, err = Apply(httpCompose, "", Fix{Kind: "pin-tag", Service: "api", TargetTag: "1.27.4"})
	if err != nil || !strings.Contains(r.Compose, "image: nginx:1.27.4") {
		t.Fatalf("pin-tag: %v\n%s", err, r.Compose)
	}
	r, err = Apply(compose, "A=1\n", Fix{Kind: "add-env-var", Keys: []string{"DB_PASS"}})
	if err != nil || !strings.Contains(r.Env, "DB_PASS=") {
		t.Fatalf("add-env-var: %v %q", err, r.Env)
	}
}
