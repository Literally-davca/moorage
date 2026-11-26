package lint

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func freePorts(taken ...int) func(int) (int, error) {
	used := map[int]bool{}
	for _, p := range taken {
		used[p] = true
	}
	return func(pref int) (int, error) {
		if pref == 0 {
			pref = 49152
		}
		for p := pref; p <= pref+1000 && p <= 65535; p++ {
			if !used[p] {
				used[p] = true
				return p, nil
			}
		}
		return 0, fmt.Errorf("no free port from %d", pref)
	}
}

func TestMaterializeMainFlow(t *testing.T) {
	compose := `services:
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: "@generate(password, 32)"
  app:
    image: app:1
    environment:
      - DB_PASS=${POSTGRES_PASSWORD}
      - API_KEY=@generate(hex)
    ports:
      - "${SERVICE_PORT_WEB:-8080}:80"
      - "@generate(port, 9000):9000"
`
	res, err := Materialize(compose, "TZ=Europe/Prague\nAPP_KEY=@generate(base64key)\n", GenOptions{FreePort: freePorts(8080)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("nothing changed")
	}
	if strings.Contains(res.Compose, "@generate") || strings.Contains(res.Env, "@generate") {
		t.Fatalf("keywords left:\n%s\n%s", res.Compose, res.Env)
	}
	for _, want := range []string{"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}", `- "API_KEY=${API_KEY}"`, `- "${SERVICE_PORT_APP}:9000"`, "${SERVICE_PORT_WEB:-8080}:80"} {
		if !strings.Contains(res.Compose, want) {
			t.Errorf("compose lacks %q:\n%s", want, res.Compose)
		}
	}
	env := parseEnvFile(res.Env)
	if len(env.vals["POSTGRES_PASSWORD"]) != 32 || len(env.vals["API_KEY"]) != 32 || !strings.HasPrefix(env.vals["APP_KEY"], "base64:") {
		t.Errorf("bad values: %v", env.vals)
	}
	if env.vals["SERVICE_PORT_WEB"] != "8081" || env.vals["SERVICE_PORT_APP"] != "9000" || env.vals["TZ"] != "Europe/Prague" {
		t.Errorf("ports/TZ: %v", env.vals)
	}
	for _, g := range res.Generated {
		if g.Key == "POSTGRES_PASSWORD" && g.Value != "" {
			t.Error("a secret leaked into the summary")
		}
		if g.Key == "SERVICE_PORT_WEB" && (g.Value != "8081" || g.Note == "") {
			t.Errorf("port summary %+v", g)
		}
	}
	// the result lints without errors and saving again changes nothing
	for _, is := range Lint(context.Background(), res.Compose, res.Env, Options{Name: "t"}) {
		if is.Severity == Error || is.Severity == DeployError {
			t.Errorf("lint after materialize: %+v", is)
		}
	}
	again, err := Materialize(res.Compose, res.Env, GenOptions{FreePort: freePorts()})
	if err != nil || again.Changed {
		t.Errorf("second save changed=%v err=%v", again.Changed, err)
	}
}

func TestMaterializeKeepsExistingValues(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres\n    environment:\n      POSTGRES_PASSWORD: \"@generate(password)\"\n      X: ${SERVICE_PASSWORD_DB}\n"
	res, err := Materialize(compose, "POSTGRES_PASSWORD=keep-me\nSERVICE_PASSWORD_DB=also\n", GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	env := parseEnvFile(res.Env)
	if env.vals["POSTGRES_PASSWORD"] != "keep-me" || env.vals["SERVICE_PASSWORD_DB"] != "also" || len(res.Generated) != 0 {
		t.Errorf("overwrote: %v %+v", env.vals, res.Generated)
	}
	if !strings.Contains(res.Compose, "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}") {
		t.Errorf("keyword not replaced:\n%s", res.Compose)
	}
}

func TestMaterializeCoolifyNames(t *testing.T) {
	compose := "services:\n  app:\n    image: x\n    environment:\n      A: ${SERVICE_PASSWORD_64_APP}\n      U: ${SERVICE_USER_APP}\n      URL: ${SERVICE_URL_APP_3000}\n"
	res, err := Materialize(compose, "", GenOptions{HostAddress: "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	env := parseEnvFile(res.Env)
	if len(env.vals["SERVICE_PASSWORD_64_APP"]) != 64 || len(env.vals["SERVICE_USER_APP"]) != 16 || env.vals["SERVICE_URL_APP_3000"] != "http://192.168.1.10:3000" {
		t.Errorf("%v", env.vals)
	}
	// managed by Coolify: untouched
	managed := compose + "    labels:\n      - coolify.managed=true\n"
	res, _ = Materialize(managed, "", GenOptions{})
	if res.Changed || res.Skipped == "" {
		t.Errorf("Coolify stack materialized: %+v", res)
	}
}

func TestMaterializeErrors(t *testing.T) {
	for _, c := range []struct{ compose, env string }{
		{"services:\n  a:\n    image: x\n    environment:\n      K: \"@generate(nope)\"\n", ""},
		{"services:\n  a:\n    image: x\n", "K=@generate(password, 2)\n"},
		{"services:\n  a:\n    image: x\n    ports:\n      - \"80:@generate(port)\"\n", ""},
		{"services:\n  a:\n    image: x\n    ports:\n      - \"@generate(port, 80):80\"\n", ""}, // no FreePort
	} {
		if _, err := Materialize(c.compose, c.env, GenOptions{}); err == nil {
			t.Errorf("no error for %q %q", c.compose, c.env)
		}
	}
	if _, err := Materialize("services:\n  a:\n    image: x\n    ports:\n      - \"@generate(port, 80):80\"\n", "", GenOptions{FreePort: func(int) (int, error) { return 0, fmt.Errorf("none") }}); err == nil {
		t.Error("no free port must fail the save")
	}
}

func TestLintGenerateRules(t *testing.T) {
	compose := `services:
  a:
    image: x:1
    command: ["run", "@generate(password)"]
    environment:
      P: "@generate(password)"
      Q: ${SERVICE_PASSWORD_A}
    ports:
      - "@generate(port, 8080):80"
`
	codes := map[string]int{}
	for _, is := range Lint(context.Background(), compose, "", Options{Name: "t"}) {
		codes[is.Code]++
		if is.Code == "missing-var" || is.Code == "schema" {
			t.Errorf("unexpected %+v", is)
		}
	}
	if codes["generate-misplaced"] != 1 || codes["generate-pending"] != 3 {
		t.Errorf("codes %v", codes)
	}
}

func TestRecipeFixAndFreePort(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:17\n    restart: unless-stopped\n  web:\n    image: lscr.io/linuxserver/jellyfin:10\n    environment:\n      - TZ=Europe/Prague\n    ports:\n      - \"8080:80\"\n"
	var recipe, taken *Issue
	issues := Lint(context.Background(), compose, "", Options{Name: "t", Running: []PortUse{{Port: 8080, Proto: "tcp", Owner: "jellyfin"}}})
	for i := range issues {
		switch {
		case issues[i].Code == "recipe-env" && issues[i].Service == "db":
			recipe = &issues[i]
		case issues[i].Code == "recipe-env" && issues[i].Service == "web":
			if strings.Contains(issues[i].Message, "TZ") {
				t.Errorf("TZ is already set: %s", issues[i].Message)
			}
		case issues[i].Code == "port-taken":
			taken = &issues[i]
		}
	}
	if recipe == nil || taken == nil || taken.Fix == nil {
		t.Fatalf("issues %+v", issues)
	}
	r, err := Apply(compose, "", *recipe.Fix)
	if err != nil || !strings.Contains(r.Compose, "    environment:\n      POSTGRES_PASSWORD: \"@generate(password)\"\n") {
		t.Fatalf("recipe fix: %v\n%s", err, r.Compose)
	}
	r, err = Apply(r.Compose, "", *taken.Fix)
	if err != nil || !strings.Contains(r.Compose, `- "@generate(port, 8080):80"`) {
		t.Fatalf("free port fix: %v\n%s", err, r.Compose)
	}
	m, err := Materialize(r.Compose, "", GenOptions{FreePort: freePorts(8080)})
	if err != nil {
		t.Fatal(err)
	}
	env := parseEnvFile(m.Env)
	if env.vals["SERVICE_PORT_WEB"] != "8081" || len(env.vals["POSTGRES_PASSWORD"]) != 32 {
		t.Errorf("%v", env.vals)
	}
	// list-style environment gets list entries
	r, err = Apply(compose, "", Fix{Kind: "add-env-generate", Service: "web", Keys: []string{"PUID==1000"}})
	if err != nil || !strings.Contains(r.Compose, "      - TZ=Europe/Prague\n      - PUID=1000\n") {
		t.Fatalf("list insert: %v\n%s", err, r.Compose)
	}
}

func TestParseRecipesRejects(t *testing.T) {
	for _, b := range []string{"recipes:\n  - image: x\n    bogus: 1\n", "recipes:\n  - image: x\n    env:\n      K: nope\n", "recipes:\n  - env: {K: password}\n", "recipes:\n  - image: x\n    env:\n      K: port\n"} {
		if _, err := ParseRecipes([]byte(b)); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
}

func TestSetServiceKey(t *testing.T) {
	compose := "services:\n  a:\n    image: x\n    mem_limit: 512m\n  b:\n    image: y\n    restart: always\n"
	r, err := Apply(compose, "", Fix{Kind: "set-mem-limit", Service: "a", Value: "1g"})
	if err != nil || !strings.Contains(r.Compose, "    mem_limit: 1g\n") {
		t.Fatalf("%v\n%s", err, r.Compose)
	}
	r, err = Apply(r.Compose, "", Fix{Kind: "set-restart-no", Service: "b"})
	if err != nil || !strings.Contains(r.Compose, "    restart: \"no\"\n") {
		t.Fatalf("%v\n%s", err, r.Compose)
	}
	r, err = Apply(r.Compose, "", Fix{Kind: "set-mem-limit", Service: "b", Value: "2g"})
	if err != nil || !strings.Contains(r.Compose, "  b:\n    mem_limit: 2g\n") {
		t.Fatalf("%v\n%s", err, r.Compose)
	}
	if _, err := Apply(compose, "", Fix{Kind: "set-mem-limit", Service: "a", Value: "1g; rm -rf"}); err == nil {
		t.Error("bad value accepted")
	}
}

func TestNetworkModePorts(t *testing.T) {
	compose := "services:\n  a:\n    image: x:1\n    network_mode: host\n    ports:\n      - \"8080:5080\"\n    restart: always\n  b:\n    image: y:1\n    network_mode: \"service:a\"\n    ports: [\"9000:9000\"]\n"
	codes := map[string]Issue{}
	for _, is := range Lint(context.Background(), compose, "", Options{Name: "t", Running: []PortUse{{Port: 8080, Proto: "tcp", Owner: "other"}}}) {
		codes[is.Code] = is
	}
	if codes["ports-ignored"].Severity != Warning || codes["ports-conflict-network"].Severity != DeployError {
		t.Fatalf("%v", codes)
	}
	if _, ok := codes["port-taken"]; ok {
		t.Error("ignored ports must not count as taken")
	}
	r, err := Apply(compose, "", *codes["ports-ignored"].Fix)
	if err != nil || strings.Contains(r.Compose, "8080:5080") || !strings.Contains(r.Compose, "    restart: always") {
		t.Fatalf("%v\n%s", err, r.Compose)
	}
}
