package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestComposeFactsNetworksAndHosts(t *testing.T) {
	sf, err := ComposeFacts(context.Background(), "app", "", `
services:
  web:
    image: nginx
    container_name: app-web
    links: ["cache:c"]
    external_links: ["broker:mq"]
    environment:
      DATABASE_URL: postgres://user:s3cret@db:5432/app
      BROKERS: kafka1:9092,kafka2:9092
      PLAIN: hello
      LOCAL: localhost:8080
      IP: 10.0.0.5:80
      TAGGED: ${PGHOST}:5432
    networks: [front, shared]
  cache:
    image: redis
networks:
  front: {}
  shared:
    external: true
    name: proxy-net
`, "PGHOST=pg-main\n")
	if err != nil {
		t.Fatal(err)
	}
	nets := map[string]bool{}
	for _, n := range sf.Networks {
		nets[n.Name] = n.External
	}
	if ext, ok := nets["proxy-net"]; !ok || !ext {
		t.Fatalf("external network by name: override missing: %+v", sf.Networks)
	}
	if ext, ok := nets["app_front"]; !ok || ext {
		t.Fatalf("own network should get the project prefix: %+v", sf.Networks)
	}
	var web ServiceFact
	for _, s := range sf.Services {
		if s.Name == "web" {
			web = s
		}
	}
	want := []string{"broker", "cache", "db", "kafka1", "kafka2", "pg-main"}
	if !reflect.DeepEqual(web.Hosts, want) {
		t.Fatalf("hosts: %v, want %v", web.Hosts, want)
	}
	if web.ContainerName != "app-web" || !reflect.DeepEqual(web.Networks, []string{"app_front", "proxy-net"}) {
		t.Fatalf("service: %+v", web)
	}
	// env values never leave the agent
	b, _ := json.Marshal(sf)
	if strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "hello") {
		t.Fatalf("facts leak env values: %s", b)
	}
}

func TestHostsInValue(t *testing.T) {
	cases := map[string][]string{
		"redis://cache:6379/0":     {"cache"},
		"db:5432":                  {"db"},
		"a:1, b:2;c:3":             {"a", "b", "c"},
		"just-a-word":              nil,
		"/var/run/x.sock":          nil,
		"db:99999":                 nil,
		"http://[::1]:80/":         {"::1"}, // dropped later by plausibleHost
		"amqp://guest:guest@mq//x": {"mq"},
	}
	for in, want := range cases {
		if got := HostsInValue(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func TestDepFactsReadsManagedStacks(t *testing.T) {
	e := newTestEngine(t)
	dir := e.cfg.StacksDir
	write := func(stack, file, body string) {
		if err := os.MkdirAll(filepath.Join(dir, stack), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, stack, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pg", "compose.yaml", "services:\n  db:\n    image: postgres\n")
	write("broken", "compose.yaml", "services:\n  x:\n    imagez: y\n")
	write("notastack", "readme.txt", "hi")
	f, err := e.DepFacts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Stacks) != 2 {
		t.Fatalf("stacks: %+v", f.Stacks)
	}
	for _, s := range f.Stacks {
		if s.Name == "broken" && (s.Error == "" || strings.Contains(s.Error, dir)) {
			t.Fatalf("broken stack error: %q", s.Error)
		}
		if s.Name == "pg" && (s.Error != "" || len(s.Services) != 1 || s.Networks[0].Name != "pg_default") {
			t.Fatalf("pg: %+v", s)
		}
	}
}
